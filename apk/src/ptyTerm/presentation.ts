/**
 * ST-06 (план 13.09.2026): presentation transaction — чистые правила показа
 * снимка Remotai через DEC 2026 (synchronized output) на xterm 6.0.0.
 *
 * Зачем. Путь replace=true писал RIS, историю, переводы строк и кадр
 * ЧЕТЫРЬМЯ отдельными write. xterm уступает управление только между чанками
 * (WriteBuffer, после 12 мс), и в rAF мог отрисоваться промежуточный кадр:
 * пустой экран после RIS или история без кадра (карта ST-06, P2; I-06:
 * «разобрано» ≠ «показано»). Здесь два чанка вместо четырёх, а на WebGL они
 * обёрнуты в 2026.
 *
 * Правила выведены из исходника 6.0.0 и зафиксированы тестами T-29a/T-29b:
 * 1. BEGIN строго ПОСЛЕ RIS в том же чанке. RIS сбрасывает режимы
 *    (CoreService.reset → decPrivateModes = DEFAULT), BEGIN до него пропал бы.
 * 2. Переводы строк и кадр идут одним write, END в конце того же чанка.
 *    Если истории нет, всё идёт одним чанком, и 2026 не нужен вовсе.
 * 3. 2026 только на WebGL. На DOM RIS и 1049 СИНХРОННО чистят строки
 *    (BufferSet → RenderService.clear → DomRenderer), а отрисовка отложена:
 *    2026 превратил бы промежуточный кадр в пустой. На DOM остаётся склейка.
 * 4. Режим приложения не закрываем. Транзакция приложения открыта, а нашего
 *    RIS нет — ни BEGIN, ни END не пишем: закроет l приложения или сторож
 *    xterm (1000 мс). Режим булев, а не счётчик: наш END оборвал бы кадр
 *    приложения посередине.
 * 5. ⚠ НИКАКИХ пар 2026 вокруг сетевых пакетов или rAF-пачек. Сетевой пакет
 *    не логический кадр приложения (ST-06, обязательные ограничения). План
 *    применяется только к границам, которые создаёт сам Remotai: снимок и
 *    маркер. Поток приложения идёт байт в байт (mayOpenPresentation).
 *
 * Модуль не знает ни xterm, ни DOM: он отдаёт байты и решения, пишет их
 * TerminalWriter (I-03).
 */

import type { SnapshotStep } from "./snapshotApply";

export const SYNC_BEGIN = "\x1b[?2026h";
export const SYNC_END = "\x1b[?2026l";
/** RIS: полный сброс терминала. Закрывает и любую открытую транзакцию 2026. */
export const RIS = "\x1bc";
/**
 * Наш сторож: сколько держим свою транзакцию. Совпадает с жёсткой константой
 * xterm SYNCHRONIZED_OUTPUT_TIMEOUT_MS=1000 (RenderService.ts:22). Но у xterm
 * сторож взводится на первой отложенной перерисовке, а у нас на open.
 */
export const PRESENTATION_WATCHDOG_MS = 1000;

export type PresentationRenderer = "webgl" | "dom";
export type PresentationChunk = string | Uint8Array;

/**
 * Откуда запись. Открывать транзакцию разрешено только на границах, которые
 * создаёт Remotai. Поток приложения, rAF-пачки и сегменты вокруг ESC[3J
 * (writeEraseChain) идут как есть: их 2026, если он есть, принадлежит
 * приложению.
 */
export type PresentationWriteKind = "stream" | "raf-batch" | "erase-segment" | "snapshot" | "marker";

export function mayOpenPresentation(kind: PresentationWriteKind): boolean {
  return kind === "snapshot" || kind === "marker";
}

function isEmpty(chunk: PresentationChunk | undefined): boolean {
  return chunk === undefined || chunk.length === 0;
}

/** Склейка частей. Все части строки — строка; есть байты — UTF-8 байты, без перекодировки байтовых частей. */
export function joinChunks(parts: ReadonlyArray<PresentationChunk | undefined>): PresentationChunk {
  const present = parts.filter((p): p is PresentationChunk => !isEmpty(p));
  if (present.every((p) => typeof p === "string")) return (present as string[]).join("");
  const encoder = new TextEncoder();
  const bytes = present.map((p) => (typeof p === "string" ? encoder.encode(p) : p));
  const out = new Uint8Array(bytes.reduce((n, b) => n + b.length, 0));
  let at = 0;
  for (const b of bytes) {
    out.set(b, at);
    at += b.length;
  }
  return out;
}

