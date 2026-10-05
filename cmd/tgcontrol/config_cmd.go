package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"tgcontrol/internal/localize"
	"time"

	"tgcontrol/internal/config"
)

// `remotai config` — настройки Remotai для AI-агента, работающего на этой машине.
//
// ЗАЧЕМ КОМАНДА, А НЕ MCP. Просьба владельца: «человек запустил Claude и
// говорит — давай посмотрим настройки». Команду умеет запустить ЛЮБОЙ агент
// (claude, codex, gemini, kimi) — сразу, без подключения и без правки его
// конфига. MCP-сервер дал бы то же самое, но только двум из четырёх и с
// настройкой в каждом аккаунте (а их теперь несколько).
//
// ГЛАВНОЕ СВОЙСТВО: агент не угадывает. `config list` печатает карту настроек
// С ОПИСАНИЯМИ и текущими значениями — её отдаёт сам агент Remotai
// (internal/web/api_settings_catalog.go), поэтому новая настройка появляется у
// AI сама, без переписывания скилла.
//
// ГРАНИЦА ВЛАСТИ (решение владельца): безопасное агент меняет сам, опасное —
// только с подтверждением человека в приложении.
func runConfig(args []string) int {
	if len(args) == 0 {
		printConfigHelp()
		return 2
	}
	switch args[0] {
	case "list", "ls":
		return configList(args[1:])
	case "get":
		return configGet(args[1:])
	case "set":
		return configSet(args[1:])
	case "requests":
		return configRequests(args[1:])
	default:
		printConfigHelp()
		return 2
	}
}

func printConfigHelp() {
	fmt.Println(localize.Text(`Настройки Remotai на этом компьютере.

  remotai config list [--json]     все настройки: что это, значение, что можно поставить
  remotai config get <ключ>        одно значение
  remotai config set <ключ> <значение> [--reason "зачем"]
  remotai config requests          что просили у владельца и чем кончилось

Настройки с пометкой «только владелец» изменить нельзя: команда отправит
человеку просьбу и дождётся ответа. Опишите причину в --reason — человек
решает по ней.`))
}

type settingItem struct {
	Key     string   `json:"key"`
	Title   string   `json:"title"`
	Hint    string   `json:"hint"`
	Type    string   `json:"type"`
	Options []string `json:"options"`
	Risk    string   `json:"risk"`
	Value   any      `json:"value"`
}

func configList(args []string) int {
	asJSON := len(args) > 0 && args[0] == "--json"
	var out struct {
		Settings []settingItem `json:"settings"`
		RiskNote string        `json:"risk_note"`
	}
	if code := localGET("/api/settings/catalog", &out); code != 0 {
		return code
	}
	for i := range out.Settings {
		out.Settings[i].Title = localize.Text(out.Settings[i].Title)
		out.Settings[i].Hint = localize.Text(out.Settings[i].Hint)
	}
	out.RiskNote = localize.Text(out.RiskNote)
	if asJSON {
		// Агенту — целиком и без украшений: он разберёт сам.
		data, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(data))
		return 0
	}
	for _, s := range out.Settings {
		mark := ""
		if s.Risk == "dangerous" {
			mark = localize.Text("  [только владелец]")
		}
		fmt.Printf("%s = %v%s\n    %s\n", s.Key, formatValue(s.Value), mark, s.Hint)
		if len(s.Options) > 0 {
			fmt.Printf(localize.Text("    допустимо: %s\n"), strings.Join(s.Options, ", "))
		}
	}
	return 0
}

func configGet(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, localize.Text("Использование: remotai config get <ключ>"))
		return 2
	}
	var out struct {
		Settings []settingItem `json:"settings"`
	}
	if code := localGET("/api/settings/catalog", &out); code != 0 {
		return code
	}
	for _, s := range out.Settings {
		if s.Key == args[0] {
			fmt.Println(formatValue(s.Value))
			return 0
		}
	}
	fmt.Fprintf(os.Stderr, localize.Text("нет такой настройки: %s (список — remotai config list)\n"), args[0])
	return 1
}

// configSet меняет настройку или просит владельца, если она опасная.
//
// Ожидание ответа — до 10 минут: человек может быть не у телефона, а бросать
// агента с «попробуйте позже» бессмысленно, он всё равно спросит снова.
func configSet(args []string) int {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, localize.Text(`Использование: remotai config set <ключ> <значение> [--reason "зачем"]`))
		return 2
	}
	key, value := args[0], args[1]
	reason := ""
	for i := 2; i < len(args)-1; i++ {
		if args[i] == "--reason" {
			reason = args[i+1]
		}
	}

	body := map[string]string{"key": key, "value": value}
	var applied struct {
		Key     string `json:"key"`
		Value   any    `json:"value"`
		Applied bool   `json:"applied"`
	}
	status, err := localPOST("/api/settings/apply", body, &applied)
	switch {
	case err != nil:
		fmt.Fprintf(os.Stderr, "remotai: %v\n", err)
		return 1
	case status == http.StatusOK:
		fmt.Printf("✅ %s = %v\n", applied.Key, formatValue(applied.Value))
		return 0
	case status == http.StatusForbidden:
		// Опасная настройка: решает человек.
		return askOwner(key, value, reason)
	default:
		return 1
	}
}

