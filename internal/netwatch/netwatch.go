// Package netwatch следит за тем, чтобы мёртвый VPN не отрезал компьютер от
// владельца — и, если отрезал, разбирает завал сам.
//
// ЖИВОЙ СЛУЧАЙ, РАДИ КОТОРОГО НАПИСАНО (разбор 02.08.2026). Клиент Hiddify
// показывал «подключено», а sing-box внутри него был мёртв: интерфейс tun0
// остался в системе вместе с маршрутами и своим DNS-сервером, и весь трафик
// уходил в никуда. Компьютер пропал из приложения на 42 часа 52 минуты; агент
// за это время сделал 3316 попыток дозвониться и ни разу не сказал человеку,
// что происходит. Починить можно было одним движением — выключить VPN, — но
// сделать это было некому: доступ к машине шёл через тот же самый канал.
//
// ЧТО ДЕЛАЕТ СТОРОЖ. Заметив, что облака нет дольше пяти минут, он проверяет
// сам релей напрямую. Если релей не отвечает, а VPN-клиент запущен — VPN
// выключается, и проверка повторяется:
//
//	связь вернулась → виноват был VPN: оставляем выключенным и пишем владельцу
//	                  (сообщение уходит уже по вернувшемуся каналу);
//	не вернулась    → VPN был ни при чём: ВОЗВРАЩАЕМ его как было и молчим.
//
// ПОЧЕМУ ВОЗВРАТ ОБЯЗАТЕЛЕН. Иначе первое же падение интернета у провайдера
// стоило бы человеку выключенного VPN — при том, что VPN в этом не виноват.
// Правило: сторож имеет право оставить систему изменённой только тогда, когда
// изменение ДОКАЗАННО помогло.
//
// ПОЧЕМУ КРИТЕРИЙ — РЕЛЕЙ, А НЕ «ЕСТЬ ЛИ ИНТЕРНЕТ». У VPN-клиента почти всегда
// есть правила прямого доступа для местных адресов, поэтому проба до
// какого-нибудь публичного DNS проходит и при мёртвом туннеле. Цель у нас
// ровно одна — вернуть связь с владельцем, ей и меряем.
package netwatch

import (
	"context"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"tgcontrol/internal/dnsfallback"
)

const (
	// offlineGrace — сколько терпим отсутствие облака, прежде чем подозревать
	// сеть. Клиент релея переподключается с backoff до минуты, поэтому пять
	// минут — это уже точно не «моргнуло».
	offlineGrace = 5 * time.Minute
	// tickInterval — как часто сторож просыпается. Дёшево: обычный тик не
	// делает ни одного сетевого запроса, пока облако на месте.
	tickInterval = 30 * time.Second
	// cooldownAfterFix — пауза после удачного вмешательства.
	cooldownAfterFix = 30 * time.Minute
	// cooldownAfterMiss — пауза после неудачного: VPN оказался ни при чём,
	// значит и через минуту будет ни при чём. Не долбим систему.
	cooldownAfterMiss = 2 * time.Hour
	// probeTimeout — бюджет одной пробы.
	probeTimeout = 8 * time.Second
	// verifyInterval — как часто перепроверяем облако ДЕЛОМ, даже когда
	// релей-клиент считает себя подключённым.
	//
	// ЗАЧЕМ (боевой случай 17.08.2026). Сторож просыпался только по обрыву
	// управляющего канала, а тот в сбое не рвался: сокет висел «подключённым»,
	// при этом НОВЫЕ соединения и исходящие запросы к релею не проходили —
	// `stream dial relay: i/o timeout` с 04:03 до 04:12 и дальше, «уведомление
	// о лимите не ушло» в 04:06 и 04:21. Связь вернулась сама в 04:30. Простой
	// вышел не меньше 18 минут при пороге в 5, и сторож не увидел его вовсе:
	// каждый тик обнулял счётчик, потому что CloudConnected() отвечал «да».
	//
	// Одна проба раз в две минуты — это 30 запросов в час, мелочь против
	// суток недоступности.
	verifyInterval = 2 * time.Minute
	// doubtAfter — сколько подряд неудачных перепроверок нужно, чтобы перестать
	// верить сокету. Три подряд — это шесть минут: одиночный таймаут в счёт не
	// идёт, а настоящий обрыв ловится за то же время, что и раньше.
	doubtAfter = 3
)

