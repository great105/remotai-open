/**
 * План применения снимка экрана `{t:"screen"}` к живому терминалу — чистая
 * последовательность шагов RIS → история → досылка строк → кадр (ST-05).
 *
 * Зачем отдельный модуль. Порядок шагов жил только внутри компонента
 * (PtyTermView, ветка msg.t==="screen"), а стенды копировали его к себе
 * («Повторяет PtyTermView», build/qa/probe-history-seam.mjs). Копия правила
 * проверяет копию (раздел 8.1 плана, L2: «без подмены правила его
 * упрощённой копией»). Здесь план собирается ТОЛЬКО из уже существующих
 * правил — takeSnapshotHistory, scrollbackLineCount, shouldReplaceHistory,
 * takeScrollbackLines, stripScrollbackErase, fillerRows, — и его исполняют и
 * компонент, и стенд сравнения (snapshotConformance.test.ts).
 *
 * Байты и порядок — ровно как в компоненте:
 *   - решение по истории снимается ВСЕГДА, даже если заменять не будем
 *     (готовность соединения расходуется, см. snapshotHistory.ts);
 *   - без замены — только кадр: своя прокрутка не хуже присланной;
 *   - с заменой — RIS, затем scrollback-часть истории (первые
 *     hist_lines − screen_rows строк) с вырезанным аномальным ESC[3J, затем
 *     столько LF, сколько покажет замер живого буфера после истории
 *     (fillerRows), затем кадр. Пустые байты истории — сразу кадр.
 *
 * Шаг filler несёт только ЦЕЛЬ: сколько LF дописать, исполнитель считает по
 * замеру ПОСЛЕ разбора истории (snapshotStepPayload + measure) — так расчёт
 * переживает перенос длинных строк и расхождение ширины emoji Go/xterm.
 *
 * Барьеры поколения, эпохи и геометрии (snapGuard, snapStale) остаются у
 * исполнителя: план — про байты, а не про актуальность операции.
 *
 * Чистое правило: ни xterm, ни DOM, ни сети — проверяется в node.
 */

import { fillerRows, scrollbackLineCount, shouldReplaceHistory, takeScrollbackLines } from "./historySeam";
import { stripScrollbackErase } from "./keepHistory";
import { takeSnapshotHistory } from "./snapshotHistory";
import type { SnapshotHistory } from "./snapshotHistory";

/** Полный сброс терминала — в очередь записи, как на sync-маркере. */
export const SNAPSHOT_RIS = "\x1bc";

export type SnapshotStep =
  | { kind: "ris" }
  | { kind: "history"; bytes: Uint8Array; lines: number }
  | { kind: "filler"; target: number }
  | { kind: "frame" };

export interface SnapshotApplyInput {
  /** Поле history сообщения (пустая строка, если его нет). */
  history: string;
  /** hist_lines сообщения; у старого агента отсутствует. */
  histLines: unknown;
  /** screen_rows сообщения. */
  screenRows: unknown;
  /** screen_cols сообщения. */
  snapCols: unknown;
  /** Ширина живого терминала. */
  termCols: number;
  /** Своя прокрутка: buffer.normal.baseY, измеренная ПОСЛЕ барьера записи. */
  localScrollback: number;
  /** Состояние истории соединения (snapshotHistoryRef). */
  historyState: SnapshotHistory;
}

export interface SnapshotApplyPlan {
  replace: boolean;
  nextHistoryState: SnapshotHistory;
  /** Строк scrollback в присланной истории; −1 — сервер не дал чисел. */
  serverScrollback: number;
  steps: SnapshotStep[];
}

export function planSnapshotApply(input: SnapshotApplyInput): SnapshotApplyPlan {
  const decision = takeSnapshotHistory(input.historyState, input.history.length > 0);
  const serverScrollback = scrollbackLineCount(input.histLines, input.screenRows);
  const replace = decision.apply && shouldReplaceHistory({
    serverScrollback,
    localScrollback: input.localScrollback,
    sameWidth: typeof input.snapCols === "number" && input.snapCols === input.termCols,
  });
  const plan = (steps: SnapshotStep[]): SnapshotApplyPlan => ({
    replace,
    nextHistoryState: decision.next,
    serverScrollback,
    steps,
  });
  if (!replace) return plan([{ kind: "frame" }]);
  const picked = takeScrollbackLines(input.history, serverScrollback);
  // Аномальная цепочка ESC[3J внутри истории режется, а не исполняется;
  // хвост возможного начала последовательности сознательно НЕ доклеивается:
  // продолжения у истории нет (тот же выбор, что был в компоненте).
  const { data } = stripScrollbackErase(new TextEncoder().encode(picked.text));
  if (data.byteLength === 0) return plan([{ kind: "ris" }, { kind: "frame" }]);
  const steps: SnapshotStep[] = [{ kind: "ris" }, { kind: "history", bytes: data, lines: picked.lines }];
  if (picked.lines > 0) steps.push({ kind: "filler", target: picked.lines });
  steps.push({ kind: "frame" });
  return plan(steps);
}

/** Замер живого буфера для шага filler: активный буфер (после RIS он нормальный). */
export interface SnapshotMeasure {
  baseY: number;
  cursorY: number;
  rows: number;
}

/**
 * Байты шага. `measure` зовётся только для filler и обязан отражать буфер
 * ПОСЛЕ разбора предыдущих шагов (исполнитель ждёт колбэк записи). Пустая
 * строка — писать нечего (кадр при этом всё равно следующий шаг).
 */
export function snapshotStepPayload(
  step: SnapshotStep,
  frame: string,
  measure: () => SnapshotMeasure,
): string | Uint8Array {
  switch (step.kind) {
    case "ris":
      return SNAPSHOT_RIS;
    case "history":
      return step.bytes;
    case "filler": {
      const m = measure();
      // Цель — все РЯДЫ, которые заняла история, а не число её строк. Шаг
      // filler идёт только после RIS и истории, так что история начата на
      // пустом экране с (0,0) и заняла ровно baseY + cursorY рядов. Строка
      // истории шире экрана клиента (зеркало не переносит напечатанное до
      // сужения) занимает несколько рядов, и при цели «по строкам» самые
      // свежие ряды оставались на экране и стирались ESC[2J кадра — клиент
      // терял их из прокрутки (стенд §6, C-03R seed 20261006, разрыв
      // history-seam-wide-lines снят 15.09).
      const k = fillerRows(Math.max(step.target, m.baseY + m.cursorY), m.baseY, m.cursorY, m.rows);
      return k > 0 ? "\n".repeat(k) : "";
    }
    case "frame":
      return frame;
  }
}
