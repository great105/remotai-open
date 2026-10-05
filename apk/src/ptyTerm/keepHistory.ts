/**
 * Защита истории терминала от «стереть буфер прокрутки» (CSI 3 J).
 *
 * ЖИВАЯ ЖАЛОБА 31.07: «у Kimi часто улетает на самый верх — смотрю вывод внизу,
 * скроллю чуть, и я в начале». Замер (build/qa/probe-resize-repaint.mjs) на
 * живой сессии показал причину: изменение высоты окна на 58 px — обычное дело
 * на телефоне, где прокрутка прячет панель браузера, — заставляет агента
 * перерисовать интерфейс, и в этих 90 КБ дважды приходит `ESC[3J`. Эта
 * последовательность стирает у терминала ВСЮ историю прокрутки: смотреть назад
 * становится нечего, и человек оказывается в начале.
 *
 * Историю на телефоне мы ценим выше, чем право приложения её стереть: экран
 * маленький, вывод уезжает быстро, и «посмотреть, что было» — главный способ
 * читать работу агента. Поэтому CSI 3 J вырезаем, а всё остальное (в том числе
 * `ESC[2J` — очистка видимого экрана) пропускаем как есть: команда `clear`
 * по-прежнему очищает экран, просто история переживает это.
 *
 * ⚠ УТОЧНЕНО 04.08.2026 (жалоба «у Кими прокрутка вверх перескакивает куда-то в
 * непонятное место»). У сплошного вырезания нашлась обратная сторона, и она
 * оказалась хуже исходной беды. Kimi живёт в ОБЫЧНОМ буфере и на каждую полную
 * перерисовку шлёт `ESC[2J ESC[3J ESC[H`, после чего печатает свою переписку
 * заново — 283 строки при экране в 31. `ESC[2J` гасит видимый экран НА МЕСТЕ, а
 * в историю уезжают все 283 строки минус экран. Просьбу «убери прошлую копию»
 * мы вырезали — и копии копились. Замер боевой сессии (`/api/pty/{id}/export`,
 * 2,85 МБ): 137 копий приветственного блока при ОДНОМ session_id, интервал
 * 265–290 строк, уникального текста ~1200 строк из 44 000. Прокручивая вверх,
 * человек переходил границу копии и попадал в текст, который только что видел
 * восемью экранами ниже.
 *
 * Решение принимает вызывающий, и оно зависит от ВЛАДЕЛЬЦА ИСТОРИИ и от того,
 * что человек делает прямо сейчас (см. PtyTermView):
 *   - история принадлежит терминалу (режим «Вывод») → просьбу выбрасываем
 *     навсегда: иначе обычный resize агента обнулит обещанную человеку историю;
 *   - история принадлежит приложению, человек у низа → просьбу пропускаем,
 *     чтобы повторные полные перерисовки не копились в обычном буфере;
 *   - история принадлежит приложению, человек читает её → стирание откладываем
 *     до возвращения к низу, чтобы не выбросить человека посреди чтения.
 *
 * ⚠ 07.08.2026: для ветки «у низа» простого пропуска оказалось мало — запись
 * в xterm асинхронна, и решение, принятое на постановке кадра в очередь,
 * устаревало к исполнению (человек за разбор жирного кадра успевал начать
 * прокрутку). Для неё добавлена `splitScrollbackErase`: поток делится на
 * сегменты данных и места стираний, и вызывающий решает про каждое стирание
 * по ходу разбора очереди xterm.
 *
 * Тонкость, ради которой функция и живёт отдельно: последовательность может
 * прийти РАЗОРВАННОЙ между двумя кадрами WebSocket («ESC[» в конце одного,
 * «3J» в начале другого). Поэтому функция возвращает ещё и «хвост» — байты,
 * которые могут оказаться началом такой последовательности; вызывающий обязан
 * приклеить их к следующей порции.
 */

import type { HistoryOwner } from "./altScroll";
import type { RetentionMode } from "./runtime/TerminalControls";

export type { RetentionMode };

export const EMPTY_BYTES = new Uint8Array(0);

export type ScrollbackEraseAction = "discard" | "defer" | "write";

/** Invalidates an erase already dequeued into an asynchronous writer barrier. */
export class ScrollbackEraseGate {
  private generation = 0;

  capture(): number {
    return this.generation;
  }

  invalidate(): void {
    this.generation++;
  }

  isCurrent(ticket: number): boolean {
    return ticket === this.generation;
  }
}

