import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import { sentryVitePlugin } from "@sentry/vite-plugin";
import path from "node:path";
import { fileURLToPath } from "node:url";

const __dirname = path.dirname(fileURLToPath(import.meta.url));

// Split large, stable vendor libraries into their own long-lived chunks so they
// stay cached across app deploys. Keep the search addon separate: shared exports
// import it eagerly, and grouping it with xterm loads the entire terminal on
// login/home. WebGL must also retain its existing dynamic-import boundary.
function manualChunks(id: string): string | undefined {
  if (!id.includes("node_modules")) return;
  if (id.includes("@xterm/addon-search")) return "xterm-search";
  if (id.includes("@xterm/addon-webgl")) return "xterm-webgl";
  if (id.includes("@xterm")) return "xterm";
  if (id.includes("@sentry")) return "sentry";
  if (id.includes("react-router")) return "router";
  if (id.includes("react-dom") || id.includes("/react/") || id.includes("scheduler")) return "react";
}

// Telegram Mini App SDK (telegram-web-app.js) активирует window.Telegram.WebApp:
// тема, авторизация по initData, expand, BackButton, haptics. Инжектим ТОЛЬКО в
// /tg/-сборку — в APK/exe/web скрипт не нужен (там getTelegram()→null по дизайну,
// см. apk/src/telegram.ts). Классический (не module) тег в <head> исполняется до
// deferred-модуля main.tsx, поэтому window.Telegram готов к первому чтению.
const isTelegramBuild = (process.env.VITE_BASE ?? "/") === "/tg/";
function telegramSdkPlugin() {
  return {
    name: "inject-telegram-sdk",
    transformIndexHtml() {
      if (!isTelegramBuild) return;
      return {
        tags: [{
          tag: "script",
          attrs: { src: "https://telegram.org/js/telegram-web-app.js" },
          injectTo: "head-prepend" as const,
        }],
      };
    },
  };
}

// Sourcemaps + Sentry upload activate only when SENTRY_AUTH_TOKEN is set, so the
// APK bundle stays lean until CI provides credentials.
const withSourcemaps = Boolean(process.env.SENTRY_AUTH_TOKEN);
const sentryPlugins = withSourcemaps
  ? [sentryVitePlugin({
      org: process.env.SENTRY_ORG,
      project: process.env.SENTRY_PROJECT ?? "tgcontrol-apk",
      authToken: process.env.SENTRY_AUTH_TOKEN,
      release: { name: `tgcontrol-apk@${process.env.VITE_APP_VERSION ?? "dev"}` },
      sourcemaps: { filesToDeleteAfterUpload: ["**/*.map"] },
    })]
  : [];

export default defineConfig(() => ({
  plugins: [react(), telegramSdkPlugin(), ...sentryPlugins],
  // "/" для APK (Capacitor), "/app/" для веб-версии на remotai.ru/app
  // (scripts: VITE_BASE=/app/ vite build).
  base: process.env.VITE_BASE ?? "/",
  resolve: {
    alias: {
      "@tgcontrol/shared": path.resolve(__dirname, "../packages/shared/src/index.ts"),
      // @xterm живёт в app-локальном node_modules (npm не хоистит его в корень),
      // а PtySearchBar теперь в packages/shared — без алиаса bare-импорт оттуда
      // не резолвится (Vite ищет вверх от importer'а, мимо apk/node_modules).
      "@xterm/xterm": path.resolve(__dirname, "node_modules/@xterm/xterm"),
      "@xterm/addon-search": path.resolve(__dirname, "node_modules/@xterm/addon-search"),
    },
  },
  build: {
    outDir: "dist",
    emptyOutDir: true,
    target: "es2022",
    cssTarget: "chrome111",
    sourcemap: withSourcemaps ? "hidden" : false,
    rollupOptions: {
      output: { manualChunks },
    },
    // NOTE: Vite 8 (rolldown) uses the oxc minifier, so the old `esbuild.drop`
    // was a no-op (console.* shipped in prod all along). console.error/warn are
    // kept intentionally; bundler-level drop_console is a follow-up once the
    // exact oxc compress option shape is verified.
  },
}));
