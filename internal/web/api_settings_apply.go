package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"tgcontrol/internal/config"
	"tgcontrol/internal/relay"
)

// Изменение настроек агентом и подтверждение опасного человеком.
//
// РЕШЕНИЕ ВЛАДЕЛЬЦА (05.08.2026): «безопасное агент меняет сам, опасное — с
// подтверждением». Граница записана в каталоге (api_settings_catalog.go), а
// здесь она применяется.
//
// ПОЧЕМУ ПОДТВЕРЖДЕНИЕ ИМЕННО В ПРИЛОЖЕНИИ, А НЕ КНОПКОЙ В TELEGRAM. Опасные
// настройки — это ровно те, что могут отрезать телефон от компьютера (облако,
// автозапуск, порт). Подтверждать такое кнопкой в чате, который ходит через то
// самое облако, — значит закладывать случай «нажал и потерял связь, а
// подтверждение не доехало». Сообщение в Telegram остаётся, но как СИГНАЛ:
// решение человек принимает на экране, где видит, что именно меняется.

// AgentRequest — просьба агента изменить опасную настройку.
type AgentRequest struct {
	ID    string `json:"id"`
	Key   string `json:"key"`
	Title string `json:"title"`
	// Value — что агент просит поставить (как он это прислал).
	Value string `json:"value"`
	// Reason — зачем, словами агента. Человек решает по нему.
	Reason    string `json:"reason,omitempty"`
	CreatedAt int64  `json:"created_at"`
	// Status: pending | approved | rejected | expired.
	Status string `json:"status"`
	// Error — если применение не удалось уже после подтверждения.
	Error string `json:"error,omitempty"`
}

// requestTTL — сколько живёт неотвеченная просьба. Час: за это время человек
// либо увидел сообщение, либо оно уже неактуально, а вечная очередь просьб
// превращается в свалку, которую никто не разбирает.
const requestTTL = time.Hour

var requestsMu sync.Mutex

func requestsPath() string {
	return filepath.Join(realUserHome(), ".tgcontrol-agent-requests.json")
}

func loadRequests() []AgentRequest {
	data, err := os.ReadFile(requestsPath())
	if err != nil {
		return nil
	}
	var list []AgentRequest
	if json.Unmarshal(data, &list) != nil {
		return nil
	}
	// Просроченные помечаем на чтении: у процесса может не быть шанса это
	// сделать (агент выключали), а показывать вечное «ждёт ответа» нельзя.
	now := time.Now()
	for i := range list {
		if list[i].Status == "pending" && now.Sub(time.Unix(list[i].CreatedAt, 0)) > requestTTL {
			list[i].Status = "expired"
		}
	}
	return list
}

func saveRequests(list []AgentRequest) error {
	// Держим только свежие: разобранные просьбы старше суток человеку не
	// нужны, а файл не должен расти вечно.
	keep := make([]AgentRequest, 0, len(list))
	cutoff := time.Now().Add(-24 * time.Hour).Unix()
	for _, r := range list {
		if r.Status == "pending" || r.CreatedAt > cutoff {
			keep = append(keep, r)
		}
	}
	data, err := json.MarshalIndent(keep, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(requestsPath(), data, 0o600)
}

// POST /api/settings/apply {key, value} — изменить настройку.
//
// Безопасную применяем сразу. Опасную НЕ применяем никогда: отвечаем 403 с
// кодом `needs_approval`, чтобы вызывающий (обычно `remotai config set`)
// объяснил агенту, что делать дальше.
func (s *Server) apiSettingsApply(w http.ResponseWriter, r *http.Request, uid int64) {
	var req struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonErrorCode(w, 400, "bad_request", "bad request", nil)
		return
	}
	spec, ok := findSetting(strings.TrimSpace(req.Key))
	if !ok {
		jsonErrorCode(w, 404, "unknown_setting", "нет такой настройки", nil)
		return
	}
	if spec.Risk == RiskDangerous {
		jsonErrorCode(w, 403, "needs_approval",
			"эту настройку меняет только владелец: "+spec.Title, nil)
		return
	}
	value, err := validateSetting(spec, req.Value)
	if err != nil {
		jsonErrorCode(w, 400, "bad_value", err.Error(), nil)
		return
	}
	if err := applySetting(spec.Key, value); err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	cfg := config.GetNoSetup()
	// Разметка команд (ST-10) применяется к менеджеру терминалов сразу:
	// перезапуск агента ради переключателя не нужен, новый терминал уже
	// откроется по новой настройке.
	if spec.Key == "shell_integration" && s.ptyManager != nil {
		s.ptyManager.SetShellIntegration(cfg.ShellIntegration)
	}
	jsonResp(w, map[string]any{"key": spec.Key, "value": settingValue(cfg, spec.Key), "applied": true})
}

