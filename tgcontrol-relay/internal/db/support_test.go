package db

import (
	"context"
	"testing"
	"time"
)

func TestSupportThreadLifecycle(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	u, err := UpsertUser(ctx, d, 101, "jane", "Женя", "ru")
	if err != nil {
		t.Fatalf("upsert user: %v", err)
	}

	// Треда нет — unread 0, GetByUser → not found.
	if n, err := SupportUnreadForUser(ctx, d, u.ID); err != nil || n != 0 {
		t.Fatalf("unread before thread = %d, %v; want 0, nil", n, err)
	}
	if _, err := GetSupportThreadByUser(ctx, d, u.ID); err != ErrSupportThreadNotFound {
		t.Fatalf("want ErrSupportThreadNotFound, got %v", err)
	}

	// Первое сообщение юзера создаёт тред.
	threadID, msgID, err := PostUserSupportMessage(ctx, d, u.ID, "не работает терминал")
	if err != nil {
		t.Fatalf("post user msg: %v", err)
	}
	if threadID == 0 || msgID == 0 {
		t.Fatalf("ids = %d/%d, want non-zero", threadID, msgID)
	}

	// Повторный get-or-create возвращает тот же тред (UNIQUE user_id).
	th, err := GetOrCreateSupportThread(ctx, d, u.ID)
	if err != nil {
		t.Fatalf("get-or-create: %v", err)
	}
	if th.ID != threadID {
		t.Fatalf("thread id = %d, want %d (дубль треда)", th.ID, threadID)
	}
	if th.UnreadAdmin != 1 || th.UnreadUser != 0 || th.Status != "open" {
		t.Fatalf("thread state: %+v", th)
	}
	if !th.LastMsgAt.Valid {
		t.Fatal("last_msg_at должен быть проставлен")
	}

	// Ответ админа бампит unread_user.
	adminMsgID, err := PostAdminSupportMessage(ctx, d, threadID, "смотрим")
	if err != nil {
		t.Fatalf("post admin msg: %v", err)
	}
	if adminMsgID == 0 {
		t.Fatal("admin msg id = 0")
	}
	if n, err := SupportUnreadForUser(ctx, d, u.ID); err != nil || n != 1 {
		t.Fatalf("unread user = %d, %v; want 1", n, err)
	}

	// Юзер прочитал → счётчик обнуляется.
	if err := MarkSupportReadUser(ctx, d, threadID); err != nil {
		t.Fatalf("mark read user: %v", err)
	}
	if n, _ := SupportUnreadForUser(ctx, d, u.ID); n != 0 {
		t.Fatalf("unread after read = %d, want 0", n)
	}

	// Сообщения в хронологии.
	msgs, err := ListSupportMessages(ctx, d, threadID)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	if len(msgs) != 2 || msgs[0].Sender != "user" || msgs[1].Sender != "admin" {
		t.Fatalf("messages: %+v", msgs)
	}
	if msgs[0].Text != "не работает терминал" || msgs[1].Text != "смотрим" {
		t.Fatalf("texts: %q / %q", msgs[0].Text, msgs[1].Text)
	}
	if _, err := time.Parse(time.RFC3339, msgs[0].CreatedAt.Format(time.RFC3339)); err != nil {
		t.Fatalf("created_at не парсится: %v", err)
	}
}

func TestSupportThreadReopenAndToggle(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	u, err := UpsertUser(ctx, d, 102, "", "", "ru")
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	threadID, _, err := PostUserSupportMessage(ctx, d, u.ID, "привет")
	if err != nil {
		t.Fatalf("post: %v", err)
	}

	// Админ прочитал → unread_admin 0.
	if err := MarkSupportReadAdmin(ctx, d, threadID); err != nil {
		t.Fatalf("mark read admin: %v", err)
	}

	// Закрыли → closed; повторный toggle → open.
	if st, err := ToggleSupportThread(ctx, d, threadID); err != nil || st != "closed" {
		t.Fatalf("toggle 1 = %q, %v; want closed", st, err)
	}
	if st, err := ToggleSupportThread(ctx, d, threadID); err != nil || st != "open" {
		t.Fatalf("toggle 2 = %q, %v; want open", st, err)
	}
	if _, err := ToggleSupportThread(ctx, d, 9999); err != ErrSupportThreadNotFound {
		t.Fatalf("toggle missing = %v, want ErrSupportThreadNotFound", err)
	}

	// Закрыли, юзер написал — тред переоткрывается.
	if _, err := ToggleSupportThread(ctx, d, threadID); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, _, err := PostUserSupportMessage(ctx, d, u.ID, "ещё вопрос"); err != nil {
		t.Fatalf("post after close: %v", err)
	}
	th, err := GetSupportThread(ctx, d, threadID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if th.Status != "open" || th.UnreadAdmin != 1 {
		t.Fatalf("after user reply: status=%s unread_admin=%d, want open/1", th.Status, th.UnreadAdmin)
	}
}

func TestListSupportThreads(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	u1, _ := UpsertUser(ctx, d, 201, "alice", "Алиса", "ru")
	u2, _ := UpsertUser(ctx, d, 202, "bob", "Боб", "ru")

	if _, _, err := PostUserSupportMessage(ctx, d, u1.ID, "вопрос от Алисы"); err != nil {
		t.Fatalf("post u1: %v", err)
	}
	tid2, _, err := PostUserSupportMessage(ctx, d, u2.ID, "вопрос от Боба")
	if err != nil {
		t.Fatalf("post u2: %v", err)
	}
	// У Боба тред прочитан админом — в списке он должен быть ниже Алисы.
	if err := MarkSupportReadAdmin(ctx, d, tid2); err != nil {
		t.Fatalf("mark read: %v", err)
	}

	threads, err := ListSupportThreads(ctx, d)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(threads) != 2 {
		t.Fatalf("threads = %d, want 2", len(threads))
	}
	if threads[0].Username != "alice" || threads[0].UnreadAdmin != 1 {
		t.Fatalf("first thread: %+v (непрочитанные должны быть сверху)", threads[0])
	}
	if threads[0].LastText != "вопрос от Алисы" {
		t.Fatalf("last_text = %q", threads[0].LastText)
	}
	if threads[1].Username != "bob" || threads[1].UnreadAdmin != 0 {
		t.Fatalf("second thread: %+v", threads[1])
	}
}