/** Policy is sampled immediately before an asynchronous xterm write. */
export function scrollbackEraseAction(
  owner: HistoryOwner,
  reading: boolean,
): ScrollbackEraseAction {
  if (owner === "terminal") return "discard";
  return reading ? "defer" : "write";
}

/** Terminal ownership invalidates pending erase; unknown keeps old behavior. */
export function keepPendingScrollbackErase(owner: HistoryOwner): boolean {
  return owner !== "terminal";
}

/**
 * ⚠ ST-04 (план стабилизации 13.09, I-02, T-04). Две функции выше — LEGACY: они
 * решают судьбу CSI 3 J по ВЛАДЕЛЬЦУ ИСТОРИИ, а владелец складывается из
 * ручного режима навигации и замера плотности потока. Замер запуском показал,
 * что на 16 КиБ вывода 31 строка давала «application» (стирание исполняется),
 * а 32 — «terminal» (стирание выбрасывается навсегда): одна строка вывода или
 * тап по «Вывод»/«Агент» меняли СОСТАВ истории. Навигация не имеет права так
 * делать — она выбирает, кто листает, а не что хранится.
 *
 * Поэтому хранение живёт отдельным правилом — политикой ПОКОЛЕНИЯ переднего
 * процесса. На вход ей подаются только поколение, есть ли агент на переднем
 * плане и что объявил реестр компьютера. Плотность, ownScrollback, ручной
 * режим, наблюдения ответа страниц и жесты сюда не входят намеренно: их
 * изменение внутри поколения не должно менять ни политику, ни итоговый
 * scrollback. Legacy-функции живут, пока PtyTermView не переключён на политику.
 */
export type EraseScrollback = "honor" | "preserve";

export interface RetentionPolicy {
  /** honor — исполнять CSI 3 J у низа и откладывать при чтении; preserve — не исполнять. */
  readonly erase: EraseScrollback;
  /** Откуда взято: реестр агента, оболочка без агента или значение по умолчанию. */
  readonly source: "registry" | "shell" | "default";
  /** Ключ поколения переднего процесса: политика меняется только на его границе. */
  readonly generation: string;
}

/**
 * Политика хранения для поколения переднего процесса.
 *   - агента на переднем плане нет → preserve/shell. Так ведёт себя код до
 *     ST-04: без агента stripScrollbackErase вырезает 3J всегда, и `clear` в
 *     оболочке не уносит историю, ради которой человек открыл терминал;
 *   - реестр компьютера объявил значение → оно (Codex на повороте шлёт
 *     2J 3J H и стирал богатую историю — это знание профиля, а не замера);
 *   - иначе honor/default: прежнее поведение «unknown» — повторные полные
 *     перерисовки (Kimi, 137 копий приветствия) не копятся ложной историей.
 * Неизвестное объявление (старый/новый агент прислал что-то своё) не
 * угадывается: считается, что реестр промолчал.
 */
export function retentionFor(input: {
  generation: string;
  agentInFg: boolean;
  declared: "" | EraseScrollback;
}): RetentionPolicy {
  const generation = String(input.generation ?? "");
  if (!input.agentInFg) return Object.freeze({ erase: "preserve", source: "shell", generation });
  const declared = input.declared;
  if (declared === "honor" || declared === "preserve") {
    return Object.freeze({ erase: declared, source: "registry", generation });
  }
  return Object.freeze({ erase: "honor", source: "default", generation });
}

/**
 * Что сделать с очередным CSI 3 J. Сэмплируется прямо перед асинхронной записью
 * в xterm (как и legacy-версия); `reading` — только сигнал «человек сейчас
 * читает» (выделение, жест, вьюпорт выше низа), маршрут навигации сюда не входит.
 */
export function eraseActionFor(policy: RetentionPolicy, reading: boolean): ScrollbackEraseAction {
  if (policy.erase === "preserve") return "discard";
  return reading ? "defer" : "write";
}

/**
 * Держать ли уже отложенное стирание. Снимается только политикой preserve, то
 * есть на границе поколения или по reset — смена режима навигации его не
 * трогает (ST-04: переход Агент → Вывод → Агент раньше уничтожал его навсегда).
 */
export function keepPendingErase(policy: RetentionPolicy): boolean {
  return policy.erase === "honor";
}

