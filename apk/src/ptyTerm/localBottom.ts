interface LocalViewport {
  readonly buffer: { readonly active: { readonly viewportY: number; readonly baseY: number } };
  scrollToBottom(): void;
}

/** xterm scrolls relative to its DOM position. After reflow that position can
 * briefly differ from buffer.viewportY; the first call reconciles them and
 * can stop short. Complete this same local action once, before another user
 * intent can intervene. Never send a competing command to the application.
 */
export function scrollLocalBottom(term: LocalViewport): void {
  term.scrollToBottom();
  const buffer = term.buffer.active;
  if (buffer.viewportY < buffer.baseY) term.scrollToBottom();
}
