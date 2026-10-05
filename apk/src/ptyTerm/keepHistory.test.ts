import { describe, expect, it } from "vitest";
import { Terminal as HeadlessTerminal } from "@xterm/headless";
import {
  eraseActionFor,
  eraseChainEnds,
  eraseRoute,
  generationDropsPendingErase,
  keepPendingErase,
  keepPendingScrollbackErase,
  navigationChoiceDropsPendingErase,
  retentionFor,
  RetentionShadowLog,
  ScrollbackEraseGate,
  scrollbackEraseAction,
  splitScrollbackErase,
  stripScrollbackErase,
} from "./keepHistory";
import type { EraseChainItem, EraseScrollback, RetentionPolicy } from "./keepHistory";
import {
  decayStreamSample,
  historyOwnerFromStream,
  resolveHistoryOwner,
  STREAM_SAMPLE_MIN_BYTES,
  STREAM_WINDOW_BYTES,
} from "./altScroll";
import type { HistoryOwner } from "./altScroll";
import { effectiveHistoryOwner, type ScrollOverride } from "./sessionRuntime";
import { TerminalWriter } from "./terminalWriter";
import { bufferText } from "./reading/bufferText";
import { builtinRetention, declaredRetention, setAgentRegistry } from "../../../packages/shared/src/agentRegistry";

const enc = (s: string) => new TextEncoder().encode(s);
const dec = (b: Uint8Array) => new TextDecoder().decode(b);

/** Пропустить поток кусками — как он и приходит из WebSocket. */
function feed(chunks: string[]): string {
  let tail: Uint8Array<ArrayBufferLike> = new Uint8Array(0);
  let out = "";
  for (const chunk of chunks) {
    const bytes = enc(chunk);
    const merged = new Uint8Array(tail.byteLength + bytes.byteLength);
    merged.set(tail, 0);
    merged.set(bytes, tail.byteLength);
    const res = stripScrollbackErase(merged);
    tail = res.tail;
    out += dec(res.data);
  }
  return out;
}

describe("защита истории терминала", () => {
  it("вырезает «стереть историю» (ESC[3J)", () => {
    // Живой случай: агент перерисовывает окно после изменения размера и стирает
    // историю — человек «улетает в самый верх» (жалоба 31.07 про Kimi).
    expect(feed(["до[3Jпосле"])).toBe("допосле");
  });

  it("вырезает вариант с приватным префиксом (ESC[?3J)", () => {
    expect(feed(["a[?3Jb"])).toBe("ab");
  });

  it("ловит последовательность, разорванную между кадрами", () => {
    // Ровно то, ради чего функция возвращает хвост: WebSocket рвёт поток где
    // угодно, и «ESC[» с «3J» приходят в разных кадрах.
    expect(feed(["хвост[", "3Jначало"])).toBe("хвостначало");
    expect(feed(["x", "[3Jy"])).toBe("xy");
    expect(feed(["x[3", "Jy"])).toBe("xy");
  });

  it("не трогает очистку экрана (ESC[2J) и другие последовательности", () => {
    // `clear` по-прежнему очищает видимый экран — история просто переживает это.
    expect(feed(["a[2Jb"])).toBe("a[2Jb");
    expect(feed(["[31mкрасный[0m"])).toBe("[31mкрасный[0m");
    expect(feed(["[H[J"])).toBe("[H[J");
    // Похожие, но другие: 13J и 3K трогать нельзя.
    expect(feed(["[13J"])).toBe("[13J");
    expect(feed(["[3K"])).toBe("[3K");
  });

  it("держит незавершённый хвост до следующего кадра, а не печатает его", () => {
    const first = stripScrollbackErase(enc("текст["));
    expect(dec(first.data)).toBe("текст");
    expect(first.tail.byteLength).toBe(2); // ESC[ ждут продолжения
  });

  it("пустой ввод не ломает разбор", () => {
    const res = stripScrollbackErase(new Uint8Array(0));
    expect(res.data.byteLength).toBe(0);
    expect(res.tail.byteLength).toBe(0);
  });

  it("считает вырезанные стирания — по ним вызывающий узнаёт об отложенном", () => {
    // Счётчик нужен экрану терминала: пока человек читает историю, стирание
    // вырезается, но о просьбе надо помнить и выполнить её при возврате к низу.
    expect(stripScrollbackErase(enc("a[3Jb[3Jc")).erased).toBe(2);
    expect(stripScrollbackErase(enc("a[2Jb")).erased).toBe(0);
  });

  it("passErase пропускает стирание как есть — история схлопывается до текущего экрана", () => {
    // Человек стоит у низа: прошлая копия переписки агента ему не нужна, и
    // просьбу «стереть историю» отдаём терминалу без изменений. Хвост при этом
    // не удерживаем — разорванную последовательность склеит сам разбор xterm.
    const res = stripScrollbackErase(enc("до[3Jпосле"), true);
    expect(dec(res.data)).toBe("до[3Jпосле");
    expect(res.tail.byteLength).toBe(0);
    const split = stripScrollbackErase(enc("хвост["), true);
    expect(dec(split.data)).toBe("хвост[");
    expect(split.tail.byteLength).toBe(0);
  });
});

