import { describe, expect, it, vi } from "vitest";
import headless from "@xterm/headless";
// Исходники для стража T-39 — средствами Vite (?raw, import.meta.glob), без
// node:fs: у apk нет типов Node, а vite/client подключён (vite-env.d.ts).
import blocksSource from "./commandBlocks.ts?raw";
import * as blocksModule from "./commandBlocks";
import {
  attachCommandBlocks, availableBlockActions, blockCopyOutcome, blockNotice, blockText, CommandBlockModel, OSC133_MAX_PAYLOAD, parseOsc133,
  type BlockPoint, type CellRange,
} from "./commandBlocks";

// ── Точки для L1: позиция маркера с управляемой утилизацией ──────────────────
type FakePoint = BlockPoint & { gone: boolean; released: number };
const pt = (line: number, col = 0): FakePoint => {
  const p = {
    gone: false, released: 0, col,
    get line() { return p.gone ? -1 : line; },
    disposed: () => p.gone,
    release: () => { p.released++; },
  };
  return p;
};

describe("T-39a: разбор OSC 133", () => {
  it("четыре известных маркера и код выхода у D", () => {
    expect(parseOsc133("A")).toEqual({ kind: "A" });
    expect(parseOsc133("B")).toEqual({ kind: "B" });
    expect(parseOsc133("C")).toEqual({ kind: "C" });
    expect(parseOsc133("D")).toEqual({ kind: "D" });
    expect(parseOsc133("D;0")).toEqual({ kind: "D", exit: 0 });
    expect(parseOsc133("D;130")).toEqual({ kind: "D", exit: 130 });
    // $LASTEXITCODE PowerShell бывает отрицательным (NTSTATUS).
    expect(parseOsc133("D;-1073741510")).toEqual({ kind: "D", exit: -1073741510 });
  });

  it("параметры вида aid=… пропускаются безопасно", () => {
    expect(parseOsc133("A;aid=42;cl=m")).toEqual({ kind: "A" });
    expect(parseOsc133("C;aid=42")).toEqual({ kind: "C" });
    expect(parseOsc133("D;0;aid=42")).toEqual({ kind: "D", exit: 0 });
    expect(parseOsc133("D;aid=42")).toEqual({ kind: "D" });
    expect(parseOsc133("D;;aid=42")).toEqual({ kind: "D" });
    expect(parseOsc133("D;1e3")).toEqual({ kind: "D" });
  });

  it("неизвестные ключи и мусор отвергаются", () => {
    for (const junk of ["", "E;ls", "P;k=i", "AA", "a", "Z;1", ";A", " A", "A B", "133;A"]) {
      expect(parseOsc133(junk)).toBeNull();
    }
  });

  it("нагрузка длиннее 256 символов отвергается целиком", () => {
    const at = "A;" + "x".repeat(OSC133_MAX_PAYLOAD - 2);
    expect(at.length).toBe(256);
    expect(parseOsc133(at)).toEqual({ kind: "A" });
    expect(parseOsc133(at + "x")).toBeNull();
    expect(parseOsc133("D;0;" + "y".repeat(10_000))).toBeNull();
  });
});

