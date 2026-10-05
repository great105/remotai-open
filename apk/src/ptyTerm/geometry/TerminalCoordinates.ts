export interface Rect { left: number; top: number; width: number; height: number }
export interface TerminalCoordinates {
  screen: Rect;
  visible: Rect;
  cols: number;
  rows: number;
  cellWidth: number;
  cellHeight: number;
}

/** Measure the transformed grid, never divide the keyboard-clipped host by rows. */
export function terminalCoordinates(screen: Rect, clip: Rect, cols: number, rows: number): TerminalCoordinates | null {
  if (screen.width <= 0 || screen.height <= 0 || cols < 1 || rows < 1) return null;
  const left = Math.max(screen.left, clip.left), top = Math.max(screen.top, clip.top);
  const right = Math.min(screen.left + screen.width, clip.left + clip.width);
  const bottom = Math.min(screen.top + screen.height, clip.top + clip.height);
  return { screen, visible: { left, top, width: Math.max(0, right - left), height: Math.max(0, bottom - top) },
    cols, rows, cellWidth: screen.width / cols, cellHeight: screen.height / rows };
}

export function measureTerminalCoordinates(host: HTMLElement, cols: number, rows: number): TerminalCoordinates | null {
  const screen = host.querySelector<HTMLElement>(".xterm-screen");
  if (!screen) return null;
  const box = host.getBoundingClientRect();
  const vv = window.visualViewport;
  const left = Math.max(box.left, vv?.offsetLeft ?? 0);
  const top = Math.max(box.top, vv?.offsetTop ?? 0);
  const right = Math.min(box.right, (vv?.offsetLeft ?? 0) + (vv?.width ?? window.innerWidth));
  const bottom = Math.min(box.bottom, (vv?.offsetTop ?? 0) + (vv?.height ?? window.innerHeight));
  return terminalCoordinates(screen.getBoundingClientRect(), {
    left, top, width: Math.max(0, right - left), height: Math.max(0, bottom - top),
  }, cols, rows);
}

export function cellAt(g: TerminalCoordinates, x: number, y: number): { col: number; row: number } {
  return {
    col: Math.max(0, Math.min(g.cols - 1, Math.floor((x - g.screen.left) / g.cellWidth))),
    row: Math.max(0, Math.min(g.rows - 1, Math.floor((y - g.screen.top) / g.cellHeight))),
  };
}

/** Inclusive rows intersecting the visible clip; viewportY is added by the caller. */
export function visibleRows(g: TerminalCoordinates): { start: number; end: number } | null {
  if (g.visible.height <= 0 || g.visible.width <= 0) return null;
  return { start: cellAt(g, g.visible.left, g.visible.top).row,
    end: Math.min(g.rows - 1, Math.ceil((g.visible.top + g.visible.height - g.screen.top) / g.cellHeight - 1e-6) - 1) };
}