/**
 * Поток кусками (как из WebSocket) через splitScrollbackErase; стирания в
 * выводе обозначаем меткой <3J>, чтобы проверять их место в потоке.
 */
function feedSplit(chunks: string[]): string {
  let tail: Uint8Array<ArrayBufferLike> = new Uint8Array(0);
  let out = "";
  for (const chunk of chunks) {
    const bytes = enc(chunk);
    const merged = new Uint8Array(tail.byteLength + bytes.byteLength);
    merged.set(tail, 0);
    merged.set(bytes, tail.byteLength);
    const res = splitScrollbackErase(merged);
    tail = res.tail;
    for (const it of res.items) {
      out += it.kind === "erase" ? "<3J>" : dec(it.data);
    }
  }
  return out;
}

const CSI3J = "\x1b[3J";

class ManualEraseTerminal {
  readonly writes: string[] = [];
  private callbacks: Array<() => void> = [];

  write(data: string | Uint8Array, callback?: () => void): void {
    this.writes.push(typeof data === "string" ? data : dec(data));
    this.callbacks.push(() => callback?.());
  }

  completeOne(): void {
    const callback = this.callbacks.shift();
    if (!callback) throw new Error("no terminal write is pending");
    callback();
  }
}

const writeHeadless = (term: HeadlessTerminal, data: string | Uint8Array) => (
  new Promise<void>((resolve) => term.write(data, resolve))
);

async function repaintAtBottom(policy: RetentionPolicy): Promise<HeadlessTerminal> {
  const term = new HeadlessTerminal({
    allowProposedApi: true,
    cols: 20,
    rows: 5,
    scrollback: 100,
  });
  await writeHeadless(term, Array.from({ length: 20 }, (_, i) => `old-${i}\r\n`).join(""));
  const { items } = splitScrollbackErase(enc(`\x1b[2J${CSI3J}\x1b[Hnew`));
  for (const item of items) {
    if (item.kind === "data") await writeHeadless(term, item.data);
    else if (eraseActionFor(policy, false) === "write") await writeHeadless(term, CSI3J);
  }
  return term;
}

/**
 * ST-04: судьба CSI 3 J — свойство ПОКОЛЕНИЯ переднего процесса, а не режима
 * навигации. Прежние тесты этого блока закрепляли обратное (terminal/Вывод →
 * выбросить, переход Агент → Вывод уничтожает отложенное); их гарантии
 * переписаны по смыслу, legacy-правило проверяется отдельно ниже, пока
 * PtyTermView на него опирается.
 */
