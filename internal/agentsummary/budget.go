package agentsummary

import (
	"sync"
	"time"
)

// Бюджет запросов к модели.
//
// ЧИСЛО, ИЗ КОТОРОГО ВСЁ СЛЕДУЕТ: у бесплатного роутера OpenRouter 50 запросов
// в сутки, пока счёт не пополняли (openrouter.KeyInfo.FreeDailyQuota). Агент за
// рабочий день заканчивает эпизоды десятки раз, и без бюджета выжимки съели бы
// дневную квоту к обеду — вместе с ней перестал бы работать и сам агент, если
// человек запускает его тем же ключом. Поэтому уведомления берут заведомо
// меньшую часть квоты и всегда умеют обойтись без модели.
const (
	// DefaultDailyLimit — сколько ПОВОДОВ в сутки максимум. Считаем поводы, а не
	// запросы: на один повод уходит до двух попыток (бесплатный роутер отвечает
	// через раз, см. internal/web/agent_summary.go), поэтому 20 поводов — это до
	// 40 запросов при квоте 50. Остаток обязан оставаться человеку: тем же
	// ключом он запускает агентов.
	DefaultDailyLimit = 20
	// DefaultPerPtyGap — не чаще одного раза на терминал. Агент, который
	// работает короткими заходами, иначе давал бы выжимку на каждый.
	DefaultPerPtyGap = 5 * time.Minute
	// rateLimitPause — сколько молчим после отказа по частоте. Долбиться в
	// исчерпанную квоту бессмысленно: она сбрасывается не раньше суток, а
	// каждый отказ — это ещё и задержка перед уведомлением.
	rateLimitPause = time.Hour
)

// Budget — счётчик на процесс агента. Только память: перезапуск сбрасывает,
// и это осознанно — врать про остаток чужой квоты мы всё равно не можем, а
// потолок нужен от шторма, а не от точного учёта.
type Budget struct {
	mu sync.Mutex

	dailyLimit int
	perPtyGap  time.Duration

	day    time.Time // начало суток, за которые считаем
	spent  int
	lastAt map[string]time.Time // терминал → когда для него звали модель
	pause  time.Time            // до этого момента молчим совсем
}

func NewBudget() *Budget {
	return &Budget{
		dailyLimit: DefaultDailyLimit,
		perPtyGap:  DefaultPerPtyGap,
		lastAt:     make(map[string]time.Time),
	}
}

// Allow — можно ли звать модель для этого терминала. Списывает запрос сразу:
// вызывающий обязан спрашивать ровно перед запросом, а неудачный запрос из
// квоты провайдера всё равно вычитается.
func (b *Budget) Allow(ptyID string, now time.Time) bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	if now.Before(b.pause) {
		return false
	}
	// Сутки считаем скользящим окном от первого запроса, а не по календарю:
	// часовые пояса и переводы времени к делу не относятся, а вот «ровно в
	// полночь всё обнулилось» дало бы всплеск ровно в тот момент, когда квота
	// провайдера ещё не сбросилась.
	if b.day.IsZero() || now.Sub(b.day) >= 24*time.Hour {
		b.day, b.spent = now, 0
		// Чистим отметки терминалов заодно: карта росла бы по числу сессий за
		// всё время жизни процесса.
		for id, at := range b.lastAt {
			if now.Sub(at) >= 24*time.Hour {
				delete(b.lastAt, id)
			}
		}
	}
	if b.spent >= b.dailyLimit {
		return false
	}
	if last, ok := b.lastAt[ptyID]; ok && now.Sub(last) < b.perPtyGap {
		return false
	}
	b.spent++
	b.lastAt[ptyID] = now
	return true
}

// NoteRateLimited — провайдер отказал по частоте: замолкаем на час.
func (b *Budget) NoteRateLimited(now time.Time) {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.pause = now.Add(rateLimitPause)
	b.mu.Unlock()
}

// Spent — сколько запросов ушло за текущие сутки (для диагностики и тестов).
func (b *Budget) Spent() int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.spent
}
