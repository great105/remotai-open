import { describe, it, expect } from "vitest";
import headless from "@xterm/headless";
import {
  MIRROR_HISTORY_LINES, captureReadDocument, completenessNotice, continuationValid, documentCell, documentRuns,
  ringCutAfterRis, type ReadDocument,
} from "./ReadDocument";
import { captureAction, consumeAction, moveBoundary, selectionRange, wordRange } from "../selection/SelectionController";
import { TerminalWriter } from "../terminalWriter";
import { registerReadAnchor } from "./readAnchor";
import { bufferText } from "./bufferText";
import { eraseActionFor, retentionFor, SCROLLBACK_ERASE, splitScrollbackErase } from "../keepHistory";

const stamp = { session: "fixture", epoch: "1", offset: 123, geometryRevision: 4 };
const write = (term: InstanceType<typeof headless.Terminal>, text: string) => new Promise<void>(resolve => term.write(text, resolve));

describe("immutable reading at the writer boundary", () => {
  it.each([48, 80, 120, 240, 400])("keeps the requested viewport when the text budget binds at %i columns", async cols => {
    const term = new headless.Terminal({ cols, rows: 3, scrollback: 200, allowProposedApi: true });
    await write(term, Array.from({ length: 100 }, (_, i) => `row-${i}`.padEnd(cols, ".")).join("\r\n"));
    const anchor = term.buffer.active.viewportY;
    const doc = captureReadDocument(term.buffer.active, cols, stamp, { rows: 50, chars: cols * 20 });
    expect(doc.firstBufferRow).toBeLessThanOrEqual(anchor);
    expect(doc.firstBufferRow + doc.lines.length).toBeGreaterThan(anchor);
    expect(doc.text).toContain(`row-${anchor}`);
    expect(doc.text.length).toBeLessThanOrEqual(cols * 20);
    term.dispose();
  });
  it.each([48, 80, 120, 240, 400])("older/newer windows meet at their actual limits at %i columns", async cols => {
    const term = new headless.Terminal({ cols, rows: 3, scrollback: 200, allowProposedApi: true });
    await write(term, Array.from({ length: 100 }, (_, i) => `row-${i} 中😀 `.padEnd(cols - 3, ".")).join("\r\n"));
    const current = captureReadDocument(term.buffer.active, cols, stamp, { rows: 50, chars: cols * 20, firstRow: 80 });
    const older = captureReadDocument(term.buffer.active, cols, stamp, { rows: 50, chars: cols * 20, endRow: current.firstBufferRow });
    expect(older.firstBufferRow + older.lines.length).toBe(current.firstBufferRow);
    const next = captureReadDocument(term.buffer.active, cols, stamp, { rows: 50, chars: cols * 20,
      firstRow: older.firstBufferRow + older.lines.length });
    expect(next.firstBufferRow).toBe(current.firstBufferRow);
    expect(next.text).toBe(current.text);
    expect(older.text.length).toBeLessThanOrEqual(cols * 20);
    term.dispose();
  });
  it("keeps all 240-column near-tail rows reachable with the production budget", async () => {
    const term = new headless.Terminal({ cols: 240, rows: 30, scrollback: 10000, allowProposedApi: true });
    await write(term, Array.from({ length: 10000 }, (_, i) => `line-${i}`.padEnd(240, ".")).join("\r\n"));
    const current = captureReadDocument(term.buffer.active, 240, stamp);
    expect(current.text).toContain("line-9970");
    const older = captureReadDocument(term.buffer.active, 240, stamp, { rows: 5000, chars: 1024 * 1024, endRow: 6000 });
    expect(older.firstBufferRow + older.lines.length).toBe(6000);
    expect(older.text).toContain("line-5999");
    term.dispose();
  });
  it("renders glyph runs at the exact same terminal columns, including wide continuations", async () => {
    const term = new headless.Terminal({ cols: 20, rows: 3, allowProposedApi: true });
    await write(term, "A😀B e\u0301 中Z");
    const doc = captureReadDocument(term.buffer.active, 20, stamp);
    const runs = documentRuns(doc.lines[0]);
    expect(runs.find(run => run.text === "B")?.col).toBe(2);
    expect(runs.find(run => run.text === "e\u0301")?.width).toBe(1);
    const wide = runs.find(run => run.text === "中")!;
    expect(wide.width).toBe(2);
    expect(documentRuns(doc.lines[0], wide.col + 1, wide.col + 2)[0]).toEqual(wide);
    expect(runs.map(run => run.text).join("")).toBe(doc.lines[0].text);
    term.dispose();
  });
  it("uses graphemes for agent text without inventing terminal columns", () => {
    const text = "A👩‍💻e\u0301Z";
    const doc = { ...stamp, id: "fixture:agent:1", version: 1, historySeq: 0, erasedBefore: false, evictedBefore: false,
      capturedAt: 0, source: "agent" as const, firstBufferRow: 0,
      truncatedBefore: false, truncatedAfter: false, cols: 0, lines: [], text };
    const emoji = selectionRange(doc, 2, 3), combined = selectionRange(doc, 7, 8);
    expect(text.slice(emoji.start, emoji.end)).toBe("👩‍💻");
    expect(text.slice(combined.start, combined.end)).toBe("e\u0301");
  });
  it("pages retained ranges through a public marker and discloses stream gaps", async () => {
    const term = new headless.Terminal({ cols: 16, rows: 3, scrollback: 200, allowProposedApi: true });
    await write(term, Array.from({ length: 100 }, (_, i) => `range-${i}`).join("\r\n"));
    const doc = captureReadDocument(term.buffer.active, 16, { ...stamp, streamGap: true }, { rows: 10, chars: 1000, firstRow: 60 });
    expect(doc.text).toContain("range-60");
    expect(doc.streamGap).toBe(true);
    const marker = registerReadAnchor(term, doc.firstBufferRow)!;
    await write(term, "\r\nlive-output");
    const older = captureReadDocument(term.buffer.active, 16, stamp, { rows: 10, chars: 1000, firstRow: marker.line - 10 });
    expect(older.text).toContain("range-50");
    expect(older.text).not.toContain("range-60");
    expect(doc.text).not.toContain("live-output");
    await write(term, Array.from({ length: 300 }, () => "\r\ntrim").join(""));
    expect(marker.isDisposed).toBe(true);
    term.dispose();
  });
  it("bounds a huge logical line without adding soft-wrap newlines or breaking Unicode", async () => {
    const term = new headless.Terminal({ cols: 40, rows: 3, scrollback: 4000, allowProposedApi: true });
    await write(term, "A😀e\u0301中".repeat(10000));
    const doc = captureReadDocument(term.buffer.active, 40, stamp, { rows: 5000, chars: 4096, firstRow: 0 });
    expect(doc.text.length).toBeLessThanOrEqual(4096);
    expect(doc.text.length).toBeGreaterThan(4000);
    expect(doc.text).not.toContain("\n");
    expect(doc.truncatedAfter).toBe(true);
    expect(doc.text).toContain("😀e\u0301中");
    term.dispose();
  });
  it("continues live output while selection stays on the captured text", async () => {
    const term = new headless.Terminal({ cols: 20, rows: 4, allowProposedApi: true });
    const writer = new TerminalWriter(term);
    writer.write("path/to/file\r\nnext");
    const captured = new Promise<ReturnType<typeof captureReadDocument>>(resolve => {
      writer.barrier(() => resolve(captureReadDocument(term.buffer.active, term.cols, stamp)));
    });
    writer.write("\x1b[H\x1b[2J\x1b[3JREPLACED");
    const doc = await captured;
    await new Promise<void>(resolve => writer.barrier(resolve));
    expect(doc.text).toContain("path/to/file\nnext");
    expect(term.buffer.active.getLine(0)?.translateToString(true)).toBe("REPLACED");
    expect(Object.isFrozen(doc)).toBe(true);
    expect(doc.epoch).toBe("1");
    expect(doc.offset).toBe(123);
    const range = wordRange(doc, 4);
    expect(doc.text.slice(range.start, range.end)).toBe("path/to/file");
    writer.dispose(); term.dispose();
  });
  it("never cuts surrogate pairs, combining cells or wide-cell continuations", async () => {
    const term = new headless.Terminal({ cols: 20, rows: 3, allowProposedApi: true });
    await write(term, "A😀e\u0301中Z");
    const doc = captureReadDocument(term.buffer.active, term.cols, stamp);
    // The default xterm Unicode table makes 😀 one cell, but 中 is wide.
    expect(documentCell(doc, 0, 4)).toBe(documentCell(doc, 0, 3));
    const emoji = selectionRange(doc, 2, 3);
    expect(doc.text.slice(emoji.start, emoji.end)).toBe("😀");
    const combining = selectionRange(doc, 4, 5);
    expect(doc.text.slice(combining.start, combining.end)).toBe("e\u0301");
    const moved = moveBoundary(doc, emoji, "end", 1);
    expect(doc.text.slice(moved.start, moved.end)).toBe("😀e\u0301");
    term.dispose();
  });
  it("retains the reading position with explicit bounded range and no persistent archive", async () => {
    const term = new headless.Terminal({ cols: 12, rows: 3, scrollback: 100, allowProposedApi: true });
    await write(term, Array.from({ length: 60 }, (_, i) => `line-${i}`).join("\r\n"));
    term.scrollToLine(5);
    const doc = captureReadDocument(term.buffer.active, 12, stamp, { rows: 10, chars: 1000 });
    expect(doc.text).toContain("line-5");
    expect(doc.lines.length).toBeLessThanOrEqual(10);
    expect(doc.truncatedAfter).toBe(true);
    const limited = captureReadDocument(term.buffer.active, 12, stamp, { rows: 100, chars: 24 });
    expect(limited.text.length).toBeLessThanOrEqual(24);
    expect(limited.truncatedAfter).toBe(true);
    await write(term, "\x1b[?1049hALT");
    expect(captureReadDocument(term.buffer.active, 12, stamp).source).toBe("screen");
    term.dispose();
  });
});

