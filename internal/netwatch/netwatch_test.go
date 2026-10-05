package netwatch

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Здоровый компьютер обязан отвечать «интернет есть».
//
// РЕГРЕСС, КОТОРЫЙ ЭТО ЛОВИТ: снимок заполнялся только в CheckNow, а тот
// вызывается лишь после потери облака. У машины, которая ни разу не теряла
// связь, Internet навсегда оставался false, а CheckedAt — нулём. На живом маке
// 10.08.2026 API отдавал `internet:false, cloud:true, checked_at:0`, и `doctor`
// печатал владельцу «интернет: нет», пока ping и curl с той же машины
// проходили. Дефект был не платформенный: так вёл себя любой здоровый агент.
func TestTickMarksInternetWhileCloudConnected(t *testing.T) {
	restore := resetState(t)
	defer restore()

	st.Lock()
	st.deps = Deps{CloudConnected: func() bool { return true }}
	st.Unlock()

	tick()

	s := Snapshot()
	if !s.Internet {
		t.Error("Internet = false при живом облаке: канал до релея и есть доказательство выхода наружу")
	}
	if !s.Cloud {
		t.Error("Cloud = false, хотя CloudConnected() вернул true")
	}
	if s.CheckedAt == 0 {
		t.Error("CheckedAt = 0: снимок не датирован, человеку нечего показать про свежесть")
	}
}

// Потеря облака не должна выдавать интернет за живой: пока проба не сделана,
// прошлое «интернет есть» устаревает вместе с самим фактом связи.
func TestTickKeepsOfflineStartWhenCloudGone(t *testing.T) {
	restore := resetState(t)
	defer restore()

	st.Lock()
	st.deps = Deps{CloudConnected: func() bool { return false }}
	st.Unlock()

	tick()

	st.Lock()
	offlineFrom := st.offlineFrom
	st.Unlock()
	if offlineFrom.IsZero() {
		t.Error("offlineFrom не отмечен: без него не посчитать, сколько компьютера нет")
	}
}

// СОКЕТ МОЖЕТ ВРАТЬ — сторож обязан перепроверять облако делом.
//
// РЕГРЕСС, КОТОРЫЙ ЭТО ЛОВИТ (боевой случай 17.08.2026). Сторож просыпался
// только по обрыву управляющего канала. В сбое канал не рвался: сокет висел
// «подключённым», а новые соединения и исходящие запросы к релею не проходили
// (`stream dial relay: i/o timeout` с 04:03 до 04:12, «уведомление о лимите не
// ушло» в 04:06 и 04:21; связь вернулась сама в 04:30). Простой вышел не меньше
// 18 минут при пороге в 5, и сторож не увидел его вовсе: каждый тик обнулял
// счётчик, потому что CloudConnected() отвечал «да».
func TestTickDoubtsConnectedSocketWhenRelayUnreachable(t *testing.T) {
	restore := resetState(t)
	defer restore()

	st.Lock()
	st.deps = Deps{
		// Сокет уверяет, что всё хорошо…
		CloudConnected: func() bool { return true },
		// …а облака на самом деле нет: порт 1 отвечает отказом сразу.
		RelayHealthURL: func() string { return "http://127.0.0.1:1/health" },
	}
	st.Unlock()

	for i := 0; i < doubtAfter; i++ {
		// Между тиками сбрасываем таймер перепроверки: в бою они разнесены на
		// verifyInterval, а тесту ждать шесть минут незачем.
		st.Lock()
		st.lastVerify = time.Time{}
		st.Unlock()
		tick()
	}

	st.Lock()
	offlineFrom := st.offlineFrom
	st.Unlock()
	if offlineFrom.IsZero() {
		t.Fatalf("после %d неудачных перепроверок сторож всё ещё верит сокету: "+
			"простой не начал считаться, и вмешательства не будет никогда", doubtAfter)
	}
}

// И обратное: на ЗДОРОВОЙ связи сторож молчит.
//
// Без этой половины проверка была бы опасной: сторож, срабатывающий от одной
// неудачной пробы, начал бы выключать VPN у человека на ровном месте.
func TestTickTrustsConnectedSocketWhileRelayAnswers(t *testing.T) {
	restore := resetState(t)
	defer restore()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	st.Lock()
	st.deps = Deps{
		CloudConnected: func() bool { return true },
		RelayHealthURL: func() string { return srv.URL + "/health" },
	}
	st.Unlock()

	for i := 0; i < doubtAfter+2; i++ {
		st.Lock()
		st.lastVerify = time.Time{}
		st.Unlock()
		tick()
	}

	st.Lock()
	offlineFrom, fails := st.offlineFrom, st.verifyFails
	st.Unlock()
	if !offlineFrom.IsZero() {
		t.Error("сторож объявил простой при живом облаке — ложная тревога")
	}
	if fails != 0 {
		t.Errorf("счётчик неудач %d при отвечающем релее — он не сбрасывается", fails)
	}
}