describe("RetentionPolicy — хранение истории по поколению (ST-04)", () => {
  const gen = "kimi:4242";

  it("без агента на переднем плане — preserve/shell: как прежнее «оболочка всегда вырезает 3J»", () => {
    for (const declared of ["", "honor", "preserve"] as const) {
      expect(retentionFor({ generation: "shell:1", agentInFg: false, declared }))
        .toEqual({ erase: "preserve", source: "shell", generation: "shell:1" });
    }
  });

  it("реестр объявил — его значение; не объявил — honor/default (прежний unknown)", () => {
    expect(retentionFor({ generation: gen, agentInFg: true, declared: "preserve" }))
      .toEqual({ erase: "preserve", source: "registry", generation: gen });
    expect(retentionFor({ generation: gen, agentInFg: true, declared: "honor" }))
      .toEqual({ erase: "honor", source: "registry", generation: gen });
    expect(retentionFor({ generation: gen, agentInFg: true, declared: "" }))
      .toEqual({ erase: "honor", source: "default", generation: gen });
    // Непонятное объявление (новый агент прислал своё) не угадывается.
    expect(retentionFor({ generation: gen, agentInFg: true, declared: "wipe" as never }))
      .toEqual({ erase: "honor", source: "default", generation: gen });
    expect(Object.isFrozen(retentionFor({ generation: gen, agentInFg: true, declared: "" }))).toBe(true);
  });

  it("preserve никогда не исполняет и не откладывает стирание", () => {
    const policy = retentionFor({ generation: gen, agentInFg: true, declared: "preserve" });
    expect(eraseActionFor(policy, false)).toBe("discard");
    expect(eraseActionFor(policy, true)).toBe("discard");
    expect(keepPendingErase(policy)).toBe(false);
  });

  it("honor исполняет у низа и откладывает при чтении", () => {
    const policy = retentionFor({ generation: gen, agentInFg: true, declared: "" });
    expect(eraseActionFor(policy, false)).toBe("write");
    expect(eraseActionFor(policy, true)).toBe("defer");
    expect(keepPendingErase(policy)).toBe(true);
  });

  it("смена режима навигации не меняет судьбу отложенного стирания", () => {
    // Раньше Агент → Вывод навсегда снимал отложенное стирание, а возврат в
    // Агент его не восстанавливал: навигация меняла состав истории (I-02).
    const gate = new ScrollbackEraseGate();
    const ticket = gate.capture();
    const seen = new Set<HistoryOwner>();
    for (const override of ["agent", "terminal", "auto", "agent"] as const) {
      seen.add(effectiveHistoryOwner("terminal", override));
      // Политику пересчитывают по тем же входам поколения — навигация в них не входит.
      const policy = retentionFor({ generation: gen, agentInFg: true, declared: "" });
      if (!keepPendingErase(policy)) gate.invalidate();
    }
    expect(seen).toEqual(new Set(["application", "terminal"])); // навигация действительно менялась
    expect(gate.isCurrent(ticket)).toBe(true);
    // Снимается только на границе поколения: после агента на передний план вернулась оболочка.
    const shell = retentionFor({ generation: "shell:77", agentInFg: false, declared: "" });
    if (!keepPendingErase(shell)) gate.invalidate();
    expect(gate.isCurrent(ticket)).toBe(false);
  });

  it("реальный xterm: preserve сохраняет старую историю на resize-repaint, honor схлопывает её", async () => {
    const preserved = await repaintAtBottom(retentionFor({ generation: "codex:1", agentInFg: true, declared: "preserve" }));
    const honored = await repaintAtBottom(retentionFor({ generation: gen, agentInFg: true, declared: "" }));
    try {
      expect(preserved.buffer.normal.baseY).toBeGreaterThan(0);
      preserved.scrollToTop();
      expect(preserved.buffer.normal.getLine(0)?.translateToString(true)).toContain("old-0");
      expect(honored.buffer.normal.baseY).toBe(0);
    } finally {
      preserved.dispose();
      honored.dispose();
    }
  });

  it("ticket не воскрешает вынутое из очереди стирание после границы поколения", () => {
    const terminal = new ManualEraseTerminal();
    const writer = new TerminalWriter(terminal, { generation: 1, epoch: "e" });
    const gate = new ScrollbackEraseGate();
    let policy = retentionFor({ generation: "kimi:1", agentInFg: true, declared: "" });

    writer.write("busy");
    const ticket = gate.capture(); // pending уже снят и ждёт EMPTY-barrier
    writer.barrier(() => {
      if (!gate.isCurrent(ticket)) return;
      if (eraseActionFor(policy, false) === "write") writer.write(CSI3J);
    });

    // Оболочка, затем новый агент (honor): стирание прежнего поколения не должно ожить.
    policy = retentionFor({ generation: "shell:2", agentInFg: false, declared: "" });
    if (!keepPendingErase(policy)) gate.invalidate();
    policy = retentionFor({ generation: "kimi:3", agentInFg: true, declared: "" });
    terminal.completeOne(); // busy → EMPTY barrier
    terminal.completeOne(); // stale callback must not enqueue CSI3J

    expect(terminal.writes).toEqual(["busy", ""]);
    expect(writer.pending).toBe(0);
  });

  it("реестр доставляет history_retention; убранное или непонятное значение снимается", () => {
    setAgentRegistry([
      { id: "codex", name: "Codex", history_retention: "preserve" },
      { id: "kimi", name: "Kimi", history_retention: "honor" },
      { id: "claude", name: "Claude" },
    ]);
    expect([declaredRetention("codex"), declaredRetention("kimi"), declaredRetention("claude"), declaredRetention(null)])
      .toEqual(["preserve", "honor", "", ""]);
    expect(retentionFor({ generation: "codex:9", agentInFg: true, declared: declaredRetention("codex") }))
      .toEqual({ erase: "preserve", source: "registry", generation: "codex:9" });
    setAgentRegistry([{ id: "codex", name: "Codex" }, { id: "kimi", name: "Kimi", history_retention: "wipe" as never }]);
    expect([declaredRetention("codex"), declaredRetention("kimi")]).toEqual(["", ""]);
  });

  it("старый агент (2.71.1) поля не шлёт: Codex получает встроенный preserve, прочие — умолчание (волна 4)", () => {
    // Порядок как в PtyTermView: state → кэш реестра → встроенный ответ.
    const declaredFor = (id: string, state?: "honor" | "preserve") => state || declaredRetention(id) || builtinRetention(id);
    setAgentRegistry([{ id: "codex", name: "Codex" }, { id: "kimi", name: "Kimi" }]);
    expect(retentionFor({ generation: "codex:1", agentInFg: true, declared: declaredFor("codex") }))
      .toEqual({ erase: "preserve", source: "registry", generation: "codex:1" });
    expect(retentionFor({ generation: "kimi:1", agentInFg: true, declared: declaredFor("kimi") }).erase).toBe("honor");
    expect([builtinRetention("claude"), builtinRetention(""), builtinRetention(null), builtinRetention("toString")])
      .toEqual(["", "", "", ""]);
    // Объявление агента главнее встроенного: новый агент сказал honor — honor.
    expect(declaredFor("codex", "honor")).toBe("honor");
    setAgentRegistry([{ id: "codex", name: "Codex", history_retention: "honor" }]);
    expect(declaredFor("codex")).toBe("honor");
  });
});

