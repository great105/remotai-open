package auth

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Bucket — простой token bucket в памяти. Достаточно для одного инстанса relay.
// При горизонтальном масштабировании заменить на Redis или таблицу rate_limits.
type Bucket struct {
	capacity float64
	rate     float64 // tokens per second
	tokens   float64
	updated  time.Time
}

func (b *Bucket) Allow() bool {
	now := time.Now()
	elapsed := now.Sub(b.updated).Seconds()
	b.tokens = min(b.capacity, b.tokens+elapsed*b.rate)
	b.updated = now
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

// Limiter держит буфер per-key (например per-IP или per-user).
type Limiter struct {
	mu       sync.Mutex
	buckets  map[string]*Bucket
	rate     float64
	capacity float64
}

// NewLimiter: rate в "за минуту". capacity = burst.
func NewLimiter(perMinute, burst int) *Limiter {
	return &Limiter{
		buckets:  make(map[string]*Bucket),
		rate:     float64(perMinute) / 60.0,
		capacity: float64(burst),
	}
}

func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[key]
	if !ok {
		b = &Bucket{
			capacity: l.capacity,
			rate:     l.rate,
			tokens:   l.capacity,
			updated:  time.Now(),
		}
		l.buckets[key] = b
	}
	return b.Allow()
}

// Cleanup периодически выкидывает старые ключи. Запускать раз в N минут.
func (l *Limiter) Cleanup(maxAge time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	for k, b := range l.buckets {
		if now.Sub(b.updated) > maxAge {
			delete(l.buckets, k)
		}
	}
}

// ClientIP возвращает IP клиента. X-Forwarded-For / X-Real-IP учитываются ТОЛЬКО
// если прямой пир — доверенный локальный реверс-прокси (nginx терминирует TLS на
// том же хосте, поэтому RemoteAddr = loopback/private). Иначе клиент мог бы
// подделать заголовок и обойти per-IP rate-limit (единственная защита от
// брутфорса pairing-кодов).
func ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil && (ip.IsLoopback() || ip.IsPrivate()) {
		// X-Real-IP nginx ставит из $remote_addr — наиболее надёжно.
		if rip := strings.TrimSpace(r.Header.Get("X-Real-IP")); rip != "" {
			return rip
		}
		// Иначе берём ПРАВЫЙ элемент XFF — IP, который реально видел наш прокси
		// (левые элементы клиент может подделать).
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			if last := strings.TrimSpace(parts[len(parts)-1]); last != "" {
				return last
			}
		}
	}
	return host
}
