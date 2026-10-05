import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import { HermesMarkdown } from "./HermesMarkdown";

const render = (source: string) => renderToStaticMarkup(createElement(HermesMarkdown, { children: source }));

describe("Hermes Markdown responses", () => {
  it("renders answer structure instead of exposing Markdown markers", () => {
    const html = render("## План\n\n**Готово**, *проверено* и `git status`.\n\n> Сначала проверка.\n\n- Файлы\n- Тесты\n\n1. Подготовить\n2. Выполнить\n\n```ts\nconst ok = true;\n```\n");
    expect(html).toContain("<h2>План</h2>");
    expect(html).toContain("<strong>Готово</strong>");
    expect(html).toContain("<em>проверено</em>");
    expect(html).toContain("<code>git status</code>");
    expect(html).toContain("<blockquote>");
    expect(html).toContain("<ul>");
    expect(html).toContain("<li>Файлы</li>");
    expect(html).toContain("<ol>");
    expect(html).toContain("<li>Выполнить</li>");
    expect(html).toContain('<pre><code class="language-ts">const ok = true;\n</code></pre>');
    expect(html).not.toContain("**Готово**");
  });

  it("supports GFM tables, task lists, strikethrough and automatic links", () => {
    const html = render("| Задача | Статус |\n| --- | --- |\n| Сборка | Готово |\n\n- [x] Проверено\n- [ ] Следующий шаг\n\n~~старое~~ https://example.com/result\n");
    expect(html).toContain('<div class="hermes-markdown-table-scroll" tabindex="0" role="region"');
    expect(html).toContain("<thead><tr><th>Задача</th><th>Статус</th></tr></thead>");
    expect(html).toContain("<td>Готово</td>");
    expect(html).toContain('type="checkbox" disabled="" checked=""');
    expect(html).toContain("<del>старое</del>");
    expect(html).toContain('href="https://example.com/result" target="_blank" rel="noopener noreferrer"');
  });

  it.each([
    ["javascript:alert%281%29", "execute"],
    ["JaVaScRiPt:alert%281%29", "mixed-case"],
    ["java&#x73;cript:alert%281%29", "entity"],
    ["vbscript:msgbox%281%29", "vbscript"],
    ["data:text/html;base64,PHNjcmlwdD4=", "data"],
    ["file:///C:/private/credentials", "file"],
  ])("keeps unsafe destination %s as text without a clickable link", (destination, label) => {
    const html = render(`[${label}](${destination})`);
    expect(html).toContain(label);
    expect(html).not.toContain("<a ");
    expect(html).not.toContain("href=");
  });

  it.each(["https://example.com/docs", "http://example.com/docs", "//example.com/docs", "mailto:help@example.com", "./notes.md"])(
    "retains safe link %s with opener and referrer protection", (destination) => {
      const html = render(`[Источник](${destination} "Документация")`);
      expect(html).toContain(`href="${destination}"`);
      expect(html).toContain('title="Документация"');
      expect(html).toContain('target="_blank" rel="noopener noreferrer"');
    });

  it("keeps local anchors and generated footnote navigation in the current page", () => {
    const html = render("[Раздел](#notes)\n\nОтвет[^1]\n\n[^1]: Источник\n");
    expect(html).toContain('href="#notes"');
    expect(html).toContain('href="#user-content-fn-1"');
    expect(html).not.toContain('target="_blank"');
  });

  it("drops raw HTML and does not render resource-bearing elements", () => {
    const html = render('<script>alert("secret")</script>\n\n<iframe src="https://example.com/track"></iframe>\n\n<img src="x" onerror="alert(1)">\n\n**Обычный ответ**');
    expect(html).toContain("<strong>Обычный ответ</strong>");
    expect(html).not.toMatch(/<(script|iframe|img)\b/);
    expect(html).not.toContain("onerror");
    expect(html).not.toContain("alert(");
  });

  it("preserves HTML examples as escaped code rather than active markup", () => {
    const html = render('```html\n<script>alert(1)</script>\n```\n\n`<img src=x onerror=alert(1)>`');
    expect(html).toContain("&lt;script&gt;alert(1)&lt;/script&gt;");
    expect(html).toContain("&lt;img src=x onerror=alert(1)&gt;");
    expect(html).not.toContain("<script>");
    expect(html).not.toContain("<img ");
  });

  it("keeps image descriptions without fetching remote or private image sources", () => {
    const html = render("![Результат](https://example.com/track.png) ![Локальный файл](file:///private.png)");
    expect(html).toContain("Результат");
    expect(html).toContain("Локальный файл");
    expect(html).not.toContain("<img");
    expect(html).not.toContain("src=");
    expect(html).not.toContain("preload");
  });

  it.each(["", "**Ответ", "[Источник](https://", "```js\nconst pending =", "| Колонка |\n| --- |\n|", "> **Думаю"])(
    "renders unfinished stream prefix %j without losing its current text", (prefix) => {
      expect(() => render(prefix)).not.toThrow();
      const html = render(prefix);
      expect(html).toContain('class="hermes-markdown"');
      if (prefix.includes("pending")) expect(html).toContain("const pending =");
      if (prefix.includes("Ответ")) expect(html).toContain("Ответ");
    });

  it("renders restored history immediately even with a live flag at mount", () => {
    const source = "## Восстановлено\n\n**Полный ответ**";
    const html = renderToStaticMarkup(createElement(HermesMarkdown, { children: source, streaming: true, streamKey: "restored-chat" }));
    expect(html).toContain("<h2>Восстановлено</h2>");
    expect(html).toContain("<strong>Полный ответ</strong>");
  });

  it("formats an unfinished answer when its closing delimiter and list arrive", () => {
    expect(render("**Ответ")).toContain("**Ответ");
    const completed = render("**Ответ готов**\n\n- Один\n- Два");
    expect(completed).toContain("<strong>Ответ готов</strong>");
    expect(completed).toContain("<li>Один</li>");
    expect(completed).toContain("<li>Два</li>");
  });
});
