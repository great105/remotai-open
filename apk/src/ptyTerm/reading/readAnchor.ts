import type { IMarker, Terminal } from "@xterm/xterm";

/** Public API takes a cursor-relative offset, not an absolute buffer index. */
export function registerReadAnchor(term: Pick<Terminal, "buffer"> & { registerMarker(offset: number): IMarker | undefined }, row?: number): IMarker | undefined {
  const b = term.buffer.active;
  if (b.type !== "normal" || row == null && b.viewportY >= b.baseY) return undefined;
  return term.registerMarker((row ?? b.viewportY) - (b.baseY + b.cursorY));
}