/** LEGACY: правило по владельцу истории живо, пока PtyTermView не переключён на RetentionPolicy. */
describe("переключатель хранения legacy|shadow|policy (план 9.1, потребитель — PtyTermView)", () => {
  it("policy исполняет политику, legacy и shadow — прежнее; расхождение видно только в shadow", () => {
    expect(eraseRoute("policy", "discard", "write")).toEqual({ use: "write", diverged: false });
    expect(eraseRoute("legacy", "discard", "write")).toEqual({ use: "discard", diverged: false });
    expect(eraseRoute("shadow", "discard", "write")).toEqual({ use: "discard", diverged: true });
    expect(eraseRoute("shadow", "write", "write")).toEqual({ use: "write", diverged: false });
  });

  it("I-02: выбор режима навигации в политике не снимает отложенное стирание ни при каком владельце", () => {
    for (const owner of ["terminal", "application", "unknown"] as const) {
      expect(navigationChoiceDropsPendingErase("policy", owner)).toBe(false);
    }
    // Положительный контроль: прежнее правило (и shadow, который его исполняет)
    // «Вывод» снимает навсегда — ради этого различия переключатель и есть.
    expect(navigationChoiceDropsPendingErase("legacy", "terminal")).toBe(true);
    expect(navigationChoiceDropsPendingErase("shadow", "terminal")).toBe(true);
    expect(navigationChoiceDropsPendingErase("legacy", "application")).toBe(false);
  });

  it("граница поколения снимает отложенное только в policy; shadow не снимает, а видит расхождение", () => {
    const preserve = retentionFor({ generation: "codex:5302@1", agentInFg: true, declared: "preserve" });
    const honor = retentionFor({ generation: "kimi:4242@1", agentInFg: true, declared: "" });
    expect(generationDropsPendingErase("policy", preserve)).toEqual({ use: true, diverged: false });
    // Прежнее правило границы поколения не знало — shadow обязан вести себя как legacy.
    expect(generationDropsPendingErase("legacy", preserve)).toEqual({ use: false, diverged: false });
    expect(generationDropsPendingErase("shadow", preserve)).toEqual({ use: false, diverged: true });
    // honor держит отложенное: снимать нечего ни в одном режиме, расхождения нет.
    for (const mode of ["policy", "legacy", "shadow"] as const) {
      expect(generationDropsPendingErase(mode, honor)).toEqual({ use: false, diverged: false });
    }
  });

  it("Агент → Вывод → Агент в политике: отложенное стирание переживает смену и снимается только политикой поколения", () => {
    const gate = new ScrollbackEraseGate();
    const ticket = gate.capture();
    const honor = retentionFor({ generation: "kimi:7@1", agentInFg: true, declared: "" });
    for (const override of ["agent", "terminal", "agent", "auto"] as const) {
      const owner = effectiveHistoryOwner("application", override);
      if (navigationChoiceDropsPendingErase("policy", owner)) gate.invalidate();
    }
    expect(gate.isCurrent(ticket)).toBe(true);
    expect(eraseActionFor(honor, false)).toBe("write");
    // Граница поколения: новый процесс без агента — preserve снимает отложенное.
    const next = retentionFor({ generation: "shell:8@2", agentInFg: false, declared: "" });
    if (!keepPendingErase(next)) gate.invalidate();
    expect(gate.isCurrent(ticket)).toBe(false);
  });

  it("журнал shadow: одно сообщение на расхождение в поколении, с потолком; новое поколение — заново", () => {
    const log = new RetentionShadowLog(2);
    expect(log.first("1", "chain:discard:write:false")).toBe(true);
    expect(log.first("1", "chain:discard:write:false")).toBe(false);
    expect(log.first("1", "deferred:discard:write:false")).toBe(true);
    expect(log.first("1", "strip:discard:defer:true")).toBe(false);
    expect(log.first("2", "chain:discard:write:false")).toBe(true);
  });
});