const enc = (s: string) => new TextEncoder().encode(s);
const settle = (writer: TerminalWriter) => new Promise<void>(resolve => writer.barrier(resolve));

describe("T-16: снимок выделения кнопки живёт одну операцию", () => {
  it("тратится только той же кнопкой и только недолго; иначе берётся текущее выделение", () => {
    const snap = captureAction("copy", "path/to/file", 1000);
    expect(Object.isFrozen(snap)).toBe(true);
    expect(consumeAction(snap, "copy", 1200)).toBe("path/to/file");
    expect(consumeAction(snap, "export", 1200)).toBeNull(); // чужая кнопка
    // Палец ушёл без click, выделение сняли тапом, потом click от VoiceOver:
    // старый невидимый диапазон копироваться не должен.
    expect(consumeAction(snap, "copy", 1000 + 1001)).toBeNull();
    expect(consumeAction(snap, "copy", 900)).toBeNull(); // часы ушли назад — снимку не верим
    expect(consumeAction(null, "copy", 1000)).toBeNull();
    // Пустой снимок — честное «ничего не было выделено», а не повод взять другое.
    expect(consumeAction(captureAction("copy", "", 0), "copy", 10)).toBe("");
    expect(consumeAction(snap, "copy", 4000, 5000)).toBe("path/to/file");
  });
});

