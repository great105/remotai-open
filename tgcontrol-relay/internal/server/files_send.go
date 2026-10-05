package server

// Доставка файла с ПК через облачного Telegram-бота (#112).
//
// В cloud-топологии на компьютере нет локального bot token, поэтому старый
// /api/files/send-to-telegram всегда был недоступен. Релей уже знает
// Telegram-аккаунт владельца и держит управляющий канал к агенту: забираем
// файл кусками по 8 МБ во временный spool, затем передаём Reader Telegram API.
// Целиком в RAM файл не живёт и управляющий websocket не получает огромный
// одиночный кадр.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"

	"tgcontrol-relay/internal/db"
	"tgcontrol-relay/internal/protocol"
	"tgcontrol-relay/internal/relayhub"
)

const (
	telegramFetchChunk = 8 << 20
	// Telegram Bot API cloud currently rejects larger document uploads. Keep a
	// little margin for multipart overhead instead of downloading doomed data.
	telegramFileLimit = 49 << 20
)

func (s *Server) handleSendFileToTelegram(w http.ResponseWriter, r *http.Request) {
	if s.UserSendFile == nil {
		writeErrCode(w, http.StatusServiceUnavailable, "telegram_unavailable", "telegram bot unavailable")
		return
	}
	claims, err := s.requireUserAuth(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	var req struct {
		DeviceID string `json:"device_id"`
		Path     string `json:"path"`
		Name     string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeErrCode(w, http.StatusBadRequest, "bad_request", "invalid JSON")
		return
	}
	if strings.TrimSpace(req.DeviceID) == "" || strings.TrimSpace(req.Path) == "" {
		writeErrCode(w, http.StatusBadRequest, "bad_path", "device_id and path required")
		return
	}
	dev, err := s.deviceForUser(r, req.DeviceID, claims.UserID)
	if err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	u, err := db.GetUserByID(r.Context(), s.DB, claims.UserID)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "user not found")
		return
	}
	if u.TelegramID <= 0 {
		writeErrCode(w, http.StatusConflict, "telegram_not_linked", "link Telegram to receive files")
		return
	}
	if !s.cloudGate(w, r, dev.UserID) {
		return
	}
	agent := s.Hub.Get(req.DeviceID)
	if agent == nil {
		writeErrCode(w, http.StatusBadGateway, "pc_offline", relayhub.ErrAgentOffline.Error())
		return
	}
	filename, size, ok := s.sendAgentFileToTelegram(w, r, agent, req.Path, req.Name, u.TelegramID)
	if !ok {
		return // ответ с кодом ошибки уже записан хелпером
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "name": filename, "size": size,
	})
}

// spoolDir — куда складывать временные файлы перед отправкой в Telegram.
// os.CreateTemp("", …) на боевом релее падает: у сервиса ProtectSystem=strict,
// и /tmp в его mount namespace read-only (живой случай 2026-07-29, первый же
// `remotai send --file`). Спулим рядом с БД — она записываема по определению.
// Пустая строка = дефолтный TMPDIR ОС (тесты и локальная разработка).
func (s *Server) spoolDir() string {
	if s.Config == nil || s.Config.DBPath == "" {
		return ""
	}
	dir := filepath.Join(filepath.Dir(s.Config.DBPath), "spool")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}
	return dir
}

