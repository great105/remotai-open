# Remotai Relay

Для бесплатного самостоятельного размещения используйте
[инструкцию Docker Compose](../docs/self-hosting.md).
Режим `SELF_HOSTED=true` открывает функции без подписки; аккаунты, авторизация
и права доступа сохраняются. Он несовместим с `BILLING_ENABLED=true`.
Без этой настройки релей сохраняет проверки подписки официального сервиса.

Multi-tenant relay сервер для TGControl: inverse WebSocket routing между десктопным агентом и Mini App/APK клиентами через один общий Telegram-бот.

## Quick start

```bash
export PORT=8090
export DB_PATH=./relay.db
export BOT_TOKEN=1234567890:xxx           # необязателен в режиме без бота
export JWT_HMAC_SECRET=$(openssl rand -hex 32)
export PUBLIC_URL=http://localhost:8090
export ALLOW_INSECURE=1                   # ТОЛЬКО для локалки
# Для /admin и уведомлений поддержки:
export ADMIN_IDS=123456789                # Telegram user ID, можно через запятую
export ADMIN_CHAT_ID=123456789            # куда бот шлёт новые обращения

go run ./cmd/relay
```

Здоровье: `curl http://localhost:8090/health`.

## API (v1)

| Метод | Путь | Что делает |
|-------|------|-----------|
| POST | `/v1/pair/request` | Десктоп просит pairing-код |
| POST | `/v1/pair/confirm` | Mini App в Telegram подтверждает код (initData auth) |
| GET  | `/v1/pair/status` | Десктоп опрашивает результат (по `code`) |
| GET  | `/v1/me` | Профиль пользователя |
| GET  | `/v1/devices` | Список устройств пользователя |
| POST | `/v1/devices/{id}/rename` | Переименовать |
| DELETE | `/v1/devices/{id}` | Отозвать |
| WS  | `/v1/agent/connect` | Outbound агент-соединение (JWT) |
| WS  | `/v1/client/{device_id}/ws` | Клиент → агент (через relay) |
| POST | `/v1/client/{device_id}/request` | HTTP-style запрос → агент |
| POST | `/v1/events` | Минимальная продуктовая аналитика (с auth или анонимно для лендинга) |
| GET/POST | `/v1/support/messages` | История и отправка сообщений встроенной поддержки |
| GET | `/v1/support/unread` | Число непрочитанных ответов поддержки |
| GET | `/admin` | Админ-кабинет: аналитика и обращения |
| GET | `/v1/admin/stats` | Статистика для администратора |
| GET/POST | `/v1/admin/support/*` | Треды, ответы и статусы поддержки |
| GET | `/health` | Liveness |

## Уведомления «агент ждёт ответа» (Telegram)

Единственный канал, который работает при закрытом приложении: релей слушает
события агентов (`pty_event` → `waiting_input` / `error`) и пишет владельцу ПК в
личный чат бота — с самим вопросом, кнопками ответа (`Да/Нет`, `1/2/3`, `Enter`)
и web_app-кнопкой на нужный терминал. Нажатие превращается в
`POST /api/pty/{id}/input` на ПК через обычный путь Hub → агент.

| Переменная | По умолчанию | Что делает |
|---|---|---|
| `NOTIFY_TG_ENABLED` | `BOT_TOKEN != ""` | Главный выключатель |
| `NOTIFY_TG_DELAY` | `30s` | Пауза перед отправкой; она же порог «человека нет в приложении» |
| `NOTIFY_TG_PTY_COOLDOWN` | `10m` | Не чаще раза в … на один терминал: вопрос, возникший внутри интервала, не теряется — ждёт его конца |
| `NOTIFY_TG_MAX_PER_HOUR` | `12` | Потолок сообщений на пользователя (`0` — без лимита) |
| `NOTIFY_TG_MIN_AGENT` | `2.30.0` | С какой версии агента показывать кнопки ответа (`any` — не проверять) |
| `NOTIFY_TG_DEEPLINK_DEVICE` | `false` | Включать ТОЛЬКО после релиза клиента, понимающего `#pty_<device>_<pty>` |

Тишина гарантируется тремя рубежами: латч эпизода на терминал (один вопрос —
одно сообщение), переспрос `GET /api/pty/{id}/state` перед отправкой (за 30
секунд человек мог ответить с ПК) и токен-бакет на пользователя. Кулдаун
терминала при этом откладывает вопрос, а не выбрасывает: агент шлёт
`waiting_input` один раз на эпизод, поэтому выброшенный вопрос не дошёл бы
никогда, а отложенный уходит сразу после конца кулдауна — и только если к тому
моменту он всё ещё висит (тот же переспрос `/state`). Ответ из чата всегда
несёт `expect_status_at` — метку эпизода из события агента (или из переспроса,
если он удался): нажатие на старую карточку ПК не применит, а честно ответит
`409 prompt_changed`. Пользователь
может выключить канал командой `/notify`; заблокировавшему бота он выключается
сам. Анонимным аккаунтам (пейринг по QR без входа в Telegram) писать некуда —
такие пропуски видны в `/metrics` как `agent_notices_total{result="no_tg"}`.

## Аналитика, поддержка и админ-кабинет

- Аналитика хранит только события воронки и дневные счётчики использования
  функций. Содержимое терминалов, файлов, экрана, клавиатуры и буфера обмена
  в неё не попадает.
- Пользовательский чат поддержки доступен только авторизованным облачным
  аккаунтам. Новое обращение создаёт Telegram-уведомление в
  `ADMIN_CHAT_ID`; ответ из `/admin` появляется в приложении.
- Вход в `https://remotai.ru/admin` идёт через deep-link
  `https://t.me/<bot>?start=admin`: бот выдаёт allowlisted администратору
  Web App-кнопку, а relay проверяет подписанный Telegram initData по
  `BOT_TOKEN` и `ADMIN_IDS`. Сессия действует не более 24 часов; настройка
  Login Widget domain в BotFather не требуется.
- Необязательный `ADMIN_TOKEN` даёт Bearer-доступ к admin API для локального
  smoke-теста. В production его следует задавать только при реальной
  необходимости и хранить как секрет.

## Развёртывание

См. `deploy/`:
- `Dockerfile` — multi-stage build (pure-Go SQLite, без CGO)
- `docker-compose.yml` — relay + Caddy с авто-TLS
- `fly.toml` — Fly.io
- `caddy/Caddyfile` — TLS termination
- `systemd/relay.service` — bare-metal

## Безопасность

- Pairing codes: 8 символов crypto/rand, TTL 5 минут, 5 попыток за 10 минут на user.
- JWT: HMAC HS256, ротация 30 дней, отзыв по `device_id`.
- Telegram initData: проверка HMAC через bot token, max_age 24ч.
- Rate-limit per-IP token bucket.
