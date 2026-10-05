import type { TextRange } from "../selection/SelectionController";
import { documentPosition, type ReadDocument } from "./ReadDocument";

/**
 * Позиция следующего совпадения относительно текущего диапазона, -1 — нет ни
 * одного. Поиск назад из самого начала (start = 0) идёт по кругу к последнему
 * совпадению: раньше lastIndexOf(query, -1) возвращал то же совпадение в 0.
 */
export function findInText(text: string, query: string, from: TextRange, direction: -1 | 1): number {
  if (!query) return -1;
  let index = -1;
  if (direction > 0) index = text.indexOf(query, from.end);
  else if (from.start > 0) index = text.lastIndexOf(query, from.start - 1);
  if (index < 0) index = direction > 0 ? text.indexOf(query) : text.lastIndexOf(query);
  return index;
}

/** Reveal a grid match locally; long/multiline matches keep their start visible. */
export function revealRange(doc: ReadDocument, range: TextRange, view: {
  left: number; width: number; cellWidth: number; cellHeight: number;
}): { left: number; top: number } {
  const start = documentPosition(doc, range.start), end = documentPosition(doc, range.end);
  const left = start.col * view.cellWidth;
  const right = (end.row === start.row ? end.col : start.col + 1) * view.cellWidth;
  return {
    left: Math.max(0, right - left > view.width || left < view.left ? left
      : right > view.left + view.width ? right - view.width : view.left),
    top: Math.max(0, (start.row - 2) * view.cellHeight),
  };
}