describe("T-39a: модель блоков", () => {
  it("A→B→C→D;0 — один завершённый блок с верными диапазонами", () => {
    const m = new CommandBlockModel();
    expect(m.accept({ kind: "A" }, pt(10, 0), "e1")).toBe(true);
    expect(m.accept({ kind: "B" }, pt(10, 13), "e1")).toBe(true);
    expect(m.accept({ kind: "C" }, pt(11, 0), "e1")).toBe(true);
    expect(m.accept({ kind: "D", exit: 0 }, pt(14, 0), "e1")).toBe(true);
    const last = m.lastComplete()!;
    expect(last.state).toBe("done");
    expect(last.exit).toBe(0);
    expect(m.integration()).toBe("active");
    expect(m.ranges(last)).toEqual({
      command: { startLine: 10, startCol: 13, endLine: 11, endCol: 0, truncated: false },
      output: { startLine: 11, startCol: 0, endLine: 14, endCol: 0, truncated: false },
      truncated: false,
    });
  });

  it("D;130 — прервана, но команда и вывод есть", () => {
    const m = new CommandBlockModel();
    m.accept({ kind: "A" }, pt(0), "e");
    m.accept({ kind: "B" }, pt(0, 2), "e");
    m.accept({ kind: "C" }, pt(1), "e");
    m.accept({ kind: "D", exit: 130 }, pt(2), "e");
    const last = m.lastComplete()!;
    expect(last.state).toBe("interrupted");
    expect(last.exit).toBe(130);
    expect(m.ranges(last).output).toEqual({ startLine: 1, startCol: 0, endLine: 2, endCol: 0, truncated: false });
  });

  it("A без D закрывает открытый блок как interrupted; lastComplete его не выдаёт", () => {
    const m = new CommandBlockModel();
    m.accept({ kind: "A" }, pt(0), "e");
    m.accept({ kind: "B" }, pt(0, 2), "e");
    m.accept({ kind: "C" }, pt(1), "e");
    m.accept({ kind: "D", exit: 0 }, pt(2), "e");
    m.accept({ kind: "A" }, pt(2), "e");
    m.accept({ kind: "B" }, pt(2, 2), "e");
    m.accept({ kind: "C" }, pt(3), "e");
    const nextPrompt = pt(9);
    m.accept({ kind: "A" }, nextPrompt, "e");
    const [first, second, third] = m.list();
    expect(second.state).toBe("interrupted");
    expect(second.endedByMark).toBe(false);
    expect(second.end).toBe(nextPrompt);
    expect(m.ranges(second).output).toEqual({ startLine: 3, startCol: 0, endLine: 9, endCol: 0, truncated: false });
    expect(third.state).toBe("prompt");
    expect(m.lastComplete()).toBe(first);
  });

  it("D без C (PowerShell) — вывод со строки после B", () => {
    const m = new CommandBlockModel();
    m.accept({ kind: "D", exit: 0 }, pt(0), "e"); // первый D до любого A — мимо
    m.accept({ kind: "A" }, pt(0), "e");
    m.accept({ kind: "B" }, pt(0, 9), "e");
    m.accept({ kind: "D", exit: 1 }, pt(4), "e");
    const last = m.lastComplete()!;
    expect(last.state).toBe("done");
    expect(last.exit).toBe(1);
    expect(m.ranges(last)).toEqual({
      command: { startLine: 0, startCol: 9, endLine: 1, endCol: 0, truncated: false },
      output: { startLine: 1, startCol: 0, endLine: 4, endCol: 0, truncated: false },
      truncated: false,
    });
  });

  it("перерисовка приглашения (A, A) не плодит пустых блоков", () => {
    const m = new CommandBlockModel();
    const first = pt(0);
    m.accept({ kind: "A" }, first, "e");
    m.accept({ kind: "A" }, pt(0), "e");
    expect(m.list()).toHaveLength(1);
    expect(first.released).toBe(1);
  });

  it("маркеры не к месту игнорируются и маркер отпускается", () => {
    const m = new CommandBlockModel();
    for (const kind of ["B", "C", "D"] as const) {
      const p = pt(0);
      expect(m.accept({ kind }, p, "e")).toBe(false);
      expect(p.released).toBe(1);
    }
    expect(m.integration()).toBe("none");
    m.accept({ kind: "A" }, pt(0), "e");
    expect(m.accept({ kind: "D", exit: 0 }, pt(0), "e")).toBe(false); // D сразу после A
    const junk = pt(1);
    expect(m.accept(null, junk, "e")).toBe(false);
    expect(junk.released).toBe(1);
  });

  it("не больше 200 блоков; вытесненные отпускают маркеры (I-11)", () => {
    const m = new CommandBlockModel();
    const firstPrompt = pt(0);
    for (let i = 0; i < 250; i++) {
      m.accept({ kind: "A" }, i === 0 ? firstPrompt : pt(i * 3), "e");
      m.accept({ kind: "B" }, pt(i * 3, 2), "e");
      m.accept({ kind: "C" }, pt(i * 3 + 1), "e");
      m.accept({ kind: "D", exit: 0 }, pt(i * 3 + 2), "e");
    }
    expect(m.list()).toHaveLength(200);
    expect(firstPrompt.released).toBe(1);
    expect(m.list()[0].prompt.line).toBe(50 * 3);
    expect(new CommandBlockModel(3).limit).toBe(3);
  });

  it("reset('epoch') и reset('snapshot') — блоки до точки недоступны: lost", () => {
    for (const reason of ["epoch", "snapshot"] as const) {
      const m = new CommandBlockModel();
      const a = pt(0);
      m.accept({ kind: "A" }, a, "e");
      m.accept({ kind: "B" }, pt(0, 2), "e");
      m.accept({ kind: "C" }, pt(1), "e");
      m.accept({ kind: "D", exit: 0 }, pt(2), "e");
      m.reset(reason);
      expect(m.integration()).toBe("lost");
      expect(m.resetReason()).toBe(reason);
      expect(m.lastComplete()).toBeNull();
      expect(m.list()).toHaveLength(0);
      expect(a.released).toBe(1);
      m.accept({ kind: "A" }, pt(5), "e");
      expect(m.integration()).toBe("active");
    }
  });

  it("смена эпохи писателя в accept сбрасывает модель сама", () => {
    const m = new CommandBlockModel();
    m.accept({ kind: "A" }, pt(0), "e1");
    m.accept({ kind: "B" }, pt(0, 2), "e1");
    // B новой эпохи: старый блок не продолжаем — он из заменённого буфера.
    expect(m.accept({ kind: "B" }, pt(0, 2), "e2")).toBe(false);
    expect(m.integration()).toBe("lost");
    expect(m.list()).toHaveLength(0);
  });

  it("reset до интеграции ничего не теряет — остаётся none", () => {
    const m = new CommandBlockModel();
    m.reset("snapshot");
    expect(m.integration()).toBe("none");
  });

  it("alt-screen (vim, less) открытый блок НЕ выбрасывает: D после выхода закрывает именно его", () => {
    // Регрессия находки скептика ST-10: после less/vim «Копировать команду»
    // молча отдавала ПРЕДЫДУЩУЮ команду — блок выбрасывался, D падал мимо.
    const m = new CommandBlockModel();
    m.accept({ kind: "A" }, pt(0), "e");
    m.accept({ kind: "B" }, pt(0, 2), "e");
    m.accept({ kind: "C" }, pt(1), "e");
    m.accept({ kind: "D", exit: 0 }, pt(2), "e");
    m.accept({ kind: "A" }, pt(2), "e");
    m.accept({ kind: "B" }, pt(2, 2), "e");
    m.accept({ kind: "C" }, pt(3), "e"); // vim
    m.noteAltScreen();
    expect(m.integration()).toBe("active");
    expect(m.list()).toHaveLength(2);
    // Выход из vim, «altdone» в нормальном буфере, затем D.
    expect(m.accept({ kind: "D", exit: 0 }, pt(4), "e")).toBe(true);
    const last = m.lastComplete()!;
    expect(last.prompt.line).toBe(2);
    expect(last.fullscreen).toBe(true);
    expect(last.state).toBe("done");
    const r = m.ranges(last);
    expect(r.command).toEqual({ startLine: 2, startCol: 2, endLine: 3, endCol: 0, truncated: false });
    expect(r.output).toEqual({ startLine: 3, startCol: 0, endLine: 4, endCol: 0, truncated: false });
    // Прежний блок пометки не получил.
    expect(m.list()[0].fullscreen).toBeUndefined();
  });

  it("alt-screen в приглашении и после завершения ничего не помечает; D без C (PowerShell) — помечает", () => {
    const m = new CommandBlockModel();
    m.accept({ kind: "A" }, pt(0), "e");
    m.noteAltScreen(); // приглашение: команды ещё нет
    m.accept({ kind: "B" }, pt(0, 2), "e");
    m.accept({ kind: "C" }, pt(1), "e");
    m.accept({ kind: "D", exit: 0 }, pt(2), "e");
    m.noteAltScreen(); // завершённый блок не трогаем
    expect(m.lastComplete()?.fullscreen).toBeUndefined();
    // PowerShell: C нет, команда идёт в состоянии input до своего D.
    m.accept({ kind: "A" }, pt(2), "e");
    m.accept({ kind: "B" }, pt(2, 4), "e");
    m.noteAltScreen();
    m.accept({ kind: "D", exit: 0 }, pt(3), "e");
    expect(m.lastComplete()?.fullscreen).toBe(true);
  });

  it("previousStart — ближайшее начало команды выше верха окна", () => {
    const m = new CommandBlockModel();
    const prompts = [pt(0), pt(10), pt(20)];
    for (const p of prompts) {
      m.accept({ kind: "A" }, p, "e");
      m.accept({ kind: "B" }, pt(p.line, 2), "e");
      m.accept({ kind: "C" }, pt(p.line + 1), "e");
      m.accept({ kind: "D", exit: 0 }, pt(p.line + 5), "e");
    }
    expect(m.previousStart(25)).toBe(20);
    expect(m.previousStart(20)).toBe(10);
    expect(m.previousStart(0)).toBeNull();
    prompts[1].gone = true; // строка ушла за край scrollback
    expect(m.previousStart(20)).toBe(0);
  });

  it("утилизированные маркеры: начало — truncated от строки 0, конец — диапазона нет", () => {
    const m = new CommandBlockModel();
    const b = pt(3, 2), c = pt(4), d = pt(9);
    m.accept({ kind: "A" }, pt(3), "e");
    m.accept({ kind: "B" }, b, "e");
    m.accept({ kind: "C" }, c, "e");
    m.accept({ kind: "D", exit: 0 }, d, "e");
    const block = m.lastComplete()!;
    b.gone = true; c.gone = true;
    expect(m.ranges(block)).toEqual({
      command: null,
      output: { startLine: 0, startCol: 0, endLine: 9, endCol: 0, truncated: true },
      truncated: true,
    });
    d.gone = true;
    expect(m.ranges(block)).toEqual({ command: null, output: null, truncated: true });
  });

  it("D без C, строка B ушла за край — начало вывода обрезано, а не выдумано", () => {
    const m = new CommandBlockModel();
    const b = pt(3, 2);
    m.accept({ kind: "A" }, pt(3), "e");
    m.accept({ kind: "B" }, b, "e");
    m.accept({ kind: "D", exit: 0 }, pt(8), "e");
    b.gone = true;
    const r = m.ranges(m.lastComplete()!);
    expect(r.command).toBeNull();
    expect(r.output).toEqual({ startLine: 0, startCol: 0, endLine: 8, endCol: 0, truncated: true });
    expect(r.truncated).toBe(true);
  });
});

