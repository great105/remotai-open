/**
 * КОМАНДЫ КАК БЛОКИ ПО МАРКЕРАМ OSC 133 (ST-10, T-39). Клиентская модель.
 *
 * Шелл с интеграцией (FinalTerm / VS Code / iTerm2 — один и тот же протокол)
 * размечает поток четырьмя маркерами:
 *   A — начало приглашения;  B — конец приглашения, дальше человек печатает;
 *   C — команда запущена, дальше её вывод;  D[;код] — команда завершилась.
 * По ним можно «скопировать последнюю команду», «скопировать её вывод» и
 * «перейти к началу предыдущей команды». Сама интеграция на стороне шелла —
 * серверный этап (карта ST-10, A1); здесь только разбор и модель.
 *
 * ⚠ СТРАЖ T-39. Маркеры дают ГРАНИЦЫ команд и больше ничего. Они не доказывают
 * ни наличия внутренней прокрутки TUI, ни того, что на экране «Агент»: долгая
 * сборка или `sleep 60` между C и D — это обычная команда, которая ещё идёт.
 * Поэтому модуль НИЧЕГО не отдаёт в маршрутизацию прокрутки (historyOwner,
 * shouldDriveViewport, ScrollRouter, navigation*, altScroll) и сам ничего оттуда
 * не импортирует. Режим «Агент» определяется деревом процессов на сервере
 * (`state.agent_kind`). Тест-страж в commandBlocks.test.ts проверяет и импорты,
 * и имена экспорта.
 *
 * ⚠ I-15. Текст команды и вывода — содержимое. Модель хранит только позиции
 * (строка, колонка), текст не хранит и в диагностику ничего не пишет;
 * копирование — только по явному действию человека.
 *
 * ⚠ I-11. Блоков не больше `limit` (по умолчанию 200); маркеры, утилизированные
 * обрезкой scrollback, дают `truncated`, а не выдуманный диапазон. После
 * смены эпохи писателя или кадра восстановления блоки до этой точки
 * недоступны — `integration() === "lost"`, интерфейс обязан так и сказать.
 *
 * I-03: адаптер вешается на парсер xterm и новых путей записи не создаёт —
 * обработчик OSC зовётся внутри разбора байтов, пришедших через TerminalWriter.
 */

export type Osc133Kind = "A" | "B" | "C" | "D";
export interface Osc133Mark { kind: Osc133Kind; exit?: number }

/** Длиннее — не маркер, а мусор или попытка раздуть память (I-11). */
export const OSC133_MAX_PAYLOAD = 256;

/**
 * Разобрать полезную нагрузку OSC 133 (то, что xterm отдаёт обработчику после
 * `133;`). Известны только A, B, C, D. Параметры после `;` вида `aid=…`,
 * `cl=m`, `k=i` пропускаются безопасно; у D первый параметр — код выхода,
 * если это целое число.
 */
export function parseOsc133(data: string): Osc133Mark | null {
  if (typeof data !== "string" || data.length === 0 || data.length > OSC133_MAX_PAYLOAD) return null;
  const parts = data.split(";");
  const key = parts[0];
  if (key !== "A" && key !== "B" && key !== "C" && key !== "D") return null;
  if (key !== "D") return { kind: key };
  const code = parts[1];
  if (code !== undefined && /^-?\d{1,10}$/.test(code)) {
    const exit = Number(code);
    if (Number.isSafeInteger(exit)) return { kind: "D", exit };
  }
  return { kind: "D" };
}

/**
 * Позиция маркера в буфере. `line` ЖИВАЯ: при обрезке scrollback xterm
 * сдвигает строку маркера, а ушедший за край маркер утилизирует (line = -1).
 * Поэтому модель читает `line` в момент вопроса, а не хранит число.
 */
export interface BlockPoint {
  readonly line: number;
  readonly col: number;
  disposed(): boolean;
  /** Отпустить маркер, когда блок вытеснен или сброшен (I-11). */
  release?(): void;
}

