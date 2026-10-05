export type TerminalGeometry = Readonly<{ cols: number; rows: number }>;

export type GeometryRevisionState = Readonly<{
  revision: number;
  geometry: TerminalGeometry;
}>;

/**
 * Advance the request revision only when xterm's real cell grid changed.
 *
 * The browser can emit many layout/viewport events which do not resize xterm.
 * Tying the token to the observed cols/rows keeps those events from making a
 * valid screen frame stale, while still invalidating a frame already in flight
 * across fit(), adopt and explicit Terminal.resize() calls.
 */
export function noteTerminalGeometry(
  state: GeometryRevisionState,
  next: TerminalGeometry,
): GeometryRevisionState {
  if (state.geometry.cols === next.cols && state.geometry.rows === next.rows) return state;
  const revision = state.revision < Number.MAX_SAFE_INTEGER ? state.revision + 1 : 0;
  return { revision, geometry: { cols: next.cols, rows: next.rows } };
}

/** A screen frame is authoritative only for the exact geometry request token. */
export function screenGeometryRevisionMatches(value: unknown, current: number): boolean {
  return Number.isSafeInteger(value)
    && (value as number) >= 0
    && value === current;
}
