package server

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"

	"tgcontrol-relay/internal/db"
)

// eventKinds — whitelist допустимых событий воронки. Всё остальное отбрасываем,
// чтобы публичный beacon не превратили в мусоросборник.
var eventKinds = map[string]bool{
	"landing_visit":    true,
	"landing_download": true,
	"app_open":         true,
	"pair_success":     true,
	"first_terminal":   true,
	// Главный шаг продукта: человек запустил AI-агента. До 30.08.2026 воронка
	// заканчивалась на «первом терминале», то есть не измеряла саму ценность.
	"first_agent":     true,
	"register_source": true,
}

// handlePostEvent — POST /v1/events. Публичный beacon аналитики (воронка).
// Авторизация опциональна: с валидным JWT/initData событие привязывается к
// user_id, без неё пишется анонимно (лендинг до регистрации) — 401 не бывает.
//
// Тело: { kind, source?, utm?, meta? }.
//
// Особый случай kind=register_source: с авторизацией проставляем
// users.source/utm_json (только если источник ещё пуст — первое касание не
// затирается) и пишем событие register; событие register_source отдельной
// строкой не нужно — вехой воронки является register.
func (s *Server) handlePostEvent(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Kind   string            `json:"kind"`
		Source string            `json:"source,omitempty"`
		UTM    map[string]string `json:"utm,omitempty"`
		Meta   map[string]string `json:"meta,omitempty"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<14)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if !eventKinds[req.Kind] {
		writeErr(w, http.StatusBadRequest, "unknown event kind")
		return
	}
	if len(req.Source) > 64 {
		req.Source = req.Source[:64]
	}

	// Опциональная авторизация: ошибка → аноним, а не 401.
	var userID *int64
	if claims, err := s.requireUserAuth(r); err == nil {
		userID = &claims.UserID
	}

	ctx := r.Context()
	if req.Kind == "register_source" {
		if userID == nil {
			writeErr(w, http.StatusBadRequest, "register_source requires auth")
			return
		}
		updated, err := db.SetUserSourceIfEmpty(ctx, s.DB, *userID, req.Source, req.UTM)
		if err != nil {
			log.Printf("[EVENTS] set user source: %v", err)
			writeErr(w, http.StatusInternalServerError, "db error")
			return
		}
		if !updated {
			// Источник уже был — первое касание не затираем, событие не пишем.
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deduped": true})
			return
		}
		if err := db.InsertEvent(ctx, s.DB, userID, "register", req.Source, req.UTM, req.Meta); err != nil {
			log.Printf("[EVENTS] insert register: %v", err)
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}

	if err := db.InsertEvent(ctx, s.DB, userID, req.Kind, req.Source, req.UTM, req.Meta); err != nil {
		log.Printf("[EVENTS] insert %s: %v", req.Kind, err)
		writeErr(w, http.StatusInternalServerError, "db error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// featureByPath — маппинг path-префикса проксируемого запроса на фичу для
// дневных счётчиков. Пустая строка — запрос не относится к считаемым фичам.
func featureByPath(path string) string {
	switch {
	case strings.HasPrefix(path, "/api/pty"):
		return "terminal"
	case strings.HasPrefix(path, "/api/files"):
		return "files"
	case strings.HasPrefix(path, "/api/ssh"):
		return "ssh"
	case strings.HasPrefix(path, "/api/system"):
		return "system"
	}
	return ""
}

// bumpFeature — учёт использования фичи дневным счётчиком. Вызывается в
// горутине из прокси/стримов: аналитика не должна блокировать запрос,
// а её ошибки (например, locked БД) — ронять боевой путь.
func (s *Server) bumpFeature(userID int64, feature string) {
	go func() {
		if err := db.BumpFeatureCounter(context.Background(), s.DB, userID, feature); err != nil {
			log.Printf("[EVENTS] bump %s: %v", feature, err)
		}
	}()
}
