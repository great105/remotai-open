package aiusage

import (
	"strings"
	"time"
)

// AgentQuota — проекция снапшота лимитов на один профиль агента в панели.
// «5ч»/«7д» — два окна подписки, которые человек видит и у самого вендора
// (Claude /usage, Codex status). Если квоту получить не удалось, процентов в
// структуре нет вообще — unknown cost is never $0: чип «0%» был бы ложью.
type AgentQuota struct {
	Status           string     `json:"status"` // available | signed_out | unavailable | unsupported | unknown
	FiveHour         *float64   `json:"five_hour_used,omitempty"`
	SevenDay         *float64   `json:"seven_day_used,omitempty"`
	Message          string     `json:"message,omitempty"`
	MessageCode      string     `json:"message_code,omitempty"`
	CapturedAt       *time.Time `json:"captured_at,omitempty"`
	Stale            bool       `json:"stale,omitempty"`
	FiveHourResetsAt *int64     `json:"five_hour_resets_at,omitempty"`
	SevenDayResetsAt *int64     `json:"seven_day_resets_at,omitempty"`
	// AccountID/AccountLabel — чей это остаток. При нескольких подписках на
	// одного вендора процент без имени аккаунта обманывает: человек видит
	// «82%», а работать собирается вторым аккаунтом, где 11%.
	AccountID    string `json:"account_id,omitempty"`
	AccountLabel string `json:"account_label,omitempty"`
}

// QuotaForAgent проецирует снапшот на агента панели (claude, codex, ...).
// Возвращает nil, когда чип не нужен вовсе: у встроенных агентов нет
// подписки, а неустановленный провайдер в снапшот не входит. loaded=false
// (сбор ни разу не завершался) даёт честный статус unknown.
func QuotaForAgent(snap Snapshot, loaded bool, agentID string) *AgentQuota {
	switch agentID {
	case "shell", "orchestrator", "researcher":
		return nil // встроенные агенты: подписки нет, чип не рисуем
	}
	if !loaded {
		return &AgentQuota{Status: "unknown"}
	}
	// Чип агента показывает остаток ТОГО аккаунта, которым агент и запустится,
	// — то есть активного. Без этого при двух подписках чип рисовал бы первый
	// попавшийся, и человек решал бы по чужому проценту.
	match := -1
	for i, p := range snap.Providers {
		if p.ID != agentID {
			continue
		}
		if match < 0 {
			match = i
		}
		if p.AccountActive {
			match = i
			break
		}
	}
	if match >= 0 {
		p := snap.Providers[match]
		quota := &AgentQuota{
			Status:       p.Status,
			Message:      p.Message,
			MessageCode:  p.MessageCode,
			AccountID:    p.AccountID,
			AccountLabel: p.AccountLabel,
		}
		captured := p.CapturedAt
		if captured.IsZero() {
			captured = snap.CapturedAt
		}
		quota.Stale = p.Stale
		quota.CapturedAt = &captured
		if p.Status == "available" || p.Stale {
			five, seven := MainWindows(p)
			if five != nil {
				quota.FiveHour = &five.UsedPercent
				quota.FiveHourResetsAt = five.ResetsAt
			}
			if seven != nil {
				quota.SevenDay = &seven.UsedPercent
				quota.SevenDayResetsAt = seven.ResetsAt
			}
		}
		return quota
	}
	return nil
}

// MainWindows keeps independent metered buckets separate. An explicit duration
// always wins over the legacy primary/secondary naming convention.
func MainWindows(p Provider) (five, seven *Window) {
	for i := range p.Windows {
		w := &p.Windows[i]
		if p.ID == "codex" {
			bucket := w.LimitID
			if bucket == "" {
				bucket, _, _ = strings.Cut(w.ID, ":")
			}
			if bucket != "codex" {
				continue
			}
		}
		if p.ID == "claude" && w.ID != "five_hour" && w.ID != "seven_day" {
			continue
		}
		if w.DurationMinutes != nil {
			switch *w.DurationMinutes {
			case 300:
				if five == nil {
					five = w
				}
			case 10080:
				if seven == nil {
					seven = w
				}
			}
			continue
		}
		switch {
		case w.ID == "five_hour":
			if five == nil {
				five = w
			}
		case w.ID == "seven_day":
			if seven == nil {
				seven = w
			}
		}
	}
	return
}
