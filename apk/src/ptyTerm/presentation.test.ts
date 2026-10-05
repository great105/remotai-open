/**
 * ST-06: presentation transaction.
 * T-29a (L1) — матрица правил плана снимка, префикса маркера и редьюсера.
 * T-29b (L2) — те же байты через настоящий парсер @xterm/headless 6.0.0.
 *
 * ⚠ Ограничение L2: в headless нет RenderService, поэтому ни отложенная
 * отрисовка 2026, ни сторож xterm 1000 мс (SYNCHRONIZED_OUTPUT_TIMEOUT_MS)
 * здесь не наблюдаются. Проверяются только состояние режима
 * (term.modes.synchronizedOutputMode) и содержимое буфера. Сторож и показ —
 * L3 (T-29c, собранный клиент).
 */
import { describe, expect, it } from "vitest";
import { Terminal as HeadlessTerminal } from "@xterm/headless";
import {
  PRESENTATION_WATCHDOG_MS,
  RIS,
  SYNC_BEGIN,
  SYNC_END,
  TXN_CLOSED,
  abandonTxn,
  appSyncOpenFor,
  beginSnapshotTxn,
  closeTxn,
  finishSnapshotTxn,
  joinChunks,
  markerPrefixOpens,
  mayOpenPresentation,
  openTxn,
  planMarkerPrefix,
  planSnapshotChunks,
  planSnapshotHead,
  planSnapshotTail,
  resetTxn,
  txnAfterParse,
  txnDeadline,
  txnWatchdog,
} from "./presentation";
import type { PresentationChunk, PresentationTxn } from "./presentation";

const text = (c: PresentationChunk): string => (typeof c === "string" ? c : new TextDecoder().decode(c));
const count = (hay: string, needle: string): number => hay.split(needle).length - 1;

const HISTORY = Array.from({ length: 12 }, (_, i) => `\x1b[3${(i % 7) + 1}mистория ${i} ✓\x1b[m`).join("\r\n") + "\r\n";
const FILLER = "\n\n";
// Как у Go-зеркала: выход из alt, сброс SGR, очистка, строки с абсолютной адресацией, курсор.
const FRAME = [
  "\x1b[?1049l\x1b[m\x1b[H\x1b[2J",
  "\x1b[1;1H\x1b[1mЗаголовок\x1b[m",
  "\x1b[2;1Hстрока 2 — ёж 🙂",
  "\x1b[3;1H\x1b[44m синий фон \x1b[m",
  "\x1b[6;1H> ввод",
  "\x1b[6;8H",
].join("");
const PRELUDE = "старый вывод\r\n".repeat(10) + "\x1b[31mкрасный\x1b[m $ ";