// ── T-39b: долгий C без D не делает терминал «Агентом» ───────────────────────
describe("T-39b: C без D 60 секунд — это просто идущая команда", () => {
  it("блок остаётся running, модель не заводит таймеров и ничего не решает", () => {
    vi.useFakeTimers();
    try {
      const m = new CommandBlockModel();
      m.accept({ kind: "A" }, pt(0), "e");
      m.accept({ kind: "B" }, pt(0, 2), "e");
      m.accept({ kind: "C" }, pt(1), "e"); // sleep 60
      vi.advanceTimersByTime(60_000);
      expect(vi.getTimerCount()).toBe(0);
      expect(m.list()[0].state).toBe("running");
      expect(m.integration()).toBe("active");
      expect(m.lastComplete()).toBeNull();
      expect(m.ranges(m.list()[0]).output).toBeNull();
    } finally {
      vi.useRealTimers();
    }
  });

  const source: string = blocksSource;

  it("страж: модуль не импортирует маршрутизацию прокрутки, React и DOM", () => {
    const imports = [...source.matchAll(/^\s*import\s[^;]*?from\s+["']([^"']+)["']/gm)].map(m => m[1]);
    const dynamic = [...source.matchAll(/\bimport\(\s*["']([^"']+)["']\s*\)/g)].map(m => m[1]);
    for (const spec of [...imports, ...dynamic]) {
      expect(spec).not.toMatch(/navigation|altScroll|scroll|sessionRuntime|PtyTermView|react|history/i);
    }
    expect(source).not.toMatch(/\b(document|window)\./);
  });

  it("страж: ни экспорт, ни методы модели не решают маршрут", () => {
    // «Model» в имени класса — не «mode»: ищем слова решения о маршруте.
    const routeWords = /route|owner|drive|scroll|navigat|agent|\btui|tuiMode|autoMode/i;
    for (const name of Object.keys(blocksModule)) expect(name).not.toMatch(routeWords);
    const methods = Object.getOwnPropertyNames(CommandBlockModel.prototype).filter(n => n !== "constructor");
    expect(methods.length).toBeGreaterThan(0);
    for (const name of methods) expect(name).not.toMatch(routeWords);
  });

  it("страж: правила маршрута не принимают блоки — не ссылаются на модуль", () => {
    const routeSources = import.meta.glob<string>([
      "./navigation*.ts", "./altScroll.ts", "./scrollResponse.ts", "./scrollProbeMemory.ts",
      "./sessionRuntime.ts", "./scrollPreference.ts", "!./*.test.ts",
    ], { query: "?raw", import: "default", eager: true });
    const files = Object.keys(routeSources);
    expect(files.length).toBeGreaterThanOrEqual(3);
    for (const f of files) {
      expect(routeSources[f], f).not.toMatch(/commandBlocks|CommandBlock|Osc133|osc133/);
    }
  });
});

