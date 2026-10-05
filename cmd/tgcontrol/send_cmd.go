package main

// `remotai send` — сообщение или файл владельцу в Telegram через облачного бота.
//
// Зачем: AI-агенты (Codex/Claude/Kimi) и обычные скрипты на этом компьютере
// получают одну простую ручку «сказать человеку» — готовый отчёт, «нужно
// решение», скриншот результата. Канал уже существует (бот знает владельца
// устройства), не хватало только публичной команды.
//
// Путь: CLI → POST {relay}/v1/agent/send с device JWT → релей → бот →
// Telegram владельца. Локальный демон не нужен для текста; для файла нужен —
// релей тянет его чанками через управляющий канал агента.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"tgcontrol/internal/localize"
	"time"

	"tgcontrol/internal/config"
	"tgcontrol/internal/dnsfallback"
	"tgcontrol/internal/relay"
)

// sendFileLimit — облачный Telegram Bot API больше не принимает (зеркало
// telegramFileLimit на релее; проверяем ДО сети, чтобы не ждать таймаут).
const sendFileLimit = 49 << 20

var sendUsage = localize.Text(`Использование:
  remotai send "текст"                      сообщение владельцу в Telegram
  remotai send --file <путь>                файл (до 49 МБ)
  remotai send --file <путь> --text "подпись"   файл + отдельное сообщение

Примеры для агентов и скриптов:
  remotai send "Сборка готова: 0 ошибок"
  remotai send --file C:\logs\build.pdf
  remotai send --file /tmp/report.html --text "Отчёт за ночь"`)

// sendArgs — разобранные флаги команды.
type sendArgs struct {
	text string
	file string
}

// parseSendArgs — чистый разбор флагов (проверяется тестом).
func parseSendArgs(args []string) (sendArgs, error) {
	var out sendArgs
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--file", "-f":
			if i+1 >= len(args) {
				return out, fmt.Errorf(localize.Text("после %s нужен путь к файлу"), args[i])
			}
			i++
			out.file = args[i]
		case "--text", "-t":
			if i+1 >= len(args) {
				return out, fmt.Errorf(localize.Text("после %s нужен текст"), args[i])
			}
			i++
			out.text = args[i]
		case "--help", "-h":
			return out, fmt.Errorf("help")
		default:
			if strings.HasPrefix(args[i], "-") {
				return out, fmt.Errorf(localize.Text("неизвестный флаг %s"), args[i])
			}
			if out.text != "" {
				return out, fmt.Errorf(localize.Text("текст уже задан — лишний аргумент %q"), args[i])
			}
			out.text = args[i]
		}
	}
	if out.text == "" && out.file == "" {
		return out, fmt.Errorf("%s", localize.Text("нужен текст или --file"))
	}
	return out, nil
}

func runSend(args []string) int {
	parsed, err := parseSendArgs(args)
	if err != nil {
		if err.Error() == "help" {
			fmt.Println(sendUsage)
			return 0
		}
		fmt.Fprintf(os.Stderr, "remotai: %v\n\n%s\n", err, sendUsage)
		return 2
	}

	// Локальные проверки ДО сети: привязка и файл.
	if !relay.Available() {
		fmt.Fprintln(os.Stderr, localize.Text("remotai: компьютер не привязан к аккаунту — сначала выполните: remotai pair"))
		return 1
	}
	fileName := ""
	if parsed.file != "" {
		abs, err := filepath.Abs(parsed.file)
		if err != nil {
			fmt.Fprintf(os.Stderr, localize.Text("remotai: путь %q не разобрать: %v\n"), parsed.file, err)
			return 1
		}
		st, err := os.Stat(abs)
		if err != nil {
			fmt.Fprintf(os.Stderr, localize.Text("remotai: файл %q не найден: %v\n"), parsed.file, err)
			return 1
		}
		if st.IsDir() {
			fmt.Fprintf(os.Stderr, localize.Text("remotai: %q — это папка, а не файл\n"), parsed.file)
			return 1
		}
		if st.Size() > sendFileLimit {
			fmt.Fprintf(os.Stderr, localize.Text("remotai: файл %d МБ — Telegram принимает не больше %d МБ\n"),
				st.Size()>>20, sendFileLimit>>20)
			return 1
		}
		parsed.file, fileName = abs, filepath.Base(abs)
	}

	cfg := config.GetNoSetup()
	jwt := cfg.RelayJWT
	if jwt == "" {
		jwt, _ = relay.LoadJWT()
	}
	if jwt == "" {
		fmt.Fprintln(os.Stderr, localize.Text("remotai: нет ключа устройства — перепривяжите: remotai pair"))
		return 1
	}

	body := map[string]any{}
	if parsed.text != "" {
		body["text"] = parsed.text
	}
	if parsed.file != "" {
		body["file"] = map[string]string{"path": parsed.file, "name": fileName}
	}
	raw, _ := json.Marshal(body)
	// Файл тянется с агента чанками по 8 МБ — на больших файлах это десятки
	// секунд, поэтому таймаут шире обычного.
	// Запасной резолвер: `remotai send` — штатный способ агента достучаться до
	// владельца, и нужен он как раз тогда, когда на машине что-то сломалось.
	client := &http.Client{Timeout: 3 * time.Minute, Transport: dnsfallback.Transport()}
	req, err := http.NewRequest(http.MethodPost, cfg.RelayHTTPBase()+"/v1/agent/send", bytes.NewReader(raw))
	if err != nil {
		fmt.Fprintf(os.Stderr, "remotai: %v\n", err)
		return 1
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, localize.Text("remotai: релей недоступен: %v\n"), err)
		return 1
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))

	var parsedResp struct {
		OK    bool     `json:"ok"`
		Muted bool     `json:"muted"`
		Sent  []string `json:"sent"`
		Error string   `json:"error"`
		Code  string   `json:"code"`
	}
	_ = json.Unmarshal(data, &parsedResp)

	switch {
	case resp.StatusCode == http.StatusOK && parsedResp.Muted:
		fmt.Println(localize.Text("⚠ Уведомления выключены в приложении — сообщение не доставлено."))
		return 1
	case resp.StatusCode == http.StatusOK && parsedResp.OK:
		what := localize.Text("сообщение")
		if len(parsedResp.Sent) > 0 {
			what = strings.Join(parsedResp.Sent, " + ")
		}
		fmt.Printf(localize.Text("✅ Отправлено в Telegram (%s).\n"), what)
		return 0
	}

	// Ошибки: человеческий текст по коду, серверное сообщение в скобках.
	hint := ""
	switch parsedResp.Code {
	case "telegram_not_linked":
		hint = localize.Text("к аккаунту не привязан Telegram — откройте бота и нажмите /start")
	case "pc_offline":
		hint = localize.Text("агент на этом компьютере не в сети — файл недоступен (текст дошёл бы и так)")
	case "rate_limited":
		hint = localize.Text("слишком много сообщений — подождите час")
	case "too_large":
		hint = localize.Text("Telegram принимает файлы не больше 49 МБ")
	case "device_not_found", "unauthorized":
		hint = localize.Text("ключ устройства не принят — перепривяжите: remotai pair")
	}
	if hint != "" {
		fmt.Fprintf(os.Stderr, "remotai: %s (%s)\n", hint, parsedResp.Error)
	} else {
		fmt.Fprintf(os.Stderr, localize.Text("remotai: не отправлено (%d %s): %s\n"), resp.StatusCode, parsedResp.Code, parsedResp.Error)
	}
	return 1
}