// askOwner заводит просьбу и ждёт ответа человека.
func askOwner(key, value, reason string) int {
	var created struct {
		Request struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"request"`
	}
	status, err := localPOST("/api/settings/request",
		map[string]string{"key": key, "value": value, "reason": reason}, &created)
	if err != nil || status != http.StatusOK {
		if err != nil {
			fmt.Fprintf(os.Stderr, "remotai: %v\n", err)
		}
		return 1
	}
	fmt.Printf(localize.Text("🔐 «%s» меняет только владелец. Отправил ему просьбу, жду ответа…\n"), created.Request.Title)
	if reason == "" {
		// Без причины человеку решать не по чему — говорим агенту прямо.
		fmt.Println(localize.Text("   (в следующий раз добавьте --reason \"зачем это нужно\" — человек решает по причине)"))
	}

	deadline := time.Now().Add(10 * time.Minute)
	for time.Now().Before(deadline) {
		time.Sleep(5 * time.Second)
		var got struct {
			Request struct {
				Status string `json:"status"`
				Error  string `json:"error"`
			} `json:"request"`
		}
		if code := localGET("/api/settings/requests?id="+created.Request.ID, &got); code != 0 {
			continue
		}
		switch got.Request.Status {
		case "approved":
			if got.Request.Error != "" {
				fmt.Fprintf(os.Stderr, localize.Text("⚠️ Владелец разрешил, но применить не удалось: %s\n"), got.Request.Error)
				return 1
			}
			fmt.Println(localize.Text("✅ Владелец подтвердил, настройка изменена"))
			return 0
		case "rejected":
			fmt.Println(localize.Text("🚫 Владелец отклонил изменение"))
			return 1
		case "expired":
			fmt.Println(localize.Text("⌛ Владелец не ответил — изменение не сделано"))
			return 1
		}
	}
	fmt.Println(localize.Text("⌛ Ответа пока нет. Продолжайте работу; проверить — remotai config requests"))
	return 1
}

func configRequests(args []string) int {
	var out struct {
		Requests []struct {
			ID     string `json:"id"`
			Title  string `json:"title"`
			Value  string `json:"value"`
			Status string `json:"status"`
			Error  string `json:"error"`
		} `json:"requests"`
	}
	if code := localGET("/api/settings/requests", &out); code != 0 {
		return code
	}
	if len(out.Requests) == 0 {
		fmt.Println(localize.Text("Просьб не было"))
		return 0
	}
	for _, r := range out.Requests {
		line := fmt.Sprintf("%s → %s: %s", r.Title, r.Value, requestStatusText(r.Status))
		if r.Error != "" {
			line += " (" + r.Error + ")"
		}
		fmt.Println(line)
	}
	return 0
}

func requestStatusText(status string) string {
	switch status {
	case "pending":
		return localize.Text("ждёт ответа")
	case "approved":
		return localize.Text("разрешено")
	case "rejected":
		return localize.Text("отклонено")
	case "expired":
		return localize.Text("просрочено")
	}
	return status
}

func formatValue(v any) string {
	switch value := v.(type) {
	case nil:
		return "—"
	case bool:
		if value {
			return localize.Text("да")
		}
		return localize.Text("нет")
	case float64: // JSON-числа
		return strconv.FormatFloat(value, 'f', -1, 64)
	case string:
		if value == "" {
			return "—"
		}
		return value
	}
	return fmt.Sprint(v)
}

// ── доступ к локальному агенту ───────────────────────────────────────────────
//
// Ходим на 127.0.0.1 с токеном из конфига: то же, что делает панель в окне.
// Это НЕ облачный путь — команда работает и без интернета, и без привязки.

func localBase() string {
	return "http://127.0.0.1:" + strconv.Itoa(config.GetNoSetup().Port())
}

func localToken() string {
	return config.GetNoSetup().APIToken
}

func localGET(path string, out any) int {
	req, err := http.NewRequest(http.MethodGet, localBase()+path, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "remotai: %v\n", err)
		return 1
	}
	req.Header.Set("X-Api-Token", localToken())
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, localize.Text("remotai: приложение не отвечает на этом компьютере — запустите Remotai"))
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "remotai: "+localErrorText(resp))
		return 1
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out); err != nil {
		fmt.Fprintf(os.Stderr, localize.Text("remotai: непонятный ответ: %v\n"), err)
		return 1
	}
	return 0
}

// localPOST возвращает HTTP-статус: вызывающему важно отличить «нельзя,
// нужно разрешение» (403) от настоящей ошибки.
func localPOST(path string, body any, out any) (int, error) {
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, localBase()+path, bytes.NewReader(raw))
	if err != nil {
		return 0, err
	}
	req.Header.Set("X-Api-Token", localToken())
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return 0, fmt.Errorf("%s", localize.Text("приложение не отвечает на этом компьютере — запустите Remotai"))
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode != http.StatusForbidden {
			fmt.Fprintln(os.Stderr, "remotai: "+errorTextFrom(data, resp.StatusCode))
		}
		return resp.StatusCode, nil
	}
	if out != nil && len(data) > 0 {
		_ = json.Unmarshal(data, out)
	}
	return resp.StatusCode, nil
}

func localErrorText(resp *http.Response) string {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return errorTextFrom(data, resp.StatusCode)
}

// errorTextFrom достаёт человеческое сообщение агента; коды HTTP агенту
// ничего не говорят, а решение у него от них не меняется.
func errorTextFrom(data []byte, status int) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(data, &e) == nil && e.Error != "" {
		return e.Error
	}
	if status == http.StatusUnauthorized {
		return localize.Text("нет доступа к локальному API (проверьте, что Remotai запущен от вашего пользователя)")
	}
	return fmt.Sprintf(localize.Text("ошибка %d"), status)
}