/**
 * Переключатель хранения (план 9.1, terminalFeatures().retention): legacy —
 * прежнее правило по владельцу истории; shadow — исполняет прежнее, новое только
 * считается и расхождение пишется в журнал; policy — исполняет политика
 * поколения. Одно место, где выбирается исполняемое решение: иначе каждый
 * потребитель стирания (цепочка, отложенное, перенос через resumed) выбирал бы
 * его по-своему.
 */
export function eraseRoute<T>(mode: RetentionMode, legacy: T, policy: T): { use: T; diverged: boolean } {
  return { use: mode === "policy" ? policy : legacy, diverged: mode === "shadow" && legacy !== policy };
}

/**
 * Снимает ли выбор режима навигации (Авто/Вывод/Агент) уже отложенное стирание.
 * В политике — никогда (I-02: навигация не меняет ни хранение, ни ANSI):
 * отложенное снимает только граница поколения или reset. В legacy и shadow
 * исполняется прежнее правило — владелец «terminal» снимает навсегда.
 */
export function navigationChoiceDropsPendingErase(mode: RetentionMode, nextOwner: HistoryOwner): boolean {
  return mode !== "policy" && !keepPendingScrollbackErase(nextOwner);
}

/**
 * Снимает ли граница поколения (новый передний процесс или новое объявление)
 * уже отложенное стирание. В политике — да, если новое поколение его не держит
 * (preserve). Прежнее правило такой границы не знало (держало, решал владелец в
 * момент досылки): legacy и shadow ничего не снимают, shadow видит расхождение.
 */
export function generationDropsPendingErase(mode: RetentionMode, next: RetentionPolicy): { use: boolean; diverged: boolean } {
  return eraseRoute(mode, false, !keepPendingErase(next));
}

/**
 * Журнал расхождений shadow без лавины: одно сообщение на сочетание
 * (точка решения, прежнее, новое, чтение) в поколении. Kimi шлёт по два CSI 3 J
 * на перерисовку — сообщение на каждое забило бы журнал агента. Ограничен по
 * числу ключей; новое поколение начинает счёт заново.
 */
export class RetentionShadowLog {
  private generation = "";
  private readonly seen = new Set<string>();
  constructor(private readonly limit = 32) {}

  /** true — это первое такое расхождение в поколении, его стоит сообщить. */
  first(generation: string, key: string): boolean {
    if (generation !== this.generation) { this.generation = generation; this.seen.clear(); }
    if (this.seen.has(key) || this.seen.size >= this.limit) return false;
    this.seen.add(key);
    return true;
  }
}

const ESC = 0x1b;
const BRACKET = 0x5b; // «[»
const THREE = 0x33; // «3»
const J = 0x4a;

/**
 * Убирает из потока `ESC [ 3 J` (и вариант `ESC [ ? 3 J`).
 *
 * @param passErase — пропустить стирание истории в терминал как есть (человек
 * стоит у низа вывода, терять ему нечего). Тогда ни вырезания, ни удержания
 * хвоста не нужно: разорванную между кадрами последовательность склеит сам
 * потоковый разбор xterm.
 * @returns data — байты для терминала; tail — незавершённый хвост, который надо
 * дописать в начало следующей порции (0–3 байта); erased — сколько стираний
 * истории вырезано (вызывающий по нему решает, что стирание отложено).
 */
export function stripScrollbackErase(
  input: Uint8Array,
  passErase = false,
): { data: Uint8Array; tail: Uint8Array; erased: number } {
  // Быстрый путь: в подавляющем большинстве кадров ESC нет вовсе.
  if (input.byteLength === 0) return { data: input, tail: EMPTY_BYTES, erased: 0 };
  if (passErase) return { data: input, tail: EMPTY_BYTES, erased: 0 };

  let erased = 0;
  const out = new Uint8Array(input.byteLength);
  let w = 0;
  let i = 0;
  while (i < input.byteLength) {
    if (input[i] !== ESC) {
      out[w++] = input[i++];
      continue;
    }
    const seq = matchScrollbackErase(input, i);
    if (seq === -1) {
      // Возможное начало нужной последовательности, но кадр кончился: придержим
      // хвост до следующей порции, иначе разрезанная посередине «ESC[3J»
      // проскочит в терминал и сотрёт историю.
      return { data: out.subarray(0, w), tail: input.slice(i), erased };
    }
    if (seq > 0) {
      i += seq; // совпало — выбрасываем целиком
      erased++;
      continue;
    }
    out[w++] = input[i++]; // обычный ESC — пропускаем как есть
  }
  return { data: out.subarray(0, w), tail: EMPTY_BYTES, erased };
}

