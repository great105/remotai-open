package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// SupportThread — тред переписки пользователя с поддержкой. Один на юзера,
// создаётся лениво при первом сообщении.
type SupportThread struct {
	ID          int64
	UserID      int64
	Status      string // open | closed
	CreatedAt   time.Time
	LastMsgAt   sql.NullTime
	UnreadUser  int
	UnreadAdmin int
	Meta        string // JSON with client/agent context from the first message
}

// SupportMessage — одно сообщение в треде поддержки.
type SupportMessage struct {
	ID        int64
	ThreadID  int64
	Sender    string // user | admin
	Text      string
	CreatedAt time.Time
}

// SupportThreadInfo — строка инбокса админки: тред + данные юзера + последнее
// сообщение для превью.
type SupportThreadInfo struct {
	ID          int64
	UserID      int64
	Username    string
	FirstName   string
	Status      string
	LastMsgAt   sql.NullTime
	UnreadAdmin int
	LastText    string
	Meta        string
}

// ErrSupportThreadNotFound — треда с таким id/юзером нет.
var ErrSupportThreadNotFound = errors.New("support thread not found")

func scanSupportThread(row interface{ Scan(...any) error }) (*SupportThread, error) {
	var t SupportThread
	err := row.Scan(&t.ID, &t.UserID, &t.Status, &t.CreatedAt, &t.LastMsgAt, &t.UnreadUser, &t.UnreadAdmin, &t.Meta)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

const supportThreadCols = `id, user_id, status, created_at, last_msg_at, unread_user, unread_admin, COALESCE(meta,'')`

// GetSupportThreadByUser возвращает тред пользователя или ErrSupportThreadNotFound.
func GetSupportThreadByUser(ctx context.Context, d *sql.DB, userID int64) (*SupportThread, error) {
	t, err := scanSupportThread(d.QueryRowContext(ctx,
		`SELECT `+supportThreadCols+` FROM support_threads WHERE user_id = ?`, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSupportThreadNotFound
	}
	return t, err
}

// GetSupportThread возвращает тред по id или ErrSupportThreadNotFound.
func GetSupportThread(ctx context.Context, d *sql.DB, id int64) (*SupportThread, error) {
	t, err := scanSupportThread(d.QueryRowContext(ctx,
		`SELECT `+supportThreadCols+` FROM support_threads WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSupportThreadNotFound
	}
	return t, err
}

// GetOrCreateSupportThread возвращает тред юзера, создавая его при первом
// обращении. INSERT OR IGNORE — гонка двух первых сообщений не плодит дубли.
func GetOrCreateSupportThread(ctx context.Context, d *sql.DB, userID int64) (*SupportThread, error) {
	if _, err := d.ExecContext(ctx,
		`INSERT OR IGNORE INTO support_threads (user_id) VALUES (?)`, userID); err != nil {
		return nil, err
	}
	return GetSupportThreadByUser(ctx, d, userID)
}

// ListSupportMessages — все сообщения треда в хронологическом порядке.
func ListSupportMessages(ctx context.Context, d *sql.DB, threadID int64) ([]SupportMessage, error) {
	rows, err := d.QueryContext(ctx, `
		SELECT id, thread_id, sender, text, created_at
		FROM support_messages WHERE thread_id = ? ORDER BY id`, threadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SupportMessage{}
	for rows.Next() {
		var m SupportMessage
		if err := rows.Scan(&m.ID, &m.ThreadID, &m.Sender, &m.Text, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// PostUserSupportMessage — сообщение от пользователя: создаёт тред при
// необходимости, бампит unread_admin и last_msg_at, переоткрывает закрытый
// тред (юзер ответил — админу снова надо смотреть).
// Возвращает id треда и id сообщения.
func PostUserSupportMessage(ctx context.Context, d *sql.DB, userID int64, text string) (threadID, msgID int64, err error) {
	t, err := GetOrCreateSupportThread(ctx, d, userID)
	if err != nil {
		return 0, 0, err
	}
	res, err := d.ExecContext(ctx,
		`INSERT INTO support_messages (thread_id, sender, text) VALUES (?, 'user', ?)`, t.ID, text)
	if err != nil {
		return 0, 0, err
	}
	msgID, err = res.LastInsertId()
	if err != nil {
		return 0, 0, err
	}
	if _, err := d.ExecContext(ctx, `
		UPDATE support_threads SET
		    unread_admin = unread_admin + 1,
		    last_msg_at  = CURRENT_TIMESTAMP,
		    status       = 'open'
		WHERE id = ?`, t.ID); err != nil {
		return 0, 0, err
	}
	return t.ID, msgID, nil
}

// SetSupportThreadMetaIfEmpty records technical context only once, alongside
// the first message. Later messages cannot silently rewrite incident context.
func SetSupportThreadMetaIfEmpty(ctx context.Context, d *sql.DB, userID int64, meta string) error {
	if meta == "" {
		return nil
	}
	thread, err := GetOrCreateSupportThread(ctx, d, userID)
	if err != nil {
		return err
	}
	_, err = d.ExecContext(ctx,
		`UPDATE support_threads SET meta = ? WHERE id = ? AND COALESCE(meta,'') = ''`,
		meta, thread.ID)
	return err
}

// PostAdminSupportMessage — ответ админа: бампит unread_user и last_msg_at.
func PostAdminSupportMessage(ctx context.Context, d *sql.DB, threadID int64, text string) (int64, error) {
	if _, err := GetSupportThread(ctx, d, threadID); err != nil {
		return 0, err
	}
	res, err := d.ExecContext(ctx,
		`INSERT INTO support_messages (thread_id, sender, text) VALUES (?, 'admin', ?)`, threadID, text)
	if err != nil {
		return 0, err
	}
	msgID, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if _, err := d.ExecContext(ctx, `
		UPDATE support_threads SET
		    unread_user = unread_user + 1,
		    last_msg_at = CURRENT_TIMESTAMP
		WHERE id = ?`, threadID); err != nil {
		return 0, err
	}
	return msgID, nil
}

// MarkSupportReadUser — пользователь прочитал ответы (при GET сообщений юзером).
//
// Вместе с непрочитанными снимаем и отметку telegram-уведомления: человек
// переписку УВИДЕЛ, поэтому следующий ответ админа — это новая новость, и она
// обязана дойти сразу, не дожидаясь конца окна дедупликации.
func MarkSupportReadUser(ctx context.Context, d *sql.DB, threadID int64) error {
	_, err := d.ExecContext(ctx,
		`UPDATE support_threads SET unread_user = 0, reply_notified_at = NULL WHERE id = ?`, threadID)
	return err
}

// ClaimSupportReplyNotify — заявка «я отправляю уведомление об ответе поддержки
// по этому треду». true — заявка получена (отметка обновлена), false — про этот
// тред уже написали меньше window назад.
//
// Один UPDATE вместо «прочитать → сравнить → записать» именно для того, чтобы
// два ответа админа подряд не дали двух сообщений: условие и запись атомарны в
// пределах SQLite, гонки двух горутин на этом месте нет.
func ClaimSupportReplyNotify(ctx context.Context, d *sql.DB, threadID int64, window time.Duration) (bool, error) {
	if window < 0 {
		window = 0
	}
	// Оба значения — текст SQLite в UTC ('YYYY-MM-DD HH:MM:SS'), сравнение
	// лексикографическое и потому корректное.
	cutoff := fmt.Sprintf("-%d seconds", int64(window.Seconds()))
	res, err := d.ExecContext(ctx, `
		UPDATE support_threads SET reply_notified_at = CURRENT_TIMESTAMP
		WHERE id = ?
		  AND (reply_notified_at IS NULL OR reply_notified_at <= datetime('now', ?))`,
		threadID, cutoff)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// Сообщения бота «поддержка ответила» (колонка users.tg_notify_support,
// миграция 0018). Флаг отдельный от tg_notify: ночные вопросы агента и ответ
// поддержки — разные каналы, и выключение первого не должно прятать второй.

// SupportNotifyEnabled — писать ли этому пользователю в Telegram про ответ
// поддержки. Несуществующий пользователь → false без ошибки: слать некуда.
func SupportNotifyEnabled(ctx context.Context, d *sql.DB, userID int64) (bool, error) {
	var on int
	err := d.QueryRowContext(ctx,
		`SELECT COALESCE(tg_notify_support, 1) FROM users WHERE id = ?`, userID).Scan(&on)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return on != 0, nil
}

// SetSupportNotifyEnabled включает/выключает эти сообщения. Вызывается из
// PUT /v1/me/notify и автоматически, когда Telegram сообщил, что бот
// заблокирован: иначе релей будет биться в стену на каждом ответе админа.
func SetSupportNotifyEnabled(ctx context.Context, d *sql.DB, userID int64, on bool) error {
	v := 0
	if on {
		v = 1
	}
	_, err := d.ExecContext(ctx, `UPDATE users SET tg_notify_support = ? WHERE id = ?`, v, userID)
	return err
}

// MarkSupportReadAdmin — админ прочитал сообщения юзера (при GET из админки).
func MarkSupportReadAdmin(ctx context.Context, d *sql.DB, threadID int64) error {
	_, err := d.ExecContext(ctx,
		`UPDATE support_threads SET unread_admin = 0 WHERE id = ?`, threadID)
	return err
}

// SupportUnreadForUser — число непрочитанных юзером ответов (бейдж). 0, если
// треда ещё нет.
func SupportUnreadForUser(ctx context.Context, d *sql.DB, userID int64) (int, error) {
	var n int
	err := d.QueryRowContext(ctx,
		`SELECT unread_user FROM support_threads WHERE user_id = ?`, userID).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return n, err
}

// ListSupportThreads — инбокс админки: непрочитанные сверху, дальше по свежести.
func ListSupportThreads(ctx context.Context, d *sql.DB) ([]SupportThreadInfo, error) {
	rows, err := d.QueryContext(ctx, `
		SELECT t.id, t.user_id, COALESCE(u.username, ''), COALESCE(u.first_name, ''),
		       t.status, t.last_msg_at, t.unread_admin,
		       COALESCE((SELECT m.text FROM support_messages m
		                 WHERE m.thread_id = t.id ORDER BY m.id DESC LIMIT 1), ''),
		       COALESCE(t.meta,'')
		FROM support_threads t
		JOIN users u ON u.id = t.user_id
		ORDER BY (t.unread_admin > 0) DESC, t.last_msg_at DESC, t.id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SupportThreadInfo{}
	for rows.Next() {
		var ti SupportThreadInfo
		if err := rows.Scan(&ti.ID, &ti.UserID, &ti.Username, &ti.FirstName,
			&ti.Status, &ti.LastMsgAt, &ti.UnreadAdmin, &ti.LastText, &ti.Meta); err != nil {
			return nil, err
		}
		out = append(out, ti)
	}
	return out, rows.Err()
}

// ToggleSupportThread — переключает status open ↔ closed, возвращает новый.
func ToggleSupportThread(ctx context.Context, d *sql.DB, threadID int64) (string, error) {
	res, err := d.ExecContext(ctx, `
		UPDATE support_threads SET
		    status = CASE WHEN status = 'open' THEN 'closed' ELSE 'open' END
		WHERE id = ?`, threadID)
	if err != nil {
		return "", err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return "", ErrSupportThreadNotFound
	}
	var status string
	if err := d.QueryRowContext(ctx,
		`SELECT status FROM support_threads WHERE id = ?`, threadID).Scan(&status); err != nil {
		return "", err
	}
	return status, nil
}