// ── T-39c: интеграции нет ────────────────────────────────────────────────────
describe("T-39c: без интеграции действия скрыты", () => {
  it("none — ни одного действия; флаг выключен — тоже", () => {
    const m = new CommandBlockModel();
    expect(m.integration()).toBe("none");
    expect(availableBlockActions(m, true)).toEqual({ copyCommand: false, copyOutput: false, jumpPrevious: false });
    m.accept({ kind: "A" }, pt(0), "e");
    m.accept({ kind: "B" }, pt(0, 2), "e");
    m.accept({ kind: "C" }, pt(1), "e");
    m.accept({ kind: "D", exit: 0 }, pt(2), "e");
    expect(availableBlockActions(m, false)).toEqual({ copyCommand: false, copyOutput: false, jumpPrevious: false });
  });

  it("положительный контроль: интеграция есть — действия видны; потеряна — скрыты", () => {
    const m = new CommandBlockModel();
    m.accept({ kind: "A" }, pt(0), "e");
    m.accept({ kind: "B" }, pt(0, 2), "e");
    m.accept({ kind: "C" }, pt(1), "e");
    m.accept({ kind: "D", exit: 0 }, pt(2), "e");
    expect(availableBlockActions(m, true)).toEqual({ copyCommand: true, copyOutput: true, jumpPrevious: true });
    m.reset("snapshot");
    expect(availableBlockActions(m, true)).toEqual({ copyCommand: false, copyOutput: false, jumpPrevious: false });
  });
});

// ── T-39d: L2 на настоящем парсере @xterm/headless 6.0.0 ─────────────────────
type HeadlessTerminal = InstanceType<typeof headless.Terminal>;
const ESC = "\x1b", BEL = "\x07", ST = `${ESC}\\`;
const osc = (payload: string, end = BEL) => `${ESC}]133;${payload}${end}`;
const write = (term: HeadlessTerminal, data: string | Uint8Array) => new Promise<void>(resolve => term.write(data, resolve));

