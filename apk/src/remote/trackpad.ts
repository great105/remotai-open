/**
 * Тачпад: правила относительного управления курсором одной машиной состояний.
 *
 * До 02.09.2026 относительный режим жил на узкой полосе справа (70–80 px), а
 * сама картинка в режиме «Тачпад» на касания не отвечала вовсе — палец «в
 * пустоту», наводка грубая. У AnyDesk весь экран — тачпад, и это главный
 * разрыв, который назвал владелец. Теперь та же машина обслуживает и полосу,
 * и всю картинку: компонент отдаёт ей касания, получает действия и применяет
 * их (курсор, клики, колесо, масштаб). Правила считаются ЗДЕСЬ и проверяются
 * тестом без телефона.
 *
 * Жесты (как у AnyDesk в режиме touchpad):
 *   один палец      — курсор относительно, с чувствительностью;
 *   тап             — левый клик ПОД КУРСОРОМ; второй тап за 300 мс — второй
 *                     обычный клик (ОС склеит в двойной; не dblclick — см.
 *                     объяснение в RemoteView);
 *   долгое нажатие  — mouse-down, палец тащит, отрыв = mouse-up;
 *   два пальца      — колесо (дробные щелчки копятся у вызывающего), по
 *                     горизонтали тоже; тап двумя = правый клик;
 *   щипок           — масштаб картинки (только там, где разрешён);
 *   три пальца      — Alt+Tab влево/вправо.
 *
 * Координаты касаний — в px клиента; сдвиг курсора отдаётся в ДОЛЯХ кадра.
 */

export interface PadPoint {
  x: number;
  y: number;
}

export interface PadSurface {
  /** Размер области, в долях которой считается сдвиг курсора (px). */
  width: number;
  height: number;
  /** Множитель чувствительности (prefs, 0.8…3). */
  sensitivity: number;
  /** Разрешён ли щипок-масштаб на этой поверхности (на полосе — нет). */
  allowZoom: boolean;
}

export type PadAction =
  | { kind: "move"; dx: number; dy: number }
  | { kind: "click"; button: "l" | "r"; second: boolean }
  | { kind: "dragStart" }
  | { kind: "dragEnd" }
  | { kind: "scroll"; dy: number; dx: number }
  | { kind: "zoom"; ratio: number }
  | { kind: "altTab"; dir: 1 | -1 }
  | { kind: "fling"; vx: number; vy: number }
  | { kind: "armLongPress"; ms: number }
  | { kind: "disarmLongPress" };

/** Порог тапа: короче — тап, дольше — движение или удержание. */
export const TAP_MS = 300;
/** Второй тап в этом окне — второй клик той же пары. */
export const DOUBLE_TAP_MS = 300;
/** Долгое нажатие без движения переходит в перетаскивание. */
export const LONG_PRESS_MS = 500;
/** Сдвиг пальца, после которого это уже не тап (px). */
export const MOVE_PX = 4;
/** Прокрутка двумя пальцами: px на один щелчок колеса. */
export const SCROLL_PX_PER_NOTCH = 8;
/** Прокрутка начинается после такого сдвига (px) — иначе дрожание пальцев. */
export const SCROLL_START_PX = 8;
/** Дистанция между пальцами изменилась на такую долю — это щипок, не прокрутка. */
export const PINCH_RATIO = 0.15;
/** Тап двумя пальцами: разъехались дальше — не тап (px). */
export const TWO_TAP_PX = 12;
/** Три пальца по горизонтали на столько px — Alt+Tab. */
export const THREE_SWIPE_PX = 50;
/** Ниже этой скорости (доли кадра за кадр анимации) инерции курсора нет. */
export const FLING_MIN_SPEED = 0.002;

interface State {
  fingers: number;
  start: PadPoint;
  last: PadPoint;
  startTime: number;
  moved: boolean;
  dragging: boolean;
  lastTapTime: number;
  // два пальца
  twoStart: PadPoint;
  twoLast: PadPoint;
  twoDist: number;
  twoScrolled: boolean;
  twoZooming: boolean;
  twoMovedApart: boolean;
  twoTime: number;
  // три пальца
  threeStartX: number;
  threeDone: boolean;
  // инерция: скользящее среднее скорости в долях кадра за кадр
  vx: number;
  vy: number;
  vTime: number;
}

function mid(a: PadPoint, b: PadPoint): PadPoint {
  return { x: (a.x + b.x) / 2, y: (a.y + b.y) / 2 };
}

function dist(a: PadPoint, b: PadPoint): number {
  return Math.hypot(a.x - b.x, a.y - b.y);
}

export class TrackpadGesture {
  private s: State = {
    fingers: 0, start: { x: 0, y: 0 }, last: { x: 0, y: 0 }, startTime: 0,
    moved: false, dragging: false, lastTapTime: 0,
    twoStart: { x: 0, y: 0 }, twoLast: { x: 0, y: 0 }, twoDist: 0,
    twoScrolled: false, twoZooming: false, twoMovedApart: false, twoTime: 0,
    threeStartX: 0, threeDone: false,
    vx: 0, vy: 0, vTime: 0,
  };
  private surface: PadSurface = { width: 1, height: 1, sensitivity: 1, allowZoom: false };

  /** Перетаскивание идёт (после долгого нажатия). */
  get dragging(): boolean { return this.s.dragging; }

