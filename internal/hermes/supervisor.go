package hermes

import (
	"context"
	"time"
)

type supervisedStartKey struct{}

func (m *Manager) SetAutoStart(enabled bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	prior := m.state.AutoStart
	m.state.AutoStart = enabled
	if err := m.saveLocked(); err != nil {
		m.state.AutoStart = prior
		return err
	}
	return nil
}

// Bounded exponential backoff applies to BOTH failed launches and short-lived
// crashes. A successful health check alone must not reset a restart storm.
func (m *Manager) supervise(ctx context.Context) {
	delay := time.Second
	attempts := 0
	var healthySince time.Time
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		m.mu.Lock()
		enabled := m.state.AutoStart && !m.closing && m.isManagedLocked()
		running := m.process != nil
		ready := m.ready
		operation := m.state.Operation
		m.mu.Unlock()
		if !enabled {
			attempts = 0
			delay = time.Second
			healthySince = time.Time{}
		} else if running {
			if ready {
				if healthySince.IsZero() {
					healthySince = time.Now()
				}
				if time.Since(healthySince) >= 5*time.Minute {
					attempts = 0
					delay = time.Second
				}
			}
		} else if operation == "" {
			healthySince = time.Time{}
			if attempts >= 5 {
				delay = 15 * time.Minute
				attempts = 0
			} else {
				attempts++
				startCtx, cancel := context.WithTimeout(ctx, m.opts.StartupTimeout+time.Second)
				_ = m.Start(context.WithValue(startCtx, supervisedStartKey{}, true))
				cancel()
				delay = time.Second << uint(attempts-1)
			}
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