describe("T-15: показанные границы совпадают с копией, мягкий перенос не искажает команду", () => {
  it("широкий символ, не влезший в последнюю колонку, не добавляет пробел в копию (cols=5, «abcd中ef»)", async () => {
    // Дефект воспроизведён на @xterm/headless 6: копия была «abcd 中ef».
    const term = new headless.Terminal({ cols: 5, rows: 4, allowProposedApi: true });
    await write(term, "abcd中ef\r\nab   cdef");
    const cell = term.buffer.active.getLine(0)!.getCell(4)!;
    expect([cell.getChars(), cell.getWidth()]).toEqual(["", 1]); // та самая незаписанная ячейка
    const doc = captureReadDocument(term.buffer.active, 5, stamp, { rows: 10, chars: 1000, firstRow: 0 });
    // Набранные пробелы у переноса значимы и сохраняются.
    expect(doc.text).toBe("abcd中ef\nab   cdef");
    expect(bufferText(term.buffer.active, 0, term.buffer.active.length - 1)).toBe(doc.text);
    // Палец на пустой ячейке попадает в конец строки «abcd», а не в выдуманный пробел.
    expect(documentCell(doc, 0, 4)).toBe(4);
    const wide = selectionRange(doc, documentCell(doc, 1, 0), documentCell(doc, 1, 2));
    expect(doc.text.slice(wide.start, wide.end)).toBe("中");
    for (const line of doc.lines) expect(documentRuns(line).map(run => run.text).join("")).toBe(line.text);
    term.dispose();
  });

  it("длинная команда через перенос, ZWJ-эмодзи и табы: копия без лишних \\n, границы по той же таблице", async () => {
    const term = new headless.Terminal({ cols: 10, rows: 8, allowProposedApi: true });
    const command = "echo /very/long/path/to/some/file.txt --flag";
    await write(term, `${command}\r\nA👩‍💻B\r\na\tb`);
    const doc = captureReadDocument(term.buffer.active, 10, stamp, { rows: 20, chars: 1000, firstRow: 0 });
    const [copied, emoji, tab] = doc.text.split("\n");
    expect(copied).toBe(command);
    expect(emoji).toBe("A👩‍💻B");
    // Таб xterm хранит сдвигом курсора: копия совпадает с показанным (пробелы до колонки 8).
    expect(tab).toBe("a       b");
    const tabRow = doc.lines.findIndex(line => line.text === tab);
    expect(doc.text.slice(documentCell(doc, tabRow, 8), documentCell(doc, tabRow, 9))).toBe("b");
    for (const line of doc.lines) expect(documentRuns(line).map(run => run.text).join("")).toBe(line.text);
    // Выделение всего документа отдаёт ровно тот же текст.
    const all = selectionRange(doc, 0, doc.text.length);
    expect(doc.text.slice(all.start, all.end)).toBe(doc.text);
    term.dispose();
  });
});