// probeHosts — адреса для ответа на вопрос «интернет вообще есть?».
// Только IP и только 443: имя тут разрешать нечем (сломанный DNS — половина
// случаев, ради которых пакет и написан), а 443 открыт в любой сети.
var probeHosts = []string{
	"77.88.8.8:443", // Яндекс DNS — ближайший и самый доступный в РФ
	"8.8.8.8:443",   // Google
	"1.1.1.1:443",   // Cloudflare
}

// Action — что сторож сделал и чем это кончилось.
type Action struct {
	At     int64  `json:"at"`
	What   string `json:"what"`   // vpn_stopped | vpn_restored | manual_stop | manual_start
	Result string `json:"result"` // link_restored | still_offline | ok
	Text   string `json:"text"`   // фраза для человека
}

// Status — то, что видит приложение, `doctor` и агент на сервере.
type Status struct {
	Internet    bool    `json:"internet"`     // прямые пробы проходят
	Cloud       bool    `json:"cloud"`        // релей отвечает
	CheckedAt   int64   `json:"checked_at"`   // когда последний раз проверяли
	VPN         *VPN    `json:"vpn"`          // найденный VPN-клиент (nil — не найден)
	Watchdog    bool    `json:"watchdog"`     // сторож включён
	OfflineFrom int64   `json:"offline_from"` // с какого момента нет облака
	Last        *Action `json:"last_action"`  // последнее вмешательство
}

// Deps — то, чего пакет не знает сам: состояние облака, куда писать владельцу
// и включён ли сторож в настройках.
type Deps struct {
	CloudConnected func() bool
	RelayHealthURL func() string // например https://remotai.ru/health; пусто — устройство не привязано
	Notify         func(text string)
	Enabled        func() bool
	// OnRepair — отметка в отчёте самодиагностики (см. internal/selfheal).
	OnRepair func(text string)
}

var st struct {
	sync.Mutex
	deps        Deps
	last        *Action
	offlineFrom time.Time
	quietUntil  time.Time
	internet    bool
	cloud       bool
	checkedAt   time.Time
	// lastVerify/verifyFails — перепроверка облака делом при живом сокете.
	lastVerify  time.Time
	verifyFails int
	// firstVerifyFail — когда перепроверка перестала проходить.
	//
	// ⚠ ЭТО И ЕСТЬ НАЧАЛО ПРОСТОЯ, а не момент, когда мы наконец перестали
	// верить сокету. Считать простой от недоверия — значит складывать две
	// осторожности подряд: сначала doubtAfter × verifyInterval на подтверждение,
	// потом ещё offlineGrace сверху, итого около одиннадцати минут до первого
	// действия. Второй боевой обрыв 17.08.2026 длился 11:14:34–11:24:52, то есть
	// 10 минут 18 секунд, — в это окно сторож не успевал ВООБЩЕ, даже с уже
	// сделанной починкой по утреннему случаю. Подтверждение обязано стоить
	// времени только себе, а не отодвигать отсчёт.
	firstVerifyFail time.Time
}

// Start запускает сторожа. Зовётся один раз при старте агента.
func Start(d Deps) {
	st.Lock()
	st.deps = d
	st.Unlock()
	go loop()
}

// Snapshot — текущее состояние для API и CLI. Сеть здесь не трогаем: отдаём
// то, что известно с последней проверки, иначе опрос из приложения устраивал
// бы шквал проб.
func Snapshot() Status {
	st.Lock()
	defer st.Unlock()
	s := Status{
		Internet:  st.internet,
		Cloud:     st.cloud,
		CheckedAt: st.checkedAt.Unix(),
		Last:      st.last,
		VPN:       DetectVPN(),
	}
	if st.deps.Enabled != nil {
		s.Watchdog = st.deps.Enabled()
	}
	if !st.offlineFrom.IsZero() {
		s.OfflineFrom = st.offlineFrom.Unix()
	}
	if st.checkedAt.IsZero() {
		s.CheckedAt = 0
	}
	return s
}

// CheckNow проверяет сеть прямо сейчас и обновляет снимок. Возвращает
// «интернет есть» и «облако отвечает».
func CheckNow(ctx context.Context) (internet, cloud bool) {
	internet = probeInternet(ctx)
	cloud = probeRelay(ctx)
	st.Lock()
	st.internet, st.cloud, st.checkedAt = internet, cloud, time.Now()
	st.Unlock()
	return internet, cloud
}

func loop() {
	// Первый проход не сразу: агент только поднялся, релею нужно время на
	// первый коннект.
	time.Sleep(2 * time.Minute)
	for {
		tick()
		time.Sleep(tickInterval)
	}
}

