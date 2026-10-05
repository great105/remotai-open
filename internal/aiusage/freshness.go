package aiusage

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func previousProvider(ctx context.Context, account Account) Provider {
	snap, _ := ctx.Value(previousSnapshotKey{}).(Snapshot)
	for _, p := range snap.Providers {
		if p.ID == account.Provider && p.AccountID == account.ID {
			return p
		}
	}
	return Provider{}
}

func collectAccount(ctx context.Context, account Account, run func(context.Context, Account) Provider) Provider {
	old := previousProvider(ctx, account)
	now := time.Now().UTC()
	if old.NextRetryAt != nil && now.Before(*old.NextRetryAt) {
		old.AccountActive, old.AccountLabel = account.Active, account.Label
		return old
	}
	p := run(ctx, account)
	return finishProviderAttempt(p, old, time.Now().UTC())
}

func finishProviderAttempt(p, old Provider, now time.Time) Provider {
	p.CheckedAt = now
	if p.Status == "available" {
		p.CapturedAt = now
		return p
	}
	if p.Status != "unavailable" {
		return p
	}
	p.failures = old.failures + 1
	delay := 30 * time.Second
	if p.MessageCode == MsgRateLimited {
		delay = 5 * time.Minute
	}
	for n := 1; n < p.failures && delay < 15*time.Minute; n++ {
		delay *= 2
	}
	if delay > 15*time.Minute {
		delay = 15 * time.Minute
	}
	retry := now.Add(delay)
	if p.NextRetryAt == nil || !p.NextRetryAt.After(now) {
		p.NextRetryAt = &retry
	}
	if (old.Status == "available" || old.Stale) && !old.CapturedAt.IsZero() {
		p.CapturedAt, p.Stale = old.CapturedAt, true
		p.Windows, p.Extra = old.Windows, old.Extra
		if p.Account == "" {
			p.Account = old.Account
		}
		if p.Plan == "" {
			p.Plan = old.Plan
		}
		if p.Source == "" {
			p.Source = old.Source
		}
	}
	return p
}

func retryAfter(value string, now time.Time) *time.Time {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds >= 0 {
		if seconds > 365*24*3600 {
			seconds = 365 * 24 * 3600
		}
		retry := now.Add(time.Duration(seconds) * time.Second)
		return &retry
	}
	if retry, err := http.ParseTime(value); err == nil && retry.After(now) {
		return &retry
	}
	return nil
}
