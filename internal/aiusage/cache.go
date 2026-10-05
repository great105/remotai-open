package aiusage

import (
	"context"
	"os"
	"sync"
	"time"
)

// Leave room for provider collection (up to 12s) and transport. Otherwise a
// client polling every minute hits a still-valid 60s cache and only gets new
// data every other tick. This does not change per-account retry backoff.
const defaultCacheTTL = 45 * time.Second
const manualRefreshFloor = 10 * time.Second

func cacheTTL() time.Duration {
	if v := os.Getenv("REMOTAI_AI_USAGE_TTL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return defaultCacheTTL
}

var collectProviders = collectAll

type previousSnapshotKey struct{}
type collectionFlight struct {
	done       chan struct{}
	generation uint64
	cancel     context.CancelFunc
}

var snapshotCache struct {
	sync.Mutex
	snap               Snapshot
	loaded, refreshing bool
	generation         uint64
	flight             *collectionFlight
}

func CachedSnapshot() (Snapshot, bool) {
	snapshotCache.Lock()
	defer snapshotCache.Unlock()
	return snapshotCache.snap, snapshotCache.loaded
}

// Generation fences both completion and readers waiting on an old account.
// Cancellation of one HTTP request never cancels work shared by other viewers.
func InvalidateCache() {
	snapshotCache.Lock()
	snapshotCache.generation++
	snapshotCache.loaded = false
	snapshotCache.snap = Snapshot{}
	if snapshotCache.flight != nil {
		snapshotCache.flight.cancel()
	}
	snapshotCache.Unlock()
}

func resetDue(snap Snapshot, now time.Time) bool {
	for _, p := range snap.Providers {
		if p.NextRetryAt != nil && now.Before(*p.NextRetryAt) {
			continue
		}
		captured := p.CapturedAt
		if captured.IsZero() {
			captured = snap.CapturedAt
		}
		for _, w := range p.Windows {
			if w.ResetsAt != nil && captured.Unix() < *w.ResetsAt && now.Unix() >= *w.ResetsAt {
				return true
			}
		}
	}
	return false
}

// Caller holds snapshotCache. Every entry point joins the SAME collection.
func startCollectionLocked(force bool) *collectionFlight {
	if snapshotCache.flight != nil {
		return snapshotCache.flight
	}
	age := time.Since(snapshotCache.snap.CapturedAt)
	if snapshotCache.loaded && age < cacheTTL() && !resetDue(snapshotCache.snap, time.Now()) {
		if !force || age < manualRefreshFloor {
			return nil
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), providerTimeout+5*time.Second)
	ctx = context.WithValue(ctx, previousSnapshotKey{}, snapshotCache.snap)
	f := &collectionFlight{done: make(chan struct{}), generation: snapshotCache.generation, cancel: cancel}
	snapshotCache.flight, snapshotCache.refreshing = f, true
	go func() {
		defer cancel()
		snap := collectProviders(ctx)
		snapshotCache.Lock()
		if snapshotCache.generation == f.generation {
			snapshotCache.snap, snapshotCache.loaded = snap, true
		}
		snapshotCache.flight, snapshotCache.refreshing = nil, false
		close(f.done)
		snapshotCache.Unlock()
	}()
	return f
}

func CollectCached(ctx context.Context) Snapshot { return CollectCachedWithRefresh(ctx, false) }

func CollectCachedWithRefresh(ctx context.Context, force bool) Snapshot {
	for {
		snapshotCache.Lock()
		if ctx.Err() != nil {
			snap := snapshotCache.snap
			snapshotCache.Unlock()
			return snap
		}
		f := startCollectionLocked(force)
		if f == nil {
			snap := snapshotCache.snap
			snapshotCache.Unlock()
			return snap
		}
		snapshotCache.Unlock()
		select {
		case <-ctx.Done():
			snap, _ := CachedSnapshot()
			return snap
		case <-f.done:
			snapshotCache.Lock()
			valid := snapshotCache.loaded && f.generation == snapshotCache.generation
			snap := snapshotCache.snap
			snapshotCache.Unlock()
			if valid {
				return snap
			}
			// Account changed during collection. Retry against its new generation.
		}
	}
}

func RefreshInBackground() {
	snapshotCache.Lock()
	startCollectionLocked(false)
	snapshotCache.Unlock()
}