describe("legacy: CSI3J по владельцу истории (до переключения PtyTermView)", () => {
  it("Вывод/terminal никогда не исполняет и не откладывает стирание", () => {
    expect(scrollbackEraseAction("terminal", false)).toBe("discard");
    expect(scrollbackEraseAction("terminal", true)).toBe("discard");
  });

  it("Агент/application исполняет у низа и откладывает во время чтения", () => {
    expect(scrollbackEraseAction("application", false)).toBe("write");
    expect(scrollbackEraseAction("application", true)).toBe("defer");
  });

  it("неизвестный Auto сохраняет прежнее консервативное поведение", () => {
    expect(scrollbackEraseAction("unknown", false)).toBe("write");
    expect(scrollbackEraseAction("unknown", true)).toBe("defer");
    expect(keepPendingScrollbackErase("unknown")).toBe(true);
  });

  it("terminal снимает отложенное стирание, в том числе через Agent → Auto", () => {
    expect(keepPendingScrollbackErase("application")).toBe(true);
    expect(keepPendingScrollbackErase("terminal")).toBe(false);
    const gate = new ScrollbackEraseGate();
    const ticket = gate.capture();
    const nextOwner = effectiveHistoryOwner("terminal", "auto");
    if (!keepPendingScrollbackErase(nextOwner)) gate.invalidate();
    expect(gate.isCurrent(ticket)).toBe(false);
  });
});