// applySetting записывает значение в конфиг агента.
func applySetting(key string, value any) error {
	cfg := config.GetNoSetup()
	switch key {
	case "default_agent":
		cfg.DefaultAgent = fmt.Sprint(value)
	case "default_cwd":
		cfg.DefaultCwd = fmt.Sprint(value)
	case "notifications_enabled":
		cfg.NotificationsEnabled = boolText(value)
	case "detect_agent_questions":
		on, _ := value.(bool)
		cfg.DetectAgentQuestions = on
	case "shell_integration":
		on, _ := value.(bool)
		// Менеджеру терминалов значение передаёт apiSettingsApply: у этой
		// функции нет сервера. Живые терминалы не затрагиваются.
		cfg.ShellIntegration = on
	case "screenshot_hotkey":
		on, _ := value.(bool)
		cfg.ScreenshotHotkey = on
		// Перезапуск агента не нужен: клавиша занимается и отпускается на ходу.
		setScreenshotHotkey(on)
	case "agent_summaries":
		on, _ := value.(bool)
		// Перезапуск не нужен: движок читает настройку на каждом поводе
		// (internal/web/agent_summary.go).
		cfg.AgentSummaries = on
	case "vpn_watchdog":
		on, _ := value.(bool)
		// Хранится строкой: пустое значение обязано читаться как «включён»,
		// иначе сторож не заработал бы ни у кого, кроме тех, кто его нашёл.
		if on {
			cfg.VPNWatchdog = ""
		} else {
			cfg.VPNWatchdog = "false"
		}
	case "inbox_dir":
		cfg.InboxDir = fmt.Sprint(value)
	case "max_concurrent_sessions":
		n, _ := value.(int)
		cfg.MaxConcurrentSessions = n
	default:
		return fmt.Errorf("настройка %q не применяется этим путём", key)
	}
	return cfg.Save()
}

func boolText(v any) string {
	if on, _ := v.(bool); on {
		return "true"
	}
	return "false"
}

