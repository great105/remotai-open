// Sentry browser SDK wrapper. Activation is opt-in via VITE_SENTRY_DSN_JS at
// build time. If unset, all functions are no-ops.
//
// VITE_SENTRY_RELEASE_PREFIX is the per-app prefix added to release labels
// (e.g. "tgcontrol-apk" or "tgcontrol-miniapp"). Default: "tgcontrol".
import type { ComponentType, ReactNode } from "react";
import * as Sentry from "@sentry/react";

let initialized = false;

export function initSentry(): void {
  const dsn = import.meta.env.VITE_SENTRY_DSN_JS as string | undefined;
  if (!dsn) return;

  const env = (import.meta.env.VITE_SENTRY_ENV as string) || "production";
  const version = (import.meta.env.VITE_APP_VERSION as string) || "dev";
  const prefix = (import.meta.env.VITE_SENTRY_RELEASE_PREFIX as string) || "tgcontrol";

  Sentry.init({
    dsn,
    release: `${prefix}@${version}`,
    environment: env,
    tracesSampleRate: 0,
    beforeSend(event, hint) {
      const err = hint?.originalException;
      if (err && (err as Error).message?.includes("ResizeObserver loop")) {
        return null;
      }
      return event;
    },
  });

  initialized = true;
}

export function captureError(err: unknown, context?: Record<string, unknown>): void {
  if (!initialized) return;
  Sentry.withScope((scope) => {
    if (context) {
      for (const [k, v] of Object.entries(context)) scope.setExtra(k, v);
    }
    Sentry.captureException(err);
  });
}

export function setUser(uid: number | string): void {
  if (!initialized) return;
  Sentry.setUser({ id: String(uid) });
}

// Record a Core Web Vital (INP/LCP/CLS/…) as a Sentry breadcrumb so it rides
// along with any error report. No-op until Sentry is initialized (DSN set).
export function recordWebVital(name: string, value: number, rating: string): void {
  if (!initialized) return;
  Sentry.addBreadcrumb({
    category: "web-vitals",
    level: rating === "poor" ? "warning" : "info",
    message: `${name} ${Math.round(value)} (${rating})`,
    data: { name, value, rating },
  });
}

// Sentry's ErrorBoundary type from @sentry/react conflicts with React 19's
// stricter ComponentType — cast to a minimal-prop component type for our use.
type ErrorBoundaryProps = {
  children?: ReactNode;
  fallback?: ReactNode | ((args: { error: unknown; resetError: () => void }) => ReactNode);
};
export const SentryErrorBoundary = Sentry.ErrorBoundary as unknown as ComponentType<ErrorBoundaryProps>;

export function isSentryActive(): boolean {
  return initialized;
}
