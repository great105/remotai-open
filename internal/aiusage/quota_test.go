package aiusage

import (
	"testing"
	"time"
)

func ptrFloat(v float64) *float64 { return &v }
func ptrInt64(v int64) *int64     { return &v }

func TestQuotaForAgentClaudeWindows(t *testing.T) {
	snap := Snapshot{
		CapturedAt: time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC),
		Providers: []Provider{{
			ID: "claude", Name: "Claude", Status: "available",
			Windows: []Window{
				{ID: "five_hour", Label: "Текущее 5-часовое окно", UsedPercent: 12.5},
				{ID: "seven_day", Label: "Недельный лимит", UsedPercent: 99.8},
				{ID: "seven_day_opus", Label: "Недельный лимит Opus", UsedPercent: 40},
			},
		}},
	}

	quota := QuotaForAgent(snap, true, "claude")
	if quota == nil {
		t.Fatal("quota=nil")
	}
	if quota.Status != "available" {
		t.Fatalf("status=%q", quota.Status)
	}
	if quota.FiveHour == nil || *quota.FiveHour != 12.5 {
		t.Fatalf("five=%v", quota.FiveHour)
	}
	if quota.SevenDay == nil || *quota.SevenDay != 99.8 {
		t.Fatalf("seven=%v", quota.SevenDay)
	}
	if quota.CapturedAt == nil || !quota.CapturedAt.Equal(snap.CapturedAt) {
		t.Fatalf("captured=%v", quota.CapturedAt)
	}
}

func TestQuotaForAgentCodexWindowsByDuration(t *testing.T) {
	snap := Snapshot{
		CapturedAt: time.Now(),
		Providers: []Provider{{
			ID: "codex", Name: "OpenAI Codex", Status: "available",
			Windows: []Window{
				{ID: "codex:primary", UsedPercent: 25, DurationMinutes: ptrInt64(300)},
				{ID: "codex:secondary", UsedPercent: 9, DurationMinutes: ptrInt64(10080)},
			},
		}},
	}

	quota := QuotaForAgent(snap, true, "codex")
	if quota == nil || quota.FiveHour == nil || quota.SevenDay == nil {
		t.Fatalf("quota=%#v", quota)
	}
	if *quota.FiveHour != 25 || *quota.SevenDay != 9 {
		t.Fatalf("five=%v seven=%v", *quota.FiveHour, *quota.SevenDay)
	}
}

func TestQuotaForAgentCodexUnknownDurationIsNotInvented(t *testing.T) {
	// primary/secondary are roles, not durations: an unknown window must not
	// be labelled five hours or seven days.
	snap := Snapshot{
		CapturedAt: time.Now(),
		Providers: []Provider{{
			ID: "codex", Name: "OpenAI Codex", Status: "available",
			Windows: []Window{
				{ID: "codex:primary", UsedPercent: 61},
				{ID: "codex:secondary", UsedPercent: 3},
			},
		}},
	}

	quota := QuotaForAgent(snap, true, "codex")
	if quota == nil || quota.FiveHour != nil || quota.SevenDay != nil {
		t.Fatalf("invented duration: %#v", quota)
	}
}

// Unknown cost is never $0: любой не-available статус обязан оставлять
// проценты пустыми — фронт рисует «?», а не «0%».
func TestQuotaForAgentUnavailableHasNoPercents(t *testing.T) {
	cases := []struct {
		name   string
		loaded bool
		snap   Snapshot
		agent  string
		want   string
	}{
		{
			name:   "snapshot never loaded",
			loaded: false,
			agent:  "claude",
			want:   "unknown",
		},
		{
			name:   "signed out",
			loaded: true,
			snap: Snapshot{Providers: []Provider{{
				ID: "claude", Status: "signed_out",
				Message: "Войдите в Claude Code на этом устройстве.", MessageCode: MsgSignIn,
			}}},
			agent: "claude",
			want:  "signed_out",
		},
		{
			name:   "provider error",
			loaded: true,
			snap: Snapshot{Providers: []Provider{{
				ID: "codex", Status: "unavailable",
				Message: "Codex не вернул текущие окна лимитов.", MessageCode: MsgNoWindows,
			}}},
			agent: "codex",
			want:  "unavailable",
		},
		{
			name:   "unsupported client",
			loaded: true,
			snap: Snapshot{Providers: []Provider{{
				ID: "kimi", Status: "unsupported", MessageCode: MsgUnsupported,
			}}},
			agent: "kimi",
			want:  "unsupported",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			quota := QuotaForAgent(tc.snap, tc.loaded, tc.agent)
			if quota == nil {
				t.Fatal("quota=nil")
			}
			if quota.Status != tc.want {
				t.Fatalf("status=%q want=%q", quota.Status, tc.want)
			}
			if quota.FiveHour != nil || quota.SevenDay != nil {
				t.Fatalf("percents must stay empty: five=%v seven=%v", quota.FiveHour, quota.SevenDay)
			}
		})
	}
}

func TestQuotaForAgentNilCases(t *testing.T) {
	snap := Snapshot{
		CapturedAt: time.Now(),
		Providers:  []Provider{{ID: "claude", Status: "available"}},
	}
	// Встроенные агенты без подписки — чипа нет вовсе.
	for _, id := range []string{"shell", "orchestrator", "researcher"} {
		if quota := QuotaForAgent(snap, true, id); quota != nil {
			t.Fatalf("%s: quota=%#v want=nil", id, quota)
		}
	}
	// Провайдер не установлен — в снапшот не вошёл, чипа нет.
	if quota := QuotaForAgent(snap, true, "codex"); quota != nil {
		t.Fatalf("codex: quota=%#v want=nil", quota)
	}
}

// Реальный ноль от вендора — легитимные данные и должен доезжать как 0,
// а не превращаться в «нет данных».
func TestQuotaForAgentRealZeroKeepsZero(t *testing.T) {
	snap := Snapshot{
		CapturedAt: time.Now(),
		Providers: []Provider{{
			ID: "claude", Status: "available",
			Windows: []Window{{ID: "five_hour", UsedPercent: 0}},
		}},
	}
	quota := QuotaForAgent(snap, true, "claude")
	if quota == nil || quota.FiveHour == nil || *quota.FiveHour != 0 {
		t.Fatalf("quota=%#v", quota)
	}
	if quota.SevenDay != nil {
		t.Fatalf("seven=%v want=nil", quota.SevenDay)
	}
}
