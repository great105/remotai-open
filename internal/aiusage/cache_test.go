package aiusage

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func resetCacheForTest(t *testing.T) {
	t.Helper()
	snapshotCache.Lock()
	snapshotCache.snap = Snapshot{}
	snapshotCache.loaded = false
	snapshotCache.refreshing = false
	snapshotCache.Unlock()
	t.Cleanup(func() {
		snapshotCache.Lock()
		snapshotCache.snap = Snapshot{}
		snapshotCache.loaded = false
		snapshotCache.refreshing = false
		snapshotCache.Unlock()
	})
}

func stubCollect(t *testing.T, calls *atomic.Int32) {
	t.Helper()
	original := collectProviders
	collectProviders = func(ctx context.Context) Snapshot {
		calls.Add(1)
		return Snapshot{CapturedAt: time.Now(), Providers: []Provider{}}
	}
	t.Cleanup(func() { collectProviders = original })
}

func TestCollectCachedServesFreshSnapshotFromCache(t *testing.T) {
	resetCacheForTest(t)
	var calls atomic.Int32
	stubCollect(t, &calls)

	CollectCached(context.Background())
	CollectCached(context.Background())
	CollectCached(context.Background())

	if got := calls.Load(); got != 1 {
		t.Fatalf("vendor calls=%d want=1 (TTL cache)", got)
	}
}

func TestCollectCachedRefreshesAfterTTL(t *testing.T) {
	resetCacheForTest(t)
	t.Setenv("REMOTAI_AI_USAGE_TTL", "1ms")
	var calls atomic.Int32
	stubCollect(t, &calls)

	CollectCached(context.Background())
	time.Sleep(10 * time.Millisecond)
	CollectCached(context.Background())

	if got := calls.Load(); got != 2 {
		t.Fatalf("vendor calls=%d want=2 after TTL expiry", got)
	}
}

// Список аккаунтов изменился — снимок обязан собраться заново, не дожидаясь
// TTL. Найдено живым прогоном на стенде: заведённый аккаунт не появлялся в
// лимитах пять минут, а пометка «сейчас» продолжала стоять у прежнего — то есть
// человек переключался ради остатка и видел остаток НЕ ТОГО аккаунта.
func TestInvalidateCacheForcesFreshCollect(t *testing.T) {
	resetCacheForTest(t)
	var calls atomic.Int32
	stubCollect(t, &calls)

	CollectCached(context.Background())
	CollectCached(context.Background())
	if got := calls.Load(); got != 1 {
		t.Fatalf("vendor calls=%d want=1 before invalidation", got)
	}

	InvalidateCache()
	if _, loaded := CachedSnapshot(); loaded {
		t.Fatal("после сброса снимка не должно быть вовсе")
	}
	CollectCached(context.Background())
	if got := calls.Load(); got != 2 {
		t.Fatalf("vendor calls=%d want=2 after invalidation", got)
	}
}

func TestRefreshInBackgroundDeduplicates(t *testing.T) {
	resetCacheForTest(t)
	original := collectProviders
	release := make(chan struct{})
	var calls atomic.Int32
	collectProviders = func(ctx context.Context) Snapshot {
		calls.Add(1)
		<-release // держим первый сбор, пока тест зовёт RefreshInBackground ещё раз
		return Snapshot{CapturedAt: time.Now(), Providers: []Provider{}}
	}
	t.Cleanup(func() { collectProviders = original })

	RefreshInBackground()
	RefreshInBackground()
	RefreshInBackground()
	close(release)

	deadline := time.Now().Add(2 * time.Second)
	for {
		snapshotCache.Lock()
		done := snapshotCache.loaded && !snapshotCache.refreshing
		snapshotCache.Unlock()
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background refresh did not finish")
		}
		time.Sleep(5 * time.Millisecond)
	}

	if got := calls.Load(); got != 1 {
		t.Fatalf("vendor calls=%d want=1 (dedup)", got)
	}
	if _, loaded := CachedSnapshot(); !loaded {
		t.Fatal("cache must be loaded after background refresh")
	}
}

// Эндпоинт агентов опирается на кэш: до первого сбора квота — unknown,
// а не выдуманный ноль.
func TestCachedSnapshotEmptyBeforeFirstCollect(t *testing.T) {
	resetCacheForTest(t)
	if _, loaded := CachedSnapshot(); loaded {
		t.Fatal("loaded=true before any collect")
	}
	quota := QuotaForAgent(Snapshot{}, false, "claude")
	if quota == nil || quota.Status != "unknown" || quota.FiveHour != nil {
		t.Fatalf("quota=%#v", quota)
	}
}
