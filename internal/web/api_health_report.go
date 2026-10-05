package web

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"tgcontrol/internal/netwatch"
	"tgcontrol/internal/paths"
	"tgcontrol/internal/selfheal"
)

// Три ручки «что с компьютером»: почему он пропадал, что у него со связью и
// хвост его собственного лога.
//
// ЗАЧЕМ ОТДЕЛЬНО ОТ /api/system/stats. Stats отвечает на «сколько занято
// памяти», а эти три — на «почему тебя не было и как тебя вернуть». Ими
// пользуются сразу трое: приложение владельца, команда `remotai doctor` и AI-
// агент С ДРУГОГО УСТРОЙСТВА аккаунта (в режиме peer_access=diag доступны
// ровно они, см. internal/relay/peer_access.go).

// GET /api/system/boot-report — что случилось в прошлый перерыв.
func (s *Server) apiBootReport(w http.ResponseWriter, r *http.Request, uid int64) {
	rep, ok := selfheal.Last()
	if !ok {
		jsonResp(w, map[string]any{"available": false})
		return
	}
	jsonResp(w, map[string]any{"available": true, "report": rep})
}

// GET /api/system/network — связь, интернет, VPN и что сторож делал последним.
func (s *Server) apiNetworkStatus(w http.ResponseWriter, r *http.Request, uid int64) {
	snap := netwatch.Snapshot()
	// ?check=1 — проверить прямо сейчас (обычный опрос отдаёт последний снимок,
	// иначе открытие экрана каждый раз стоило бы пачки сетевых проб).
	if r.URL.Query().Get("check") == "1" {
		ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
		defer cancel()
		netwatch.CheckNow(ctx)
		snap = netwatch.Snapshot()
	}
	jsonResp(w, snap)
}

// POST /api/system/vpn {"action":"stop"|"start"} — выключить зависший VPN или
// включить его обратно. Это и есть «дотянуться до компьютера и починить»:
// действие доступно и соседнему устройству аккаунта, потому что чаще всего
// именно оно и нужно, когда компьютер пропал.
func (s *Server) apiVPNControl(w http.ResponseWriter, r *http.Request, uid int64) {
	var req struct {
		Action string `json:"action"`
	}
	body, _ := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<10))
	if len(body) > 0 {
		_ = json.Unmarshal(body, &req)
	}
	switch strings.ToLower(strings.TrimSpace(req.Action)) {
	case "stop", "off":
		v, err := netwatch.StopNow()
		if err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		jsonResp(w, map[string]any{"ok": true, "action": "stop", "vpn": v.Name,
			"start_hint": v.StartHint, "status": netwatch.Snapshot()})
	case "start", "on":
		how, err := netwatch.StartNow()
		if err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		jsonResp(w, map[string]any{"ok": true, "action": "start", "how": how, "status": netwatch.Snapshot()})
	default:
		jsonError(w, "action: stop | start", http.StatusBadRequest)
	}
}

// GET /api/system/logs?tail=N — последние строки remotai.log.
//
// Читаем ХВОСТ, а не файл: на боевой машине лог доходил до 19 МБ, и отдать его
// целиком значило бы уронить и канал, и того, кто попросил.
func (s *Server) apiAgentLogs(w http.ResponseWriter, r *http.Request, uid int64) {
	n := 200
	if v := r.URL.Query().Get("tail"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
			n = min(parsed, 2000)
		}
	}
	path := filepath.Join(paths.Base(), "remotai.log")
	lines, size, err := tailLines(path, n)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResp(w, map[string]any{"path": path, "size": size, "lines": lines})
}

// tailLines возвращает последние n строк файла, читая с конца кусками.
func tailLines(path string, n int) ([]string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	size := st.Size()

	const chunk = 64 << 10
	// Оценка сверху: 400 байт на строку с запасом, но не больше 4 МБ — этого
	// хватает и на 2000 строк, и на аномально длинные строки вывода агентов.
	want := int64(n) * 400
	if want > 4<<20 {
		want = 4 << 20
	}
	if want < chunk {
		want = chunk
	}
	start := size - want
	if start < 0 {
		start = 0
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, size, err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, size, err
	}
	text := string(data)
	if start > 0 {
		// Первая строка почти наверняка обрезана посередине — выбрасываем.
		if i := strings.IndexByte(text, '\n'); i >= 0 {
			text = text[i+1:]
		}
	}
	lines := strings.Split(strings.ReplaceAll(strings.TrimRight(text, "\n"), "\r\n", "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, size, nil
}
