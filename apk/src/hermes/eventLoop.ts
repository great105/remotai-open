export interface HermesEventLoopOptions {
  load: (signal: AbortSignal) => Promise<unknown>;
  visible: () => boolean;
  subscribe: (changed: () => void) => () => void;
}

export function startHermesEventLoop(options: HermesEventLoopOptions): () => void {
  let stopped = false;
  let timer: ReturnType<typeof setTimeout> | undefined;
  let current: AbortController | undefined;
  const clearTimer = () => { if (timer !== undefined) clearTimeout(timer); timer = undefined; };
  const fire = async () => {
    clearTimer();
    if (stopped || current || !options.visible()) return;
    const controller = new AbortController();
    current = controller;
    let delay = 32;
    try {
      const requestedDelay = await options.load(controller.signal);
      if (typeof requestedDelay === "number" && Number.isFinite(requestedDelay)) delay = Math.max(32, Math.min(requestedDelay, 2000));
    }
    catch { delay = 1000; }
    finally {
      // A hidden request may finish after a new foreground request starts.
      if (current === controller) {
        current = undefined;
        if (!stopped && options.visible()) timer = setTimeout(() => { void fire(); }, delay);
      }
    }
  };
  const changed = () => {
    clearTimer();
    if (stopped) return;
    if (!options.visible()) {
      const controller = current;
      current = undefined;
      controller?.abort();
    } else { void fire(); }
  };
  const unsubscribe = options.subscribe(changed);
  void fire();
  return () => {
    if (stopped) return;
    stopped = true;
    clearTimer();
    current?.abort();
    current = undefined;
    unsubscribe();
  };
}