describe("T-29a: план снимка и маркера (L1)", () => {
  it("WebGL, RIS и история: два чанка, BEGIN строго после RIS в первом, END в конце второго", () => {
    const plan = planSnapshotChunks({ renderer: "webgl", ris: RIS, history: HISTORY, filler: FILLER, frame: FRAME, appSyncOpen: false });
    expect(plan.opened).toBe(true);
    expect(plan.chunks).toHaveLength(2);
    const [head, tail] = plan.chunks.map(text);
    expect(head).toBe(RIS + SYNC_BEGIN + HISTORY);
    expect(head.indexOf(SYNC_BEGIN)).toBeGreaterThan(head.indexOf(RIS));
    expect(tail).toBe(FILLER + FRAME + SYNC_END);
    expect(count(head + tail, SYNC_BEGIN)).toBe(1);
    expect(count(head + tail, SYNC_END)).toBe(1);
  });

  it("DOM: 2026 нет вовсе, склейка в два чанка остаётся", () => {
    const plan = planSnapshotChunks({ renderer: "dom", ris: RIS, history: HISTORY, filler: FILLER, frame: FRAME, appSyncOpen: false });
    expect(plan.opened).toBe(false);
    expect(plan.chunks.map(text)).toEqual([RIS + HISTORY, FILLER + FRAME]);
    const all = plan.chunks.map(text).join("");
    expect(all.includes(SYNC_BEGIN) || all.includes(SYNC_END)).toBe(false);
  });

  it("без истории: один чанк без 2026 на любом рендере, RIS уходит в тот же чанк", () => {
    for (const renderer of ["webgl", "dom"] as const) {
      expect(planSnapshotChunks({ renderer, frame: FRAME, appSyncOpen: false }))
        .toEqual({ chunks: [FRAME], opened: false });
      expect(planSnapshotChunks({ renderer, ris: RIS, filler: FILLER, frame: FRAME, appSyncOpen: false }))
        .toEqual({ chunks: [RIS + FILLER + FRAME], opened: false });
    }
  });

  it("транзакция приложения открыта и нашего RIS нет: ни BEGIN, ни END", () => {
    const withHistory = planSnapshotChunks({ renderer: "webgl", history: HISTORY, frame: FRAME, appSyncOpen: true });
    expect(withHistory.opened).toBe(false);
    expect(withHistory.chunks.map(text)).toEqual([HISTORY, FRAME]);
    const single = planSnapshotChunks({ renderer: "webgl", frame: FRAME, appSyncOpen: true });
    expect(single).toEqual({ chunks: [FRAME], opened: false });
  });

  it("с нашим RIS транзакция приложения уже сброшена: свою открываем и закрываем", () => {
    const plan = planSnapshotChunks({ renderer: "webgl", ris: RIS, history: HISTORY, frame: FRAME, appSyncOpen: true });
    expect(plan.opened).toBe(true);
    expect(text(plan.chunks[1]).endsWith(SYNC_END)).toBe(true);
  });

  it("байтовые части склеиваются в UTF-8 без перекодировки", () => {
    const enc = new TextEncoder();
    const plan = planSnapshotChunks({
      renderer: "webgl", ris: enc.encode(RIS), history: HISTORY, filler: FILLER, frame: enc.encode(FRAME), appSyncOpen: false,
    });
    expect(plan.chunks.every((c) => c instanceof Uint8Array)).toBe(true);
    expect(plan.chunks.map(text)).toEqual([RIS + SYNC_BEGIN + HISTORY, FILLER + FRAME + SYNC_END]);
    expect(joinChunks(["a", undefined, "", "b"])).toBe("ab");
  });

  it("голова и хвост по отдельности дают тот же план; END хвоста можно взять из редьюсера", () => {
    const head = planSnapshotHead({ renderer: "webgl", ris: RIS, history: HISTORY, appSyncOpen: false });
    let txn: PresentationTxn = openTxn(TXN_CLOSED, { owner: "snapshot", token: 5, now: 0 }).state;
    const closed = closeTxn(txn, 5);
    txn = closed.state;
    expect(planSnapshotTail(head, { filler: FILLER, frame: FRAME, end: closed.bytes })).toBe(FILLER + FRAME + SYNC_END);
    // повторное закрытие END не даёт: второй l не появится
    expect(closeTxn(txn, 5).bytes).toBe("");
  });

  it("байты приложения внутри кадра и истории идут как есть; план только для границ Remotai", () => {
    const appFrame = `${SYNC_BEGIN}кадр приложения${SYNC_END}`;
    const plan = planSnapshotChunks({ renderer: "dom", frame: appFrame, appSyncOpen: false });
    expect(plan.chunks).toEqual([appFrame]);
    expect(mayOpenPresentation("snapshot")).toBe(true);
    expect(mayOpenPresentation("marker")).toBe(true);
    expect(mayOpenPresentation("stream")).toBe(false);
    expect(mayOpenPresentation("raf-batch")).toBe(false);
    expect(mayOpenPresentation("erase-segment")).toBe(false);
  });

  it("префикс маркера: RIS+BEGIN только на WebGL при reset", () => {
    expect(planMarkerPrefix({ renderer: "webgl", isReset: true })).toBe(RIS + SYNC_BEGIN);
    expect(planMarkerPrefix({ renderer: "dom", isReset: true })).toBe(RIS);
    expect(planMarkerPrefix({ renderer: "webgl", isReset: false })).toBe("");
    expect(planMarkerPrefix({ renderer: "dom", isReset: false })).toBe("");
    expect(markerPrefixOpens({ renderer: "webgl", isReset: true })).toBe(true);
    expect(markerPrefixOpens({ renderer: "dom", isReset: true })).toBe(false);
    expect(markerPrefixOpens({ renderer: "webgl", isReset: false })).toBe(false);
  });
});