/** Точка из маркера xterm (IMarker) — без импорта xterm, по форме. */
export function pointFromMarker(
  marker: { readonly line: number; readonly isDisposed: boolean; dispose?(): void },
  col: number,
): BlockPoint {
  return {
    get line() { return marker.line; },
    col,
    disposed: () => marker.isDisposed || marker.line < 0,
    release: () => { try { marker.dispose?.(); } catch { /* уже утилизирован */ } },
  };
}

export type BlockState = "prompt" | "input" | "running" | "done" | "interrupted";
export type BlockResetReason = "epoch" | "snapshot";
export type BlockIntegration = "none" | "active" | "lost";

export interface CommandBlock {
  readonly id: number;
  readonly epoch: string | number;
  readonly state: BlockState;
  /** Код выхода из D, если шелл его прислал. */
  readonly exit?: number;
  /** A — начало приглашения. */
  readonly prompt: BlockPoint;
  /** B — начало команды (конец приглашения). */
  readonly input?: BlockPoint;
  /** C — начало вывода. */
  readonly output?: BlockPoint;
  /** Конец вывода: D или, если D не было, A следующего приглашения. */
  readonly end?: BlockPoint;
  /** Конец задан маркером D (а не следующим A). */
  readonly endedByMark: boolean;
  /**
   * Пока команда шла, экран брала полноэкранная программа (alt-screen: less,
   * vim, git log с пейджером, htop, man). Её рисунок в блок не входит — там
   * только то, что команда напечатала в обычном буфере до входа и после выхода.
   */
  readonly fullscreen?: boolean;
}

/** Диапазон ячеек: начало включительно, конец (endLine, endCol) — исключая. */
export interface CellRange {
  startLine: number;
  startCol: number;
  endLine: number;
  endCol: number;
  /** Начало диапазона ушло за край scrollback: текст неполный. */
  truncated: boolean;
}

export interface BlockRanges {
  command: CellRange | null;
  output: CellRange | null;
  /** Хоть один нужный маркер утилизирован — диапазон неполный или пропал. */
  truncated: boolean;
}

type MutableBlock = { -readonly [K in keyof CommandBlock]: CommandBlock[K] };

/** SIGINT: 128 + 2. Ctrl+C во время команды или на пустом приглашении. */
const EXIT_INTERRUPTED = 130;

export class CommandBlockModel {
  private blocks: MutableBlock[] = [];
  private seen = false;
  private lost: BlockResetReason | null = null;
  private epochKey: string | number | undefined;
  private nextId = 1;
  private lastResetReason: BlockResetReason | null = null;
  /** Блоки до восстановления пропали, а новый ещё не завершился (I-11). */
  private gapSinceRestore = false;

  constructor(readonly limit = 200) {}

  /**
   * Принять маркер в позиции `at` при эпохе писателя `epoch`.
   *
   * Переходы: A → prompt, B → input, C → running, D → done (D;130 —
   * interrupted). D без C: вывод начинается со строки после B (так работает
   * PowerShell, у которого нет PS0). A без D закрывает открытый блок как
   * interrupted; брошенное приглашение без B (перерисовка prompt) просто
   * заменяется. Маркер не к месту игнорируется. Возвращает, принят ли он.
   */
  accept(mark: Osc133Mark | null, at: BlockPoint, epoch: string | number): boolean {
    if (!mark) return this.ignore(at);
    if (this.epochKey !== undefined && epoch !== this.epochKey) this.reset("epoch");
    this.epochKey = epoch;
    const open = this.open();
    switch (mark.kind) {
      case "A": {
        if (open) {
          if (open.state === "prompt") this.remove(open);
          else { open.state = "interrupted"; open.end = at; open.endedByMark = false; }
        }
        this.seen = true;
        this.lost = null;
        this.blocks.push({ id: this.nextId++, epoch, state: "prompt", prompt: at, endedByMark: false });
        while (this.blocks.length > Math.max(1, this.limit)) this.release(this.blocks.shift()!);
        return true;
      }
      case "B":
        if (!open || open.state !== "prompt") return this.ignore(at);
        open.input = at;
        open.state = "input";
        return true;
      case "C":
        if (!open || (open.state !== "input" && open.state !== "prompt")) return this.ignore(at);
        open.output = at;
        open.state = "running";
        return true;
      case "D":
        if (!open || open.state === "prompt") return this.ignore(at);
        open.end = at;
        open.endedByMark = true;
        open.exit = mark.exit;
        open.state = mark.exit === EXIT_INTERRUPTED ? "interrupted" : "done";
        this.gapSinceRestore = false;
        return true;
    }
  }