/**
 * Шаги плана снимка (snapshotApply.ts), разложенные на две записи показа:
 * голова (RIS + история) и хвост (досылка строк + кадр). Байты шагов не
 * меняются — меняется только разбиение на write() (доказательство —
 * snapshotPresentation.test.ts). null — план не одной из четырёх форм
 * planSnapshotApply ([кадр], [RIS, кадр], [RIS, история, кадр],
 * [RIS, история, досылка, кадр]): исполнитель идёт прежними шагами.
 */
export interface SnapshotStepParts {
  /** В плане есть RIS (замена истории). */
  readonly ris: boolean;
  /** Байты истории; null — истории нет. */
  readonly history: Uint8Array | null;
  /** Шаг досылки строк: мерится по живому буферу ПОСЛЕ разбора головы. */
  readonly filler: Extract<SnapshotStep, { kind: "filler" }> | null;
}

export function snapshotStepParts(steps: readonly SnapshotStep[]): SnapshotStepParts | null {
  const kinds = steps.map((s) => s.kind).join(",");
  if (kinds !== "frame" && kinds !== "ris,frame" && kinds !== "ris,history,frame" && kinds !== "ris,history,filler,frame") return null;
  let history: Uint8Array | null = null;
  let filler: Extract<SnapshotStep, { kind: "filler" }> | null = null;
  for (const step of steps) {
    if (step.kind === "history") history = step.bytes;
    else if (step.kind === "filler") filler = step;
  }
  return { ris: steps[0].kind === "ris", history, filler };
}

export interface SnapshotHeadInput {
  renderer: PresentationRenderer;
  /** Байты RIS пути replace. Нет — кадр ложится поверх текущего экрана. */
  ris?: PresentationChunk;
  /** История (scrollback) от теневого буфера сервера. */
  history?: PresentationChunk;
  /** term.modes.synchronizedOutputMode на барьере снимка: транзакция приложения открыта. */
  appSyncOpen: boolean;
}

export interface SnapshotHead {
  /** Первый чанк (RIS + BEGIN + история); null — всё уйдёт одним чанком хвоста. */
  readonly chunk: PresentationChunk | null;
  /** Мы открыли 2026: хвост обязан его закрыть. */
  readonly opened: boolean;
  /** Что перенесено в хвост (RIS, когда истории нет). */
  readonly carry: PresentationChunk | undefined;
}

/**
 * Первый чанк снимка. Разделение на голову и хвост нужно адаптеру: число
 * переводов строк (filler) меряется по живому буферу ПОСЛЕ разбора истории
 * (PtyTermView, ветка replace), и хвост собирается уже после этого.
 */
export function planSnapshotHead(input: SnapshotHeadInput): SnapshotHead {
  if (isEmpty(input.history)) {
    return { chunk: null, opened: false, carry: isEmpty(input.ris) ? undefined : input.ris };
  }
  // Без нашего RIS открытая транзакция приложения переживёт наш кадр и
  // удержит показ сама. Свой BEGIN тут лишний, свой END был бы вреден.
  const sync = input.renderer === "webgl" && !(input.appSyncOpen && isEmpty(input.ris));
  return {
    chunk: joinChunks([input.ris, sync ? SYNC_BEGIN : undefined, input.history]),
    opened: sync,
    carry: undefined,
  };
}

export interface SnapshotTailInput {
  /** Переводы строк, досыпающие историю до низа экрана. */
  filler?: PresentationChunk;
  /** Кадр экрана (самодостаточный: alt-screen, очистка, строки, курсор). */
  frame: PresentationChunk;
  /**
   * Закрывающие байты. По умолчанию SYNC_END, если голова открыла 2026.
   * Адаптер с редьюсером передаёт сюда closeTxn(...).bytes: один источник END.
   */
  end?: PresentationChunk;
}

export function planSnapshotTail(head: SnapshotHead, input: SnapshotTailInput): PresentationChunk {
  const end = input.end !== undefined ? input.end : (head.opened ? SYNC_END : undefined);
  return joinChunks([head.carry, input.filler, input.frame, end]);
}

export interface SnapshotChunkInput extends SnapshotHeadInput, SnapshotTailInput {}

export interface SnapshotChunkPlan {
  readonly chunks: PresentationChunk[];
  readonly opened: boolean;
}