describe("T-29a: редьюсер PresentationTxn (L1)", () => {
  it("open только из closed: повторный open отвергнут, не вложенный счётчик", () => {
    const a = openTxn(TXN_CLOSED, { owner: "marker", token: 1, now: 100 });
    expect(a.ok).toBe(true);
    expect(a.state).toEqual({ kind: "open", owner: "marker", token: 1, deadline: 100 + PRESENTATION_WATCHDOG_MS });
    const b = openTxn(a.state, { owner: "snapshot", token: 2, now: 150 });
    expect(b.ok).toBe(false);
    expect(b.state).toBe(a.state);
    // чужой токен не закрывает; наш закрывает один раз
    expect(closeTxn(a.state, 2)).toEqual({ state: a.state, bytes: "" });
    const c = closeTxn(a.state, 1);
    expect(c).toEqual({ state: TXN_CLOSED, bytes: SYNC_END });
    expect(closeTxn(c.state, 1)).toEqual({ state: TXN_CLOSED, bytes: "" });
  });

  it("abandon идемпотентен и отдаёт END только за свою транзакцию и только пока режим стоит", () => {
    const open = openTxn(TXN_CLOSED, { owner: "snapshot", token: 7, now: 0 }).state;
    expect(abandonTxn(open, 8).bytes).toBe("");
    expect(abandonTxn(open, 7, false)).toEqual({ state: TXN_CLOSED, bytes: "" });
    const first = abandonTxn(open, 7);
    expect(first.bytes).toBe(SYNC_END);
    expect(abandonTxn(first.state, 7).bytes).toBe("");
    expect(abandonTxn(TXN_CLOSED, 7).bytes).toBe("");
  });

  it("сторож по инжектируемым часам: до срока ничего, в срок END и closed", () => {
    const open = openTxn(TXN_CLOSED, { owner: "marker", token: 3, now: 1_000 }).state;
    expect(txnDeadline(open)).toBe(1_000 + PRESENTATION_WATCHDOG_MS);
    expect(txnWatchdog(open, 1_999)).toEqual({ state: open, bytes: "", expired: false });
    expect(txnWatchdog(open, 2_000)).toEqual({ state: TXN_CLOSED, bytes: SYNC_END, expired: true });
    expect(txnWatchdog(open, 2_000, false).bytes).toBe("");
    expect(txnWatchdog(TXN_CLOSED, 9_999).expired).toBe(false);
    expect(txnDeadline(TXN_CLOSED)).toBeNull();
  });

  it("RIS в потоке закрывает транзакцию сам: resetTxn без байтов, после него open снова возможен", () => {
    const open = openTxn(TXN_CLOSED, { owner: "marker", token: 1, now: 0 }).state;
    const reset = resetTxn(open);
    expect(reset).toEqual(TXN_CLOSED);
    expect(openTxn(reset, { owner: "snapshot", token: 2, now: 10 }).ok).toBe(true);
  });
});