/** Последовательность «стереть историю прокрутки» — ею же и досылаем отложенное. */
export const SCROLLBACK_ERASE = "\x1b[3J";

/**
 * Элемент цепочки записи: кусок байтов для терминала либо место, где в потоке
 * стояло «стереть историю» (сама последовательность убрана — исполнять её или
 * отложить, вызывающий решает ПО ХОДУ разбора, см. PtyTermView).
 */
export type EraseChainItem =
  | { kind: "data"; data: Uint8Array }
  | { kind: "erase" };

/**
 * ⚠ ДОБАВЛЕНО 07.08.2026 (жалоба «в Codex/Kimi вьюпорт дёргается туда-сюда»).
 * Режим «пропустить стирание» из stripScrollbackErase оказался дырявым по
 * времени: решение принималось при постановке кадра в очередь xterm, а
 * term.write разбирает очередь АСИНХРОННО (порциями ~12 мс, жирный кадр Kimi
 * 90 КБ — сотни мс). За это окно человек успевал начать прокрутку вверх, и
 * исполнившийся `ESC[3J` выбрасывал его наверх посреди чтения.
 *
 * Поэтому для случая «человек у низа» поток теперь делится на элементы: данные
 * и места стираний. Вызывающий пишет данные по очереди и перед КАЖДЫМ стиранием
 * перепроверяет, что человек всё ещё у низа (тогда досылает SCROLLBACK_ERASE
 * сам); ушёл — стирание откладывается по старой механике pendingErase. Порядок
 * байт при этом не меняется: стирание встаёт ровно туда, где стояло в потоке.
 *
 * Хвост — как у stripScrollbackErase: возможное начало разорванной между
 * кадрами последовательности, приклеить к следующей порции.
 */
export function splitScrollbackErase(
  input: Uint8Array,
): { items: EraseChainItem[]; tail: Uint8Array } {
  if (input.byteLength === 0) return { items: [], tail: EMPTY_BYTES };
  const items: EraseChainItem[] = [];
  let segStart = 0;
  let i = 0;
  const pushSegment = (end: number) => {
    if (end > segStart) items.push({ kind: "data", data: input.slice(segStart, end) });
  };
  while (i < input.byteLength) {
    if (input[i] !== ESC) { i++; continue; }
    const seq = matchScrollbackErase(input, i);
    if (seq === -1) {
      // Кадр кончился на возможном начале последовательности: отрезаем
      // набранное в сегмент, хвост ждёт продолжения (см. stripScrollbackErase).
      pushSegment(i);
      return { items, tail: input.slice(i) };
    }
    if (seq > 0) {
      pushSegment(i);
      items.push({ kind: "erase" });
      i += seq;
      segStart = i;
      continue;
    }
    i++; // обычный ESC — остаётся внутри сегмента
  }
  pushSegment(input.byteLength);
  return { items, tail: EMPTY_BYTES };
}

/**
 * Где в input кончается каждый элемент цепочки splitScrollbackErase — индекс
 * байта ПОСЛЕ элемента. Элементы идут встык: данные — свой срез, стирание —
 * сама последовательность (4 или 5 байт). Последний конец равен
 * input.byteLength − tail. Нужно, чтобы позиция потока двигалась вместе с
 * каждой записью цепочки (ST-09: resume с отданного xterm).
 */
export function eraseChainEnds(input: Uint8Array, items: readonly EraseChainItem[]): number[] {
  const ends: number[] = [];
  let pos = 0;
  for (const it of items) {
    if (it.kind === "data") pos += it.data.byteLength;
    else {
      const seq = matchScrollbackErase(input, pos);
      if (seq > 0) pos += seq;
    }
    ends.push(pos);
  }
  return ends;
}

/**
 * Сколько байт занимает `ESC[3J` начиная с позиции i.
 * 0 — это не она; -1 — данных не хватает (возможное начало).
 */
function matchScrollbackErase(b: Uint8Array, i: number): number {
  const n = b.byteLength;
  if (i + 1 >= n) return -1;
  if (b[i + 1] !== BRACKET) return 0;
  let k = i + 2;
  // Приватный префикс «?»: некоторые приложения шлют ESC[?3J.
  if (k < n && b[k] === 0x3f) k++;
  if (k >= n) return -1;
  if (b[k] !== THREE) return 0;
  k++;
  if (k >= n) return -1;
  if (b[k] !== J) return 0;
  return k + 1 - i;
}
