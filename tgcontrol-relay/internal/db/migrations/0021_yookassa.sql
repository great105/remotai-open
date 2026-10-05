-- Приём денег через ЮKassa (магазин 1451249, remotai.ru).
--
-- Таблица subscriptions существует с 0002 и написана под Stripe. Stripe так и
-- не подключили (решение PROD-007: в РФ платим через ЮKassa), поэтому здесь не
-- новая таблица, а недостающие поля — иначе у одного пользователя оказалось бы
-- две подписки в двух местах.
--
-- Что здесь хранится и чего здесь НЕТ. Токен способа оплаты (payment_method_id)
-- — это ссылка на карту внутри ЮKassa, по ней идут повторные списания. Самих
-- реквизитов карты у нас нет и быть не может: их видит только ЮKassa, нам
-- достаётся четыре последние цифры и тип, и то лишь чтобы человек узнал свою
-- карту на экране отвязки.
--
-- ⚠ Требование ЮKassa (менеджер, 01.09.2026): при отвязке карты мы ОБЯЗАНЫ
-- удалить токен у себя. Отсюда payment_method_id обнуляемый, а не «помечаемый
-- удалённым»: помеченная строка — это всё ещё хранимый токен.

ALTER TABLE subscriptions ADD COLUMN provider TEXT DEFAULT 'yookassa';

-- Токен способа оплаты для автосписаний. NULL = карта не привязана либо
-- отвязана человеком.
ALTER TABLE subscriptions ADD COLUMN payment_method_id TEXT;

-- Витрина карты для экрана отвязки: «MasterCard •••• 4444».
ALTER TABLE subscriptions ADD COLUMN card_last4 TEXT;
ALTER TABLE subscriptions ADD COLUMN card_type TEXT;

-- Когда человек отвязал карту сам. Держим ради разбора обращений: «я отвязал,
-- а с меня списали» — вопрос, на который нужен ответ временем, а не памятью.
ALTER TABLE subscriptions ADD COLUMN unbound_at TIMESTAMP;

-- Платежи ЮKassa: журнал и защита от повторной обработки уведомления.
--
-- ⚠ ЮKassa не гарантирует однократную доставку webhook: одно и то же
-- payment.succeeded приходит повторно при таймауте нашего ответа. Без этой
-- таблицы повтор продлил бы подписку второй раз за один платёж.
CREATE TABLE IF NOT EXISTS payments (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    payment_id      TEXT UNIQUE NOT NULL,          -- id платежа в ЮKassa
    user_id         INTEGER NOT NULL REFERENCES users(id),
    tier            TEXT NOT NULL,                 -- какую полку оплатили
    amount_minor    INTEGER NOT NULL,              -- в копейках, без дробей
    currency        TEXT NOT NULL DEFAULT 'RUB',
    status          TEXT NOT NULL,                 -- pending | succeeded | canceled
    method          TEXT,                          -- sbp | bank_card | …
    recurring       INTEGER NOT NULL DEFAULT 0,    -- 1 = автосписание, не первый платёж
    created_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    settled_at      TIMESTAMP                      -- когда пришёл окончательный статус
);

CREATE INDEX IF NOT EXISTS idx_payments_user ON payments(user_id, created_at);
CREATE INDEX IF NOT EXISTS idx_payments_status ON payments(status);