/**
 * Текст диапазона ячеек — как его скопировал бы человек: мягкий перенос
 * склеивается без перевода строки, жёсткий даёт `\n`. Колонки — ячейки буфера,
 * поэтому широкие символы режутся ровно по своим границам.
 */
function rangeText(term: HeadlessTerminal, r: CellRange): string {
  const buf = term.buffer.active;
  let out = "";
  for (let l = r.startLine; l <= r.endLine; l++) {
    const line = buf.getLine(l);
    if (!line) break;
    const start = l === r.startLine ? r.startCol : 0;
    const end = l === r.endLine ? r.endCol : term.cols;
    if (l === r.endLine && end === 0 && l > r.startLine) break;
    const continues = l < r.endLine && !!buf.getLine(l + 1)?.isWrapped;
    if (l > r.startLine && !line.isWrapped) out += "\n";
    out += line.translateToString(!continues, start, end);
  }
  return out;
}

function summary(term: HeadlessTerminal, m: CommandBlockModel) {
  return m.list().map(b => {
    const r = m.ranges(b);
    return {
      state: b.state, exit: b.exit,
      command: r.command && rangeText(term, r.command),
      output: r.output && rangeText(term, r.output),
      truncated: r.truncated,
    };
  });
}

function screenText(term: HeadlessTerminal): string {
  const buf = term.buffer.active;
  const lines: string[] = [];
  for (let i = 0; i < buf.length; i++) lines.push(buf.getLine(i)?.translateToString(true) ?? "");
  return lines.join("\n");
}

const PROMPT = "user@host:~$ ";
const STREAM =
  osc("D;0") // первый PROMPT_COMMAND до любого A — мимо
  + osc("A") + PROMPT + osc("B") + "echo A😀B 中文" + "\r\n" + osc("C") + "A😀B 中文\r\n" + osc("D;0")
  + osc("A;aid=7", ST) + PROMPT + osc("B", ST) + "sleep 60" + "\r\n" + osc("C;aid=7") + "^C\r\n" + osc("D;130;aid=7")
  + osc("E;ls") // чужой ключ (VS Code-вариант) — не маркер и не текст
  + `${ESC}[?1049h` + osc("A") + "tui" + `${ESC}[?1049l` // в alt-буфере маркеры не ставятся
  + osc("A") + PROMPT + osc("B");

const EXPECTED = [
  { state: "done", exit: 0, command: "echo A😀B 中文", output: "A😀B 中文", truncated: false },
  { state: "interrupted", exit: 130, command: "sleep 60", output: "^C", truncated: false },
  { state: "input", exit: undefined, command: null, output: null, truncated: false },
];

async function runChunks(chunks: Uint8Array[], opts: { cols?: number; rows?: number; scrollback?: number } = {}) {
  const term = new headless.Terminal({ cols: opts.cols ?? 40, rows: opts.rows ?? 10, scrollback: opts.scrollback ?? 1000, allowProposedApi: true });
  const model = new CommandBlockModel();
  const handler = attachCommandBlocks(term, model, () => "epoch-1");
  // Куски ставятся в очередь write по одному: xterm разбирает каждый ОТДЕЛЬНО,
  // перенося состояние парсера и UTF-8 декодера между ними, — ровно как чанки
  // из WebSocket. Ждём только последний (иначе сотни терминалов не успевают).
  for (let i = 0; i < chunks.length - 1; i++) term.write(chunks[i]);
  await write(term, chunks[chunks.length - 1]);
  return { term, model, handler };
}