// trustSocket — можно ли верить «подключён» от релей-клиента.
//
// Раз в verifyInterval проверяем облако настоящим запросом. Пока проходит —
// верим. Не прошло doubtAfter раз подряд — перестаём: сокет висит живым, а
// связи через него нет (боевой случай 17.08.2026, см. verifyInterval).
//
// Между перепроверками возвращаем последнее решение, поэтому обычный тик
// по-прежнему не делает ни одного сетевого запроса.
func trustSocket(d Deps) bool {
	st.Lock()
	due := time.Since(st.lastVerify) >= verifyInterval
	fails := st.verifyFails
	st.Unlock()

	if !due {
		return fails < doubtAfter
	}

	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout+2*time.Second)
	defer cancel()
	ok := probeRelay(ctx)

	st.Lock()
	st.lastVerify = time.Now()
	if ok {
		st.verifyFails = 0
		st.firstVerifyFail = time.Time{}
	} else {
		st.verifyFails++
		if st.verifyFails == 1 {
			// Простой начался ЗДЕСЬ, а не когда счётчик добежит до doubtAfter.
			st.firstVerifyFail = time.Now()
		}
	}
	fails = st.verifyFails
	st.Unlock()

	if !ok && fails == doubtAfter {
		log.Printf("[NETWATCH] релей-клиент считает себя подключённым, но облако не отвечает %d раза подряд — считаю связь потерянной", fails)
	}
	return fails < doubtAfter
}

func tick() {
	st.Lock()
	d := st.deps
	quiet := st.quietUntil
	st.Unlock()

	if d.CloudConnected == nil {
		return
	}
	if d.CloudConnected() && trustSocket(d) {
		st.Lock()
		st.offlineFrom = time.Time{}
		st.cloud = true
		// Живой канал до релея — это и есть доказательство выхода наружу, причём
		// более сильное, чем проба: он держится прямо сейчас. Без этой отметки
		// internet оставался false у КАЖДОГО здорового компьютера — снимок
		// заполняется только в CheckNow, а тот зовётся лишь когда облако уже
		// потеряно. Живой мак 10.08.2026 отдавал `internet:false, cloud:true,
		// checked_at:0`, и `doctor` печатал владельцу «интернет: нет», пока ping
		// и curl с той же машины проходили.
		st.internet = true
		st.checkedAt = time.Now()
		st.Unlock()
		return
	}

	st.Lock()
	if st.offlineFrom.IsZero() {
		// Если облако уже не отвечало на пробы — отсчёт ведём с ПЕРВОЙ такой
		// пробы: к этому моменту связи фактически не было, и подтверждение
		// недоверия сокету не должно стоить простою лишних минут (см.
		// firstVerifyFail). Иначе действие откладывалось до ~11 минут, а
		// боевые обрывы этого дня длились 10 и 18 минут.
		if !st.firstVerifyFail.IsZero() {
			st.offlineFrom = st.firstVerifyFail
		} else {
			st.offlineFrom = time.Now()
		}
	}
	since := st.offlineFrom
	st.Unlock()

	if time.Since(since) < offlineGrace || time.Now().Before(quiet) {
		return
	}
	if d.Enabled != nil && !d.Enabled() {
		return
	}

	// Есть ли вообще что выключать: без VPN-клиента сторожу делать нечего.
	vpn := DetectVPN()
	if vpn == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	internet, cloud := CheckNow(ctx)
	if cloud {
		// Релей отвечает по HTTP, а управляющего канала нет — это не сеть, а
		// наша связь (отозванная привязка, разрыв WS). VPN тут ни при чём.
		return
	}
	log.Printf("[NETWATCH] облака нет %s, релей не отвечает, интернет=%v — выключаю %s (pid %d)",
		time.Since(since).Round(time.Minute), internet, vpn.Name, vpn.PID)

	if err := StopVPN(vpn); err != nil {
		log.Printf("[NETWATCH] выключить %s не удалось: %v", vpn.Name, err)
		st.Lock()
		st.quietUntil = time.Now().Add(cooldownAfterMiss)
		st.Unlock()
		return
	}

	// Даём системе снять tun-интерфейс и вернуть маршруты.
	restored := false
	for i := 0; i < 4; i++ {
		time.Sleep(6 * time.Second)
		c2, cancel2 := context.WithTimeout(context.Background(), probeTimeout+2*time.Second)
		_, ok := CheckNow(c2)
		cancel2()
		if ok {
			restored = true
			break
		}
	}

	now := time.Now()
	if restored {
		text := "🔌 " + vpn.Name + " показывал «подключено», но связи через него не было — я его выключил, и компьютер снова на связи."
		if vpn.StartHint != "" {
			text += "\nВключить обратно: " + vpn.StartHint + " — или кнопкой «Включить VPN» в приложении (Настройки → Система)."
		}
		st.Lock()
		st.last = &Action{At: now.Unix(), What: "vpn_stopped", Result: "link_restored", Text: text}
		st.quietUntil = now.Add(cooldownAfterFix)
		d = st.deps
		st.Unlock()
		log.Printf("[NETWATCH] связь вернулась после выключения %s", vpn.Name)
		if d.OnRepair != nil {
			d.OnRepair("выключен зависший " + vpn.Name)
		}
		if d.Notify != nil {
			d.Notify(text)
		}
		return
	}

	// VPN оказался ни при чём — возвращаем как было. Иначе падение интернета у
	// провайдера стоило бы человеку выключенного VPN.
	back, err := StartVPN(vpn)
	st.Lock()
	st.quietUntil = now.Add(cooldownAfterMiss)
	if err != nil {
		st.last = &Action{At: now.Unix(), What: "vpn_stopped", Result: "still_offline",
			Text: "Связь не вернулась и после выключения " + vpn.Name + " — дело не в нём. Включить обратно не удалось: " + err.Error()}
	} else {
		st.last = &Action{At: now.Unix(), What: "vpn_restored", Result: "still_offline",
			Text: "Связи нет и без " + vpn.Name + " — значит дело не в нём. Вернул его как было (" + back + ")."}
	}
	txt := st.last.Text
	st.Unlock()
	log.Printf("[NETWATCH] %s", txt)
}

