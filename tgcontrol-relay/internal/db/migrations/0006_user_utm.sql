-- См. 0006_user_source.sql: отдельный statement делает восстановление после
-- частично применённой миграции предсказуемым.
ALTER TABLE users ADD COLUMN utm_json TEXT DEFAULT '';
