// Package dnsfallback дозванивается до хоста даже тогда, когда системный
// резолвер сломан.
//
// Живой случай 31.07–02.08.2026 (машина владельца): VPN-клиент (Hiddify)
// умер, но оставил в системе интерфейс tun0 со своим DNS-сервером
// 172.19.0.2. Маршрут наружу при этом работал — браузер на той же машине
// спокойно ходил на remotai.ru через собственный DoH, — а вот агент шёл
// системным резолвером и получал «lookup remotai.ru: no such host». Итог: 42
// часа 52 минуты без облака, 3316 неудачных попыток, попутно встало
// автообновление. Полтора десятка терминалов с работающими агентами всё это
// время были недоступны с телефона.
//
// Поэтому на ошибке РАЗРЕШЕНИЯ ИМЕНИ мы повторяем резолв через публичные DNS
// напрямую, минуя настройки системы, и подключаемся по полученному адресу.
// TLS этим не ослабляется: имя хоста для SNI и проверки сертификата берётся
// из URL, а не из того, что ответил резолвер (проверено замером: дозвон по
// подменённому IP падает с «x509: certificate is valid for … not remotai.ru»).
package dnsfallback

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"sync"
	"time"
)

// publicResolvers — куда идти, когда системный резолвер отказал. Cloudflare,
// Google и Quad9: три независимых оператора, чтобы отказ одного не значил
// ничего. Спрашиваем их ПАРАЛЛЕЛЬНО — в РФ часть из них деградирует, и
// последовательный обход съедал бы весь бюджет дозвона.
var publicResolvers = []string{"1.1.1.1:53", "8.8.8.8:53", "9.9.9.9:53"}

const (
	dialTimeout   = 15 * time.Second
	keepAlive     = 30 * time.Second
	resolveBudget = 3 * time.Second
	cacheTTL      = 5 * time.Minute
	// negativeTTL — короткая память о неудаче. Без неё каждый реконнект к
	// несуществующему имени (опечатка в relay_url) заново платил бы полный
	// бюджет опроса публичных резолверов.
	negativeTTL = 30 * time.Second
	// tlsPort — единственный порт, для которого мы готовы обойти системный
	// резолвер. По нешифрованному каналу это было бы опасно: имя, которое
	// внутренний DNS специально не отдаёт, публичный резолвер разрешит в чужой
	// адрес, и агент отправит туда device-JWT открытым текстом. Для TLS такой
	// беды нет — сертификат чужого адреса не сойдётся с именем.
	tlsPort = "443"
)

type cacheEntry struct {
	ips []string // пусто = отрицательный ответ
	at  time.Time
}

var cache = struct {
	sync.Mutex
	m map[string]cacheEntry
}{m: map[string]cacheEntry{}}

// sharedTransport — один транспорт на всех: у каждого свой пул соединений и
// свои idle-сокеты, а клиентов этих в коде несколько (проверка обновлений,
// скачивание, пейринг), и создаются они на каждый вызов.
var sharedTransport = sync.OnceValue(func() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = DialContext
	return t
})

// Transport — http.Transport с нашим дозвоном. Для всего, что ходит на
// remotai.ru: проверка обновлений, скачивание новой версии, пейринг. Иначе
// агент в таком состоянии не может ни выйти на связь, ни обновиться — то есть
// не может даже привезти себе исправление.
func Transport() *http.Transport { return sharedTransport() }

// DialContext — замена net.Dialer.DialContext, которую можно подставить в
// http.Transport или websocket.Dialer. Обычный путь не меняется вовсе: пока
// системный резолвер работает, это ровно тот же вызов.
func DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	d := &net.Dialer{Timeout: dialTimeout, KeepAlive: keepAlive}
	conn, err := d.DialContext(ctx, network, addr)
	if err == nil || !isNameError(err) || ctx.Err() != nil {
		return conn, err
	}

	host, port, splitErr := net.SplitHostPort(addr)
	if splitErr != nil || port != tlsPort || net.ParseIP(host) != nil {
		return nil, err
	}
	ips := lookupPublic(host)
	if len(ips) == 0 {
		return nil, err // возвращаем ИСХОДНУЮ ошибку: она точнее описывает беду
	}
	for _, ip := range ips {
		alt, dialErr := d.DialContext(ctx, network, net.JoinHostPort(ip, port))
		if dialErr == nil {
			log.Printf("[DNS] системный резолвер не знает %s — подключились через публичный DNS (%s)", host, ip)
			return alt, nil
		}
	}
	// Адреса из кэша больше не отвечают (переезд сервера, смена сети) —
	// забываем, чтобы следующая попытка спросила заново, а не повторяла путь к
	// мёртвому IP все пять минут.
	forget(host)
	return nil, err
}

// isNameError — это отказ РАЗРЕШЕНИЯ ИМЕНИ, а не отказ соединения. Важно не
// путать: при «connection refused» или таймауте соединения обходить системный
// резолвер незачем — адрес мы узнали, просто на той стороне никого нет.
func isNameError(err error) bool {
	var dnsErr *net.DNSError
	if !errors.As(err, &dnsErr) {
		return false
	}
	// Отмена контекста тоже приходит как DNSError — это не поломка резолвера.
	return dnsErr.IsNotFound || dnsErr.IsTemporary || dnsErr.IsTimeout
}

// lookupPublic спрашивает адрес у публичных DNS напрямую (PreferGo — свой
// резолвер Go, чтобы не уйти обратно в системный).
//
// Контекст здесь СОБСТВЕННЫЙ, а не от дозвона: у стрим-канала весь бюджет 15
// секунд, и если резолв умрёт вместе с ним, удачный ответ не попадёт в кэш —
// следующая попытка заплатит столько же и снова не успеет. Так и получалось
// бы «ПК на связи, а терминал не открывается никогда».
func lookupPublic(host string) []string {
	cache.Lock()
	if hit, ok := cache.m[host]; ok {
		ttl := cacheTTL
		if len(hit.ips) == 0 {
			ttl = negativeTTL
		}
		if time.Since(hit.at) < ttl {
			ips := hit.ips
			cache.Unlock()
			return ips
		}
	}
	cache.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), resolveBudget)
	defer cancel()

	type answer struct{ ips []string }
	results := make(chan answer, len(publicResolvers))
	for _, server := range publicResolvers {
		go func(server string) {
			r := &net.Resolver{
				PreferGo: true,
				Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
					d := net.Dialer{Timeout: resolveBudget}
					// Идём на конкретный сервер, а не на тот, что предложила
					// система: именно её настройки и сломаны.
					return d.DialContext(ctx, network, server)
				},
			}
			addrs, err := r.LookupHost(ctx, host)
			if err != nil {
				results <- answer{}
				return
			}
			results <- answer{ips: addrs}
		}(server)
	}

	for range publicResolvers {
		select {
		case a := <-results:
			if len(a.ips) > 0 {
				remember(host, a.ips)
				return a.ips
			}
		case <-ctx.Done():
			remember(host, nil)
			return nil
		}
	}
	remember(host, nil)
	return nil
}

func remember(host string, ips []string) {
	cache.Lock()
	defer cache.Unlock()
	if len(cache.m) > 64 {
		cache.m = map[string]cacheEntry{} // хостов у нас единицы; проще очистить целиком
	}
	cache.m[host] = cacheEntry{ips: ips, at: time.Now()}
}

func forget(host string) {
	cache.Lock()
	delete(cache.m, host)
	cache.Unlock()
}