/**
 * Кадр полной перерисовки агента: 2J 3J H и `lines` строк (каждая с \r\n),
 * ровно `bytes` байт ASCII. Ширина терминала больше строки: одна строка потока —
 * одна строка буфера, как в замере плотности altScroll.
 */
function repaintFrame(frame: number, lines: number, bytes = STREAM_WINDOW_BYTES): string {
  const prefix = `\x1b[2J${CSI3J}\x1b[H`;
  const body = bytes - prefix.length - lines * 2;
  const per = Math.floor(body / lines);
  let out = prefix;
  for (let i = 0; i < lines; i++) {
    const len = per + (i === lines - 1 ? body - per * lines : 0);
    out += `f${frame}-l${i} `.padEnd(len, ".") + "\r\n";
  }
  return out;
}

/**
 * Один и тот же поток через настоящий xterm с решением по каждому стиранию.
 * Навигационный вердикт считается ровно так, как его считает экран (замер
 * плотности с затуханием, ownScrollback, ручной режим), и подаётся В ТОМ ЖЕ
 * объекте, что и входы политики: правило обязано его игнорировать.
 */
async function runRetention(lines: number, override: ScrollOverride,
  input: { agentInFg: boolean; declared: "" | EraseScrollback },
  decide: (policy: RetentionPolicy, navigation: HistoryOwner) => ReturnType<typeof eraseActionFor> =
    policy => eraseActionFor(policy, false)) {
  const term = new HeadlessTerminal({ cols: 600, rows: 10, scrollback: 1000, allowProposedApi: true });
  let sample = { lines: 0, bytes: 0 };
  const navigation: HistoryOwner[] = [], policies: RetentionPolicy[] = [];
  for (let frame = 0; frame < 3; frame++) {
    const data = repaintFrame(frame, lines);
    expect(enc(data).byteLength).toBe(STREAM_WINDOW_BYTES);
    sample = decayStreamSample(sample.lines + lines, sample.bytes + data.length);
    const nav = effectiveHistoryOwner(resolveHistoryOwner({ stream: historyOwnerFromStream(sample.lines, sample.bytes),
      ownScrollback: term.buffer.normal.baseY, agent: input.agentInFg, alt: false }), override);
    navigation.push(nav);
    const noisy = { generation: "kimi:4242", ...input, stream: nav, ownScrollback: term.buffer.normal.baseY, override };
    const policy = retentionFor(noisy);
    policies.push(policy);
    for (const item of splitScrollbackErase(enc(data)).items) {
      if (item.kind === "data") await writeHeadless(term, item.data);
      else if (decide(policy, nav) === "write") await writeHeadless(term, CSI3J);
    }
  }
  const buffer = term.buffer.normal;
  const text = bufferText(buffer, 0, buffer.length - 1);
  term.dispose();
  return { text, navigation, policies };
}

const copies = (text: string) => (text.match(/-l0 \./g) ?? []).length;

