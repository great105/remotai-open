package hermes

import (
	"context"
	"errors"
	"time"
)

func (m *Manager) SetDeliveryTransport(send func(context.Context, AttentionRecord) error, ready func() bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.notify = send
	m.deliveryReady = ready
}
func (m *Manager) SetDelivery(enabled bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if enabled && (m.notify == nil || m.deliveryReady == nil || !m.deliveryReady()) {
		return errors.New("Подключите Telegram к владельцу компьютера и включите уведомления в настройках Remotai")
	}
	prior := m.state.DeliveryEnabled
	m.state.DeliveryEnabled = enabled
	if err := m.saveLocked(); err != nil {
		m.state.DeliveryEnabled = prior
		return err
	}
	return nil
}
func (m *Manager) deliverAttention(ctx context.Context) {
	timer := time.NewTicker(time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		m.mu.Lock()
		enabled := m.state.DeliveryEnabled && !m.closing
		send, ready := m.notify, m.deliveryReady
		m.mu.Unlock()
		if !enabled || send == nil || ready == nil || !ready() {
			continue
		}
		j := m.control
		if j == nil {
			continue
		}
		j.mu.Lock()
		var notice *AttentionRecord
		for i := range j.Attention {
			r := &j.Attention[i]
			if r.Delivery == "" && (r.State == "pending" || r.State == "unread") {
				r.Delivery = "sending"
				if err := m.saveControlLocked(); err != nil {
					r.Delivery = ""
					break
				}
				copy := *r
				copy.Params = nil
				copy.RawID = nil
				notice = &copy
				break
			}
		}
		j.mu.Unlock()
		if notice == nil {
			continue
		}
		sendCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := send(sendCtx, *notice)
		cancel()
		j.mu.Lock()
		for i := range j.Attention {
			r := &j.Attention[i]
			if r.ID == notice.ID {
				r.Delivery = "delivered"
				if err != nil {
					r.Delivery = "uncertain"
				}
				_ = m.saveControlLocked()
				break
			}
		}
		j.mu.Unlock()
	}
}
