package web

// Подключение OpenRouter: один ключ — и агент работает на любой из четырёх сотен
// моделей, включая бесплатные, без подписки на Claude или ChatGPT.
//
// ЧТО ЗДЕСЬ ВАЖНО ЗНАТЬ, ПРЕЖДЕ ЧЕМ ПРАВИТЬ:
//
//  1. КЛЮЧ НЕ ВЫХОДИТ НАРУЖУ НИ ОДНИМ ЭНДПОИНТОМ. Наружу едет только метка,
//     которую маскирует сам OpenRouter («sk-or-v1-611...a5d»), и цифры расхода.
//     Ровно то же правило, что у паролей SSH-серверов.
//  2. БАЛАНС АККАУНТА ПОКАЗАТЬ НЕЛЬЗЯ. Проверено живым ключом: /api/v1/credits
//     отвечает 403 обычным ключом вывода, /me и /activity — тоже. Доступен
//     только расход ключа и остаток ЕГО лимита, если человек этот лимит задал.
//     Поэтому интерфейс говорит «потрачено», а не «баланс», и предлагает завести
//     ключ с лимитом — тогда остаток появляется честным образом.
//  3. ДНЕВНАЯ КВОТА БЕСПЛАТНЫХ МОДЕЛЕЙ ЗАВИСИТ ОТ ПОПОЛНЕНИЯ: 50 запросов в
//     сутки, пока человек ни разу не платил, и 1000 после разового пополнения.
//     50 — это меньше одной агентной задачи, поэтому число называем прямо.

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"tgcontrol/internal/openrouter"
)

// orModelsCache — каталог моделей меняется не чаще, чем раз в дни, а весит
// несколько мегабайт: 400 моделей. Держим снимок, чтобы открытие экрана не
// стоило человеку секунды ожидания и лишнего трафика на телефоне.
type orModelsCacheT struct {
	at     time.Time
	models []openrouter.Model
}

var orModelsCache orModelsCacheT

const orModelsTTL = 30 * time.Minute

