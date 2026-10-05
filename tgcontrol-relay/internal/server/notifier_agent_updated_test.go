package server

import (
	"context"
	"testing"
	"time"

	"tgcontrol-relay/internal/db"
)

// Тесты потока «агент обновился»: Telegram подменяем каналом через колбэк
// UserNotifyText, как это делают тесты pty-notifier'а с UserNotify.

type updateNotice struct {
	chatID     int64
	deviceName string
	version    string
}

func (e *notifyEnv) captureUpdates() chan updateNotice {
	ch := make(chan updateNotice, 8)
	e.srv.UserNotifyText = func(ctx context.Context, chatID int64, deviceName, version string) error {
		ch <- updateNotice{chatID: chatID, deviceName: deviceName, version: version}
		return nil
	}
	return ch
}

func agentUpdatedEvent(version string) map[string]any {
	return map[string]any{
		"type": "agent_updated", "version": version, "ts": time.Now().UnixMilli(),
	}
}

func expectUpdate(t *testing.T, ch chan updateNotice, within time.Duration) updateNotice {
	t.Helper()
	select {
	case u := <-ch:
		return u
	case <-time.After(within):
		t.Fatalf("уведомление об обновлении не пришло за %s", within)
		return updateNotice{}
	}
}

func expectUpdateSilence(t *testing.T, ch chan updateNotice, within time.Duration) {
	t.Helper()
	select {
	case u := <-ch:
		t.Fatalf("ждали тишину, а пришло уведомление: %+v", u)
	case <-time.After(within):
	}
}

// Базовый сценарий: агент обновился → одно сообщение с именем ПК и версией.
func TestAgentUpdatedNotifies(t *testing.T) {
	e := newNotifyEnv(t)
	updates := e.captureUpdates()
	ag := e.dialAgent(t, 200, `{"ok":true}`)

	ag.sendPtyEvent(t, agentUpdatedEvent("2.46.5"))

	u := expectUpdate(t, updates, 3*time.Second)
	if u.version != "2.46.5" {
		t.Fatalf("версия потеряна: %+v", u)
	}
	if u.deviceName == "" {
		t.Fatalf("имя устройства потеряно: %+v", u)
	}
}

// Повтор того же события (переподключение агента) молчит: дедуп живёт в
// events и переживает реконнекты; новая версия — снова сообщение.
func TestAgentUpdatedDedupPerVersion(t *testing.T) {
	e := newNotifyEnv(t)
	updates := e.captureUpdates()
	ag := e.dialAgent(t, 200, `{"ok":true}`)

	ag.sendPtyEvent(t, agentUpdatedEvent("2.46.5"))
	expectUpdate(t, updates, 3*time.Second)

	ag.sendPtyEvent(t, agentUpdatedEvent("2.46.5"))
	ag.sendPtyEvent(t, agentUpdatedEvent("2.46.5"))
	expectUpdateSilence(t, updates, 700*time.Millisecond)

	ag.sendPtyEvent(t, agentUpdatedEvent("2.46.6"))
	if u := expectUpdate(t, updates, 3*time.Second); u.version != "2.46.6" {
		t.Fatalf("новая версия не дошла: %+v", u)
	}
}

// Выключенный /notify гасит и эти сообщения; включение обратно их возвращает
// (дедуп-отметки при молчании не ставятся — иначе человек никогда не узнал бы
// про обновление, случившееся, пока канал был выключен).
func TestAgentUpdatedMutedSilent(t *testing.T) {
	e := newNotifyEnv(t)
	updates := e.captureUpdates()
	ag := e.dialAgent(t, 200, `{"ok":true}`)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.SetNotifyEnabled(ctx, e.srv.DB, ownerUserID(t, e), false); err != nil {
		t.Fatalf("mute: %v", err)
	}

	ag.sendPtyEvent(t, agentUpdatedEvent("2.46.5"))
	expectUpdateSilence(t, updates, 700*time.Millisecond)

	if err := db.SetNotifyEnabled(ctx, e.srv.DB, ownerUserID(t, e), true); err != nil {
		t.Fatalf("unmute: %v", err)
	}
	ag.sendPtyEvent(t, agentUpdatedEvent("2.46.5"))
	expectUpdate(t, updates, 3*time.Second)
}

// Шум наружу не уходит: пустая версия и старый реплей истории.
func TestAgentUpdatedIgnoresNoise(t *testing.T) {
	e := newNotifyEnv(t)
	updates := e.captureUpdates()
	ag := e.dialAgent(t, 200, `{"ok":true}`)

	ag.sendPtyEvent(t, map[string]any{"type": "agent_updated", "version": "", "ts": time.Now().UnixMilli()})
	stale := agentUpdatedEvent("2.46.5")
	stale["ts"] = time.Now().Add(-10 * time.Minute).UnixMilli() // реплей истории
	ag.sendPtyEvent(t, stale)

	expectUpdateSilence(t, updates, 700*time.Millisecond)
}
