package web

import (
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"

	"github.com/gorilla/websocket"

	"tgcontrol/internal/connstat"
	"tgcontrol/internal/wsutil"
)

// classifyWSError превращает ошибку чтения WebSocket в короткую причину разрыва
// для диагностики стабильности. Главное — отличить «клиент сам закрылся» от
// «связь молча умерла по таймауту» (keepalive не дождался pong — признак плохой
// сети/туннеля) и от прочих сетевых ошибок.
func classifyWSError(err error) string {
	if err == nil {
		return "client-close"
	}
	if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
		return "client-close"
	}
	// Приватный код от relay-моста: дальнее (клиентское) плечо стрима упало
	// раньше локального — телефон ушёл в фон / закрыл вкладку / потерял сеть.
	// Отличаем это от настоящего обрыва туннеля или локального сбоя.
	var ce *websocket.CloseError
	if errors.As(err, &ce) && ce.Code == wsutil.CloseClientGone {
		return "client-gone"
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "timeout" // read deadline сработал — завис/мёртв клиент или туннель
	}
	if errors.Is(err, io.EOF) || websocket.IsUnexpectedCloseError(err) {
		return "connection-lost"
	}
	return "io-error: " + err.Error()
}

// GET /api/diag/connections?limit=N — срез стабильности WS-соединений (PTY +
// удалёнка): активные сейчас, разрывы за 5 минут с причинами, средний RTT и лента
// последних событий. Loopback-only: это диагностика для локального оператора, а
// не публичный эндпоинт. Без авторизации, потому что requireLoopback уже
// ограничивает доступ самой машиной.
func (s *Server) apiDiagConnections(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	jsonResp(w, struct {
		connstat.Snapshot
		RTC []map[string]any `json:"rtc,omitempty"`
	}{
		Snapshot: connstat.Default.Snapshot(limit),
		RTC:      rtcDiagSnapshot(),
	})
}
