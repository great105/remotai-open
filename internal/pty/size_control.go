package pty

import (
	"sort"
	"strconv"
	"time"
)

const sizeOwnerLease = 30 * time.Second

type SizeViewer struct {
	ID     string `json:"id"`
	Cols   int    `json:"cols"`
	Rows   int    `json:"rows"`
	CanOwn bool   `json:"can_own"`
}

// A negotiated extension inside the existing PTY socket. Legacy viewers keep
// the minimum-size policy and never receive an unsolicited control message.
type SizeControlState struct {
	Type         string       `json:"t"`
	Version      int          `json:"v"`
	Capabilities []string     `json:"capabilities"`
	Revision     uint64       `json:"revision"`
	You          string       `json:"you"`
	Owner        string       `json:"owner"`
	LeaseUntil   int64        `json:"lease_until"`
	Viewers      []SizeViewer `json:"viewers"`
	Error        string       `json:"error,omitempty"`
}

func (v *Viewer) ControlsWake() <-chan struct{} { return v.controlsWake }

// Called only with viewMu. Coalesce notifications; the writer samples the most
// recent state rather than queueing obsolete snapshots for slow clients.
func (s *Session) notifySizeControlsLocked() {
	for v := range s.viewers {
		if v.controls {
			select {
			case v.controlsWake <- struct{}{}:
			default:
			}
		}
	}
}

func (s *Session) EnableSizeControls(v *Viewer) {
	s.viewMu.Lock()
	defer s.viewMu.Unlock()
	if _, exists := s.viewers[v]; !exists {
		return
	}
	if !v.controls {
		v.controls = true
		s.sizeRevision++
	}
	s.notifySizeControlsLocked()
}

func (s *Session) SizeControls(v *Viewer) (SizeControlState, bool) {
	s.viewMu.Lock()
	defer s.viewMu.Unlock()
	if _, exists := s.viewers[v]; !exists || !v.controls {
		return SizeControlState{}, false
	}
	state := SizeControlState{Type: "terminal-controls", Version: 1,
		Capabilities: []string{"size-owner-v1"}, Revision: s.sizeRevision,
		You: v.id, Viewers: []SizeViewer{}, Error: v.controlError}
	v.controlError = ""
	if s.sizeOwner != nil {
		state.Owner = s.sizeOwner.id
		state.LeaseUntil = s.sizeLeaseUntil.UnixMilli()
	}
	for other := range s.viewers {
		state.Viewers = append(state.Viewers, SizeViewer{ID: other.id, Cols: other.cols, Rows: other.rows, CanOwn: other.controls})
	}
	sort.Slice(state.Viewers, func(i, j int) bool { return state.Viewers[i].ID < state.Viewers[j].ID })
	return state, true
}

func (s *Session) clearSizeOwnerLocked() {
	if s.sizeLeaseTimer != nil {
		s.sizeLeaseTimer.Stop()
		s.sizeLeaseTimer = nil
	}
	s.sizeOwner = nil
	s.sizeLeaseUntil = time.Time{}
	s.sizeRevision++
}

func (s *Session) renewSizeOwnerLocked(now time.Time) {
	if s.sizeLeaseTimer != nil {
		s.sizeLeaseTimer.Stop()
	}
	s.sizeLeaseUntil = now.Add(sizeOwnerLease)
	deadline := s.sizeLeaseUntil
	s.sizeLeaseTimer = time.AfterFunc(sizeOwnerLease, func() { s.expireSizeOwner(deadline, time.Now()) })
}

func (s *Session) expireSizeOwner(deadline, now time.Time) {
	s.viewMu.Lock()
	if s.sizeOwner == nil || !s.sizeLeaseUntil.Equal(deadline) || now.Before(deadline) {
		s.viewMu.Unlock()
		return
	}
	s.clearSizeOwnerLocked()
	s.notifySizeControlsLocked()
	s.viewMu.Unlock()
	_ = s.resizeToViewers(false)
}

// Only the current owner may release/transfer. Claim is a compare-and-swap
// against the shown state: simultaneous devices cannot silently steal control.
func (s *Session) ChangeSizeControl(v *Viewer, op, target string, revision uint64) {
	now := time.Now()
	s.viewMu.Lock()
	if _, exists := s.viewers[v]; !exists || !v.controls {
		s.viewMu.Unlock()
		return
	}
	v.controlError = ""
	changed := false
	if s.sizeOwner != nil && !now.Before(s.sizeLeaseUntil) {
		s.clearSizeOwnerLocked()
		changed = true
	}
	if op != "renew" && revision != s.sizeRevision {
		v.controlError = "stale"
	} else {
		switch op {
		case "claim":
			if s.sizeOwner != nil && s.sizeOwner != v {
				v.controlError = "owned"
			} else if v.cols < 2 || v.rows < 2 {
				v.controlError = "size_unknown"
			} else {
				s.sizeOwner = v
				s.sizeRevision++
				s.renewSizeOwnerLocked(now)
				changed = true
			}
		case "release":
			if s.sizeOwner != v {
				v.controlError = "not_owner"
			} else {
				s.clearSizeOwnerLocked()
				changed = true
			}
		case "transfer":
			var next *Viewer
			for other := range s.viewers {
				if other.id == target && other.controls && other.cols >= 2 && other.rows >= 2 {
					next = other
					break
				}
			}
			if s.sizeOwner != v {
				v.controlError = "not_owner"
			} else if next == nil {
				v.controlError = "viewer_missing"
			} else {
				s.sizeOwner = next
				s.sizeRevision++
				s.renewSizeOwnerLocked(now)
				changed = true
			}
		case "renew":
			if s.sizeOwner != v {
				v.controlError = "not_owner"
			} else {
				s.renewSizeOwnerLocked(now)
			}
		default:
			v.controlError = "unknown_action"
		}
	}
	requestRevision := s.sizeRevision
	if !changed {
		select {
		case v.controlsWake <- struct{}{}:
		default:
		}
	}
	s.viewMu.Unlock()
	if changed {
		err := s.resizeToViewers(false)
		s.viewMu.Lock()
		if err != nil {
			// Do not report an acquired policy as successful when the backend
			// rejected its geometry. Never roll back a newer transfer/request.
			if s.sizeRevision == requestRevision && s.sizeOwner != nil {
				s.clearSizeOwnerLocked()
			}
			v.controlError = "resize_failed"
		}
		s.notifySizeControlsLocked()
		s.viewMu.Unlock()
	}
}

func (s *Session) nextViewerIDLocked() string {
	s.viewerSequence++
	return strconv.FormatUint(s.viewerSequence, 10)
}
