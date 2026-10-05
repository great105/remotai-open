package db

import (
	"context"
	"database/sql"
	"fmt"
)

// Account entitlements authorize cloud access and built-in server features.
// Local work on one's own computer is free. Server features consult the relay
// even when initiated locally: a file editable on the PC cannot grant access.

// StartTrialIfNeeded starts the single trial on first cloud or server use.
//
// Канон: отсчёт 30 дней идёт от первого платного сценария, а не от
// регистрации. Раньше trial_end проставлялся при создании аккаунта, и у человека,
// который зарегистрировался и вернулся через месяц, проба сгорала, ни разу не
// начавшись.
//
// Условие в WHERE делает вызов идемпотентным и безопасным при гонке двух
// одновременных подключений: триал стартует ровно один раз и только тому, у кого
// его ещё не было. Founder и платные тарифы не трогаются вовсе.
func StartTrialIfNeeded(ctx context.Context, d *sql.DB, userID int64, days int) error {
	if days <= 0 {
		return nil
	}
	_, err := d.ExecContext(ctx, fmt.Sprintf(`
		UPDATE users
		   SET trial_end = datetime('now','+%d days')
		 WHERE id = ?
		   AND trial_end IS NULL
		   AND COALESCE(founder,0) = 0
		   AND COALESCE(tier,'free') = 'free'
		   AND NOT EXISTS (
		       SELECT 1 FROM subscriptions s WHERE s.user_id = users.id
		       AND s.tier <> 'free' AND s.tier <> ''
		       AND datetime(s.current_period_end) > datetime('now')
		   )
	`, days), userID)
	return err
}

// CloudDecision — ответ на вопрос «пускать ли в облако» вместе с причиной,
// которую не стыдно показать человеку.
type CloudDecision struct {
	Allowed bool
	Tier    string // эффективный тариф на момент решения
	Reason  string // для отказа: текст для человека
	Code    string // машинный код отказа для клиента
}

// CloudAccess решает, разрешено ли пользователю облачное управление.
//
// The legacy beta argument is ignored: it cannot grant a paid entitlement.
func CloudAccess(ctx context.Context, d *sql.DB, userID int64, _ bool) (CloudDecision, error) {
	u, err := GetUserByID(ctx, d, userID)
	if err != nil {
		return CloudDecision{}, err
	}
	tier := u.EffectiveTier(false)
	if tier != "free" {
		return CloudDecision{Allowed: true, Tier: tier}, nil
	}
	// Формулировка канона: НЕ «доступ заблокирован». Человек ничего не потерял —
	// дома всё работает как раньше, выключено только расстояние.
	return CloudDecision{
		Allowed: false,
		Tier:    tier,
		Code:    "subscription_required",
		Reason:  "Для облачного доступа и работы с серверами нужна подписка Про. Локальная работа на своём компьютере остаётся бесплатной.",
	}, nil
}
