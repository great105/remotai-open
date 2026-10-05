package relayhub

import (
	"math"
	"sync"
	"time"
)

// Присутствие клиента — «вопрос агента и так дойдёт до человека без Telegram».
//
// Нужно релейному notifier'у: вопрос не надо дублировать в Telegram, если
// пользователь и так в приложении и видит его на экране. Присутствием считается
// живой клиентский канал к устройству, который ДОВЕДЁТ вопрос до человека:
//   - control-WS  /v1/client/{id}/ws   (приложение открыто),
//   - сшитый стрим /v1/client/{id}/stream (открыт терминал или экран ПК).
//
// «Живой сокет» и «человек смотрит» — РАЗНЫЕ вещи, и это стоило нам дефекта:
// забытая вкладка remotai.ru/app держала присутствие вечно (сокет открыт, ping
// идёт), и уведомление «агент ждёт ответа» не приходило никогда. Поэтому клиент
// сообщает видимость кадрами hidden/visible, а хендлер control-WS на них снимает
// и возвращает присутствие, не разрывая сокет (server/ws_client.go). Кто кадров
// не шлёт — присутствует от коннекта до разрыва, как раньше: это и старые
// клиенты, и нативный APK (в фоне он показывает системное уведомление сам, так
// что второй сигнал в Telegram был бы лишним).
//
// Реестр in-memory, как и весь Hub: при горизонтальном масштабировании релея его
// придётся выносить в общий pub/sub (см. комментарий в начале hub.go).
//
// Чего этот реестр НЕ знает (осознанные дыры, закрывать не здесь):
//
//  1. Стрим в фоновой вкладке. /v1/client/{id}/stream отмечает присутствие на всё
//     время жизни моста, а вкладку с открытым терминалом можно свернуть — WS
//     терминала при этом остаётся открытым (PtyTermView реконнектит по возврату,
//     а не закрывается по уходу). Пока страница держит и control-WS, кадр hidden
//     присутствие control-канала снимает, но стрим продолжает его держать.
//     Правильное лечение — там же, в ws_stream.go: стрим должен считаться
//     присутствием только вместе с видимой страницей (общая для сокетов метка
//     страницы) либо не считаться вовсе.
//  2. Локальный клиент на самом ПК. Человек сидит за компьютером с открытым окном
//     Remotai — его WS идёт на localhost, релей о нём не знает и через 30 секунд
//     пишет в Telegram. Частично спасает переспрос состояния терминала перед
//     отправкой (notifier.ptyState: за это время на вопрос обычно уже ответили),
//     полностью — признак «к агенту прямо сейчас подключён локальный клиент» в
//     ответе GET /api/pty/{id}/state у агента (у web-сервера есть список
//     ws-соединений, internal/web/server.go: wsConns) + проверка этого признака
//     в notifier.fire рядом с проверкой присутствия.

// AwayForever — сколько «нет» клиента, которого хаб не видел ни разу с момента
// старта процесса. Явное значение вместо нуля: ноль означал бы «ушёл только
// что», и после рестарта релея notifier молчал бы, пока человек не зайдёт и не
// выйдет из приложения.
const AwayForever = time.Duration(math.MaxInt64)

// presenceKey — пара «устройство + пользователь». Именно пара, а не device:
// к одному ПК может быть допущено несколько аккаунтов (гранты), и присутствие
// одного из них не должно глушить уведомление другому.
type presenceKey struct {
	device string
	user   int64
}

// ClientAttach отмечает живой клиентский канал и возвращает detach. detach
// идемпотентен — его безопасно вызывать через defer и повторно.
//
// Пара «detach → снова ClientAttach» на одном и том же соединении — штатный
// сценарий (кадры видимости в ws_client.go): отметка «ушёл» ставится в момент
// detach, поэтому фоновая вкладка честно копит время отсутствия, а возврат
// обнуляет его. Notifier из этого получает бесплатную отсрочку: пока времени
// прошло меньше holdDelay, он переносит отправку, а не шлёт её.
func (h *Hub) ClientAttach(deviceID string, userID int64) func() {
	key := presenceKey{device: deviceID, user: userID}
	h.presMu.Lock()
	h.present[key]++
	delete(h.lastGone, key)
	h.presMu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			h.presMu.Lock()
			if n := h.present[key]; n > 1 {
				h.present[key] = n - 1
			} else {
				delete(h.present, key)
				h.lastGone[key] = time.Now()
			}
			h.presMu.Unlock()
		})
	}
}

// ClientActive — есть ли ПРЯМО СЕЙЧАС живой клиентский канал этой пары.
func (h *Hub) ClientActive(deviceID string, userID int64) bool {
	h.presMu.Lock()
	defer h.presMu.Unlock()
	return h.present[presenceKey{device: deviceID, user: userID}] > 0
}

// ClientAwayFor — сколько времени клиента нет. active=true → человек в
// приложении прямо сейчас (away=0). Для ни разу не виденной пары возвращается
// AwayForever.
func (h *Hub) ClientAwayFor(deviceID string, userID int64) (away time.Duration, active bool) {
	key := presenceKey{device: deviceID, user: userID}
	h.presMu.Lock()
	defer h.presMu.Unlock()
	if h.present[key] > 0 {
		return 0, true
	}
	if t, ok := h.lastGone[key]; ok {
		return time.Since(t), false
	}
	return AwayForever, false
}

// PrunePresence чистит отметки «ушёл» старше olderThan. Без джанитора карта
// растёт по числу пар (device,user) за всё время работы процесса — за месяц
// аптайма это тихий рост RSS. Возвращает число удалённых записей.
func (h *Hub) PrunePresence(olderThan time.Duration) int {
	cutoff := time.Now().Add(-olderThan)
	n := 0
	h.presMu.Lock()
	for k, t := range h.lastGone {
		if t.Before(cutoff) {
			delete(h.lastGone, k)
			n++
		}
	}
	h.presMu.Unlock()
	return n
}
