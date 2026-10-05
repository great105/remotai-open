package hermes

import (
	"context"
	"time"
)

// An application-owned observer subscribes without any browser/RPC request.
// ensureBridge's lifecycle barrier supplies exactly one socket per generation.
func (m *Manager) observeBackend(ctx context.Context) {
	defer func() {
		m.mu.Lock()
		m.observerActive = false
		b := m.bridge
		m.bridge = nil
		m.mu.Unlock()
		if b != nil {
			b.close()
		}
	}()
	delay := 100 * time.Millisecond
	for {
		if ctx.Err() != nil {
			return
		}
		m.mu.Lock()
		closing := m.closing
		eligible := m.ready && m.process != nil && m.isManagedLocked()
		m.mu.Unlock()
		if closing {
			return
		}
		if eligible {
			connectCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			b, err := m.ensureBridge(connectCtx)
			cancel()
			if err == nil {
				started := time.Now()
				select {
				case <-ctx.Done():
					return
				case <-b.done:
				}
				if time.Since(started) > 30*time.Second {
					delay = 100 * time.Millisecond
				} else if delay < 5*time.Second {
					delay *= 2
				}
				if delay > 5*time.Second {
					delay = 5 * time.Second
				}
			} else {
				m.mu.Lock()
				// A connected socket which never sends gateway.ready must not poison every
				// future attempt. Retire only the exact attempted bridge, never its successor.
				retire := b != nil && m.bridge == b
				if retire {
					m.bridge = nil
					m.epoch++
					m.epochStart = m.seq
					m.events = nil
					m.resetEventsLocked()
				}
				if !m.closing && m.ready {
					m.state.LastError = "Наблюдение Hermes прервано; подключение будет повторено"
				}
				m.mu.Unlock()
				if retire {
					b.close()
				}
				if delay < 5*time.Second {
					delay *= 2
				}
				if delay > 5*time.Second {
					delay = 5 * time.Second
				}
			}
		} else {
			delay = 100 * time.Millisecond
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