describe("T-29a: снимок вместе с редьюсером после маркера reset (L1)", () => {
  /** WebGL-маркер reset: RIS+BEGIN уже в потоке, транзакция маркера наша. */
  const marker = (): PresentationTxn => openTxn(TXN_CLOSED, { owner: "marker", token: 1, now: 0 }).state;

  it("appSyncOpenFor: транзакция приложения — только режим без нашей открытой транзакции", () => {
    expect(appSyncOpenFor(false, TXN_CLOSED)).toBe(false);
    expect(appSyncOpenFor(true, TXN_CLOSED)).toBe(true);
    expect(appSyncOpenFor(true, marker())).toBe(false);
    expect(appSyncOpenFor(false, marker())).toBe(false);
  });

  it("маркер открыт, снимок без RIS и истории: END за маркер в том же чанке, что кадр", () => {
    const begun = beginSnapshotTxn({ renderer: "webgl", modeOn: true, txn: marker(), token: 2, now: 100 });
    expect(begun.head.chunk).toBeNull();
    expect(begun.closeToken).toBe(1);
    const done = finishSnapshotTxn(begun, { frame: FRAME, modeOn: true });
    expect(text(done.chunk)).toBe(FRAME + SYNC_END);
    expect(done.txn).toEqual(TXN_CLOSED);
    // голый режим как appSyncOpen принял бы маркер за приложение: ни BEGIN, ни END
    expect(planSnapshotChunks({ renderer: "webgl", frame: FRAME, appSyncOpen: true })).toEqual({ chunks: [FRAME], opened: false });
  });

  it("маркер открыт, история без RIS: голова перенимает транзакцию с новым сроком, END один в хвосте", () => {
    const begun = beginSnapshotTxn({ renderer: "webgl", history: HISTORY, modeOn: true, txn: marker(), token: 2, now: 100 });
    expect(text(begun.head.chunk!)).toBe(SYNC_BEGIN + HISTORY);
    expect(begun.txn).toEqual({ kind: "open", owner: "snapshot", token: 2, deadline: 100 + PRESENTATION_WATCHDOG_MS });
    expect(begun.closeToken).toBe(2);
    const done = finishSnapshotTxn(begun, { filler: FILLER, frame: FRAME, modeOn: true });
    expect(text(done.chunk)).toBe(FILLER + FRAME + SYNC_END);
    expect(done.txn).toEqual(TXN_CLOSED);
  });

  it("маркер открыт, снимок с RIS: RIS закрывает маркер сам, END только за снимок", () => {
    const withHistory = beginSnapshotTxn({ renderer: "webgl", ris: RIS, history: HISTORY, modeOn: true, txn: marker(), token: 2, now: 100 });
    expect(text(withHistory.head.chunk!)).toBe(RIS + SYNC_BEGIN + HISTORY);
    const a = finishSnapshotTxn(withHistory, { filler: FILLER, frame: FRAME, modeOn: true });
    expect(count(text(withHistory.head.chunk!) + text(a.chunk), SYNC_END)).toBe(1);
    expect(a.txn).toEqual(TXN_CLOSED);

    const noHistory = beginSnapshotTxn({ renderer: "webgl", ris: RIS, modeOn: true, txn: marker(), token: 2, now: 100 });
    expect(noHistory.head.chunk).toBeNull();
    expect(noHistory.closeToken).toBeNull();
    const b = finishSnapshotTxn(noHistory, { frame: FRAME, modeOn: true });
    expect(text(b.chunk)).toBe(RIS + FRAME);
    expect(b.txn).toEqual(TXN_CLOSED);
  });

  it("транзакция приложения, нашей нет: ни BEGIN, ни END, редьюсер закрыт", () => {
    const begun = beginSnapshotTxn({ renderer: "webgl", history: HISTORY, modeOn: true, txn: TXN_CLOSED, token: 2, now: 100 });
    expect(begun.head.opened).toBe(false);
    expect(begun.closeToken).toBeNull();
    const done = finishSnapshotTxn(begun, { filler: FILLER, frame: FRAME, modeOn: true });
    expect([text(begun.head.chunk!), text(done.chunk)]).toEqual([HISTORY, FILLER + FRAME]);
    expect(done.txn).toEqual(TXN_CLOSED);
  });

  it("WebGL потерян между маркером и снимком (DOM): 2026 в голове нет, END за маркер пишет хвост", () => {
    const begun = beginSnapshotTxn({ renderer: "dom", history: HISTORY, modeOn: true, txn: marker(), token: 2, now: 100 });
    expect(text(begun.head.chunk!)).toBe(HISTORY);
    expect(begun.closeToken).toBe(1);
    expect(text(finishSnapshotTxn(begun, { filler: FILLER, frame: FRAME, modeOn: true }).chunk)).toBe(FILLER + FRAME + SYNC_END);
  });

  it("режим уже снят к хвосту (DECSTR в истории): END не пишется, редьюсер всё равно закрыт", () => {
    const begun = beginSnapshotTxn({ renderer: "webgl", history: HISTORY, modeOn: true, txn: marker(), token: 2, now: 100 });
    const done = finishSnapshotTxn(begun, { filler: FILLER, frame: FRAME, modeOn: false });
    expect(text(done.chunk)).toBe(FILLER + FRAME);
    expect(done.txn).toEqual(TXN_CLOSED);
  });
});

function write(t: HeadlessTerminal, data: PresentationChunk): Promise<void> {
  if (data.length === 0) return Promise.resolve();
  return new Promise((resolve) => t.write(data, resolve));
}

function makeTerm(opts: { scrollOnEraseInDisplay?: boolean } = {}): HeadlessTerminal {
  return new HeadlessTerminal({ allowProposedApi: true, cols: 24, rows: 6, scrollback: 200, ...opts });
}

/** Состояние, которое видно человеку и продолжению: ячейки, атрибуты, переносы, курсор, буфер, режим. */
function state(t: HeadlessTerminal) {
  const b = t.buffer.active;
  const lines: string[] = [];
  for (let y = 0; y < b.length; y++) {
    const line = b.getLine(y);
    if (!line) continue;
    let cells = line.isWrapped ? "W|" : "-|";
    for (let x = 0; x < t.cols; x++) {
      const c = line.getCell(x);
      if (!c) continue;
      cells += `${c.getChars() || " "}:${c.getWidth()}:${c.getFgColorMode()}/${c.getFgColor()}:${c.getBgColorMode()}/${c.getBgColor()}:${c.isBold()};`;
    }
    lines.push(cells);
  }
  return {
    type: b.type,
    cursor: [b.cursorX, b.cursorY],
    baseY: b.baseY,
    viewportY: b.viewportY,
    length: b.length,
    lines,
    sync: t.modes.synchronizedOutputMode,
  };
}