// ПРОСТОЙ СЧИТАЕТСЯ С ПЕРВОЙ НЕУДАЧНОЙ ПРОБЫ, А НЕ С МОМЕНТА НЕДОВЕРИЯ.
//
// РЕГРЕСС, КОТОРЫЙ ЭТО ЛОВИТ (второй боевой обрыв 17.08.2026). Отсчёт от
// недоверия складывал две осторожности подряд: doubtAfter × verifyInterval на
// подтверждение (≈6 мин), и только потом offlineGrace (5 мин) — итого около
// одиннадцати минут до первого действия. Дневной обрыв длился 11:14:34–11:24:52,
// то есть 10 минут 18 секунд: сторож не успевал в него ВООБЩЕ — ни до починки
// по утреннему случаю, ни после неё. Записей [NETWATCH] за весь день нет ни
// одной, при двух обрывах.
func TestOfflineCountsFromFirstFailedProbe(t *testing.T) {
	restore := resetState(t)
	defer restore()

	st.Lock()
	st.deps = Deps{
		CloudConnected: func() bool { return true },
		RelayHealthURL: func() string { return "http://127.0.0.1:1/health" },
	}
	st.Unlock()

	// Первая неудачная проба.
	st.Lock()
	st.lastVerify = time.Time{}
	st.Unlock()
	tick()

	// Пробы разнесены на verifyInterval, и к моменту недоверия связи нет уже
	// несколько минут. Отматываем именно ПЕРВУЮ неудачу — так же, как это
	// выглядит в бою; ждать шесть минут тесту незачем.
	aged := offlineGrace + time.Minute
	st.Lock()
	st.firstVerifyFail = time.Now().Add(-aged)
	st.Unlock()

	for i := 1; i < doubtAfter; i++ {
		st.Lock()
		st.lastVerify = time.Time{}
		st.Unlock()
		tick()
	}

	st.Lock()
	offlineFrom := st.offlineFrom
	st.Unlock()
	if offlineFrom.IsZero() {
		t.Fatal("простой не начал считаться вовсе — сторож всё ещё верит сокету")
	}
	if time.Since(offlineFrom) < offlineGrace {
		t.Fatalf("простой отсчитан от недоверия, а не от первой неудачной пробы: "+
			"насчитано %s вместо ≥%s. Именно эта арифметика пропустила дневной "+
			"обрыв 11:14:34–11:24:52 (10 мин 18 с)",
			time.Since(offlineFrom).Round(time.Second), offlineGrace)
	}
}

// resetState изолирует тест от глобального состояния пакета.
func resetState(t *testing.T) func() {
	t.Helper()
	st.Lock()
	saved := struct {
		deps            Deps
		internet        bool
		cloud           bool
		checkedAt       time.Time
		offlineFrom     time.Time
		quietUntil      time.Time
		lastVerify      time.Time
		verifyFails     int
		firstVerifyFail time.Time
	}{st.deps, st.internet, st.cloud, st.checkedAt, st.offlineFrom, st.quietUntil,
		st.lastVerify, st.verifyFails, st.firstVerifyFail}
	st.deps = Deps{}
	st.internet, st.cloud = false, false
	st.checkedAt, st.offlineFrom = time.Time{}, time.Time{}
	// ⚠ ТИШИНА ДО КОНЦА ТЕСТА — не косметика, а защита боевой машины. Дальше в
	// tick() стоит DetectVPN() и StopVPN(): без этой отсечки тест на машине
	// владельца, где Hiddify запущен, ВЫКЛЮЧИЛ БЫ ЕМУ VPN по-настоящему.
	// Правило канона: опасное на стенде не проверяем вовсе.
	st.quietUntil = time.Now().Add(time.Hour)
	// Перепроверку тоже обнуляем: иначе тесты тянули бы друг другу счётчик
	// неудач и падали бы в зависимости от порядка запуска.
	st.lastVerify, st.verifyFails, st.firstVerifyFail = time.Time{}, 0, time.Time{}
	st.Unlock()

	return func() {
		st.Lock()
		st.deps = saved.deps
		st.internet, st.cloud = saved.internet, saved.cloud
		st.checkedAt, st.offlineFrom = saved.checkedAt, saved.offlineFrom
		st.quietUntil = saved.quietUntil
		st.lastVerify, st.verifyFails = saved.lastVerify, saved.verifyFails
		st.firstVerifyFail = saved.firstVerifyFail
		st.Unlock()
	}
}