/** Детерминированный генератор (mulberry32) — разбиения воспроизводимы по seed. */
function rng(seed: number) {
  let a = seed >>> 0;
  return () => {
    a = (a + 0x6d2b79f5) >>> 0;
    let t = a;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

// ── Полноэкранная программа внутри команды (находка скептика ST-10) ─────────
// Проводка как в PtyTermView: onBufferChange → noteAltScreen. Поток — байты
// bash с нашей разметкой: команда, C, вход в alt-screen, рисунок программы,
// выход, «altdone» в обычном буфере, D;0. До исправления блок выбрасывался при
// входе в alt, D падал мимо, и копировалась ПРЕДЫДУЩАЯ команда.
describe("T-39: alt-screen внутри команды на настоящем парсере", () => {
  const run = async (stream: string) => {
    const term = new headless.Terminal({ cols: 40, rows: 10, scrollback: 1000, allowProposedApi: true });
    const model = new CommandBlockModel();
    attachCommandBlocks(term, model, () => "epoch-1");
    term.buffer.onBufferChange(active => { if (active.type === "alternate") model.noteAltScreen(); });
    await write(term, stream);
    const last = model.lastComplete();
    const r = last ? model.ranges(last) : null;
    const text = (range: CellRange | null | undefined) => (range ? blockText(term.buffer.normal, range, term.cols) : null);
    return { last, command: text(r?.command), output: text(r?.output) };
  };
  const first = osc("A") + PROMPT + osc("B") + "echo first\r\n" + osc("C") + "first\r\n" + osc("D;0");

  it("вывод после выхода из программы — вывод ЭТОЙ команды, рисунок программы в него не входит", async () => {
    const got = await run(first
      + osc("A") + PROMPT + osc("B") + "tui; echo altdone\r\n" + osc("C")
      + `${ESC}[?1049h${ESC}[HFULLSCREEN-APP${ESC}[?1049l` + "altdone\r\n" + osc("D;0")
      + osc("A") + PROMPT + osc("B"));
    expect(got.command).toBe("tui; echo altdone");
    expect(got.output).toBe("altdone");
    expect(got.last?.fullscreen).toBe(true);
    expect(blockCopyOutcome(got.last, "output", got.last ? { startLine: 0, startCol: 0, endLine: 0, endCol: 0, truncated: false } : null, got.output ?? ""))
      .toBe("fullscreen");
  });

  it("less и выход по q: команда — эта, вывода вне программы нет — честная пометка", async () => {
    const got = await run(first
      + osc("A") + PROMPT + osc("B") + "less notes.txt\r\n" + osc("C")
      + `${ESC}[?1049h${ESC}[Hpager text${ESC}[?1049l` + osc("D;0")
      + osc("A") + PROMPT + osc("B"));
    expect(got.command).toBe("less notes.txt");
    expect(got.output).toBe("");
    expect(blockCopyOutcome(got.last, "output", { startLine: 0, startCol: 0, endLine: 0, endCol: 0, truncated: false }, got.output ?? ""))
      .toBe("fullscreen-empty");
  });

  it("положительный контроль: команда без alt-screen — без пометки", async () => {
    const got = await run(first + osc("A") + PROMPT + osc("B"));
    expect(got.command).toBe("echo first");
    expect(got.output).toBe("first");
    expect(got.last?.fullscreen).toBeUndefined();
  });
});

describe("T-39d: OSC 133 на настоящем парсере xterm 6", () => {
  const bytes = new TextEncoder().encode(STREAM);

  it("целый поток: команды и вывод копируются точно (A😀B, CJK), OSC не течёт в текст", async () => {
    const { term, model, handler } = await runChunks([bytes]);
    expect(summary(term, model)).toEqual(EXPECTED);
    expect(model.lastComplete()?.exit).toBe(130);
    const text = screenText(term);
    expect(text).not.toContain("133");
    expect(text).not.toContain("aid=");
    expect(text).not.toContain("ls");
    handler.dispose();
    term.dispose();
  });

  it("разрез по ЛЮБОЙ границе байта (внутри OSC, ESC, UTF-8 эмодзи и CJK) даёт тот же результат", async () => {
    let checked = 0;
    for (let cut = 1; cut < bytes.length; cut++) {
      const { term, model, handler } = await runChunks([bytes.subarray(0, cut), bytes.subarray(cut)]);
      expect(summary(term, model), `разрез на байте ${cut}`).toEqual(EXPECTED);
      handler.dispose();
      term.dispose();
      checked++;
    }
    expect(checked).toBe(bytes.length - 1);
  }, 120_000);

  it("случайное дробление на 2–40 кусков (seed) и побайтовая подача — тот же результат", async () => {
    const random = rng(133);
    for (let round = 0; round < 60; round++) {
      const pieces = 2 + Math.floor(random() * 39);
      const cuts = new Set<number>();
      while (cuts.size < Math.min(pieces - 1, bytes.length - 1)) cuts.add(1 + Math.floor(random() * (bytes.length - 1)));
      const sorted = [0, ...[...cuts].sort((a, b) => a - b), bytes.length];
      const chunks = sorted.slice(1).map((end, i) => bytes.subarray(sorted[i], end));
      const { term, model, handler } = await runChunks(chunks);
      expect(summary(term, model), `seed 133, раунд ${round}`).toEqual(EXPECTED);
      handler.dispose();
      term.dispose();
    }
    const single = Array.from(bytes, b => Uint8Array.of(b));
    const { term, model } = await runChunks(single);
    expect(summary(term, model)).toEqual(EXPECTED);
    term.dispose();
  }, 120_000);

  it("длинная команда с мягким переносом копируется одной строкой", async () => {
    const command = "echo 0123456789abcdefghijklmnopqrstuvwxyz ok";
    const stream = osc("A") + "$ " + osc("B") + command + "\r\n" + osc("C") + "out\r\n" + osc("D;0");
    const { term, model } = await runChunks([new TextEncoder().encode(stream)], { cols: 20 });
    const r = model.ranges(model.lastComplete()!);
    expect(r.command!.endLine - r.command!.startLine).toBeGreaterThanOrEqual(2);
    expect(rangeText(term, r.command!)).toBe(command);
    expect(rangeText(term, r.output!)).toBe("out");
    term.dispose();
  });

  it("обрезка scrollback утилизирует маркер — блок помечается truncated, текст не выдумывается", async () => {
    const enc = new TextEncoder();
    const lines = Array.from({ length: 40 }, (_, i) => `line-${i}\r\n`).join("");
    const stream = osc("A") + "$ " + osc("B") + "cat big" + "\r\n" + osc("C") + lines + osc("D;0") + osc("A") + "$ " + osc("B");
    const { term, model } = await runChunks([enc.encode(stream)], { cols: 20, rows: 5, scrollback: 10 });
    const block = model.lastComplete()!;
    expect(block.input!.disposed()).toBe(true);
    expect(block.output!.disposed()).toBe(true);
    expect(block.end!.disposed()).toBe(false);
    const r = model.ranges(block);
    expect(r.truncated).toBe(true);
    expect(r.command).toBeNull();
    expect(r.output!.truncated).toBe(true);
    const out = rangeText(term, r.output!);
    expect(out.startsWith("line-0\n")).toBe(false);
    expect(out.endsWith("line-39")).toBe(true);
    // Начало этой команды ушло за край — перейти к нему нельзя.
    const lastPrompt = model.list()[1].prompt.line;
    expect(model.previousStart(lastPrompt)).toBeNull();

    // Ещё 40 строк — ушёл и D: диапазона вывода нет вовсе, действия скрыты.
    await write(term, enc.encode(lines));
    const gone = model.ranges(block);
    expect(gone).toEqual({ command: null, output: null, truncated: true });
    expect(availableBlockActions(model, true).copyOutput).toBe(false);
    expect(availableBlockActions(model, true).copyCommand).toBe(false);
    term.dispose();
  });
});

// ── ST-10: текст блока для «Копировать команду / вывод» (продуктовая функция) ──
describe("blockText: то, что уходит в буфер обмена (T-15, I-07)", () => {
  it("совпадает с эталонным разбором на всём потоке: команда, вывод, эмодзи, CJK", async () => {
    const { term, model } = await runChunks([new TextEncoder().encode(STREAM)]);
    for (const b of model.list()) {
      const r = model.ranges(b);
      if (r.command) expect(blockText(term.buffer.active, r.command, term.cols)).toBe(rangeText(term, r.command));
      if (r.output) expect(blockText(term.buffer.active, r.output, term.cols)).toBe(rangeText(term, r.output));
    }
    const done = model.list()[0];
    expect(blockText(term.buffer.active, model.ranges(done).command!, term.cols)).toBe("echo A😀B 中文");
    expect(blockText(term.buffer.active, model.ranges(done).output!, term.cols)).toBe("A😀B 中文");
    term.dispose();
  });

  it("конец в колонке 0 не тащит перевод строки; многострочный вывод — через \\n", async () => {
    const stream = osc("A") + "$ " + osc("B") + "seq 3" + "\r\n" + osc("C") + "1\r\n2\r\n3\r\n" + osc("D;0");
    const { term, model } = await runChunks([new TextEncoder().encode(stream)]);
    const r = model.ranges(model.lastComplete()!);
    expect(blockText(term.buffer.active, r.command!, term.cols)).toBe("seq 3");
    expect(blockText(term.buffer.active, r.output!, term.cols)).toBe("1\n2\n3");
    term.dispose();
  });

  it("широкий символ, не влезший в последнюю колонку, не даёт пробела внутри команды (T-15)", async () => {
    // cols=7: «$ » (2) + «abcd» (4) = 6 колонок заняты, «中» шириной 2 в
    // оставшуюся одну не влезает и уезжает на новую строку — в колонке 6
    // остаётся незаписанная ячейка.
    const command = "abcd中ef";
    const stream = osc("A") + "$ " + osc("B") + command + "\r\n" + osc("C") + "x\r\n" + osc("D;0");
    const { term, model } = await runChunks([new TextEncoder().encode(stream)], { cols: 7 });
    const r = model.ranges(model.lastComplete()!);
    expect(r.command!.endLine - r.command!.startLine).toBeGreaterThanOrEqual(2);
    expect(blockText(term.buffer.active, r.command!, term.cols)).toBe(command);
    // Эталон теста (translateToString без правила T-15) здесь дал бы пробел.
    expect(rangeText(term, r.command!)).not.toBe(command);
    term.dispose();
  });

  it("набранные пробелы у мягкого переноса сохраняются", async () => {
    const command = "echo a    b";
    const stream = osc("A") + "$ " + osc("B") + command + "\r\n" + osc("C") + "a    b\r\n" + osc("D;0");
    const { term, model } = await runChunks([new TextEncoder().encode(stream)], { cols: 8 });
    const r = model.ranges(model.lastComplete()!);
    expect(blockText(term.buffer.active, r.command!, term.cols)).toBe(command);
    term.dispose();
  });
});

describe("blockNotice: «блоки до восстановления недоступны» (I-11)", () => {
  const cycle = (m: CommandBlockModel, base: number, exit = 0) => {
    m.accept({ kind: "A" }, pt(base), 0);
    m.accept({ kind: "B" }, pt(base, 2), 0);
    m.accept({ kind: "C" }, pt(base + 1), 0);
    m.accept({ kind: "D", exit }, pt(base + 2), 0);
  };

  it("без интеграции пометки нет; сброс до первого маркера — тоже", () => {
    const m = new CommandBlockModel();
    expect(blockNotice(m, true)).toBe("none");
    m.reset("snapshot");
    expect(blockNotice(m, true)).toBe("none");
    expect(m.integration()).toBe("none");
  });

  it("сброс эпохи или кадр после блоков — пометка до первой завершённой команды", () => {
    for (const reason of ["epoch", "snapshot"] as const) {
      const m = new CommandBlockModel();
      cycle(m, 0);
      expect(blockNotice(m, true)).toBe("none");
      m.reset(reason);
      expect(blockNotice(m, true)).toBe("restored");
      expect(availableBlockActions(m, true)).toEqual({ copyCommand: false, copyOutput: false, jumpPrevious: false });
      // Новое приглашение: интеграция снова активна, но блоков до сброса нет.
      m.accept({ kind: "A" }, pt(10), 0);
      m.accept({ kind: "B" }, pt(10, 2), 0);
      expect(m.integration()).toBe("active");
      expect(blockNotice(m, true)).toBe("restored");
      m.accept({ kind: "C" }, pt(11), 0);
      m.accept({ kind: "D", exit: 0 }, pt(12), 0);
      expect(blockNotice(m, true)).toBe("none");
      expect(availableBlockActions(m, true).copyCommand).toBe(true);
    }
  });

  it("alt-screen — не сброс: пометки «до восстановления» нет, действия есть; флаг выключен — пометки нет", () => {
    const m = new CommandBlockModel();
    cycle(m, 0);
    m.accept({ kind: "A" }, pt(3), 0);
    m.accept({ kind: "B" }, pt(3, 2), 0);
    m.accept({ kind: "C" }, pt(4), 0);
    m.noteAltScreen();
    expect(blockNotice(m, true)).toBe("none");
    expect(m.lastComplete()).not.toBeNull();
    m.accept({ kind: "D", exit: 0 }, pt(5), 0);
    expect(m.lastComplete()?.prompt.line).toBe(3);
    expect(availableBlockActions(m, true)).toMatchObject({ copyCommand: true, copyOutput: true });
    m.reset("epoch");
    expect(blockNotice(m, false)).toBe("none");
  });

  it("подпись копирования: полноэкранная программа видна, обрезка важнее, у команды пометки нет", () => {
    const m = new CommandBlockModel();
    m.accept({ kind: "A" }, pt(0), 0);
    m.accept({ kind: "B" }, pt(0, 2), 0);
    m.accept({ kind: "C" }, pt(1), 0);
    m.noteAltScreen();
    m.accept({ kind: "D", exit: 0 }, pt(1), 0);
    const block = m.lastComplete()!;
    const r = m.ranges(block);
    expect(blockCopyOutcome(block, "output", r.output, "")).toBe("fullscreen-empty");
    expect(blockCopyOutcome(block, "output", r.output, "altdone")).toBe("fullscreen");
    expect(blockCopyOutcome(block, "command", r.command, "less notes")).toBe("copied");
    expect(blockCopyOutcome(block, "output", { ...r.output!, truncated: true }, "x")).toBe("truncated");
    // Без блока и без текста — просто «нечего копировать».
    expect(blockCopyOutcome(null, "output", null, "")).toBe("nothing");
    const plain = new CommandBlockModel();
    cycle(plain, 0);
    const p = plain.lastComplete()!;
    expect(blockCopyOutcome(p, "output", plain.ranges(p).output, "")).toBe("nothing");
    expect(blockCopyOutcome(p, "output", plain.ranges(p).output, "ok")).toBe("copied");
  });

  it("долгая команда без D (sleep 60): блок открыт, прежний завершённый остаётся последним (T-39b)", () => {
    vi.useFakeTimers();
    try {
      const m = new CommandBlockModel();
      cycle(m, 0);
      const before = m.lastComplete();
      m.accept({ kind: "A" }, pt(3), 0);
      m.accept({ kind: "B" }, pt(3, 2), 0);
      m.accept({ kind: "C" }, pt(4), 0);
      vi.advanceTimersByTime(60_000);
      expect(m.list()[m.list().length - 1].state).toBe("running");
      expect(m.lastComplete()).toBe(before);
      expect(blockNotice(m, true)).toBe("none");
      // Ctrl+C: D;130 закрывает блок как прерванный.
      m.accept({ kind: "D", exit: 130 }, pt(5), 0);
      expect(m.lastComplete()?.state).toBe("interrupted");
    } finally { vi.useRealTimers(); }
  });
});