// probeInternet — есть ли выход наружу вообще. Пробы идут параллельно: первая
// удачная и есть ответ.
func probeInternet(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	res := make(chan bool, len(probeHosts))
	for _, addr := range probeHosts {
		go func(a string) {
			var d net.Dialer
			conn, err := d.DialContext(ctx, "tcp", a)
			if err == nil {
				conn.Close()
			}
			res <- err == nil
		}(addr)
	}
	for i := 0; i < len(probeHosts); i++ {
		select {
		case ok := <-res:
			if ok {
				return true
			}
		case <-ctx.Done():
			return false
		}
	}
	return false
}

// probeRelay — отвечает ли облако по HTTP. Тем же запасным резолвером, что и
// весь остальной трафик агента (мёртвый VPN оставляет в системе свой DNS).
func probeRelay(ctx context.Context) bool {
	st.Lock()
	d := st.deps
	st.Unlock()
	if d.RelayHealthURL == nil {
		return false
	}
	url := d.RelayHealthURL()
	if url == "" {
		// Устройство не привязано — облака у него нет по определению; тогда
		// «связь есть» = «интернет есть».
		return probeInternet(ctx)
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	client := &http.Client{
		Timeout:   probeTimeout,
		Transport: &http.Transport{DialContext: dnsfallback.DialContext, Proxy: http.ProxyFromEnvironment},
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode < 500
}

// StopNow / StartNow — ручное управление из приложения, CLI и с сервера.
func StopNow() (*VPN, error) {
	v := DetectVPN()
	if v == nil {
		return nil, ErrNoVPN
	}
	if err := StopVPN(v); err != nil {
		return v, err
	}
	st.Lock()
	st.last = &Action{At: time.Now().Unix(), What: "manual_stop", Result: "ok", Text: v.Name + " выключен по команде."}
	// Ручное выключение молчит сторожа: человек знает, что делает.
	st.quietUntil = time.Now().Add(cooldownAfterFix)
	st.Unlock()
	log.Printf("[NETWATCH] %s выключен по команде", v.Name)
	return v, nil
}

func StartNow() (string, error) {
	how, err := StartVPN(DetectVPNTarget())
	if err != nil {
		return "", err
	}
	st.Lock()
	st.last = &Action{At: time.Now().Unix(), What: "manual_start", Result: "ok", Text: "VPN запущен по команде (" + how + ")."}
	st.quietUntil = time.Now().Add(2 * time.Minute)
	st.Unlock()
	log.Printf("[NETWATCH] VPN запущен по команде (%s)", how)
	return how, nil
}