describe("T-14: выбранная версия текста стабильна при clear/reset, live идёт независимо", () => {
  it("документ переживает 2J/3J/RIS, id и версия не меняются; honor при чтении откладывает стирание", async () => {
    const term = new headless.Terminal({ cols: 20, rows: 4, scrollback: 100, allowProposedApi: true });
    const writer = new TerminalWriter(term);
    writer.write(Array.from({ length: 12 }, (_, i) => `old-${i}`).join("\r\n"));
    const policy = retentionFor({ generation: "kimi:1", agentInFg: true, declared: "" });
    let historySeq = 0;
    const doc = await new Promise<ReadDocument>(resolve => writer.barrier(() => resolve(captureReadDocument(
      term.buffer.active, term.cols, { ...stamp, historySeq, retention: policy.erase }, { rows: 100, chars: 10000, firstRow: 0 }))));
    const before = { id: doc.id, version: doc.version, text: doc.text };
    expect(doc.retention).toBe("honor");
    expect(doc.text).toContain("old-0\nold-1");

    // Reader открыт: стирание истории ОТКЛАДЫВАЕТСЯ, live-экран перерисовывается.
    let pending = 0;
    for (const item of splitScrollbackErase(enc("\x1b[2J\x1b[3J\x1b[Hrepaint")).items) {
      if (item.kind === "data") writer.write(item.data);
      else if (eraseActionFor(policy, true) === "defer") pending++;
    }
    await settle(writer);
    expect(pending).toBe(1);
    expect(term.buffer.active.baseY).toBeGreaterThan(0);
    expect(term.buffer.active.getLine(term.buffer.active.baseY)?.translateToString(true)).toBe("repaint");
    // Resumed без пропуска: эпоха writer другая, история та же — листать можно.
    expect(continuationValid(doc, { historySeq, geometryRevision: doc.geometryRevision })).toBe(true);

    // Reader закрыт, человек у низа: отложенное стирание исполняется.
    if (eraseActionFor(policy, false) === "write") { writer.write(SCROLLBACK_ERASE); historySeq++; pending--; }
    await settle(writer);
    expect(pending).toBe(0);
    expect(term.buffer.active.baseY).toBe(0);
    // После исполненного стирания листание от старого документа честно блокируется.
    expect(continuationValid(doc, { historySeq, geometryRevision: doc.geometryRevision })).toBe(false);

    writer.write("\x1bcRESET"); historySeq++;
    await settle(writer);
    expect(term.buffer.active.getLine(0)?.translateToString(true)).toBe("RESET");
    // Замороженная версия не редактировалась ни стиранием, ни RIS.
    expect(Object.isFrozen(doc)).toBe(true);
    expect({ id: doc.id, version: doc.version, text: doc.text }).toEqual(before);
    writer.dispose(); term.dispose();
  });

  it("preserve: стирание не исполняется ни при чтении, ни у низа", () => {
    const policy = retentionFor({ generation: "codex:1", agentInFg: true, declared: "preserve" });
    expect([eraseActionFor(policy, true), eraseActionFor(policy, false)]).toEqual(["discard", "discard"]);
  });
});

