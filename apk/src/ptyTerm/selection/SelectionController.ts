import type { ReadDocument } from "../reading/ReadDocument";

export interface TextRange { start: number; end: number }
const boundaryCache = new WeakMap<ReadDocument, number[]>();

export function cellBoundaries(doc: ReadDocument): number[] {
  let boundaries = boundaryCache.get(doc);
  if (!boundaries) {
    if (doc.source === "agent") {
      // Native text has graphemes rather than terminal cells (including emoji
      // joiners and combining marks). Segmenter is present in supported engines.
      const Segmenter = (Intl as unknown as { Segmenter?: new(locale: string | undefined, options: { granularity: "grapheme" }) => {
        segment(input: string): Iterable<{ index: number }>;
      } }).Segmenter;
      if (Segmenter) boundaries = [...Array.from(new Segmenter(undefined, { granularity: "grapheme" }).segment(doc.text), part => part.index), doc.text.length];
      else { boundaries = [0]; for (const character of doc.text) boundaries.push(boundaries[boundaries.length - 1] + character.length); }
    } else boundaries = Array.from(new Set(doc.lines.flatMap(line => [...line.columns, line.end]))).sort((a, b) => a - b);
    boundaryCache.set(doc, boundaries);
  }
  return boundaries;
}

function boundaryIndex(boundaries: number[], value: number): number {
  let lo = 0, hi = boundaries.length;
  while (lo < hi) { const mid = (lo + hi) >>> 1; if (boundaries[mid] < value) lo = mid + 1; else hi = mid; }
  return lo;
}

/** Never split a terminal cell (surrogate pairs, combining marks, wide glyphs). */
export function selectionRange(doc: ReadDocument, anchor: number, head: number): TextRange {
  const boundaries = cellBoundaries(doc);
  const lo = Math.max(0, Math.min(anchor, head)), hi = Math.min(doc.text.length, Math.max(anchor, head));
  const start = boundaryIndex(boundaries, lo), end = boundaryIndex(boundaries, hi);
  return { start: boundaries[boundaries[start] === lo ? start : start - 1] ?? 0,
    end: boundaries[end] ?? doc.text.length };
}

export function wordRange(doc: ReadDocument, offset: number): TextRange {
  const boundaries = cellBoundaries(doc);
  const index = Math.max(0, boundaries.findIndex((b, i) => b <= offset && (boundaries[i + 1] ?? Infinity) > offset));
  const word = /[\p{L}\p{N}_/\\.~:@$-]/u;
  let lo = index, hi = Math.min(boundaries.length - 1, index + 1);
  if (word.test(doc.text.slice(boundaries[lo], boundaries[hi]))) {
    while (lo > 0 && word.test(doc.text.slice(boundaries[lo - 1], boundaries[lo]))) lo--;
    while (hi < boundaries.length - 1 && word.test(doc.text.slice(boundaries[hi], boundaries[hi + 1]))) hi++;
  }
  return { start: boundaries[lo] ?? 0, end: boundaries[hi] ?? doc.text.length };
}

/**
 * Снимок выделения для ОДНОЙ операции кнопки toolbar (ST-07, T-16).
 *
 * Снимок нужен, потому что фокус, который получает кнопка, может свернуть
 * нативное Selection до click. Но раньше он жил до следующего click: если
 * палец ушёл с кнопки без click, а выделение затем сняли тапом, click без
 * pointerdown (VoiceOver/TalkBack) копировал старый невидимый диапазон.
 * Теперь снимок годится только той же кнопке и только недолго.
 */
export interface ActionSnapshot<A extends string = string> {
  readonly action: A;
  readonly text: string;
  readonly at: number;
}

export function captureAction<A extends string>(action: A, text: string, now: number): ActionSnapshot<A> {
  return Object.freeze({ action, text, at: now });
}

/** Текст снимка, если он той же кнопки и не старше maxAgeMs; иначе null — брать текущее выделение. */
export function consumeAction<A extends string>(snap: ActionSnapshot<A> | null | undefined, action: A,
  now: number, maxAgeMs = 1000): string | null {
  if (!snap || snap.action !== action) return null;
  const age = now - snap.at;
  return age >= 0 && age <= maxAgeMs ? snap.text : null;
}

export function moveBoundary(doc: ReadDocument, range: TextRange, end: "start" | "end", direction: -1 | 1): TextRange {
  const bounds = cellBoundaries(doc);
  const index = bounds.indexOf(range[end]);
  const next = bounds[Math.max(0, Math.min(bounds.length - 1, index + direction))] ?? range[end];
  return selectionRange(doc, end === "start" ? next : range.start, end === "end" ? next : range.end);
}