function indexOfBytes(hay: Uint8Array, needle: Uint8Array): number {
  outer: for (let i = 0; i + needle.length <= hay.length; i++) {
    for (let j = 0; j < needle.length; j++) if (hay[i + j] !== needle[j]) continue outer;
    return i;
  }
  return -1;
}

function concatBytes(a: Uint8Array, b: Uint8Array): Uint8Array {
  const out = new Uint8Array(a.length + b.length);
  out.set(a, 0);
  out.set(b, a.length);
  return out;
}

const isContinuation = (b: number): boolean => (b & 0xc0) === 0x80;
const utf8Length = (lead: number): number => ((lead & 0xe0) === 0xc0 ? 2 : (lead & 0xf0) === 0xe0 ? 3 : (lead & 0xf8) === 0xf0 ? 4 : 1);

/** Начало незаконченной UTF-8 последовательности в конце prefix; -1 — конец чистый. */
function openUtf8SequenceAt(bytes: Uint8Array, cut: number): number {
  let lead = cut - 1;
  while (lead >= 0 && cut - lead < 4 && isContinuation(bytes[lead])) lead--;
  if (lead < 0 || lead >= cut || isContinuation(bytes[lead])) return -1;
  return cut - lead < utf8Length(bytes[lead]) ? lead : -1;
}

/**
 * ⚠ ДЕФЕКТ @xterm/xterm 6.0.0 (Utf8ToUtf32.decode), найден этим тестом.
 * Хвост незаконченной UTF-8 последовательности xterm держит в `interim` и
 * дочитывает его циклом `while ((tmp = interim[++pos] & 0x3F) && pos < 4)`.
 * Продолжение 0x80 даёт 0x3F-часть 0, и цикл принимает его за «пустой байт»:
 * xterm решает, что байтов меньше, дочитывает лишний, видит не-продолжение и
 * ВЫБРАСЫВАЕТ символ. Попадают — … “ ” • (e2 80 xx) и весь U+2000–U+203F при
 * разрезе write сразу после e2 80. К 2026 отношения не имеет.
 */
function xtermUtf8DefectCut(bytes: Uint8Array, cut: number): boolean {
  const lead = openUtf8SequenceAt(bytes, cut);
  if (lead < 0) return false;
  for (let i = lead + 1; i < cut; i++) if (bytes[i] === 0x80) return true;
  return false;
}

