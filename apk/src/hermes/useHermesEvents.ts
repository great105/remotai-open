import { useEffect, useRef } from "react";
import { startHermesEventLoop } from "./eventLoop";

/** Keep one cancellable host long poll; do not restart it on every token render. */
export function useHermesEvents(load: (signal: AbortSignal) => Promise<unknown>, enabled: boolean): void {
  const loadRef = useRef(load);
  useEffect(() => { loadRef.current = load; });
  useEffect(() => {
    if (!enabled) return;
    return startHermesEventLoop({
      load: signal => loadRef.current(signal),
      visible: () => document.visibilityState === "visible",
      subscribe: changed => {
        document.addEventListener("visibilitychange", changed);
        return () => document.removeEventListener("visibilitychange", changed);
      },
    });
  }, [enabled]);
}
