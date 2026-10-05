export interface StreamPresentationOptions {
  streaming: boolean;
  reducedMotion?: boolean;
  /** Session + message identity; never reuse a smoothing queue across chats. */
  streamKey?: string;
}

/** Presentation only: source/copy and Markdown parsing remain authoritative. */
export function createStreamPresentation(initial: string, initialKey?: string) {
  let identity = initialKey;
  let visible = initial;
  let target = initial;
  let deadline = 0;
  let timer: ReturnType<typeof setTimeout> | undefined;
  const listeners = new Set<() => void>();
  const publish = (next: string) => {
    if (next === visible) return;
    visible = next;
    listeners.forEach(listener => listener());
  };
  const stop = () => {
    if (timer !== undefined) clearTimeout(timer);
    timer = undefined;
  };
  // Never manufacture a dangling UTF-16 surrogate at a presentation boundary.
  const boundary = (length: number) => {
    const before = target.charCodeAt(length - 1);
    const after = target.charCodeAt(length);
    return before >= 0xd800 && before <= 0xdbff && after >= 0xdc00 && after <= 0xdfff
      ? length + 1 : length;
  };
  const tick = () => {
    timer = undefined;
    const remaining = Math.max(1, Math.ceil((deadline - Date.now()) / 40) + 1);
    const length = visible.length + Math.ceil((target.length - visible.length) / remaining);
    publish(target.slice(0, boundary(length)));
    if (visible !== target) timer = setTimeout(tick, 40);
  };
  return {
    getSnapshot: () => visible,
    // Used during React render so completion/reset is immediate, not effect-late.
    getDisplay(source: string, options: StreamPresentationOptions) {
      return !options.streaming || options.reducedMotion || options.streamKey !== identity
        || !source.startsWith(target) || source.length > 16000 ? source : visible;
    },
    subscribe(listener: () => void) {
      listeners.add(listener);
      return () => { listeners.delete(listener); };
    },
    dispose: stop,
    update(source: string, options: StreamPresentationOptions) {
      const reset = options.streamKey !== identity || !source.startsWith(target);
      identity = options.streamKey;
      target = source;
      // These are render budgets, not measured device performance thresholds.
      if (!options.streaming || options.reducedMotion || reset || source.length > 16000) {
        stop();
        publish(source);
        return;
      }
      // Large batches bypass the artificial queue except for a small tail.
      if (target.length - visible.length > 800) publish(target.slice(0, boundary(target.length - 800)));
      if (visible === target || timer !== undefined) return;
      deadline = Date.now() + 160;
      timer = setTimeout(tick, 40);
    },
  };
}
