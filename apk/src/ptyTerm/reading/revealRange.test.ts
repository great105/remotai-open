import { describe, expect, it } from "vitest";
import headless from "@xterm/headless";
import { captureReadDocument } from "./ReadDocument";
import { findInText, revealRange } from "./revealRange";

const stamp = { session: "fixture", epoch: "1", offset: 0, geometryRevision: 0 };
const write = (term: InstanceType<typeof headless.Terminal>, text: string) => new Promise<void>(resolve => term.write(text, resolve));

/**
 * T-17 (L1): поиск раскрывает совпадение по обеим осям локально — PTY не
 * меняет размер, reader только двигает свою прокрутку. Документ на 240
 * колонок, окно телефона 48 колонок (≈390 px при ячейке 8 px).
 */
async function wideDocument() {
  const term = new headless.Terminal({ cols: 240, rows: 5, scrollback: 200, allowProposedApi: true });
  const rows = Array.from({ length: 40 }, (_, i) => `row-${i}`.padEnd(240, "."));
  rows[30] = `row-30`.padEnd(200, ".") + "TARGET" + ".".repeat(34);
  // Многострочное совпадение: начинается в колонке 200 строки 20 и уходит на 21.
  rows[20] = `row-20`.padEnd(200, ".") + "MULTI" + ".".repeat(35);
  rows[21] = "LINE" + `-row-21`.padEnd(236, ".");
  await write(term, rows.join("\r\n"));
  const doc = captureReadDocument(term.buffer.active, 240, stamp, { rows: 100, chars: 100000, firstRow: 0 });
  term.dispose();
  return doc;
}

describe("revealRange — совпадение в колонке 200 при окне 48 колонок", () => {
  const cellWidth = 8, cellHeight = 20, width = 48 * cellWidth;

  it("однострочное совпадение целиком входит в окно по горизонтали, строка — по вертикали", async () => {
    const doc = await wideDocument();
    const start = doc.text.indexOf("TARGET");
    const target = revealRange(doc, { start, end: start + 6 }, { left: 0, width, cellWidth, cellHeight });
    // Колонки 200..205 видны: [200*8, 206*8] внутри [left, left+width].
    expect(target.left).toBeLessThanOrEqual(200 * cellWidth);
    expect(target.left + width).toBeGreaterThanOrEqual(206 * cellWidth);
    expect(target.top).toBe((30 - 2) * cellHeight);
  });

  it("уже видимое совпадение не сдвигает горизонталь", async () => {
    const doc = await wideDocument();
    const start = doc.text.indexOf("TARGET");
    const view = { left: 180 * cellWidth, width, cellWidth, cellHeight };
    expect(revealRange(doc, { start, end: start + 6 }, view).left).toBe(180 * cellWidth);
  });

  it("совпадение слева от окна раскрывается от своего начала", async () => {
    const doc = await wideDocument();
    const start = doc.text.indexOf("TARGET");
    const target = revealRange(doc, { start, end: start + 6 }, { left: 230 * cellWidth, width, cellWidth, cellHeight });
    expect(target.left).toBe(200 * cellWidth);
  });

  it("многострочное совпадение показывает своё начало в колонке 200", async () => {
    const doc = await wideDocument();
    const start = doc.text.indexOf("MULTI");
    const end = doc.text.indexOf("LINE") + 4;
    expect(end).toBeGreaterThan(start);
    const target = revealRange(doc, { start, end }, { left: 0, width, cellWidth, cellHeight });
    expect(target.left).toBeLessThanOrEqual(200 * cellWidth);
    expect(target.left + width).toBeGreaterThan(200 * cellWidth);
    expect(target.top).toBe((20 - 2) * cellHeight);
  });

  it("совпадение длиннее окна: видно его начало, а не хвост", async () => {
    const doc = await wideDocument();
    const lineStart = doc.lines[10].start;
    const range = { start: lineStart + 150, end: lineStart + 150 + 60 }; // 60 колонок > 48
    const target = revealRange(doc, range, { left: 0, width, cellWidth, cellHeight });
    expect(target.left).toBe(150 * cellWidth);
  });
});

describe("findInText — переходы между совпадениями", () => {
  const text = "foo bar foo baz foo";

  it("вперёд — следующее после текущего, с конца — по кругу к первому", () => {
    expect(findInText(text, "foo", { start: 0, end: 3 }, 1)).toBe(8);
    expect(findInText(text, "foo", { start: 16, end: 19 }, 1)).toBe(0);
  });

  it("назад из позиции 0 — по кругу к ПОСЛЕДНЕМУ совпадению, а не то же самое", () => {
    // Раньше lastIndexOf(query, -1) возвращал 0: кнопка ↑ стояла на месте.
    expect(findInText(text, "foo", { start: 0, end: 3 }, -1)).toBe(16);
  });

  it("назад из середины — предыдущее", () => {
    expect(findInText(text, "foo", { start: 16, end: 19 }, -1)).toBe(8);
    expect(findInText(text, "foo", { start: 8, end: 11 }, -1)).toBe(0);
  });

  it("нет совпадений или пустой запрос — -1", () => {
    expect(findInText(text, "qux", { start: 0, end: 0 }, 1)).toBe(-1);
    expect(findInText(text, "", { start: 0, end: 0 }, -1)).toBe(-1);
  });
});
