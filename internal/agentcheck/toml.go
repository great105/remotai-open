package agentcheck

import (
	"bufio"
	"os"
	"strings"
)

// codexConfig — ровно то, что из config.toml Codex нужно, чтобы понять, КУДА он
// ходит. Полного разбора TOML здесь нет намеренно: библиотеки в модуле нет, а
// читать надо пять ключей верхнего уровня и две таблицы. Всё остальное (MCP,
// проекты, уведомления) пропускается, и значения оттуда в память не попадают —
// в config.toml у людей бывают и заголовки с токенами.
type codexConfig struct {
	Found         bool
	Profile       string
	Model         string
	ModelProvider string
	// OpenAIBaseURL — `openai_base_url`: подмена адреса встроенного провайдера
	// openai. Проверено ЗАПУСКОМ 29.09.2026 (codex-cli 0.158.0): с
	// `-c openai_base_url=…` запросы ушли на подменённый адрес, а переменная
	// окружения OPENAI_BASE_URL не подействовала вовсе.
	OpenAIBaseURL string
	Profiles      map[string]codexProfile
	Providers     map[string]codexProvider
}

type codexProfile struct {
	Model         string
	ModelProvider string
}

type codexProvider struct {
	Name    string
	BaseURL string
	EnvKey  string
	WireAPI string
}

// readCodexConfig читает config.toml. Нет файла — пустая структура без ошибки:
// это обычное состояние, у Codex всё по умолчанию.
func readCodexConfig(path string) codexConfig {
	cfg := codexConfig{Profiles: map[string]codexProfile{}, Providers: map[string]codexProvider{}}
	f, err := os.Open(path)
	if err != nil {
		return cfg
	}
	defer f.Close()
	cfg.Found = true
	parseCodexConfig(bufio.NewScanner(f), &cfg)
	return cfg
}

func parseCodexConfig(sc *bufio.Scanner, cfg *codexConfig) {
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	table := []string{}
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			if strings.HasPrefix(line, "[[") {
				table = []string{"\x00array"}
				continue
			}
			end := strings.LastIndex(line, "]")
			if end < 0 {
				table = []string{"\x00bad"}
				continue
			}
			table = splitTableName(line[1:end])
			continue
		}
		eq := strings.Index(line, "=")
		if eq < 0 {
			continue
		}
		key := unquoteKey(strings.TrimSpace(line[:eq]))
		raw := strings.TrimSpace(line[eq+1:])
		val, ok := tomlString(raw)
		if !ok {
			continue
		}
		switch {
		case len(table) == 0:
			switch key {
			case "profile":
				cfg.Profile = val
			case "model":
				cfg.Model = val
			case "model_provider":
				cfg.ModelProvider = val
			case "openai_base_url":
				cfg.OpenAIBaseURL = val
			}
		case len(table) == 2 && table[0] == "profiles":
			p := cfg.Profiles[table[1]]
			switch key {
			case "model":
				p.Model = val
			case "model_provider":
				p.ModelProvider = val
			}
			cfg.Profiles[table[1]] = p
		case len(table) == 2 && table[0] == "model_providers":
			p := cfg.Providers[table[1]]
			switch key {
			case "name":
				p.Name = val
			case "base_url":
				p.BaseURL = val
			case "env_key":
				p.EnvKey = val
			case "wire_api":
				p.WireAPI = val
			}
			cfg.Providers[table[1]] = p
		}
	}
}

// splitTableName разбирает `a.b`, `a."b c"`, `a.'b.c'`.
func splitTableName(s string) []string {
	var parts []string
	var cur strings.Builder
	quote := byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			} else {
				cur.WriteByte(c)
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '.':
			parts = append(parts, strings.TrimSpace(cur.String()))
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	parts = append(parts, strings.TrimSpace(cur.String()))
	return parts
}

func unquoteKey(k string) string {
	if len(k) >= 2 && (k[0] == '"' || k[0] == '\'') && k[len(k)-1] == k[0] {
		return k[1 : len(k)-1]
	}
	return k
}

// tomlString — значение-строка в одинарных или двойных кавычках (с хвостовым
// комментарием или без). Числа, массивы и таблицы нам не нужны.
func tomlString(raw string) (string, bool) {
	if raw == "" {
		return "", false
	}
	q := raw[0]
	if q != '"' && q != '\'' {
		return "", false
	}
	var b strings.Builder
	for i := 1; i < len(raw); i++ {
		c := raw[i]
		if q == '"' && c == '\\' && i+1 < len(raw) {
			i++
			switch raw[i] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			default:
				b.WriteByte(raw[i])
			}
			continue
		}
		if c == q {
			return b.String(), true
		}
		b.WriteByte(c)
	}
	return "", false
}