  /** Новое касание (touchstart): points — ВСЕ пальцы на поверхности. */
  start(points: PadPoint[], now: number, surface: PadSurface): PadAction[] {
    const s = this.s;
    this.surface = surface;
    const out: PadAction[] = [];
    s.fingers = Math.max(s.fingers, points.length);
    if (points.length === 1 && s.fingers === 1) {
      const p = points[0];
      s.start = { ...p }; s.last = { ...p };
      s.startTime = now; s.moved = false;
      s.vx = 0; s.vy = 0; s.vTime = now;
      if (!s.dragging) out.push({ kind: "armLongPress", ms: LONG_PRESS_MS });
    }
    if (points.length === 2) {
      out.push({ kind: "disarmLongPress" });
      const m = mid(points[0], points[1]);
      s.twoStart = m; s.twoLast = m;
      s.twoDist = dist(points[0], points[1]);
      s.twoScrolled = false; s.twoZooming = false; s.twoMovedApart = false;
      s.twoTime = now;
    }
    if (points.length === 3) {
      out.push({ kind: "disarmLongPress" });
      s.threeStartX = (points[0].x + points[1].x + points[2].x) / 3;
      s.threeDone = false;
    }
    return out;
  }

  /** Долгое нажатие сработало (таймер вызывающего). */
  longPress(): PadAction[] {
    const s = this.s;
    if (s.fingers !== 1 || s.moved || s.dragging) return [];
    s.dragging = true;
    return [{ kind: "dragStart" }];
  }

  /** Движение (touchmove): points — все пальцы на поверхности. */
  move(points: PadPoint[], now: number): PadAction[] {
    const s = this.s;
    const out: PadAction[] = [];
    const { width, height, sensitivity } = this.surface;

    if (points.length === 3 && !s.threeDone) {
      const avgX = (points[0].x + points[1].x + points[2].x) / 3;
      const dx = avgX - s.threeStartX;
      if (Math.abs(dx) > THREE_SWIPE_PX) {
        s.threeDone = true;
        out.push({ kind: "altTab", dir: dx > 0 ? 1 : -1 });
      }
      return out;
    }

    if (points.length === 2 && s.fingers >= 2 && !s.dragging) {
      const m = mid(points[0], points[1]);
      const d = dist(points[0], points[1]);
      if (Math.abs(d - s.twoDist) > TWO_TAP_PX || dist(m, s.twoStart) > TWO_TAP_PX) s.twoMovedApart = true;
      const ratio = s.twoDist > 0 ? Math.abs(d - s.twoDist) / s.twoDist : 0;
      if (this.surface.allowZoom && (s.twoZooming || (!s.twoScrolled && ratio >= PINCH_RATIO))) {
        // Щипок: обратно в прокрутку жест не превращается — иначе экран
        // дёргается между масштабом и колесом.
        s.twoZooming = true;
        if (s.twoDist > 0) out.push({ kind: "zoom", ratio: d / s.twoDist });
        s.twoLast = m;
        return out;
      }
      if (!s.twoScrolled && dist(m, s.twoStart) > SCROLL_START_PX) s.twoScrolled = true;
      if (s.twoScrolled) {
        const dy = (m.y - s.twoLast.y) / SCROLL_PX_PER_NOTCH;
        const dx = (m.x - s.twoLast.x) / SCROLL_PX_PER_NOTCH;
        if (dy !== 0 || dx !== 0) out.push({ kind: "scroll", dy, dx });
      }
      s.twoLast = m;
      return out;
    }

    if (points.length !== 1) return out;
    const p = points[0];
    const dxPx = p.x - s.last.x;
    const dyPx = p.y - s.last.y;
    if (!s.moved && dist(p, s.start) > MOVE_PX) {
      s.moved = true;
      if (!s.dragging) out.push({ kind: "disarmLongPress" });
    }
    if (s.moved || s.dragging) {
      const dx = (dxPx * sensitivity) / Math.max(1, width);
      const dy = (dyPx * sensitivity) / Math.max(1, height);
      if (dx !== 0 || dy !== 0) out.push({ kind: "move", dx, dy });
      const dt = now - s.vTime;
      if (dt > 0 && dt < 100) {
        s.vx = 0.7 * s.vx + 0.3 * dx;
        s.vy = 0.7 * s.vy + 0.3 * dy;
      }
      s.vTime = now;
    }
    s.last = { ...p };
    return out;
  }

  /** Отрыв (touchend/touchcancel): remaining — пальцы, оставшиеся на поверхности. */
  end(remaining: number, now: number): PadAction[] {
    const s = this.s;
    const out: PadAction[] = [];
    out.push({ kind: "disarmLongPress" });
    if (remaining > 0) return out;

    if (s.dragging) {
      s.dragging = false;
      out.push({ kind: "dragEnd" });
      this.resetMulti();
      return out;
    }

    if (s.fingers === 2 && !s.twoMovedApart && !s.twoScrolled && !s.twoZooming
      && now - s.twoTime < TAP_MS) {
      out.push({ kind: "click", button: "r", second: false });
      s.lastTapTime = 0;
    }

    if (s.fingers <= 1 && !s.moved && now - s.startTime < TAP_MS) {
      const second = now - s.lastTapTime < DOUBLE_TAP_MS;
      out.push({ kind: "click", button: "l", second });
      s.lastTapTime = second ? 0 : now;
    }

    if (s.fingers <= 1 && s.moved) {
      const speed = Math.hypot(s.vx, s.vy);
      if (speed > FLING_MIN_SPEED) out.push({ kind: "fling", vx: s.vx, vy: s.vy });
    }

    this.resetMulti();
    return out;
  }

  private resetMulti(): void {
    const s = this.s;
    s.fingers = 0;
    s.moved = false;
    s.twoScrolled = false; s.twoZooming = false; s.twoMovedApart = false;
    s.threeDone = false;
  }
}