describe("T-04: 31/32/33 строки на 16 КиБ и режим навигации не меняют RetentionPolicy", () => {
  it.each([
    ["агент без объявления → honor", { agentInFg: true, declared: "" as const }, { erase: "honor", source: "default" }, 1],
    ["агент, реестр preserve", { agentInFg: true, declared: "preserve" as const }, { erase: "preserve", source: "registry" }, 3],
    ["оболочка → preserve", { agentInFg: false, declared: "" as const }, { erase: "preserve", source: "shell" }, 3],
  ])("%s: одна политика и один итоговый scrollback во всех вариантах", async (_name, input, expected, frames) => {
    const reference = new Map<number, string>();
    const policies = new Set<string>(), navigation = new Set<HistoryOwner>(), legacy = new Set<string>();
    for (const lines of [31, 32, 33]) {
      for (const override of ["auto", "terminal", "agent"] as const) {
        const run = await runRetention(lines, override, input);
        run.policies.forEach(policy => policies.add(JSON.stringify(policy)));
        run.navigation.forEach(owner => { navigation.add(owner); legacy.add(scrollbackEraseAction(owner, false)); });
        if (!reference.has(lines)) reference.set(lines, run.text);
        expect(run.text).toBe(reference.get(lines));
        // honor у низа: ровно одна копия перерисовки (защита от 137 копий);
        // preserve: копии остаются — обещанная история не стирается.
        expect(copies(run.text)).toBe(frames);
        expect(run.text).toContain("f2-l0 ");
      }
    }
    expect([...policies]).toEqual([JSON.stringify({ ...expected, generation: "kimi:4242" })]);
    // Положительный контроль (раздел 8.2): стенд действительно пересекает порог.
    // Навигация и legacy-правило по владельцу меняются, политика — нет.
    expect(navigation).toEqual(new Set(["application", "terminal"]));
    if (input.agentInFg) expect(legacy).toEqual(new Set(["discard", "write"]));
  });

  it("вердикт плотности по-настоящему переключается между 31 и 32 строками", () => {
    expect(historyOwnerFromStream(31, STREAM_WINDOW_BYTES)).toBe("application");
    expect(historyOwnerFromStream(32, STREAM_WINDOW_BYTES)).toBe("terminal");
    expect(historyOwnerFromStream(33, STREAM_WINDOW_BYTES)).toBe("terminal");
  });

  it("меньше 4096 байт при ownScrollback 199/200: навигация меняется, политика и итог — нет", async () => {
    const results: { stream: HistoryOwner; nav: HistoryOwner; policy: string; text: string; baseY: number }[] = [];
    for (const own of [199, 200]) {
      const term = new HeadlessTerminal({ cols: 80, rows: 5, scrollback: 1000, allowProposedApi: true });
      await writeHeadless(term, Array.from({ length: own + 5 }, (_, i) => `old-${i}`).join("\r\n"));
      expect(term.buffer.normal.baseY).toBe(own);
      const small = `\x1b[2J${CSI3J}\x1b[Hsmall-repaint\r\nline-2`;
      expect(small.length).toBeLessThan(STREAM_SAMPLE_MIN_BYTES);
      const stream = historyOwnerFromStream(2, small.length);
      const nav = resolveHistoryOwner({ stream, ownScrollback: term.buffer.normal.baseY, agent: true, alt: false });
      const noisy = { generation: "claude:7", agentInFg: true, declared: "" as const, ownScrollback: own, stream: nav };
      const policy = retentionFor(noisy);
      for (const item of splitScrollbackErase(enc(small)).items) {
        if (item.kind === "data") await writeHeadless(term, item.data);
        else if (eraseActionFor(policy, false) === "write") await writeHeadless(term, CSI3J);
      }
      const buffer = term.buffer.normal;
      results.push({ stream, nav, policy: JSON.stringify(policy), text: bufferText(buffer, 0, buffer.length - 1), baseY: buffer.baseY });
      term.dispose();
    }
    expect(results[0].stream).toBe("unknown");
    // Положительный контроль: навигационная страховка по глубине действительно разная.
    expect(results.map(r => r.nav)).toEqual(["application", "terminal"]);
    expect(results[0].policy).toBe(results[1].policy);
    expect(results[0].text).toBe(results[1].text);
    expect(results.map(r => r.baseY)).toEqual([0, 0]);
  });
});

