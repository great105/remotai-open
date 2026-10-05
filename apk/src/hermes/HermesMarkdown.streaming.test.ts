import { afterAll, beforeAll, describe, expect, it } from "vitest";
import { chromium, type Browser, type Page } from "playwright";
import { createServer, type ViteDevServer } from "vite";
import { fileURLToPath } from "node:url";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

// Opt-in: the normal node unit suite must not require a Chromium install.
// HERMES_STREAM_BROWSER=1 npm test -- apk/src/hermes/HermesMarkdown.streaming.test.ts
let cacheDir: string;
let server: ViteDevServer;
let browser: Browser;
let origin: string;

beforeAll(async () => {
  cacheDir = await mkdtemp(join(process.env.TMPDIR || tmpdir(), "hermes-stream-test-"));
  server = await createServer({
    configFile: false,
    cacheDir,
    root: fileURLToPath(new URL("../../", import.meta.url)),
    // This fixture renders Markdown only; use the real message catalog without
    // loading unrelated terminal widgets through the shared package barrel.
    resolve: { alias: { "@tgcontrol/shared": fileURLToPath(new URL("../../../packages/shared/src/i18n.ts", import.meta.url)) } },
    server: { host: "127.0.0.1", port: 0 },
    optimizeDeps: {
      noDiscovery: true,
      entries: [],
      include: ["react", "react-dom", "react-dom/client", "react/jsx-dev-runtime", "react-markdown", "remark-gfm"],
    },
    plugins: [{
      name: "isolated-hermes-stream-fixture",
      resolveId: id => id === "/hermes-stream-fixture.js" ? "\0hermes-stream-fixture" : undefined,
      load: id => id === "\0hermes-stream-fixture" ? `
        import React from 'react';
        import { createRoot } from 'react-dom/client';
        import { flushSync } from 'react-dom';
        import { HermesMarkdown } from '/src/hermes/HermesMarkdown.tsx';
        import '/src/hermes/hermes.css';
        const root = createRoot(document.getElementById('root'));
        window.renderSource = (children, streaming = false, streamKey = 'chat-a') =>
          flushSync(() => root.render(React.createElement(React.StrictMode, null, React.createElement(HermesMarkdown, { children, streaming, streamKey, className: 'hermes-message-content' }))));
        window.renderSource('Начало ', true);
      ` : undefined,
      configureServer(vite) {
        vite.middlewares.use((req, res, next) => {
          if (req.url !== "/hermes-stream-test") return next();
          res.setHeader("Content-Type", "text/html");
          res.end('<html><head><meta name="viewport" content="width=device-width, initial-scale=1"></head><body class="hermes-page"><div id="root" style="width:100%;padding:12px;box-sizing:border-box"></div><script type="module" src="/hermes-stream-fixture.js"></script></body></html>');
        });
      },
    }],
  });
  await server.listen();
  origin = server.resolvedUrls!.local[0];
  browser = await chromium.launch({ headless: true });
}, 30000);

afterAll(async () => {
  await browser?.close();
  await server?.close();
  if (cacheDir) await rm(cacheDir, { recursive: true, force: true });
});

async function fixture(reducedMotion: "reduce" | "no-preference" = "no-preference") {
  const page = await browser.newPage({ viewport: { width: 360, height: 640 }, reducedMotion });
  await page.goto(new URL("/hermes-stream-test", origin).href);
  await page.waitForFunction(() => typeof (window as any).renderSource === "function");
  await page.clock.install();
  await page.clock.pauseAt(new Date());
  return page;
}

const render = (page: Page, source: string, streaming = true, streamKey = "chat-a") =>
  page.evaluate(({ source, streaming, streamKey }) => (window as any).renderSource(source, streaming, streamKey), { source, streaming, streamKey });

describe.skipIf(!process.env.HERMES_STREAM_BROWSER)("Hermes Markdown mounted stream", () => {
  it("reveals poll chunks gradually but flushes completion synchronously", async () => {
    const page = await fixture();
    try {
      const source = "Начало " + "новый текст ".repeat(20) + "конец";
      await render(page, source);
      expect((await page.locator(".hermes-markdown").textContent())!.length).toBeLessThan(source.length - 20);
      await page.clock.runFor(40);
      const intermediate = await page.locator(".hermes-markdown").textContent();
      expect(intermediate!.length).toBeGreaterThan("Начало ".length);
      expect(intermediate!.length).toBeLessThan(source.length);
      await render(page, source + "**Готово**", false);
      expect(await page.locator("strong").textContent()).toBe("Готово");
      await page.clock.runFor(200);
      expect(await page.locator("strong").textContent()).toBe("Готово");
    } finally { await page.close(); }
  }, 30000);

  it("respects reduced motion and a live preference change", async () => {
    const page = await fixture("reduce");
    try {
      const source = "Начало " + "много текста ".repeat(15) + "конец";
      await render(page, source);
      expect(await page.locator(".hermes-markdown").textContent()).toBe(source);
      await page.emulateMedia({ reducedMotion: "no-preference" });
      await render(page, source + " следующее добавление");
      await page.emulateMedia({ reducedMotion: "reduce" });
      // Chromium delivers media change events on a frame; the fixture clock is paused.
      await page.clock.runFor(40);
      expect(await page.locator(".hermes-markdown").textContent()).toBe(source + " следующее добавление");
    } finally { await page.close(); }
  }, 30000);

  it("renders static history and switches identities without stale frames", async () => {
    const page = await fixture();
    try {
      await render(page, "## История\n\n**Сразу**", false);
      expect(await page.locator("h2").textContent()).toBe("История");
      await render(page, "Одинаковое начало", true);
      await render(page, "Одинаковое начало старый хвост", true);
      await render(page, "Одинаковое начало другой чат", true, "chat-b");
      expect(await page.locator(".hermes-markdown").textContent()).toBe("Одинаковое начало другой чат");
      await page.clock.runFor(200);
      expect(await page.locator(".hermes-markdown").textContent()).toBe("Одинаковое начало другой чат");
    } finally { await page.close(); }
  }, 30000);

  it("keeps wide tables and code inside the 360px response column", async () => {
    const page = await fixture();
    try {
      await render(page, "| Имя | Значение |\n| --- | --- |\n| " + "длинное".repeat(40) + " | готово |\n\n```ts\n" + "example".repeat(70) + "\n```", false);
      const metrics = await page.evaluate(() => {
        const table = document.querySelector(".hermes-markdown-table-scroll") as HTMLElement;
        const code = document.querySelector("pre") as HTMLElement;
        const response = document.querySelector(".hermes-markdown") as HTMLElement;
        return {
          viewport: innerWidth,
          responseWidth: response.getBoundingClientRect().width,
          tableWidth: table.getBoundingClientRect().width,
          tableScrollable: table.scrollWidth > table.clientWidth,
          codeWidth: code.getBoundingClientRect().width,
          codeScrollable: code.scrollWidth > code.clientWidth,
          fontSize: getComputedStyle(response).fontSize,
          lineHeight: getComputedStyle(response).lineHeight,
        };
      });
      expect(metrics.tableWidth).toBeLessThanOrEqual(metrics.responseWidth);
      expect(metrics.codeWidth).toBeLessThanOrEqual(metrics.responseWidth);
      expect(metrics.tableScrollable).toBe(true);
      expect(metrics.codeScrollable).toBe(true);
      console.log("Hermes response comfort fixture:", JSON.stringify(metrics));
    } finally { await page.close(); }
  }, 30000);
});
