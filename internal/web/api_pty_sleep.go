package web

import (
	"errors"
	"net/http"

	"tgcontrol/internal/pty"
)

// POST /api/pty/{id}/sleep — усыпить агента терминала (internal/pty/agent_sleep.go).
// Кнопка «Усыпить» в ряду инструментов терминала: решает человек, таймера нет.
func (s *Server) apiPtySleep(w http.ResponseWriter, r *http.Request, uid int64) {
	id := r.PathValue("id")
	rec, err := s.ptyManager.Sleep(id)
	if err != nil {
		writeSleepError(w, err)
		return
	}
	s.broadcastPtyListChanged(uid, "sleep", id)
	jsonResp(w, map[string]any{"ok": true, "sleep": rec})
}

// POST /api/pty/{id}/wake — забрать запись сна, чтобы набрать команду
// продолжения. Команду собирает клиент: аккаунт и прокси агента живут в его
// окружении и на сервере в текст команды не попадают.
func (s *Server) apiPtyWake(w http.ResponseWriter, r *http.Request, uid int64) {
	id := r.PathValue("id")
	rec, err := s.ptyManager.Wake(id)
	if err != nil {
		writeSleepError(w, err)
		return
	}
	jsonResp(w, map[string]any{"ok": true, "sleep": rec})
}

func writeSleepError(w http.ResponseWriter, err error) {
	var se *pty.SleepError
	if !errors.As(err, &se) {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	status := http.StatusConflict
	switch se.Code {
	case "not_found":
		status = http.StatusNotFound
	case "no_store", "kill_failed":
		status = http.StatusInternalServerError
	}
	jsonErrorCode(w, status, se.Code, se.Message, nil)
}