describe("splitScrollbackErase — цепочка для записи с решением по ходу разбора", () => {
  it("делит поток на сегменты, сохраняя места стираний", () => {
    // Кадр Kimi: ESC[2J ESC[3J ESC[H + перепечатка. Стирание стоит на своём
    // месте — между «2J» и «H», порядок байт не меняется.
    expect(feedSplit(["a\x1b[2J" + CSI3J + "\x1b[Hb"])).toBe("a\x1b[2J<3J>\x1b[Hb");
  });

  it("несколько стираний за кадр — каждое отдельным элементом", () => {
    expect(feedSplit(["до" + CSI3J + "середина" + CSI3J + "после"])).toBe("до<3J>середина<3J>после");
  });

  it("подряд идущие стирания не теряются и без данных между ними", () => {
    expect(feedSplit(["a" + CSI3J + CSI3J + "b"])).toBe("a<3J><3J>b");
  });

  it("ловит вариант с приватным префиксом (ESC[?3J)", () => {
    expect(feedSplit(["a\x1b[?3Jb"])).toBe("a<3J>b");
  });

  it("ловит последовательность, разорванную между кадрами", () => {
    expect(feedSplit(["хвост\x1b[", "3Jначало"])).toBe("хвост<3J>начало");
    expect(feedSplit(["x\x1b", "[3Jy"])).toBe("x<3J>y");
    expect(feedSplit(["x\x1b[3", "Jy"])).toBe("x<3J>y");
  });

  it("не трогает другие последовательности и не дробит данные зря", () => {
    // Без стираний весь кадр — один сегмент данных.
    const res = splitScrollbackErase(enc("a\x1b[2Jb\x1b[31mкрасный\x1b[0m"));
    expect(res.items).toHaveLength(1);
    expect(res.items[0].kind).toBe("data");
    expect(feedSplit(["\x1b[13J"])).toBe("\x1b[13J");
    expect(feedSplit(["\x1b[3K"])).toBe("\x1b[3K");
  });

  it("держит незавершённый хвост до следующего кадра", () => {
    const res = splitScrollbackErase(enc("текст\x1b["));
    expect(res.items).toHaveLength(1);
    expect(dec((res.items[0] as { kind: "data"; data: Uint8Array }).data)).toBe("текст");
    expect(res.tail.byteLength).toBe(2); // ESC[ ждут продолжения
  });

  it("стирание в самом начале кадра — первым элементом", () => {
    // Для вызывающего это сигнал поставить барьер: решение о стирании должно
    // дождаться своей очереди в буфере записи xterm.
    const res = splitScrollbackErase(enc(CSI3J + "данные"));
    expect(res.items[0].kind).toBe("erase");
    expect(feedSplit([CSI3J + "данные"])).toBe("<3J>данные");
  });

  it("пустой ввод не ломает разбор", () => {
    const res = splitScrollbackErase(new Uint8Array(0));
    expect(res.items).toHaveLength(0);
    expect(res.tail.byteLength).toBe(0);
  });

  it("кадр без стираний (Claude в alt-screen) проходит одним сегментом без изменений", () => {
    // Регрессионный: alt-screen сессии не должны ничего заметить — данные
    // уходят как были, лишних элементов не появляется.
    const src = "строка вывода\x1b[Hещё\x1b[2Kстрока";
    const items: EraseChainItem[] = splitScrollbackErase(enc(src)).items;
    expect(items).toHaveLength(1);
    expect(items[0].kind).toBe("data");
    expect(dec((items[0] as { kind: "data"; data: Uint8Array }).data)).toBe(src);
  });
});

describe("eraseChainEnds — позиция потока после каждого элемента цепочки (ST-09)", () => {
  it("данные двигают позицию на свой срез, стирание — на саму последовательность, оба написания", () => {
    const input = enc(`ab${CSI3J}cde\x1b[?3Jf\x1b[`);
    const { items, tail } = splitScrollbackErase(input);
    expect(items.map((i) => i.kind)).toEqual(["data", "erase", "data", "erase", "data"]);
    expect(eraseChainEnds(input, items)).toEqual([2, 6, 9, 14, 15]);
    // Последний конец — ровно граница удержанного хвоста (appliedEnd склейки).
    expect(input.byteLength - tail.byteLength).toBe(15);
  });

  it("стирание первым элементом, подряд идущие стирания и многобайтный текст", () => {
    const input = enc(`${CSI3J}${CSI3J}данные`);
    const { items, tail } = splitScrollbackErase(input);
    expect(eraseChainEnds(input, items)).toEqual([4, 8, input.byteLength]);
    expect(tail.byteLength).toBe(0);
    expect(eraseChainEnds(new Uint8Array(0), [])).toEqual([]);
  });
});
