package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"tgcontrol/internal/config"
)

// HealthCheck represents one row of the readiness dashboard.
//
// Note — уже готовая к показу РУССКАЯ фраза, законченная сама по себе: её
// печатают как есть и карточка готовности в клиенте, и Telegram-команда
// /leaving, и оба не умеют доклеивать к ней Extra. Поэтому число (счётчик,
// задержка) должно попадать И в текст ноты; Extra остаётся машинным дублем для
// возможной обработки. Названия строк («Терминалы», «Облако») в ноту не
// повторяем — их даёт сама поверхность.
type HealthCheck struct {
	OK    bool   `json:"ok"`
	Note  string `json:"note,omitempty"`
	Extra any    `json:"extra,omitempty"`
}

// HealthReport is the response of GET /api/health/full — the "ready to leave
// home" card on the dashboard.
type HealthReport struct {
	OK     bool                   `json:"ok"`
	Checks map[string]HealthCheck `json:"checks"`
	RanAt  string                 `json:"ran_at"`
	TookMS int64                  `json:"took_ms"`
}

// HealthSnapshot computes the same readiness report served by
// GET /api/health/full but for in-process callers (Telegram /leaving command,
// CLI diagnostics). Pass nil ctx for a sensible default.
func (s *Server) HealthSnapshot(ctx context.Context) HealthReport {
	if ctx == nil {
		ctx = context.Background()
	}
	return s.computeHealthReport(ctx)
}

// apiHealthFull aggregates "is the user safe to walk out the door?" signals.
// Available without auth: the result intentionally omits secrets and only
// reports booleans + short URLs / counts so it's safe to expose.
func (s *Server) apiHealthFull(w http.ResponseWriter, r *http.Request) {
	report := s.computeHealthReport(r.Context())
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(report)
}