  /**
   * Сбросить модель: содержимое буфера заменено (новая эпоха писателя, кадр
   * восстановления, собранный из ячеек, а не из байтов, — маркеры до него
   * пропали на сервере, screen.go:526-565). Блоки до этой точки недоступны:
   * integration() = "lost" до следующего приглашения (I-11).
   */
  reset(reason: BlockResetReason): void {
    this.lastResetReason = reason;
    for (const block of this.blocks) this.release(block);
    this.blocks = [];
    if (this.seen) { this.lost = reason; this.gapSinceRestore = true; }
  }

  /**
   * Экран взяла полноэкранная программа (alt-screen). Это НЕ сброс: нормальный
   * буфер и его маркеры alt-screen переживают, и открытый блок остаётся верным —
   * команда B..C лежит в нормальном буфере, а C..D ровно то, что команда
   * напечатала вне полноэкранной программы (до входа и после выхода). D после
   * выхода закрывает блок как обычно.
   *
   * ⚠ Прежде открытый блок здесь выбрасывался, D после выхода падал мимо, и
   * после less, vim, git log с пейджером «Копировать команду/вывод» молча
   * отдавали ПРЕДЫДУЩУЮ команду (находка скептика ST-10). Теперь блок только
   * помечается fullscreen — интерфейс честно говорит, что рисунок программы в
   * вывод не входит. Помечается и «input»: у PowerShell нет C, и команда идёт
   * в этом состоянии до своего D.
   */
  noteAltScreen(): void {
    const open = this.open();
    if (open && (open.state === "running" || open.state === "input")) open.fullscreen = true;
  }

  /**
   * После сброса (новая эпоха, кадр, RIS) ещё ни одна команда не завершилась:
   * прежние блоки пропали, новых нет. Интерфейс говорит «блоки до
   * восстановления недоступны», а не молча прячет действия (I-11). Следующее
   * приглашение (A) делает integration() снова "active", но честная пометка
   * держится до первого D.
   */
  restoredGap(): boolean {
    return this.gapSinceRestore;
  }

  integration(): BlockIntegration {
    if (this.lost) return "lost";
    return this.seen ? "active" : "none";
  }

  /** Причина последнего сброса — для честной подписи «блоки до … недоступны». */
  resetReason(): BlockResetReason | null {
    return this.lastResetReason;
  }

  /** Последняя команда, для которой пришёл D (завершена или прервана по D;130). */
  lastComplete(): CommandBlock | null {
    for (let i = this.blocks.length - 1; i >= 0; i--) {
      if (this.blocks[i].endedByMark) return this.blocks[i];
    }
    return null;
  }

  /** Все блоки, старые первыми (только чтение). */
  list(): readonly CommandBlock[] {
    return this.blocks;
  }

  /**
   * Строка начала ближайшей команды ВЫШЕ `viewportLine` (верх видимого окна в
   * строках буфера). Утилизированные маркеры пропускаются. `null` — выше
   * ничего нет. Прокрутку делает не модель, а единственный исполнитель
   * ScrollRouter (ST-03), если маршрут разрешает локальную прокрутку.
   */
  previousStart(viewportLine: number): number | null {
    for (let i = this.blocks.length - 1; i >= 0; i--) {
      const p = this.blocks[i].prompt;
      if (p.disposed()) continue;
      const line = p.line;
      if (line >= 0 && line < viewportLine) return line;
    }
    return null;
  }

