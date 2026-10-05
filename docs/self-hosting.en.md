# Self-hosting Remotai

**English** · [Русский](self-hosting.md)

Run Remotai on your own infrastructure without a subscription to our service.
Self-hosted access includes terminals, AI agents, server tools and device
management through your own relay. You pay your infrastructure and chosen AI
providers directly. The application supports English and Russian. Choose a
language on the sign-in screen or in Settings → Help and privacy; the choice is
saved on your device. Terminal output and your own file/project names are
preserved. For CLI help and the Windows tray, set `REMOTAI_LANGUAGE=en` or
`REMOTAI_LANGUAGE=ru`; otherwise they use the operating system's language.

If you prefer a ready-made service with maintenance, use
[official Remotai](https://remotai.ru). Its managed cloud requires a subscription
and currently uses Russian payment methods.

## Requirements

- A Linux server with Docker Engine and Docker Compose v2.
- A domain with A/AAAA records pointing to that server.
- Incoming ports 80 and 443 available.
- Node.js 22+ to create the local configuration. The client and relay are built
  inside Docker.

You do not need a Telegram bot, SMTP, OAuth or a payment provider to get started.
Pair a computer using a code and keep your account in the browser. To sign into
the same account from multiple browsers, configure your own Telegram bot or
SMTP. Keep the initial browser session until persistent sign-in is configured.

## Start on your server

Clone the repository, then run these commands from its root, replacing the
example domain with your own:

```bash
git clone https://github.com/great105/remotai-open.git
cd remotai-open
node scripts/init-self-hosted.mjs https://remotai.example.com
docker compose --env-file deploy/self-hosted/.env -f deploy/self-hosted/compose.yaml up -d --build
```

Open `https://remotai.example.com/app/`. Caddy issues a TLS certificate
automatically. The relay is accessible within the container network; only
Caddy is exposed externally. The health endpoint is
`https://remotai.example.com/health`.

The initializer generates a unique token-signing secret, saves it in
`deploy/self-hosted/.env` and does not print its value. Running it again does
not overwrite that file. Back it up with your data: changing the secret ends
existing sessions. Never add the file to Git or public archives.

The relay operator sets `SELF_HOSTED=true`. This mode does not require a
Remotai subscription, does not consume a trial, and has no plan limit on
the number of devices. Authentication, device ownership and participant roles
are still checked. Enabling `BILLING_ENABLED` at the same time is rejected.
`SELF_HOSTED` is off by default in an ordinary relay deployment.

## Try it locally

For a trial on your own computer, use a local address:

```bash
node scripts/init-self-hosted.mjs http://localhost:18086
docker compose --env-file deploy/self-hosted/.env -f deploy/self-hosted/compose.yaml up -d --build
```

Open `http://localhost:18086/app/`. Ports are bound to loopback only. This
address works in a browser on that computer, not on your phone. For phone
access, use a public domain with HTTPS. To change the address, edit
`PUBLIC_URL`, `SITE_ADDRESS` and the ports in `.env`, then rebuild the web
container.

## Connect a computer or VPS

Build your agent from the same source using the
[README instructions](../README.md). On the target computer:

```bash
./build/remotai-self-hosted install --no-pair
./build/remotai-self-hosted pair --relay https://remotai.example.com
```

On Windows, use `build\remotai-self-hosted.exe` instead. On your phone,
open your server's `/app/`, choose code entry and confirm the code issued by
the agent. The web client is already configured for your relay. If using the
official app, enter your relay address in the custom-server field on the
sign-in screen. Pair under the same operating-system user that runs the agent.

Use your own agent builds for self-hosting. `scripts/build-self-hosted.mjs`
marks its output as `self-hosted`, so official automatic updates do not
replace it. You can additionally set `"disable_auto_update": true` in the
agent configuration to stop background requests to the official update
manifest. The build command does not run the agent or install startup integration.

## Optional sign-in methods

For Telegram, set your own `BOT_TOKEN` and `BOT_USERNAME` in `.env`.
Obtain the token from BotFather; the official Remotai service's bots are not
used. This deployment serves the `/app/` web client. A Telegram Mini App needs
a separate build with `VITE_BASE=/tg/` and its URL configured through BotFather.

For email, set `SMTP_HOST`, `SMTP_PORT`, `SMTP_USER`, `SMTP_PASS` and
`SMTP_FROM`. Recreate the containers using the same Compose command after
changing the configuration.

## Updates and backups

Account and pairing data are in the `relay_data` volume; certificates are
in `caddy_data`. Back up the database and `.env` before updating. For a database
snapshot, stop the relay container, copy its volume data, then start it again.
Keep the volumes during an ordinary update.

```bash
docker compose --env-file deploy/self-hosted/.env -f deploy/self-hosted/compose.yaml ps
docker compose --env-file deploy/self-hosted/.env -f deploy/self-hosted/compose.yaml logs --tail=100 relay
docker compose --env-file deploy/self-hosted/.env -f deploy/self-hosted/compose.yaml down
```

The last command stops containers while preserving volumes. `--volumes`
deletes data and should not be used for an ordinary shutdown.

Remote screen access uses WebSocket without a mandatory TURN server. If you
need WebRTC through a complex NAT, configure your own TURN server and its
network ports separately.