// sendAgentFileToTelegram — общий хвост /v1/files/send и /v1/agent/send:
// скачать файл с ПК кусками по 8 МБ во временный spool и отправить документом
// в Telegram. ok=false — ответ с кодом ошибки уже записан (too_large,
// pc_offline, file_transfer_failed, spool_failed, telegram_send_failed).
func (s *Server) sendAgentFileToTelegram(
	w http.ResponseWriter,
	r *http.Request,
	agent agentSender,
	path, reqName string,
	telegramID int64,
) (string, int64, bool) {
	tmp, err := os.CreateTemp(s.spoolDir(), "remotai-tg-file-*")
	if err != nil {
		writeErrCode(w, http.StatusInternalServerError, "spool_failed", err.Error())
		return "", 0, false
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()

	size, filename, err := fetchAgentFile(r.Context(), agent, path, reqName, tmp)
	if err != nil {
		var tooLarge *telegramTooLargeError
		if errors.As(err, &tooLarge) {
			writeErrCode(w, http.StatusRequestEntityTooLarge, "too_large",
				fmt.Sprintf("Telegram принимает файлы не больше %d МБ", telegramFileLimit>>20))
			return "", 0, false
		}
		code := "file_transfer_failed"
		status := http.StatusBadGateway
		if errors.Is(err, relayhub.ErrAgentOffline) || errors.Is(err, relayhub.ErrAgentDisconnect) {
			code = "pc_offline"
		}
		writeErrCode(w, status, code, err.Error())
		return "", 0, false
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		writeErrCode(w, http.StatusInternalServerError, "spool_failed", err.Error())
		return "", 0, false
	}

	sendCtx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	if err := s.UserSendFile(sendCtx, telegramID, filename, tmp); err != nil {
		writeErrCode(w, http.StatusBadGateway, "telegram_send_failed", err.Error())
		return "", 0, false
	}
	return filename, size, true
}

type telegramTooLargeError struct{ size int64 }

func (e *telegramTooLargeError) Error() string {
	return fmt.Sprintf("file is too large for Telegram: %d bytes", e.size)
}

type agentSender interface {
	Send(context.Context, protocol.Cmd, time.Duration) (*protocol.CmdResult, error)
}

func fetchAgentFile(
	ctx context.Context,
	agent agentSender,
	path string,
	requestedName string,
	dst io.Writer,
) (total int64, filename string, err error) {
	filename = safeTelegramFilename(requestedName)
	if filename == "" {
		filename = safeTelegramFilename(pathBaseAny(path))
	}
	if filename == "" {
		filename = "file"
	}

	var offset int64
	var mtime string
	first := true
	for first || offset < total {
		first = false
		query := map[string]string{
			"path": path, "offset": strconv.FormatInt(offset, 10),
			"len": strconv.Itoa(telegramFetchChunk),
		}
		if total > 0 {
			query["expect_size"] = strconv.FormatInt(total, 10)
		}
		if mtime != "" {
			query["expect_mtime"] = mtime
		}
		res, sendErr := agent.Send(ctx, protocol.Cmd{
			Type: protocol.MsgCmd, RequestID: uuid.NewString(),
			Method: http.MethodGet, Path: "/api/files/download", Query: query,
		}, 60*time.Second)
		if sendErr != nil {
			return 0, "", sendErr
		}
		if res.StatusCode < 200 || res.StatusCode >= 300 {
			return 0, "", fmt.Errorf("agent returned HTTP %d: %s", res.StatusCode, strings.TrimSpace(string(res.Body)))
		}
		if total == 0 {
			rawSize := headerValue(res.Headers, "X-File-Size")
			total, err = strconv.ParseInt(rawSize, 10, 64)
			if err != nil || total < 0 {
				return 0, "", fmt.Errorf("agent did not return a valid file size")
			}
			if total > telegramFileLimit {
				return total, filename, &telegramTooLargeError{size: total}
			}
			mtime = headerValue(res.Headers, "X-File-Mtime")
		}
		if int64(len(res.Body)) > total-offset {
			return 0, "", fmt.Errorf("agent returned more bytes than file size")
		}
		if len(res.Body) == 0 && offset < total {
			return 0, "", fmt.Errorf("agent returned an empty chunk before EOF")
		}
		if _, err := dst.Write(res.Body); err != nil {
			return 0, "", err
		}
		offset += int64(len(res.Body))
		if total == 0 {
			break
		}
	}
	if offset != total {
		return 0, "", fmt.Errorf("incomplete file: got %d of %d bytes", offset, total)
	}
	return total, filename, nil
}

func headerValue(headers map[string]string, name string) string {
	for key, value := range headers {
		if strings.EqualFold(key, name) {
			return value
		}
	}
	return ""
}

func pathBaseAny(path string) string {
	parts := strings.FieldsFunc(path, func(r rune) bool { return r == '/' || r == '\\' })
	if len(parts) == 0 {
		return ""
	}
	return parts[len(parts)-1]
}

func safeTelegramFilename(name string) string {
	name = strings.TrimSpace(name)
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '/' || r == '\\' {
			return -1
		}
		return r
	}, name)
	if len([]byte(name)) > 180 {
		runes := []rune(name)
		for len(runes) > 0 && len([]byte(string(runes))) > 180 {
			runes = runes[:len(runes)-1]
		}
		name = string(runes)
	}
	return name
}