  /**
   * Диапазоны команды и вывода блока в ячейках буфера.
   *
   * Команда: от B до C. D без C — команда до конца строки B, вывод со строки
   * после B. Вывод: от C до конца (D или следующего A). Ещё идущая команда
   * вывода не имеет (конец неизвестен). Начало за краем scrollback — диапазон
   * от строки 0 с truncated; конец за краем — диапазона нет, truncated.
   */
  ranges(block: CommandBlock): BlockRanges {
    let truncated = false;
    const range = (start: BlockPoint | { line: number; col: number; derived: true } | undefined,
      end: BlockPoint | { line: number; col: number; derived: true } | undefined): CellRange | null => {
      if (!start || !end) return null;
      const endGone = "derived" in end ? end.line < 0 : end.disposed();
      if (endGone) { truncated = true; return null; }
      const startGone = "derived" in start ? start.line < 0 : start.disposed();
      if (startGone) truncated = true;
      const startLine = startGone ? 0 : start.line, startCol = startGone ? 0 : start.col;
      return { startLine, startCol, endLine: end.line, endCol: end.col, truncated: startGone };
    };
    const input = block.input;
    // Строка после B — для D без C. Если сам B утилизирован, эта строка тоже
    // могла уйти: считаем её недоступной, и range пометит начало обрезанным.
    const afterInput = input && !block.output
      ? { line: input.disposed() ? -1 : input.line + 1, col: 0, derived: true as const }
      : undefined;
    const commandEnd = block.output ?? (block.endedByMark ? afterInput : undefined);
    const outputStart = block.output ?? (block.endedByMark ? afterInput : undefined);
    const command = range(input, commandEnd);
    const output = range(outputStart, block.end);
    return { command, output, truncated };
  }

  private open(): MutableBlock | undefined {
    const last = this.blocks[this.blocks.length - 1];
    return last && (last.state === "prompt" || last.state === "input" || last.state === "running") ? last : undefined;
  }

  private ignore(at: BlockPoint): false {
    at.release?.();
    return false;
  }

  private remove(block: MutableBlock): void {
    const i = this.blocks.indexOf(block);
    if (i >= 0) this.blocks.splice(i, 1);
    this.release(block);
  }

  /** Отпустить маркеры блока. `end` чужой, если это A следующего блока. */
  private release(block: MutableBlock): void {
    block.prompt.release?.();
    block.input?.release?.();
    block.output?.release?.();
    if (block.endedByMark) block.end?.release?.();
  }
}

/**
 * Какие действия показывать (T-39c). Без интеграции, после её потери и при
 * выключенном флаге — ничего: «Копировать экран» остаётся прежним путём.
 */
export function availableBlockActions(model: CommandBlockModel, enabled: boolean): {
  copyCommand: boolean; copyOutput: boolean; jumpPrevious: boolean;
} {
  const none = { copyCommand: false, copyOutput: false, jumpPrevious: false };
  if (!enabled || model.integration() !== "active") return none;
  const last = model.lastComplete();
  const r = last ? model.ranges(last) : null;
  return {
    copyCommand: !!r?.command,
    copyOutput: !!r?.output,
    jumpPrevious: model.list().some(b => !b.prompt.disposed()),
  };
}

/**
 * Пометка в ряду действий (I-11): "restored" — блоки до восстановления экрана
 * пропали (сброс эпохи, кадр, RIS), а новая команда ещё не завершилась.
 * Без интеграции и при выключенном флаге — ничего.
 */
export function blockNotice(model: CommandBlockModel, enabled: boolean): "none" | "restored" {
  return enabled && model.restoredGap() ? "restored" : "none";
}

/**
 * Что сказать после «Копировать команду/вывод» (I-11: неполнота видна).
 *   copied — обычный случай;  truncated — начало вытеснено из scrollback;
 *   fullscreen — вывод скопирован, но рисунок полноэкранной программы в него не
 *     входит (он жил в alt-буфере);
 *   fullscreen-empty — команда печатала только в полноэкранной программе:
 *     копировать нечего, и это не «ошибка», а честный ответ;
 *   nothing — пустой текст без особой причины.
 * Обрезка важнее пометки полноэкранной программы: она про потерю начала.
 */
