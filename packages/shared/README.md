# @tgcontrol/shared

Shared TypeScript code used by both `apk/` (Capacitor Android app) and `miniapp/` (Telegram Mini App / web).

## What lives here

Currently:
- `types.ts` — shared TypeScript types (Session, Agent, etc.)
- `observability.ts` — Sentry client wrapper (opt-in via `VITE_SENTRY_DSN_JS`)

## Migration plan

The frontends still duplicate most of their code (api.ts, i18n.ts, components/, pages/, styles.css). Each of those has diverged between apk and miniapp and needs case-by-case migration:

- `api.ts` — diverged (~30% of lines): apk has token auth, miniapp has Telegram initData. Plan: extract a common transport layer, keep auth providers per-app.
- `i18n.ts` — diverged (~50%): apk has extra keys for Login/Features. Plan: shared dictionary + per-app overrides.
- `styles.css` — diverged (~20%): apk has Capacitor-specific safe-area rules. Plan: shared base + per-app overrides.
- `pages/` and `components/` — most differ. Plan: extract platform-agnostic versions one screen at a time, inject platform adapters via props.

Add new modules here when you extract them; update the imports in `apk/src/*` and `miniapp/src/*` to reference `@tgcontrol/shared`.

## Consumed by

Both apps reference this package via path alias `@tgcontrol/shared` configured in `tsconfig.json` and `vite.config.ts`.
