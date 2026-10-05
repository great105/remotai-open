package aiusage

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestUsageConcurrentHTTPAndBackgroundShareCollection(t *testing.T) {
	resetCacheForTest(t)
	original := collectProviders
	defer func() { collectProviders = original }()
	var calls atomic.Int32
	entered, release := make(chan struct{}, 4), make(chan struct{})
	collectProviders = func(context.Context) Snapshot {
		calls.Add(1)
		entered <- struct{}{}
		<-release
		return Snapshot{CapturedAt: time.Now()}
	}
	var readers sync.WaitGroup
	for i := 0; i < 3; i++ {
		readers.Add(1)
		go func() { defer readers.Done(); CollectCached(context.Background()) }()
	}
	RefreshInBackground()
	<-entered
	time.Sleep(30 * time.Millisecond)
	close(release)
	readers.Wait()
	if calls.Load() != 1 {
		t.Fatalf("collections=%d want=1", calls.Load())
	}
}

func TestUsageInvalidationRejectsLateAccountResult(t *testing.T) {
	resetCacheForTest(t)
	original := collectProviders
	defer func() { collectProviders = original }()
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	collectProviders = func(context.Context) Snapshot {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
			return Snapshot{CapturedAt: time.Now(), Providers: []Provider{{AccountID: "old"}}}
		}
		return Snapshot{CapturedAt: time.Now(), Providers: []Provider{{AccountID: "new"}}}
	}
	result := make(chan Snapshot, 1)
	go func() { result <- CollectCached(context.Background()) }()
	<-entered
	InvalidateCache()
	close(release)
	select {
	case snap := <-result:
		if len(snap.Providers) != 1 || snap.Providers[0].AccountID != "new" {
			t.Fatalf("stale account returned: %#v", snap)
		}
	case <-time.After(time.Second):
		t.Fatal("new generation did not finish")
	}
}

func TestUsageManualRefreshAndResetBypassTTL(t *testing.T) {
	for _, reset := range []bool{false, true} {
		t.Run(map[bool]string{false: "manual", true: "reset"}[reset], func(t *testing.T) {
			resetCacheForTest(t)
			var calls atomic.Int32
			stubCollect(t, &calls)
			CollectCached(context.Background())
			snapshotCache.Lock()
			snapshotCache.snap.CapturedAt = time.Now().Add(-30 * time.Second)
			if reset {
				at := time.Now().Add(-time.Second).Unix()
				snapshotCache.snap.Providers = []Provider{{Windows: []Window{{ResetsAt: &at}}}}
			}
			snapshotCache.Unlock()
			CollectCachedWithRefresh(context.Background(), !reset)
			if calls.Load() != 2 {
				t.Fatalf("calls=%d want=2", calls.Load())
			}
		})
	}
}

func TestUsageMinutePollDoesNotSkipAfterSlowCollection(t *testing.T) {
	resetCacheForTest(t)
	var calls atomic.Int32
	stubCollect(t, &calls)
	CollectCached(context.Background())
	snapshotCache.Lock()
	// The next client tick is one minute after request start, while the
	// provider's observation arrived up to providerTimeout later.
	snapshotCache.snap.CapturedAt = time.Now().Add(-time.Minute + providerTimeout)
	snapshotCache.Unlock()
	CollectCached(context.Background())
	if calls.Load() != 2 {
		t.Fatal("a minute poll reused the previous observation after a slow collection")
	}
}

func TestUsageFailureRetainsSuccessAndRespectsRetryAfter(t *testing.T) {
	now := time.Now().UTC()
	old := Provider{ID: "claude", AccountID: "a", Status: "available", CapturedAt: now.Add(-time.Minute), Windows: []Window{{ID: "five_hour", UsedPercent: 63}}}
	retry := now.Add(75 * time.Second)
	failed := finishProviderAttempt(Provider{ID: "claude", AccountID: "a", Status: "unavailable", MessageCode: MsgRateLimited, NextRetryAt: &retry}, old, now)
	if !failed.Stale || !failed.CapturedAt.Equal(old.CapturedAt) || failed.Windows[0].UsedPercent != 63 || !failed.NextRetryAt.Equal(retry) {
		t.Fatalf("lost age/data/retry: %#v", failed)
	}
	ctx := context.WithValue(context.Background(), previousSnapshotKey{}, Snapshot{Providers: []Provider{failed}})
	reused := collectAccount(ctx, Account{ID: "a", Provider: "claude"}, func(context.Context, Account) Provider { t.Fatal("polled during Retry-After"); return Provider{} })
	if !reused.CheckedAt.Equal(now) {
		t.Fatal("a skipped retry must not claim a new attempt")
	}
	signedOut := finishProviderAttempt(Provider{Status: "signed_out"}, old, now)
	if len(signedOut.Windows) != 0 || signedOut.Stale {
		t.Fatal("signed-out account retained usable numbers")
	}
	recovered := finishProviderAttempt(Provider{Status: "available", Windows: []Window{{UsedPercent: 12}}}, failed, now.Add(time.Minute))
	if recovered.Stale || recovered.NextRetryAt != nil || !recovered.CapturedAt.After(old.CapturedAt) {
		t.Fatal("recovery did not clear stale/backoff")
	}
}

func TestUsageRetryAfterFormatsAndBackoff(t *testing.T) {
	now := time.Date(2026, 9, 6, 16, 0, 0, 0, time.UTC)
	for value, delay := range map[string]time.Duration{"90": 90 * time.Second, "Sun, 06 Sep 2026 16:03:00 GMT": 3 * time.Minute} {
		got := retryAfter(value, now)
		if got == nil || got.Sub(now) != delay {
			t.Fatalf("retry %q: %v", value, got)
		}
	}
	old := Provider{}
	for i := 0; i < 12; i++ {
		old = finishProviderAttempt(Provider{Status: "unavailable"}, old, now)
	}
	if old.NextRetryAt.Sub(now) != 15*time.Minute {
		t.Fatal("backoff must be bounded")
	}
}

func TestUsageCodexMainNeverBorrowsSparkWindow(t *testing.T) {
	week, short := int64(10080), int64(300)
	p := Provider{ID: "codex", Windows: []Window{
		{ID: "codex:primary", DurationMinutes: &week, UsedPercent: 24},
		{ID: "codex_bengalfox:primary", DurationMinutes: &short, UsedPercent: 0},
		{ID: "codex_bengalfox:secondary", DurationMinutes: &week, UsedPercent: 0},
	}}
	five, seven := MainWindows(p)
	if five != nil || seven == nil || seven.UsedPercent != 24 {
		t.Fatalf("mixed buckets five=%v seven=%v", five, seven)
	}
}
