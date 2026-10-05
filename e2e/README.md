# tgcontrol e2e

End-to-end tests for tgcontrol using Playwright. Covers the core happy paths:

1. **PTY** (`tests/pty.spec.ts`) — create a PTY via REST, exchange data via WebSocket, expect echo'd marker back.
2. **Remote Desktop** (`tests/remote.spec.ts`) — open `/ws/screen`, expect at least one binary frame plus the `info` packet within 5s.
3. **AI session** (`tests/session.spec.ts`) — create a `shell` session, send a command, poll the output buffer for the marker.
4. **Cloud pairing** (`tests/pairing.spec.ts`) — issue a pairing code on a LOCAL relay, confirm it (synthetic Telegram initData), device-JWT via `/v1/pair/status`, device appears in `/v1/devices`. Wrong code → 404 with machine code `pair_code_not_found`.
5. **SSH center** (`tests/ssh-center.spec.ts`) — add a server (API), connect from the UI (TOFU + password dialogs), then click «Подключиться» again: the app must RETURN to the already-open terminal (toast «Открыт ваш терминал этого сервера») instead of spawning a duplicate SSH session (regression of v2.42.14, `findOpenSshTerminal`).

The `shell` agent is used for the third test because it has no external CLI dependency.

## Test harnesses (Go helpers)

Two specs spin up local Go helpers automatically in `beforeAll` (only Go toolchain needed, binaries go to %TEMP%):

- `tgcontrol-relay/cmd/e2e-standalone` — full relay HTTP API WITHOUT the Telegram bot (the production `cmd/relay` calls getMe on boot and dies with a fake token). The fake `BOT_TOKEN` env is used only as the initData HMAC key; `fixtures/harness.ts → buildInitData` signs synthetic `tma` auth with it. Listens on a free port, prints `READY host:port`.
- `e2e/helpers/sshd` — minimal real SSH server (`x/crypto/ssh`): password auth, PTY+shell echo, deterministic host key (fixed seed, so re-runs don't trip `host_key_mismatch`). Prints `READY host:port`.

## Setup

```bash
cd e2e
npm install
npx playwright install chromium
```

## Running

Tests assume `tgcontrol.exe` is already running on port `8080`. They do **not** auto-start it (that conflicts with the Windows Service).

```bash
# Start tgcontrol in another shell (or rely on the installed service)
./tgcontrol.exe

# Then run the suite
cd e2e
npm test                  # headless
npm run test:ui           # Playwright UI mode (debug)
npm run test:headed       # show the browser
npm run report            # open last HTML report
```

### Authentication

API tests authenticate via the static API token from `config.json`. The fixture in `fixtures/auth.ts`:

1. Collects candidate tokens: `../config.json` (repo exe), `./config.json`, and the installed layout `%LOCALAPPDATA%\Remotai\config.json` (`TGCONTROL_API_TOKEN` env overrides all).
2. Calls `POST /api/auth/login/token` with each candidate until one succeeds (the winner is cached in %TEMP% so the suite doesn't pay a 401 per test; 429 rate-limits are retried with backoff).
3. Sets `Authorization: Bearer <access>` on the API request context and injects the JWT into the page's `localStorage` before any React code runs. The page fixture also seeds `tgcontrol_server` / `remotai.local.token` — the same thing the exe window does via `/miniapp?token=…` — otherwise `RequireAuth` redirects to `/cloud-login`.

### Configuring a different host

```bash
TGCONTROL_PORT=9090 npm test
TGCONTROL_URL=https://bot.123mysite.xyz npm test
```

## Layout

```
e2e/
├── playwright.config.ts   # config, single worker, retries on CI
├── fixtures/
│   ├── auth.ts            # `test`, `expect`, `auth`, `api`, authenticated `page`
│   └── harness.ts         # Go helper build/spawn (READY line), Telegram initData HMAC
├── helpers/
│   └── sshd/main.go       # minimal SSH server for ssh-center.spec.ts
└── tests/
    ├── pty.spec.ts
    ├── remote.spec.ts
    ├── session.spec.ts
    ├── pairing.spec.ts
    └── ssh-center.spec.ts
```

(The relay harness lives in `tgcontrol-relay/cmd/e2e-standalone/` because Go `internal/` packages can't be imported from outside that module.)

## Notes / known gaps

- **WebSocket auth via query string**: the tests pass `?access_token=...` because Playwright's request fixture can't add headers to raw WebSocket connections. The Go server must accept this query param for `/ws/pty/{id}` and `/ws/screen`. If it doesn't, the WS tests will fail with 401.
- **Polling-based session test**: the session test polls `/api/sessions/{name}` for output. If your build instead pushes output via SSE/WS only, switch the assertion to subscribe to the events stream.
- **ssh-center UI test needs the local agent UI**: it drives `/miniapp/#/ssh` served by the running agent; the served bundle must include the v2.42.14 reuse behavior. Password-vs-TOFU dialog order depends on whether the machine has default keys in `~/.ssh` — the test handles both.
- **CI**: tests are tagged `forbidOnly` on CI and retry up to 2×. `tgcontrol.exe` startup in CI is out of scope here — point the suite at a deployed instance with `TGCONTROL_URL`.