describe("T-29b: те же байты через @xterm/headless 6.0.0 (L2)", () => {
  it("RIS+BEGIN+история+переводы+кадр+END, разрез в каждой байтовой позиции: режим снят, буфер как у непрерывной записи", async () => {
    const plan = planSnapshotChunks({ renderer: "webgl", ris: RIS, history: HISTORY, filler: FILLER, frame: FRAME, appSyncOpen: false });
    const enc = new TextEncoder();
    const bytes = enc.encode(plan.chunks.map(text).join(""));
    const prelude = enc.encode(PRELUDE);

    // Эталон: как пишет адаптер, двумя write; плюс проверка, что одним write — то же.
    const ref = makeTerm();
    await write(ref, PRELUDE);
    for (const c of plan.chunks) await write(ref, c);
    const expected = state(ref);
    ref.dispose();
    expect(expected.sync).toBe(false);
    const whole = makeTerm();
    await write(whole, concatBytes(prelude, bytes));
    expect(state(whole)).toEqual(expected);
    whole.dispose();
    const expectedJson = JSON.stringify(expected);

    const beginAt = indexOfBytes(bytes, enc.encode(SYNC_BEGIN));
    const endAt = indexOfBytes(bytes, enc.encode(SYNC_END));
    expect(beginAt).toBeGreaterThan(0);
    expect(endAt).toBe(bytes.length - SYNC_END.length);

    const cutOnce = async (cut: number, carryUtf8: boolean) => {
      // carryUtf8: незаконченная UTF-8 последовательность не отдаётся xterm, а
      // переносится в следующую запись (так обходится дефект декодера).
      const open = carryUtf8 ? openUtf8SequenceAt(bytes, cut) : -1;
      const split = open >= 0 ? open : cut;
      const t = makeTerm();
      // Пролог из целых символов: склейка с началом среза разбор не меняет.
      await write(t, concatBytes(prelude, bytes.subarray(0, split)));
      const mode = t.modes.synchronizedOutputMode;
      await write(t, bytes.subarray(split));
      const same = JSON.stringify(state(t)) === expectedJson;
      t.dispose();
      return { mode, same };
    };

    const mismatches: number[] = [];
    const defects: number[] = [];
    const wrongMode: number[] = [];
    for (let cut = 0; cut <= bytes.length; cut++) {
      const { mode, same } = await cutOnce(cut, false);
      // Режим включён ровно между полностью разобранными BEGIN и END: разрез
      // внутри самих \x1b[?2026h/l парсер переносит через границу write.
      const inside = cut >= beginAt + SYNC_BEGIN.length && cut < endAt + SYNC_END.length;
      if (mode !== inside) wrongMode.push(cut);
      if (!same) mismatches.push(cut);
      if (xtermUtf8DefectCut(bytes, cut)) defects.push(cut);
    }
    // 2026 и склейка чанков: верно в КАЖДОЙ позиции, включая разрез внутри h/l.
    expect(wrongMode).toEqual([]);
    // Буфер расходится ровно там, где режет дефект декодера xterm (разрез
    // после e2 80 в «—» кадра), и больше нигде. Обновили xterm и дефект ушёл —
    // этот expect упадёт: убрать исключение и перепроверить.
    expect(defects.length).toBeGreaterThan(0);
    expect(mismatches).toEqual(defects);
    // Положительный контроль: с переносом незаконченного UTF-8 эти же разрезы совпадают.
    for (const cut of defects) expect((await cutOnce(cut, true)).same).toBe(true);
  }, 120_000);

  it("дефект декодера xterm 6.0.0: разрез write после продолжения 0x80 теряет символ, прочие разрезы нет", async () => {
    const enc = new TextEncoder();
    const lineAfter = async (s: string, cut: number) => {
      const bytes = enc.encode(`a ${s} b`);
      const t = makeTerm();
      await write(t, bytes.subarray(0, 2 + cut));
      await write(t, bytes.subarray(2 + cut));
      const line = t.buffer.active.getLine(0)?.translateToString(true);
      t.dispose();
      return line;
    };
    expect(await lineAfter("—", 2)).toBe("a  b"); // e2 80 | 94: тире потеряно
    expect(await lineAfter("…", 2)).toBe("a  b"); // e2 80 | a6: многоточие потеряно
    expect(await lineAfter("—", 1)).toBe("a — b"); // e2 | 80 94: цело
    expect(await lineAfter("✓", 2)).toBe("a ✓ b"); // e2 9c | 93: цело
    expect(await lineAfter("р", 1)).toBe("a р b"); // d1 | 80: двухбайтовые не задеты
  });

  it("DOM-план без 2026 даёт тот же буфер, что WebGL-план: 2026 меняет только показ", async () => {
    const webgl = makeTerm();
    const dom = makeTerm();
    await write(webgl, PRELUDE);
    await write(dom, PRELUDE);
    for (const c of planSnapshotChunks({ renderer: "webgl", ris: RIS, history: HISTORY, filler: FILLER, frame: FRAME, appSyncOpen: false }).chunks) {
      await write(webgl, c);
    }
    for (const c of planSnapshotChunks({ renderer: "dom", ris: RIS, history: HISTORY, filler: FILLER, frame: FRAME, appSyncOpen: false }).chunks) {
      await write(dom, c);
    }
    expect(state(dom)).toEqual(state(webgl));
    webgl.dispose();
    dom.dispose();
  });

  it("транзакция приложения открыта → снимок → l в хвосте: режим снимается только на l приложения", async () => {
    const t = makeTerm();
    await write(t, PRELUDE);
    await write(t, `${SYNC_BEGIN}приложение рисует`);
    expect(t.modes.synchronizedOutputMode).toBe(true);
    const plan = planSnapshotChunks({ renderer: "webgl", frame: FRAME, appSyncOpen: t.modes.synchronizedOutputMode });
    for (const c of plan.chunks) await write(t, c);
    expect(t.modes.synchronizedOutputMode).toBe(true);
    await write(t, ` конец кадра${SYNC_END}`);
    expect(t.modes.synchronizedOutputMode).toBe(false);
    t.dispose();

    // То же с историей без нашего RIS: два чанка, но END приложения не подменяем.
    const h = makeTerm();
    await write(h, `${SYNC_BEGIN}приложение рисует`);
    const withHistory = planSnapshotChunks({ renderer: "webgl", history: HISTORY, filler: FILLER, frame: FRAME, appSyncOpen: true });
    expect(withHistory.chunks).toHaveLength(2);
    for (const c of withHistory.chunks) {
      await write(h, c);
      expect(h.modes.synchronizedOutputMode).toBe(true);
    }
    await write(h, SYNC_END);
    expect(h.modes.synchronizedOutputMode).toBe(false);
    h.dispose();

    // Контроль: обёртка BEGIN…END «на всякий случай» закрыла бы транзакцию приложения до его l.
    const naive = makeTerm();
    await write(naive, `${SYNC_BEGIN}приложение рисует`);
    await write(naive, joinChunks([SYNC_BEGIN, FRAME, SYNC_END]));
    expect(naive.modes.synchronizedOutputMode).toBe(false);
    naive.dispose();
  });

  it("фактическое поведение 6.0.0: RIS и DECSTR внутри транзакции сбрасывают режим, режим не вложенный", async () => {
    const t = makeTerm();
    await write(t, SYNC_BEGIN);
    expect(t.modes.synchronizedOutputMode).toBe(true);
    await write(t, RIS);
    expect(t.modes.synchronizedOutputMode).toBe(false);
    await write(t, SYNC_BEGIN);
    await write(t, "\x1b[!p"); // DECSTR
    expect(t.modes.synchronizedOutputMode).toBe(false);
    await write(t, SYNC_BEGIN + SYNC_BEGIN + SYNC_END);
    expect(t.modes.synchronizedOutputMode).toBe(false);
    // поэтому BEGIN до RIS пропадает, а после RIS держится
    await write(t, SYNC_BEGIN + RIS);
    expect(t.modes.synchronizedOutputMode).toBe(false);
    await write(t, RIS + SYNC_BEGIN);
    expect(t.modes.synchronizedOutputMode).toBe(true);
    t.dispose();
  });

  it("маркер reset на WebGL, реплей, снимок с RIS: итог режима false, буфер как без 2026", async () => {
    const run = async (renderer: "webgl" | "dom") => {
      const t = makeTerm();
      await write(t, PRELUDE);
      await write(t, planMarkerPrefix({ renderer, isReset: true }));
      expect(t.modes.synchronizedOutputMode).toBe(renderer === "webgl");
      await write(t, "реплей кольца\r\n".repeat(8));
      for (const c of planSnapshotChunks({ renderer, ris: RIS, history: HISTORY, filler: FILLER, frame: FRAME, appSyncOpen: false }).chunks) {
        await write(t, c);
      }
      const s = state(t);
      t.dispose();
      return s;
    };
    const webgl = await run("webgl");
    expect(webgl.sync).toBe(false);
    expect(webgl).toEqual(await run("dom"));
  });

  it("маркер reset на WebGL → реплей → снимок без RIS: режим снят в конце хвоста; по голому режиму висел бы до сторожа", async () => {
    for (const history of [undefined, HISTORY]) {
      const filler = history ? FILLER : undefined;
      const afterMarker = async () => {
        const t = makeTerm();
        await write(t, PRELUDE);
        await write(t, planMarkerPrefix({ renderer: "webgl", isReset: true }));
        await write(t, "реплей кольца\r\n".repeat(8));
        expect(t.modes.synchronizedOutputMode).toBe(true);
        return t;
      };

      const t = await afterMarker();
      const txn = openTxn(resetTxn(TXN_CLOSED), { owner: "marker", token: 1, now: 0 }).state;
      const begun = beginSnapshotTxn({ renderer: "webgl", history, modeOn: t.modes.synchronizedOutputMode, txn, token: 2, now: 50 });
      if (begun.head.chunk !== null) {
        await write(t, begun.head.chunk);
        // голова разобрана, кадра ещё нет: удержание продолжается
        expect(t.modes.synchronizedOutputMode).toBe(true);
      }
      const done = finishSnapshotTxn(begun, { filler, frame: FRAME, modeOn: t.modes.synchronizedOutputMode });
      await write(t, done.chunk);
      expect(t.modes.synchronizedOutputMode).toBe(false);
      expect(done.txn).toEqual(TXN_CLOSED);

      // Контроль: appSyncOpen = голый режим. Буфер тот же, но режим висит.
      const naive = await afterMarker();
      const plan = planSnapshotChunks({ renderer: "webgl", history, filler, frame: FRAME, appSyncOpen: naive.modes.synchronizedOutputMode });
      for (const c of plan.chunks) await write(naive, c);
      expect(naive.modes.synchronizedOutputMode).toBe(true);
      expect({ ...state(naive), sync: false }).toEqual(state(t));
      t.dispose();
      naive.dispose();
    }
  });

  it("ED2/ED3 внутри транзакции при scrollOnEraseInDisplay=false: место чтения и история на месте, ED3 чистит историю", async () => {
    const fill = Array.from({ length: 12 }, (_, i) => `line${i}`).join("\r\n") + "\r\n";
    const t = makeTerm({ scrollOnEraseInDisplay: false });
    await write(t, fill);
    const b = t.buffer.active;
    const before = { length: b.length, baseY: b.baseY };
    t.scrollLines(-3); // человек читает выше живого низа (сценарий xterm #5801)
    const readingAt = b.viewportY;
    expect(readingAt).toBeLessThan(b.baseY);
    await write(t, `${SYNC_BEGIN}\x1b[2J\x1b[Hперерисовка${SYNC_END}`);
    expect(t.modes.synchronizedOutputMode).toBe(false);
    expect({ length: b.length, baseY: b.baseY }).toEqual(before);
    expect(b.viewportY).toBe(readingAt);
    await write(t, `${SYNC_BEGIN}\x1b[3J\x1b[2J\x1b[Hперерисовка 2${SYNC_END}`);
    expect(b.length).toBe(t.rows);
    expect(b.baseY).toBe(0);
    t.dispose();

    // Контроль: при true тот же ED2 вытолкнул бы экран в историю. Шов истории
    // Go-зеркала рассчитан именно на false (screen.go), клиент его не задаёт.
    const pushed = makeTerm({ scrollOnEraseInDisplay: true });
    await write(pushed, fill);
    const pb = pushed.buffer.active;
    const pushedBefore = pb.length;
    await write(pushed, `${SYNC_BEGIN}\x1b[2J\x1b[Hперерисовка${SYNC_END}`);
    expect(pb.length).toBeGreaterThan(pushedBefore);
    pushed.dispose();
  });
});

