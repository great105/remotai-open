package relay

import (
	"testing"
	"time"
)

// TestJitteredBackoff: задержка всегда в [ReconnectMin, ceil], не превышает
// потолок, и при большом ceil реально «размазана» (не константа).
func TestJitteredBackoff(t *testing.T) {
	if got := jitteredBackoff(ReconnectMin); got != ReconnectMin {
		t.Fatalf("ceil==min: got %s, want %s", got, ReconnectMin)
	}
	if got := jitteredBackoff(0); got != ReconnectMin {
		t.Fatalf("ceil<min: got %s, want %s (нижняя граница)", got, ReconnectMin)
	}

	ceil := 30 * time.Second
	seen := make(map[time.Duration]struct{})
	for i := 0; i < 2000; i++ {
		d := jitteredBackoff(ceil)
		if d < ReconnectMin || d > ceil {
			t.Fatalf("out of range: %s not in [%s, %s]", d, ReconnectMin, ceil)
		}
		seen[d] = struct{}{}
	}
	// Full Jitter обязан давать разброс — иначе thundering herd не гасится.
	if len(seen) < 100 {
		t.Fatalf("distribution too narrow: only %d distinct delays in 2000 draws", len(seen))
	}
}