func (s *Server) computeHealthReport(ctx context.Context) HealthReport {
	start := time.Now()
	report := HealthReport{
		Checks: make(map[string]HealthCheck),
	}

	cfg := config.GetNoSetup()
	// Topology: a cloud-paired install (relay) doesn't need the Telegram bot or
	// a cloudflared tunnel — flagging those as "problems" just scares the user.
	relayConfigured, relayConnected := false, false
	if s.relayStatus != nil {
		relayConfigured, relayConnected = s.relayStatus()
	}
	// LAN-топология: пользователь сознательно выбрал «только локальная сеть» в
	// мастере (или просто не подключил ни облако, ни бота). Раньше bot/tunnel/
	// notify в этом режиме считались ПРОБЛЕМАМИ, и карточка готовности вечно
	// писала «Есть нерешённые проблемы» — при этом починить их из приложения
	// нельзя, UI туннеля удалён. Это первое, что видит бесплатный LAN-юзер.
	lanOnly := cfg.Mode == config.ModeStandalone || (s.botToken == "" && !relayConfigured)

	// 1) App itself
	report.Checks["app"] = HealthCheck{OK: true, Note: "Веб-сервер запущен"}

	// 2) Telegram bot — real liveness, not just "token present". The watchdog
	// reports whether getUpdates/GetMe has actually reached Telegram recently.
	if s.botToken == "" {
		if cfg.IsCentralBot() || relayConfigured {
			// Cloud topology — Telegram bot is simply not part of the setup.
			report.Checks["bot"] = HealthCheck{OK: true, Note: "Не используется (доступ через облако)"}
		} else if lanOnly {
			report.Checks["bot"] = HealthCheck{OK: true, Note: "Не используется (локальная сеть)"}
		} else {
			report.Checks["bot"] = HealthCheck{OK: false, Note: "Нет TELEGRAM_BOT_TOKEN"}
		}
	} else if s.botLiveness != nil {
		ok, last := s.botLiveness()
		if ok {
			report.Checks["bot"] = HealthCheck{OK: true, Note: "Бот на связи с Telegram"}
		} else if last.IsZero() {
			report.Checks["bot"] = HealthCheck{OK: false, Note: "Бот ещё не дозвонился до Telegram — проверьте интернет или включите VPN"}
		} else {
			// Ноту читает человек с телефона, а не тот, кто правит .env: имя
			// переменной TELEGRAM_PROXY ему ничего не говорит и адресовано не
			// ему. Время — до минут: секунды в этой фразе ничего не решают, а
			// строка целиком едет в карточку готовности.
			report.Checks["bot"] = HealthCheck{
				OK:   false,
				Note: "Нет связи с Telegram с " + last.Format("15:04") + " — похоже, Telegram заблокирован (нужен VPN или прокси)",
			}
		}
	} else {
		report.Checks["bot"] = HealthCheck{OK: true, Note: "Токен бота задан"}
	}

	// 3) Tunnel — not needed when remote access goes through the cloud relay.
	tunnelRunning := false
	if s.tunnelManager != nil {
		info := s.tunnelManager.Info()
		switch string(info.Status) {
		case "running":
			tunnelRunning = true
			report.Checks["tunnel"] = HealthCheck{OK: true, Note: info.URL, Extra: info}
		case "stopped":
			if relayConnected {
				report.Checks["tunnel"] = HealthCheck{OK: true, Note: "Не используется (доступ через облако)"}
			} else if lanOnly {
				report.Checks["tunnel"] = HealthCheck{OK: true, Note: "Не используется (локальная сеть)"}
			} else if cfg.TunnelURL != "" {
				report.Checks["tunnel"] = HealthCheck{OK: false, Note: "Туннель остановлен (раньше: " + cfg.TunnelURL + ")"}
			} else {
				report.Checks["tunnel"] = HealthCheck{OK: false, Note: "Туннель не настроен"}
			}
		default:
			report.Checks["tunnel"] = HealthCheck{OK: false, Note: string(info.Status), Extra: info}
		}
	} else if relayConnected {
		report.Checks["tunnel"] = HealthCheck{OK: true, Note: "Не используется (доступ через облако)"}
	} else if lanOnly {
		report.Checks["tunnel"] = HealthCheck{OK: true, Note: "Не используется (локальная сеть)"}
	} else {
		report.Checks["tunnel"] = HealthCheck{OK: false, Note: "Туннель не настроен"}
	}

	// 3b) Cloud relay — shown whenever настроен (paired at least once).
	if relayConfigured {
		if relayConnected {
			report.Checks["relay"] = HealthCheck{OK: true, Note: "Облако подключено (" + cfg.RelayHTTPBase() + ")"}
		} else {
			report.Checks["relay"] = HealthCheck{OK: false, Note: "Нет связи с облаком — телефон не достучится"}
		}
	}

	// 4) PTY (interactive terminals)
	active := len(s.ptyManager.List())
	// Число — в самой ноте: и карточка готовности, и /leaving печатают только
	// Note, поэтому раньше получалась обрубленная фраза «Терминалы — Активных
	// терминалов» без единой цифры.
	report.Checks["pty"] = HealthCheck{OK: true, Note: fmt.Sprintf("Активных терминалов: %d", active), Extra: active}

	// 5) Public reachability — only probe when there's a tunnel URL; relay
	// connectivity already proves the device is reachable from the internet.
	pickedURL := ""
	if t, ok := report.Checks["tunnel"]; ok && tunnelRunning && t.OK {
		pickedURL = t.Note
	}
	if pickedURL != "" {
		probeCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
		ok, lat := pingPublic(probeCtx, pickedURL+"/health")
		cancel()
		if ok {
			report.Checks["public"] = HealthCheck{
				OK:    true,
				Note:  fmt.Sprintf("Доступен из интернета (%d мс)", lat.Milliseconds()),
				Extra: lat.Milliseconds(),
			}
		} else {
			report.Checks["public"] = HealthCheck{OK: false, Note: "Не отвечает на /health через публичный URL"}
		}
	} else if relayConnected {
		report.Checks["public"] = HealthCheck{OK: true, Note: "Доступен из интернета через облако"}
	} else {
		report.Checks["public"] = HealthCheck{OK: false, Note: "Нет публичного URL — работает только в локальной сети"}
	}

	// 6) Notifications: presence of bot token and event buffer are proxies.
	notifyOK := s.botToken != "" && s.eventBuf != nil
	notifyNote := "Telegram + WS"
	if !notifyOK {
		switch {
		case relayConnected && s.eventBuf != nil:
			// Cloud topology: live WS-события доходят до телефона через релей;
			// отсутствие Telegram-каналa — не «проблема», а особенность режима.
			notifyOK = true
			notifyNote = "WS-события через облако (без Telegram)"
		case lanOnly && s.eventBuf != nil:
			// LAN: телефон в той же сети получает события по WS напрямую.
			notifyOK = true
			notifyNote = "WS-события по локальной сети"
		default:
			notifyNote = "Уведомления частично отключены"
		}
	}
	report.Checks["notify"] = HealthCheck{OK: notifyOK, Note: notifyNote}

	// Aggregate
	overall := true
	for k, c := range report.Checks {
		// "public" being down only downgrades to "warning", not full red,
		// because LAN-only mode is still valid.
		//
		// "tunnel" туда же: это СПОСОБ того же внешнего доступа, и клиент их
		// давно показывает одной строкой («Публичный URL» и «Доступ из
		// интернета» — одна мысль дважды). Пока туннель был отдельной красной
		// причиной, карточка готовности говорила «Есть нерешённые проблемы» без
		// единого названного виновника: строки-то на экране нет.
		if !c.OK && k != "public" && k != "tunnel" {
			overall = false
		}
	}
	report.OK = overall
	report.RanAt = time.Now().UTC().Format(time.RFC3339)
	report.TookMS = time.Since(start).Milliseconds()
	return report
}

var publicProbeClient = &http.Client{Timeout: 5 * time.Second}

// pingPublic does a GET against the supplied URL and returns whether the
// response is healthy and the round-trip latency.
func pingPublic(ctx context.Context, url string) (bool, time.Duration) {
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, 0
	}
	resp, err := publicProbeClient.Do(req)
	if err != nil {
		return false, time.Since(start)
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 500, time.Since(start)
}