describe("T-29: режим 2026 снят не нами — удержание закрывается (ревью ST-06)", () => {
  const marker = (token = 1): PresentationTxn => openTxn(TXN_CLOSED, { owner: "marker", token, now: 0 }).state;

  it("L1: закрывается только открытая транзакция, чей BEGIN уже разобран, и только при снятом режиме", () => {
    // BEGIN ещё в очереди за прежними записями: снятый режим — не наш.
    expect(txnAfterParse(marker(), { parsedToken: null, modeOn: false })).toEqual({ state: marker(), ended: false });
    // Разобран прежний токен (прошлый маркер), наш ещё нет.
    expect(txnAfterParse(marker(2), { parsedToken: 1, modeOn: false })).toEqual({ state: marker(2), ended: false });
    // Режим стоит — удерживаем дальше.
    expect(txnAfterParse(marker(), { parsedToken: 1, modeOn: true })).toEqual({ state: marker(), ended: false });
    // BEGIN разобран, режима нет — удерживать нечего.
    expect(txnAfterParse(marker(), { parsedToken: 1, modeOn: false })).toEqual({ state: TXN_CLOSED, ended: true });
    // Закрытую не трогаем (идемпотентно).
    expect(txnAfterParse(TXN_CLOSED, { parsedToken: 1, modeOn: false })).toEqual({ state: TXN_CLOSED, ended: false });
  });

  it("L2: реплей за маркером с парами ?2026h/?2026l приложения снимает режим — транзакция маркера закрыта; без l — остаётся", async () => {
    // Манера Kimi (probe-kimi-width-mix.mjs): кадр статус-полосы в своей рамке 2026.
    const kimiFrame = `${SYNC_BEGIN}\x1b[4;1H⠋ Working… ██\x1b[K\x1b[5;1H Tip: /web\x1b[K\x1b[6;1H${SYNC_END}`;
    const replay = `conv-01 строка\r\nconv-02 строка\r\n${kimiFrame}`;
    for (const withEnd of [true, false]) {
      const t = makeTerm();
      await write(t, PRELUDE);
      let txn = marker();
      await write(t, planMarkerPrefix({ renderer: "webgl", isReset: true }));
      const parsedToken = 1; // колбэк записи маркера
      expect(t.modes.synchronizedOutputMode).toBe(true);
      expect(txnAfterParse(txn, { parsedToken, modeOn: t.modes.synchronizedOutputMode }).ended).toBe(false);
      await write(t, withEnd ? replay : replay.slice(0, replay.length - SYNC_END.length));
      const r = txnAfterParse(txn, { parsedToken, modeOn: t.modes.synchronizedOutputMode });
      txn = r.state;
      expect(r.ended).toBe(withEnd);
      expect(txn.kind).toBe(withEnd ? "closed" : "open");
      // Режим булев: END приложения снял и НАШ BEGIN — удерживать уже нечего.
      expect(t.modes.synchronizedOutputMode).toBe(!withEnd);
      t.dispose();
    }
  });
});
