package db

import (
	"context"
	"testing"
)

// Флаг «ошибки в терминале» независим от tg_notify и по умолчанию выключен:
// событие error приходит без самой строки, и рассылать его всем — шум.
func TestNotifyErrorsPrefIndependent(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	u, err := UpsertUser(ctx, d, 4242, "jane", "Женя", "ru")
	if err != nil {
		t.Fatalf("upsert user: %v", err)
	}

	on, err := NotifyErrorsEnabled(ctx, d, u.ID)
	if err != nil {
		t.Fatalf("read errors pref: %v", err)
	}
	if on {
		t.Fatalf("по умолчанию сообщения об ошибках обязаны быть выключены")
	}
	// Вопросы агента при этом включены — это разные настройки.
	if questions, err := NotifyEnabled(ctx, d, u.ID); err != nil || !questions {
		t.Fatalf("вопросы агента по умолчанию включены: %v, %v", questions, err)
	}

	if err := SetNotifyErrorsEnabled(ctx, d, u.ID, true); err != nil {
		t.Fatalf("set errors pref: %v", err)
	}
	if on, err := NotifyErrorsEnabled(ctx, d, u.ID); err != nil || !on {
		t.Fatalf("включение не сохранилось: %v, %v", on, err)
	}
	// Выключение вопросов не должно трогать ошибки, и наоборот.
	if err := SetNotifyEnabled(ctx, d, u.ID, false); err != nil {
		t.Fatalf("set questions pref: %v", err)
	}
	if on, err := NotifyErrorsEnabled(ctx, d, u.ID); err != nil || !on {
		t.Fatalf("флаг ошибок сброшен вместе с вопросами: %v, %v", on, err)
	}

	// Несуществующий пользователь — false без ошибки: слать всё равно некуда.
	if on, err := NotifyErrorsEnabled(ctx, d, 999999); err != nil || on {
		t.Fatalf("неизвестный пользователь: %v, %v", on, err)
	}
}