/** Весь снимок одним планом: один или два чанка вместо четырёх. */
export function planSnapshotChunks(input: SnapshotChunkInput): SnapshotChunkPlan {
  const head = planSnapshotHead(input);
  const tail = planSnapshotTail(head, { filler: input.filler, frame: input.frame });
  return { chunks: head.chunk === null ? [tail] : [head.chunk, tail], opened: head.opened };
}

/**
 * Префикс маркера reset. На WebGL RIS + BEGIN удерживает старую картинку,
 * пока разбирается реплей кольца: холст WebGL при clear() хранит последний
 * кадр до следующей отрисовки. Это смягчение, а не исправление: кадр,
 * опоздавший дольше 1000 мс, покажет реплей по сторожу xterm. Настоящее
 * исправление — «кадр сразу за reset» в координаторе ST-05.
 */
export function planMarkerPrefix(input: { renderer: PresentationRenderer; isReset: boolean }): string {
  if (!input.isReset) return "";
  return input.renderer === "webgl" ? RIS + SYNC_BEGIN : RIS;
}

/** Открыл ли префикс маркера нашу транзакцию (для openTxn). */
export function markerPrefixOpens(input: { renderer: PresentationRenderer; isReset: boolean }): boolean {
  return input.isReset && input.renderer === "webgl";
}

export type PresentationOwner = "marker" | "snapshot";

export type PresentationTxn =
  | { readonly kind: "closed" }
  | { readonly kind: "open"; readonly owner: PresentationOwner; readonly token: number; readonly deadline: number };

export const TXN_CLOSED: PresentationTxn = { kind: "closed" };

/**
 * Открыть нашу транзакцию. Только из closed: режим 2026 булев, вложенного
 * счётчика нет, и второй open означал бы, что первый END закроет чужое.
 * Перед RIS адаптер зовёт resetTxn: RIS сам закрывает любую транзакцию.
 */
export function openTxn(
  s: PresentationTxn,
  input: { owner: PresentationOwner; token: number; now: number },
): { state: PresentationTxn; ok: boolean } {
  if (s.kind === "open") return { state: s, ok: false };
  return {
    state: { kind: "open", owner: input.owner, token: input.token, deadline: input.now + PRESENTATION_WATCHDOG_MS },
    ok: true,
  };
}

function finish(s: PresentationTxn, token: number, modeStillOn: boolean): { state: PresentationTxn; bytes: string } {
  if (s.kind !== "open" || s.token !== token) return { state: s, bytes: "" };
  // END только если открывали мы (токен наш) и режим ещё стоит: RIS или
  // DECSTR в потоке могли его уже снять, тогда лишний l ничего не даёт.
  return { state: TXN_CLOSED, bytes: modeStillOn ? SYNC_END : "" };
}

/** Штатное завершение (кадр дописан). Идемпотентно: повтор и чужой токен дают "". */
export function closeTxn(s: PresentationTxn, token: number, modeStillOn = true): { state: PresentationTxn; bytes: string } {
  return finish(s, token, modeStillOn);
}

/** Отказ от кадра (устарел, барьер сорван, новый маркер). Идемпотентно, END только за себя. */
export function abandonTxn(s: PresentationTxn, token: number, modeStillOn = true): { state: PresentationTxn; bytes: string } {
  return finish(s, token, modeStillOn);
}

/** В потоке пишется RIS: он сам закрывает транзакцию, END не нужен. */
export function resetTxn(_s: PresentationTxn): PresentationTxn {
  return TXN_CLOSED;
}

/**
 * Сторож по инжектируемым часам: отсутствующий конец не даёт бесконечной
 * заморозки. Основная граница — сторож xterm (1000 мс). Наш нужен, чтобы
 * снять флаг «удерживаем старую картинку» (гашение кликов по старым
 * координатам) и не держать своё состояние открытым вечно.
 */
export function txnWatchdog(
  s: PresentationTxn,
  now: number,
  modeStillOn = true,
): { state: PresentationTxn; bytes: string; expired: boolean } {
  if (s.kind !== "open" || now < s.deadline) return { state: s, bytes: "", expired: false };
  return { state: TXN_CLOSED, bytes: modeStillOn ? SYNC_END : "", expired: true };
}

export function txnDeadline(s: PresentationTxn): number | null {
  return s.kind === "open" ? s.deadline : null;
}

/**
 * Открыта ли транзакция ПРИЛОЖЕНИЯ. Режим 2026 булев: после маркера reset на
 * WebGL он стоит из-за НАШЕГО префикса RIS+BEGIN. Взять голый
 * term.modes.synchronizedOutputMode — снимок без RIS принял бы транзакцию
 * маркера за транзакцию приложения, не написал бы END, и показ кадра ждал бы
 * сторожа (xterm 1000 мс). Открытая транзакция в редьюсере — наша.
 */