// GET /api/openrouter — состояние подключения.
//
// Ключ `configured` есть всегда: по нему клиент отличает агента, который умеет
// OpenRouter, от старого, который про него не знает (то же правило, что с
// `accounts`).
func (s *Server) apiOpenRouterStatus(w http.ResponseWriter, r *http.Request, uid int64) {
	out := map[string]any{
		"configured": s.openrouter.Configured(),
		"env_name":   openrouter.EnvName,
		"model":      s.openrouter.Model(),
	}
	if s.openrouter.Foreign() {
		// Файл цел, но зашифрован другой учётной записью Windows. Сказать это
		// словами — не то же самое, что показать пустое поле: ключ не пропал,
		// он просто недоступен отсюда.
		out["foreign"] = true
	}
	if !s.openrouter.Configured() {
		jsonResp(w, out)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	info, err := openrouter.New(s.openrouter.Key()).KeyInfo(ctx)
	if err != nil {
		// Ключ сохранён, но сейчас не отвечает. Это РАЗНЫЕ новости: «ключ
		// отозвали» человек чинит сам, «OpenRouter недоступен» проходит само.
		if errors.Is(err, openrouter.ErrUnauthorized) {
			out["key_state"] = "rejected"
		} else {
			out["key_state"] = "unreachable"
		}
		jsonResp(w, out)
		return
	}
	out["key_state"] = "ok"
	out["label"] = info.Label
	out["usage"] = info.Usage
	out["usage_daily"] = info.UsageDaily
	out["usage_weekly"] = info.UsageWeekly
	out["usage_monthly"] = info.UsageMonthly
	out["is_free_tier"] = info.IsFreeTier
	out["free_daily_quota"] = info.FreeDailyQuota()
	out["free_rpm"] = openrouter.FreeRequestsPerMinute
	if info.Limit != nil {
		out["limit"] = *info.Limit
	}
	if info.LimitRemaining != nil {
		out["limit_remaining"] = *info.LimitRemaining
	}
	jsonResp(w, out)
}

// POST /api/openrouter/key {key} — сохранить ключ.
//
// Ключ ПРОВЕРЯЕТСЯ ДО СОХРАНЕНИЯ. Сохранить непроверенный — значит отдать
// человеку зелёную галочку, а отказ он увидит через полчаса внутри агента,
// строкой на английском, посреди работы.
func (s *Server) apiOpenRouterSetKey(w http.ResponseWriter, r *http.Request, uid int64) {
	var req struct {
		Key string `json:"key"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonErrorCode(w, 400, "bad_request", "bad request", nil)
		return
	}
	key := strings.TrimSpace(req.Key)
	if key == "" {
		jsonErrorCode(w, 400, "key_empty", "ключ не введён", nil)
		return
	}
	if !openrouter.LooksLikeKey(key) {
		// Форма ключа проверяется до сети: человек, вставивший ключ от другого
		// сервиса, узнаёт об этом сразу и по делу.
		jsonErrorCode(w, 400, "key_malformed", "это не похоже на ключ OpenRouter — он начинается с sk-or-", nil)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	info, err := openrouter.New(key).KeyInfo(ctx)
	if err != nil {
		if errors.Is(err, openrouter.ErrUnauthorized) {
			jsonErrorCode(w, 400, "key_rejected", "OpenRouter не принял этот ключ", nil)
			return
		}
		jsonErrorCode(w, 502, "openrouter_unreachable", "OpenRouter сейчас не отвечает", nil)
		return
	}
	if err := s.openrouter.Set(key); err != nil {
		jsonErrorCode(w, 500, "save_failed", "не удалось сохранить ключ", nil)
		return
	}

	// Подключил ключ — и всё работает, без выбора из четырёхсот моделей.
	//
	// По умолчанию ставим «Free Models Router»: он сам подбирает бесплатную
	// модель под каждый запрос и учитывает, что агенту нужны инструменты
	// (проверено запуском — см. RouterModel). Это ровно то, чего ждут от
	// «подключить ключ»: не выбирать модель, а начать работать.
	//
	// Выбор ЧЕЛОВЕКА не трогаем никогда: если модель уже задана, она главнее —
	// её выбирали осознанно, в том числе платную.
	if s.openrouter.Model() == "" {
		if err := s.openrouter.SetModel(openrouter.RouterModel); err != nil {
			// Не повод валить подключение: ключ уже сохранён и работает,
			// модель человек всегда может выбрать сам.
			log.Printf("[OPENROUTER] модель по умолчанию не сохранилась: %v", err)
		}
	}

	jsonResp(w, map[string]any{
		"ok":               true,
		"label":            info.Label,
		"is_free_tier":     info.IsFreeTier,
		"free_daily_quota": info.FreeDailyQuota(),
		"model":            s.openrouter.Model(),
		// ЧЕСТНОЕ ОГРАНИЧЕНИЕ, а не мелкий шрифт: терминалы, открытые до этой
		// минуты, ключа не получат — они родились раньше и унаследовали прежнее
		// окружение. Клиент обязан сказать это словами.
		"applies_to_new_terminals": true,
	})
}

// DELETE /api/openrouter/key — забыть ключ.
func (s *Server) apiOpenRouterForgetKey(w http.ResponseWriter, r *http.Request, uid int64) {
	if err := s.openrouter.Clear(); err != nil {
		jsonErrorCode(w, 500, "forget_failed", "не удалось удалить ключ", nil)
		return
	}
	jsonResp(w, map[string]any{"ok": true})
}

// GET /api/openrouter/models[?free=1] — каталог моделей.
func (s *Server) apiOpenRouterModels(w http.ResponseWriter, r *http.Request, uid int64) {
	if !s.openrouter.Configured() {
		jsonErrorCode(w, 400, "no_key", "ключ OpenRouter не подключён", nil)
		return
	}
	onlyFree := r.URL.Query().Get("free") == "1"

	models := orModelsCache.models
	if models == nil || time.Since(orModelsCache.at) > orModelsTTL {
		ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
		defer cancel()
		fresh, err := openrouter.New(s.openrouter.Key()).Models(ctx)
		if err != nil {
			// Просроченный снимок лучше пустого экрана: каталог меняется
			// медленно, а человек пришёл выбрать модель.
			if models == nil {
				jsonErrorCode(w, 502, "openrouter_unreachable", "не удалось получить список моделей", nil)
				return
			}
		} else {
			models = fresh
			orModelsCache = orModelsCacheT{at: time.Now(), models: fresh}
		}
	}

	out := make([]openrouter.Model, 0, len(models))
	for _, m := range models {
		if onlyFree && !m.Free {
			continue
		}
		out = append(out, m)
	}
	free := 0
	for _, m := range models {
		if m.Free {
			free++
		}
	}
	jsonResp(w, map[string]any{
		"models":     out,
		"total":      len(models),
		"free_total": free,
		"selected":   s.openrouter.Model(),
	})
}

// POST /api/openrouter/model {model} — какой моделью запускать агента.
//
// Пустое значение = «не задавать модель», агент возьмёт свою по умолчанию.
func (s *Server) apiOpenRouterSetModel(w http.ResponseWriter, r *http.Request, uid int64) {
	var req struct {
		Model string `json:"model"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonErrorCode(w, 400, "bad_request", "bad request", nil)
		return
	}
	if err := s.openrouter.SetModel(req.Model); err != nil {
		jsonErrorCode(w, 500, "save_failed", "не удалось сохранить модель", nil)
		return
	}
	jsonResp(w, map[string]any{"ok": true, "model": s.openrouter.Model()})
}