export type BlockCopyOutcome = "copied" | "truncated" | "fullscreen" | "fullscreen-empty" | "nothing";
export function blockCopyOutcome(block: CommandBlock | null, kind: "command" | "output",
  range: CellRange | null, text: string): BlockCopyOutcome {
  const fullscreen = kind === "output" && !!block?.fullscreen;
  if (!text) return fullscreen && range ? "fullscreen-empty" : "nothing";
  if (range?.truncated) return "truncated";
  return fullscreen ? "fullscreen" : "copied";
}

/** Строка буфера по форме (xterm 6 и headless 6) — ровно то, что нужно тексту блока. */
export interface BlockTextLine {
  readonly isWrapped: boolean;
  readonly length: number;
  translateToString(trimRight?: boolean, startColumn?: number, endColumn?: number): string;
  getCell(x: number): { getChars(): string; getWidth(): number } | undefined;
}
export interface BlockTextBuffer { getLine(y: number): BlockTextLine | undefined }

/**
 * Текст диапазона ячеек — как его скопировал бы человек (T-15, I-07).
 *
 * Мягкий перенос склеивается без перевода строки, жёсткий даёт `\n`. Колонки —
 * ячейки буфера, поэтому широкие символы и эмодзи режутся ровно по своим
 * границам. У строки, продолженной мягким переносом, справа снимаются только
 * НЕЗАПИСАННЫЕ ячейки (пустой getChars и ширина 1 — там xterm оставляет место,
 * когда широкий символ не влез): набранные пробелы у переноса значимы. Это
 * то же правило, что у logicalRowText (чтение, «Копировать экран»). Конец
 * в колонке 0 следующей строки — перевод строки перед ней в блок не входит
 * (вывод «…\r\n», затем D в начале строки).
 *
 * Звать на барьере писателя (flushTermWrites), иначе конец блока может быть
 * ещё не разобран. Текст — содержимое: только в буфер обмена по явному
 * действию, никогда в диагностику (I-15).
 */
export function blockText(buffer: BlockTextBuffer, r: CellRange, cols: number): string {
  let out = "";
  for (let l = Math.max(0, r.startLine); l <= r.endLine; l++) {
    const line = buffer.getLine(l);
    if (!line) break;
    const start = l === r.startLine ? Math.max(0, r.startCol) : 0;
    let end = l === r.endLine ? r.endCol : cols;
    if (l === r.endLine && end <= 0 && l > r.startLine) break;
    if (l > r.startLine && !line.isWrapped) out += "\n";
    const continues = l < r.endLine && !!buffer.getLine(l + 1)?.isWrapped;
    if (!continues) { out += line.translateToString(true, start, end); continue; }
    end = Math.max(start, Math.min(end, line.length));
    while (end > start) {
      const cell = line.getCell(end - 1);
      if (!cell || cell.getChars() !== "" || cell.getWidth() !== 1) break;
      end--;
    }
    out += line.translateToString(false, start, end);
  }
  return out;
}

/** Терминал по форме — ровно то, что нужно адаптеру (xterm 6 и headless 6). */
export interface OscTerminal {
  parser: { registerOscHandler(ident: number, callback: (data: string) => boolean): { dispose(): void } };
  registerMarker(cursorYOffset?: number): { readonly line: number; readonly isDisposed: boolean; dispose(): void } | undefined;
  buffer: { active: { readonly type: string; readonly cursorX: number } };
}

/**
 * Подключить модель к парсеру xterm. Обработчик зовётся синхронно внутри
 * разбора, поэтому курсор и маркер стоят ровно там, где в потоке был маркер,
 * как бы ни были разрезаны чанки (T-27). В alt-буфере маркеры не ставятся:
 * там рисует полноэкранная программа, и нормального буфера это не касается.
 * Мусорная нагрузка маркера не создаёт.
 */
export function attachCommandBlocks(term: OscTerminal, model: CommandBlockModel,
  epoch: () => string | number): { dispose(): void } {
  return term.parser.registerOscHandler(133, data => {
    if (term.buffer.active.type !== "normal") return true;
    const mark = parseOsc133(data);
    if (!mark) return true;
    const marker = term.registerMarker(0);
    if (!marker) return true;
    model.accept(mark, pointFromMarker(marker, term.buffer.active.cursorX), epoch());
    return true;
  });
}
