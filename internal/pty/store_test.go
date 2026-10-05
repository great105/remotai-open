package pty

import (
	"path/filepath"
	"slices"
	"testing"
)

// TestMetaStore_ModesRoundTrip covers the DEC-mode persistence added for the
// "scroll dies after every auto-update" bug: modes written for a session with a
// host record must survive a reload (simulated server restart), while sessions
// without a host record (nothing to re-attach to) must not pollute the file.
func TestMetaStore_ModesRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pty.json")
	s := NewMetaStoreAt(path)

	// Session with a live host record — modes must persist.
	if err := s.PutHost("abc", hostRecord{HostPID: 42, PipeName: "p", CWD: "c", Shell: "sh"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetModes("abc", []int{1049, 1006}); err != nil {
		t.Fatal(err)
	}

	// Session without a host record — SetModes is a no-op.
	if err := s.SetModes("ghost", []int{1049}); err != nil {
		t.Fatal(err)
	}

	// Simulated restart: a fresh store reading the same file.
	s2 := NewMetaStoreAt(path)
	if got := s2.Get("abc").Modes; !slices.Equal(got, []int{1049, 1006}) {
		t.Fatalf("modes after reload = %v, want [1049 1006]", got)
	}
	if got := s2.Get("ghost").Modes; got != nil {
		t.Fatalf("ghost session persisted modes: %v", got)
	}

	// Clearing modes (app left alt-screen) persists the removal.
	if err := s2.SetModes("abc", nil); err != nil {
		t.Fatal(err)
	}
	s3 := NewMetaStoreAt(path)
	if got := s3.Get("abc").Modes; got != nil {
		t.Fatalf("modes not cleared after reload: %v", got)
	}
	// Host record must survive the mode churn.
	if h := s3.Get("abc").Host; h == nil || h.HostPID != 42 {
		t.Fatalf("host record lost: %+v", h)
	}
}
