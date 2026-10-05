package pty

import (
	"errors"
	"testing"
	"time"
)

type refusedSizeOwner struct{ sizeSpy }

func TestSizeOwnerFitsEveryViewerIncludingLegacyPhone(t *testing.T) {
	s, _ := newSizeSession(t, 240, 60)
	pc := s.AddViewer()
	t.Cleanup(func() { s.RemoveViewer(pc) })
	_ = s.ResizeFor(pc, 240, 60)
	s.EnableSizeControls(pc)
	state, _ := s.SizeControls(pc)
	s.ChangeSizeControl(pc, "claim", "", state.Revision)
	phone := s.AddViewer() // Deliberately does not negotiate size controls.
	t.Cleanup(func() { s.RemoveViewer(phone) })
	_ = s.ResizeFor(phone, 48, 30)
	if cols, rows := s.AppliedSize(); cols != 48 || rows != 30 {
		t.Fatalf("desktop owner clips the legacy phone: %dx%d", cols, rows)
	}
	s.ChangeSizeControl(pc, "renew", "", 0)
	_ = s.ResizeFor(pc, 300, 80)
	if cols, rows := s.AppliedSize(); cols != 48 || rows != 30 {
		t.Fatalf("owner renewal/resize bypasses viewer capacity: %dx%d", cols, rows)
	}
}

func (s *refusedSizeOwner) ResizeOrdered(int, int, func()) error {
	return errors.New("fixture resize rejected")
}

func TestSizeOwnerDoesNotClaimSuccessAfterBackendRejection(t *testing.T) {
	s, _ := newSizeSession(t, 80, 40)
	pc := s.AddViewer()
	t.Cleanup(func() { s.RemoveViewer(pc) })
	s.pty = &refusedSizeOwner{}
	_ = s.ResizeFor(pc, 100, 40)
	s.EnableSizeControls(pc)
	state, _ := s.SizeControls(pc)
	s.ChangeSizeControl(pc, "claim", "", state.Revision)
	state, _ = s.SizeControls(pc)
	if state.Owner != "" || state.Error != "resize_failed" {
		t.Fatalf("rejected claim: %+v", state)
	}
}

func TestSizeOwnerIsOptionalAndTransferIsExplicit(t *testing.T) {
	s, _ := newSizeSession(t, 80, 40)
	pc, phone, legacy := s.AddViewer(), s.AddViewer(), s.AddViewer()
	t.Cleanup(func() { s.RemoveViewer(pc); s.RemoveViewer(phone); s.RemoveViewer(legacy) })
	_ = s.ResizeFor(pc, 80, 40)
	_ = s.ResizeFor(phone, 80, 24)
	_ = s.ResizeFor(legacy, 70, 28)
	assertSize := func(cols, rows int) {
		t.Helper()
		c, r := s.AppliedSize()
		if c != cols || r != rows {
			t.Fatalf("size %dx%d, want %dx%d", c, r, cols, rows)
		}
	}
	assertSize(70, 24)
	if _, ok := s.SizeControls(legacy); ok {
		t.Fatal("legacy viewer received unsolicited extension")
	}
	s.EnableSizeControls(pc)
	s.EnableSizeControls(phone)
	state, _ := s.SizeControls(pc)
	s.ChangeSizeControl(pc, "claim", "", state.Revision)
	assertSize(70, 24)
	state, _ = s.SizeControls(phone)
	s.ChangeSizeControl(phone, "claim", "", state.Revision)
	denied, _ := s.SizeControls(phone)
	if denied.Error != "owned" || denied.Owner != pc.id {
		t.Fatalf("competing claim: %+v", denied)
	}
	assertSize(70, 24)
	_ = s.ResizeFor(legacy, 50, 20)
	assertSize(50, 20)
	state, _ = s.SizeControls(pc)
	s.ChangeSizeControl(pc, "transfer", legacy.id, state.Revision)
	denied, _ = s.SizeControls(pc)
	if denied.Error != "viewer_missing" {
		t.Fatalf("cannot transfer to an unnegotiated client: %+v", denied)
	}
	s.ChangeSizeControl(pc, "transfer", phone.id, state.Revision)
	assertSize(50, 20)
	state, _ = s.SizeControls(phone)
	s.ChangeSizeControl(phone, "release", "", state.Revision)
	assertSize(50, 20)
}

func TestSizeOwnerLeaseRejectsOldTimersAndStaleClaims(t *testing.T) {
	s, _ := newSizeSession(t, 80, 40)
	pc, phone := s.AddViewer(), s.AddViewer()
	t.Cleanup(func() { s.RemoveViewer(pc); s.RemoveViewer(phone) })
	_ = s.ResizeFor(pc, 100, 40)
	_ = s.ResizeFor(phone, 80, 24)
	s.EnableSizeControls(pc)
	s.EnableSizeControls(phone)
	state, _ := s.SizeControls(pc)
	_ = s.ResizeFor(phone, 79, 24)
	s.ChangeSizeControl(pc, "claim", "", state.Revision)
	state, _ = s.SizeControls(pc)
	if state.Error != "stale" || state.Owner != "" {
		t.Fatalf("stale state accepted: %+v", state)
	}
	s.ChangeSizeControl(pc, "claim", "", state.Revision)
	s.viewMu.Lock()
	old := s.sizeLeaseUntil
	s.renewSizeOwnerLocked(old)
	current := s.sizeLeaseUntil
	s.viewMu.Unlock()
	s.expireSizeOwner(old, current)
	state, _ = s.SizeControls(pc)
	if state.Owner != pc.id {
		t.Fatal("superseded timer released a renewed owner")
	}
	s.expireSizeOwner(current, current)
	state, _ = s.SizeControls(pc)
	if state.Owner != "" {
		t.Fatal("expired lease still owns geometry")
	}
	if cols, rows := s.AppliedSize(); cols != 79 || rows != 24 {
		t.Fatalf("expiry did not restore viewer minimum: %dx%d", cols, rows)
	}
	s.ChangeSizeControl(pc, "claim", "", state.Revision)
	s.RemoveViewer(pc)
	state, _ = s.SizeControls(phone)
	if state.Owner != "" {
		t.Fatal("disconnected owner retained its lease")
	}
	// A detached viewer cannot mutate geometry or renew through an old callback.
	s.ChangeSizeControl(pc, "renew", "", state.Revision)
	_ = s.ResizeFor(pc, 5, 5)
	if cols, _ := s.AppliedSize(); cols == 5 {
		t.Fatal("detached viewer changed geometry")
	}
	s.expireSizeOwner(time.Time{}, time.Now())
}
