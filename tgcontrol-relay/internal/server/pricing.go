package server

import "net/http"

// Единственный источник цен для всех интерфейсов (APK, веб, окно exe, Telegram).
// Значения — из `ПРОДВИЖЕНИЕ/КАНОН-ПРОДУКТА.md`, раздел «Полки и цены».
// Раньше цены жили в коде агента ($9/$29) и расходились с витриной: человек
// видел на сайте одно, а в приложении другое.
//
// Цены в МИНОРНЫХ единицах (копейки, центы) — чтобы нигде не возникло дробей.
type plan struct {
	// hidden — полка не показывается на витрине. Пока такая одна: проверочный
	// платёж на 10 ₽, которым владелец проверяет живые карты и весь путь денег
	// целиком. Показывать её всем нельзя — это была бы лазейка «Про за десятку».
	hidden      bool
	ID          string `json:"id"`
	Name        string `json:"name"`
	CloudDevs   int    `json:"cloud_devices"`
	Monthly     int    `json:"monthly_rub"`
	Annual      int    `json:"annual_rub"`
	MonthlyUSD  int    `json:"monthly_usd_cents"`
	AnnualUSD   int    `json:"annual_usd_cents"`
	Description string `json:"description"`
}

// Полки канона. «Локально» — не тариф в смысле оплаты, а состояние по умолчанию:
// дома всё работает бесплатно и навсегда.
var plans = []plan{
	{
		ID: "local", Name: "Локально", CloudDevs: 0,
		Monthly: 0, Annual: 0, MonthlyUSD: 0, AnnualUSD: 0,
		Description: "Локальная работа на своём компьютере: терминалы, агенты, файлы и экран — бесплатно навсегда. Серверы, SSH/SFTP и проброс портов входят в Про.",
	},
	{
		ID: "pro", Name: "Про", CloudDevs: 5,
		Monthly: 90000, Annual: 900000, MonthlyUSD: 1800, AnnualUSD: 18000,
		Description: "Серверы, SSH/SFTP, проброс портов и управление из любой точки: 5 облачных устройств, терминалы и агенты без ограничений, экран 30 к/с. Аренда VPS оплачивается отдельно.",
	},
	{
		// Проверочный платёж. Даёт сутки Про — ровно чтобы убедиться, что деньги
		// доходят, чек приходит и доступ открывается. Месяц за десять рублей не
		// продаём даже себе: тогда проверка врала бы про настоящий путь.
		hidden: true,
		ID:     "test", Name: "Проверка оплаты", CloudDevs: 5,
		Monthly: 1000, Annual: 0, MonthlyUSD: 0, AnnualUSD: 0,
		Description: "Проверочный платёж 10 ₽: сутки Про. Виден только администратору.",
	},
	{
		ID: "fleet", Name: "Флит", CloudDevs: 25,
		Monthly: 199000, Annual: 1990000, MonthlyUSD: 3900, AnnualUSD: 39000,
		Description: "Парк машин и другие люди: 25 облачных устройств, приоритет экрана, трое участников включены.",
	},
}

// handlePricing — GET /v1/pricing. Публично: витрина нужна и до входа.
//
// trial_days отдаём здесь же, чтобы приложение не хардкодило «30 дней»: если
// срок пробы меняется в конфиге, тексты в интерфейсе меняются вместе с ним.
func (s *Server) handlePricing(w http.ResponseWriter, r *http.Request) {
	if s.selfHosted() {
		writeJSON(w, http.StatusOK, map[string]any{
			"currency": "RUB", "trial_days": 0, "billing_enabled": false,
			"self_hosted": true, "payment_mode": "none", "period_days": 0,
			"plans": []plan{{ID: "local", Name: "Свой сервер", Description: "Полный доступ на своей инфраструктуре без подписки Remotai. Сервер и ИИ-сервисы оплачиваются их провайдерам отдельно."}},
			"note":  "Самостоятельное размещение бесплатно. Готовый сервис с обслуживанием доступен на https://remotai.ru.",
		})
		return
	}
	trialDays := 30
	billing := false
	if s.Config != nil {
		if s.Config.TrialDays > 0 {
			trialDays = s.Config.TrialDays
		}
		billing = s.Config.BillingEnabled
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"currency":   "RUB",
		"trial_days": trialDays,
		// Пока оплата не подключена, приложение не должно рисовать кнопку
		// «Оплатить», ведущую в никуда.
		"billing_enabled": billing,
		// Витрина показывает только публичные полки: скрытая «Проверка оплаты»
		// не должна попасть ни на сайт, ни в приложение к обычному человеку.
		"plans":        publicPlans(),
		"payment_mode": "one_time",
		"period_days":  30,
		"note":         "Локальная работа на своём компьютере бесплатна навсегда. Серверы, SSH/SFTP, проброс портов и облачный доступ входят в Про. Аренда VPS оплачивается отдельно.",
	})
}

// planByID — полка канона по её идентификатору. Один источник цен на весь
// продукт: витрина /v1/pricing и касса обязаны называть одну и ту же сумму,
// иначе человек увидит на сайте одно, а в форме оплаты другое.
func planByID(id string) (plan, bool) {
	for _, p := range plans {
		if p.ID == id {
			return p, true
		}
	}
	return plan{}, false
}

// publicPlans — полки для витрины: всё, кроме скрытых.
func publicPlans() []plan {
	out := make([]plan, 0, len(plans))
	for _, p := range plans {
		if p.hidden {
			continue
		}
		out = append(out, p)
	}
	return out
}