// POST /api/settings/request {key, value, reason} — попросить владельца.
//
// Возвращает id: по нему `remotai config set` дожидается решения и говорит
// агенту, чем кончилось. Ответ человека — в приложении.
func (s *Server) apiSettingsRequest(w http.ResponseWriter, r *http.Request, uid int64) {
	var req struct {
		Key    string `json:"key"`
		Value  string `json:"value"`
		Reason string `json:"reason"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonErrorCode(w, 400, "bad_request", "bad request", nil)
		return
	}
	spec, ok := findSetting(strings.TrimSpace(req.Key))
	if !ok {
		jsonErrorCode(w, 404, "unknown_setting", "нет такой настройки", nil)
		return
	}
	if _, err := validateSetting(spec, req.Value); err != nil {
		jsonErrorCode(w, 400, "bad_value", err.Error(), nil)
		return
	}

	item := AgentRequest{
		ID:        fmt.Sprintf("req-%d", time.Now().UnixNano()),
		Key:       spec.Key,
		Title:     spec.Title,
		Value:     strings.TrimSpace(req.Value),
		Reason:    strings.TrimSpace(req.Reason),
		CreatedAt: time.Now().Unix(),
		Status:    "pending",
	}
	requestsMu.Lock()
	list := append(loadRequests(), item)
	err := saveRequests(list)
	requestsMu.Unlock()
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}

	// Сигнал владельцу: экран приложения он может и не смотреть. Текст
	// называет и настройку, и причину — решать по одному ключу нельзя.
	go sendOwnerText(fmt.Sprintf(
		"🔐 Агент просит изменить настройку: %s → %s.\n%s\nПодтвердите в приложении: раздел «Агенты».",
		spec.Title, item.Value, item.Reason))

	jsonResp(w, map[string]any{"request": item})
}

// GET /api/settings/requests — что агент просил и чем кончилось.
func (s *Server) apiSettingsRequests(w http.ResponseWriter, r *http.Request, uid int64) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	requestsMu.Lock()
	list := loadRequests()
	requestsMu.Unlock()
	if id != "" {
		for _, item := range list {
			if item.ID == id {
				jsonResp(w, map[string]any{"request": item})
				return
			}
		}
		jsonErrorCode(w, 404, "not_found", "просьба не найдена", nil)
		return
	}
	if list == nil {
		list = []AgentRequest{}
	}
	jsonResp(w, map[string]any{"requests": list})
}

// POST /api/settings/requests/answer {id, approve} — ответ человека.
//
// Подтверждение и ПРИМЕНЯЕТ настройку: разводить «согласился» и «сработало» по
// разным шагам значило бы оставить человека с ощущением, что он разрешил, а
// ничего не произошло.
func (s *Server) apiSettingsAnswer(w http.ResponseWriter, r *http.Request, uid int64) {
	var req struct {
		ID      string `json:"id"`
		Approve bool   `json:"approve"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonErrorCode(w, 400, "bad_request", "bad request", nil)
		return
	}

	requestsMu.Lock()
	defer requestsMu.Unlock()
	list := loadRequests()
	idx := -1
	for i := range list {
		if list[i].ID == req.ID {
			idx = i
			break
		}
	}
	if idx < 0 {
		jsonErrorCode(w, 404, "not_found", "просьба не найдена", nil)
		return
	}
	if list[idx].Status != "pending" {
		jsonErrorCode(w, 409, "already_answered", "на эту просьбу уже ответили", nil)
		return
	}

	if !req.Approve {
		list[idx].Status = "rejected"
		_ = saveRequests(list)
		jsonResp(w, map[string]any{"request": list[idx]})
		return
	}

	list[idx].Status = "approved"
	if err := applyDangerousSetting(list[idx].Key, list[idx].Value); err != nil {
		// Честно записываем неудачу: человек разрешил, но применить не вышло —
		// молчать об этом нельзя, иначе он будет думать, что настройка изменена.
		list[idx].Error = err.Error()
	}
	_ = saveRequests(list)
	jsonResp(w, map[string]any{"request": list[idx]})
}

// applyDangerousSetting выполняет подтверждённое опасное изменение.
//
// Каждое — своим путём, а не записью в конфиг: у порта, автозапуска и облака
// есть побочные действия (перезапуск слушателя, задача планировщика, отзыв
// ключа), и делать их «просто сохранением поля» неправильно.
func applyDangerousSetting(key, raw string) error {
	spec, ok := findSetting(key)
	if !ok {
		return fmt.Errorf("нет такой настройки")
	}
	value, err := validateSetting(spec, raw)
	if err != nil {
		return err
	}
	switch key {
	case "autostart":
		if on, _ := value.(bool); on {
			return EnableAutostart()
		}
		return DisableAutostart()
	case "web_port":
		port, _ := value.(int)
		if port < 1 || port > 65535 {
			return fmt.Errorf("порт вне диапазона")
		}
		cfg := config.GetNoSetup()
		cfg.WebPort = fmt.Sprint(port)
		if err := cfg.Save(); err != nil {
			return err
		}
		// Новый порт начинает работать после перезапуска агента — говорим об
		// этом прямо, а не делаем вид, что уже всё.
		return nil
	case "peer_access":
		mode := relay.NormalizePeerAccess(fmt.Sprint(value))
		cfg := config.GetNoSetup()
		cfg.PeerAccess = mode
		return cfg.Save()
	case "cloud":
		// Отключение облака — единственное необратимое здесь действие, и оно
		// требует ещё и подтверждения в самом приложении (кнопка «Отключить
		// облако»). Через эту дверь его не делаем вовсе.
		return fmt.Errorf("облако отключается только в приложении: это стирает ключ компьютера")
	}
	return fmt.Errorf("настройка %q не применяется", key)
}