describe("T-22: версия документа и честные границы полноты", () => {
  it("id и версия: свой счётчик модуля или номер вызывающего; каждое снятие — новая версия", async () => {
    const term = new headless.Terminal({ cols: 12, rows: 3, allowProposedApi: true });
    await write(term, "text");
    const a = captureReadDocument(term.buffer.active, 12, stamp);
    const b = captureReadDocument(term.buffer.active, 12, stamp);
    expect(b.version).toBeGreaterThan(a.version);
    expect(a.id).toBe(`fixture:1:${a.version}`);
    expect(a.id).not.toBe(b.id);
    const given = captureReadDocument(term.buffer.active, 12, { ...stamp, version: a.version + 100 });
    expect(given.version).toBe(a.version + 100);
    expect(given.id).toBe(`fixture:1:${a.version + 100}`);
    // Счётчик модуля не откатывается ниже номера, выданного вызывающим.
    expect(captureReadDocument(term.buffer.active, 12, stamp).version).toBeGreaterThan(given.version);
    // Старый вызов без новых полей: значения по умолчанию, ничего не ломается.
    expect(a).toMatchObject({ historySeq: 0, erasedBefore: false, evictedBefore: false, retention: undefined });
    term.dispose();
  });

  it("буфер на пределе scrollback — evictedBefore; обычная обрезка окна — только truncatedBefore", async () => {
    const capacity = { scrollback: 50, rows: 3 };
    const full = new headless.Terminal({ cols: 12, rows: 3, scrollback: 50, allowProposedApi: true });
    await write(full, Array.from({ length: 200 }, (_, i) => `line-${i}`).join("\r\n"));
    expect(full.buffer.active.length).toBe(53);
    const evicted = captureReadDocument(full.buffer.active, 12, { ...stamp, capacity }, { rows: 100, chars: 10000, firstRow: 0 });
    expect(evicted).toMatchObject({ evictedBefore: true, truncatedBefore: false });
    expect(completenessNotice(evicted)).toBe("pty.readEvicted");
    // Окно в середине доступного: старше ещё есть в буфере — это «есть ещё текст», не вытеснение.
    const middle = captureReadDocument(full.buffer.active, 12, { ...stamp, capacity }, { rows: 10, chars: 10000, firstRow: 20 });
    expect(middle.truncatedBefore).toBe(true);
    expect(completenessNotice(middle)).toBe("pty.readLimited");

    const roomy = new headless.Terminal({ cols: 12, rows: 3, scrollback: 50, allowProposedApi: true });
    await write(roomy, Array.from({ length: 20 }, (_, i) => `line-${i}`).join("\r\n"));
    const kept = captureReadDocument(roomy.buffer.active, 12, { ...stamp, capacity }, { rows: 100, chars: 10000, firstRow: 0 });
    expect(kept).toMatchObject({ evictedBefore: false, truncatedBefore: false, truncatedAfter: false });
    expect(completenessNotice(kept)).toBe("pty.readFrozen");
    // Стёртое приложением в этом поколении — отдельное уведомление, важнее вытеснения.
    const erased = captureReadDocument(full.buffer.active, 12, { ...stamp, capacity, erasedBefore: true }, { rows: 100, chars: 10000, firstRow: 0 });
    expect(completenessNotice(erased)).toBe("pty.readErased");
    expect(completenessNotice({ ...erased, streamGap: true })).toBe("pty.historyGap");
    expect(completenessNotice({ ...erased, partial: true })).toBe("pty.readPartial");
    // У alt-экрана своего scrollback нет: вытеснения не бывает.
    await write(full, "\x1b[?1049hALT");
    expect(captureReadDocument(full.buffer.active, 12, { ...stamp, capacity: { scrollback: 0, rows: 3 } }).evictedBefore).toBe(false);
    full.dispose(); roomy.dispose();
  });

  it("после нашего RIS с хвоста эпохи документ честно неполон (I-11, волна 4)", async () => {
    // Маркер reset: база > 0 — пришёл только хвост кольца; база 0 — поток целиком.
    expect(ringCutAfterRis({ kind: "marker", base: 7_000_000 })).toBe(true);
    expect(ringCutAfterRis({ kind: "marker", base: 0 })).toBe(false);
    expect(ringCutAfterRis({ kind: "marker", base: undefined })).toBe(false);
    // Кадр с заменой: история зеркала на пределе (500 строк scrollback) — отдано не всё.
    expect(ringCutAfterRis({ kind: "frame", histLines: MIRROR_HISTORY_LINES + 24, screenRows: 24, base: 900 })).toBe(true);
    expect(ringCutAfterRis({ kind: "frame", histLines: 60, screenRows: 24, base: 900 })).toBe(false);
    // Истории нет вовсе, а поток до кадра был — всё раньше экрана стёрто нашим RIS.
    expect(ringCutAfterRis({ kind: "frame", histLines: 0, screenRows: 24, base: 900 })).toBe(true);
    expect(ringCutAfterRis({ kind: "frame", histLines: 0, screenRows: 24, base: 0 })).toBe(false);

    const term = new headless.Terminal({ cols: 12, rows: 3, scrollback: 50, allowProposedApi: true });
    await write(term, Array.from({ length: 20 }, (_, i) => `tail-${i}`).join("\r\n"));
    const capacity = { scrollback: 50, rows: 3 };
    const cut = captureReadDocument(term.buffer.active, 12, { ...stamp, capacity, ringTruncatedBefore: true }, { rows: 100, chars: 10000, firstRow: 0 });
    expect(cut.ringTruncatedBefore).toBe(true);
    expect(completenessNotice(cut)).toBe("pty.readRingLimited");
    // Стёртое приложением важнее; окно в середине — «есть ещё текст».
    expect(completenessNotice({ ...cut, erasedBefore: true })).toBe("pty.readErased");
    expect(completenessNotice({ ...cut, truncatedBefore: true })).toBe("pty.readLimited");
    // Без отметки — прежнее «текст закреплён» (baseline: отметки не было вовсе).
    expect(completenessNotice(captureReadDocument(term.buffer.active, 12, { ...stamp, capacity }, { rows: 100, chars: 10000, firstRow: 0 })))
      .toBe("pty.readFrozen");
    // Кадр без истории (hist_lines 0) живой агент шлёт только в alt-screen: у
    // чтения vim/htop/less отметки нет — «текст закреплён», а не «более ранний
    // вывод сюда не пришёл» (волна 6). До исправления здесь было readRingLimited.
    await write(term, "\x1b[?1049hVIM");
    const alt = captureReadDocument(term.buffer.active, 12, { ...stamp, capacity, ringTruncatedBefore: true });
    expect(alt).toMatchObject({ source: "screen", ringTruncatedBefore: false });
    expect(completenessNotice(alt)).toBe("pty.readFrozen");
    // Выход из alt: нормальный буфер после такой замены и правда пуст — отметка есть.
    await write(term, "\x1b[?1049l");
    expect(captureReadDocument(term.buffer.active, 12, { ...stamp, capacity, ringTruncatedBefore: true }, { rows: 100, chars: 10000, firstRow: 0 })
      .ringTruncatedBefore).toBe(true);
    term.dispose();
  });

  it("якорь у самого конца при лимите объёма входит в окно", async () => {
    const term = new headless.Terminal({ cols: 80, rows: 3, scrollback: 500, allowProposedApi: true });
    await write(term, Array.from({ length: 300 }, (_, i) => `tail-${i}`.padEnd(80, ".")).join("\r\n"));
    const last = term.buffer.active.length - 1;
    const doc = captureReadDocument(term.buffer.active, 80, stamp, { rows: 5000, chars: 80 * 10, anchorRow: last });
    expect(doc.firstBufferRow + doc.lines.length - 1).toBe(last);
    expect(doc.text).toContain("tail-299");
    expect(doc.text.length).toBeLessThanOrEqual(800);
    expect(doc).toMatchObject({ truncatedBefore: true, truncatedAfter: false });
    term.dispose();
  });
});
