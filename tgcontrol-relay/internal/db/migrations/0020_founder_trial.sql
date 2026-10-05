-- Разделение тарифов: колонки founder и trial_end.
--
-- founder: пользователь получает Pro навсегда (срок не проверяется, отмена
--   подписки не сбрасывает). Пометка нынешних делается устойчивым backfill'ом
--   в коде (backfillFounders в db.go), а НЕ здесь: UPDATE по created_at падает
--   на искусственно битой схеме без этой колонки (тест
--   TestMigrateRepairsPartiallyAppliedUserColumns).
-- trial_end: до этого момента новый пользователь получает Pro на пробу.
--
-- Пока BETA_FREE=1 обе колонки роли не играют (все и так на Pro). Начинают
-- действовать при выключении беты вместе с запуском оплаты.
ALTER TABLE users ADD COLUMN founder INTEGER NOT NULL DEFAULT 0;
ALTER TABLE users ADD COLUMN trial_end TIMESTAMP;
