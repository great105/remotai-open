-- Отдельный файл намеренно: SQLite не поддерживает ADD COLUMN IF NOT EXISTS.
-- db.Migrate игнорирует duplicate column name для одного statement, поэтому
-- частично применённая миграция не мешает следующей колонке.
ALTER TABLE users ADD COLUMN source TEXT DEFAULT '';
