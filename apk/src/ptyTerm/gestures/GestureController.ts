export interface MotionSample { dy: number; t: number }

/** Shared live/reader friction, independent of the display refresh rate. */
export const decayVelocity = (velocity: number, elapsedMs: number): number => velocity * Math.pow(0.94, elapsedMs / 16);

/** A held finger has zero release velocity, even after a fast initial movement. */
export function releaseVelocity(samples: readonly MotionSample[], releasedAt: number, windowMs = 100): number {
  const last = samples[samples.length - 1];
  if (!last || releasedAt - last.t > windowMs) return 0;
  const tail = samples.filter(s => s.t >= releasedAt - windowMs);
  if (tail.length < 2) return 0;
  const span = releasedAt - tail[0].t;
  if (span < 16) return 0;
  // The first delta happened BEFORE the first retained timestamp.
  return tail.slice(1).reduce((sum, s) => sum + s.dy, 0) / span;
}

export interface EdgeRect { left: number; top: number; right: number; bottom: number }

/**
 * Куда автопрокручивать, пока ручку выделения держат у края (контракт «Ручка
 * выделения у края», T-12). Раньше считалась только вертикаль, и документ шире
 * экрана (120/240/400 колонок) вбок не ехал: ручку приходилось отпускать.
 * Поле края не больше половины размера, чтобы на узкой области обе стороны не
 * срабатывали сразу; в самом центре направления нет.
 */
export function edgeDirection(point: { x: number; y: number }, rect: EdgeRect, margin = 36): { dx: -1 | 0 | 1; dy: -1 | 0 | 1 } {
  const axis = (value: number, lo: number, hi: number): -1 | 0 | 1 => {
    const field = Math.max(0, Math.min(margin, (hi - lo) / 2));
    return value < lo + field ? -1 : value > hi - field ? 1 : 0;
  };
  return { dx: axis(point.x, rect.left, rect.right), dy: axis(point.y, rect.top, rect.bottom) };
}

/** Окно совместимых событий мыши после касания, мс (браузер шлёт их сразу за touchend). */
export const TAP_COMPAT_MOUSE_MS = 700;

/**
 * Одно касание — один клик приложению (ST-10, I-01: одно намерение — один
 * исполнитель). Касание при слежении приложения за мышью уже ушло кликом в
 * ячейку (sendTapAsClick). Следом браузер шлёт совместимые mousedown/mouseup
 * того же касания, и xterm отдавал бы их ВТОРЫМ кликом — замер: «<0;211;8»,
 * затем «<0;212;8» (соседняя ячейка), так же на baseline. true — это событие
 * из окна после нашего клика, xterm его не видит. Настоящая мышь касанием не
 * предваряется — её клики не трогаются.
 */
export function compatMouseAfterTap(now: number, tapClickAt: number, windowMs = TAP_COMPAT_MOUSE_MS): boolean {
  if (!Number.isFinite(now) || !Number.isFinite(tapClickAt)) return false;
  const since = now - tapClickAt;
  return since >= 0 && since < windowMs;
}

export class WheelAccumulator {
  private remainder = 0;
  private lastAt = -Infinity;
  reset(): void { this.remainder = 0; this.lastAt = -Infinity; }
  lines(event: { deltaX: number; deltaY: number; deltaMode: number; ctrlKey: boolean; metaKey: boolean },
    cellHeight: number, visibleHeight: number, now: number): number {
    if (event.ctrlKey || event.metaKey || Math.abs(event.deltaX) > Math.abs(event.deltaY) || cellHeight <= 0) return 0;
    if (now - this.lastAt > 200 || Math.sign(this.remainder) !== Math.sign(event.deltaY)) this.remainder = 0;
    this.lastAt = now;
    const px = event.deltaY * (event.deltaMode === 1 ? cellHeight : event.deltaMode === 2 ? visibleHeight : 1);
    this.remainder += px / cellHeight;
    const lines = Math.trunc(this.remainder);
    this.remainder -= lines;
    return lines;
  }
}