export function appSyncOpenFor(modeOn: boolean, txn: PresentationTxn): boolean {
  return modeOn && txn.kind !== "open";
}

/**
 * Режим снят не нами (ревью ST-06, 14.09). Наша транзакция открыта, её
 * открывающие байты xterm уже разобрал (parsedToken — токен, чей BEGIN
 * разобран), а режима больше нет. Его сняли `?2026l` приложения (Kimi рисует
 * кадры в 2026, и реплей кольца за маркером несёт его собственные пары),
 * RIS/DECSTR в потоке или сторож xterm (RenderService.ts:349). Режим булев:
 * удерживать нечего, на экране уже новая картинка. Закрываем без END (писать
 * его незачем), иначе флаг «удерживаем старую картинку» гасил бы касания по
 * живой картинке до нашего сторожа. Пока BEGIN не разобран (лежит в очереди за
 * прежними записями), снятый режим ещё не наш — транзакция остаётся.
 */
export function txnAfterParse(
  s: PresentationTxn,
  input: { parsedToken: number | null; modeOn: boolean },
): { state: PresentationTxn; ended: boolean } {
  if (s.kind !== "open" || input.modeOn || input.parsedToken !== s.token) return { state: s, ended: false };
  return { state: TXN_CLOSED, ended: true };
}

export interface SnapshotTxnInput {
  renderer: PresentationRenderer;
  ris?: PresentationChunk;
  history?: PresentationChunk;
  /** term.modes.synchronizedOutputMode на барьере снимка, как есть. */
  modeOn: boolean;
  /** Редьюсер до снимка: может держать транзакцию маркера. */
  txn: PresentationTxn;
  /** Токен транзакции снимка. */
  token: number;
  now: number;
}

export interface SnapshotTxnHead {
  readonly head: SnapshotHead;
  /** Редьюсер после записи головы. */
  readonly txn: PresentationTxn;
  /** Чью транзакцию закрывает хвост; null — закрывать нечего. */
  readonly closeToken: number | null;
}

/**
 * Голова снимка вместе с редьюсером (ST-06). Транзакция маркера reset не
 * должна пережить снимок:
 * - RIS снимка (в голове или перенесённый в хвост) закрывает её сам;
 * - без RIS голова с 2026 перенимает её без разрыва режима (BEGIN при уже
 *   стоящем режиме ничего не меняет), END пишет хвост;
 * - без RIS и без 2026 в голове END за маркер пишет хвост, в том же чанке,
 *   что кадр: кадр и снятие удержания разбираются одной записью.
 * Режим приложения по-прежнему не трогаем (appSyncOpenFor).
 */
export function beginSnapshotTxn(input: SnapshotTxnInput): SnapshotTxnHead {
  const head = planSnapshotHead({
    renderer: input.renderer,
    ris: input.ris,
    history: input.history,
    appSyncOpen: appSyncOpenFor(input.modeOn, input.txn),
  });
  const txn = isEmpty(input.ris) ? input.txn : resetTxn(input.txn);
  if (head.opened) {
    // Из closed — обычное открытие; из open (маркер без нашего RIS) —
    // передача снимку с новым сроком: режим не прерывался.
    const opened = openTxn(TXN_CLOSED, { owner: "snapshot", token: input.token, now: input.now }).state;
    return { head, txn: opened, closeToken: input.token };
  }
  return { head, txn, closeToken: txn.kind === "open" ? txn.token : null };
}

/**
 * Хвост снимка: filler + кадр + END за ту транзакцию, что осталась нашей.
 * modeOn — режим после разбора головы: RIS или DECSTR в истории могли его
 * снять, тогда лишний l не пишем.
 */
export function finishSnapshotTxn(
  begun: SnapshotTxnHead,
  tail: { filler?: PresentationChunk; frame: PresentationChunk; modeOn: boolean },
): { chunk: PresentationChunk; txn: PresentationTxn } {
  const closed = begun.closeToken === null
    ? { state: begun.txn, bytes: "" }
    : closeTxn(begun.txn, begun.closeToken, tail.modeOn);
  return {
    chunk: planSnapshotTail(begun.head, { filler: tail.filler, frame: tail.frame, end: closed.bytes }),
    txn: closed.state,
  };
}
