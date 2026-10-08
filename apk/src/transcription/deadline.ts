import { cancelled } from "./pipeline";

/** Bound the await itself, including transports that fail to honour abort. */
export async function withDeadline<T>(parent: AbortSignal, ms: number, code: string, work: (signal: AbortSignal) => Promise<T>): Promise<T> {
  const controller = new AbortController();
  let timer: ReturnType<typeof setTimeout> | undefined;
  let abort!: () => void;
  const interrupted = new Promise<never>((_, reject) => {
    abort = () => { controller.abort(); reject(cancelled()); };
    parent.addEventListener("abort", abort, { once: true });
    timer = setTimeout(() => { controller.abort(); reject(new Error(code)); }, ms);
  });
  try {
    if (parent.aborted) { abort(); return await interrupted; }
    return await Promise.race([interrupted, work(controller.signal)]);
  } finally {
    clearTimeout(timer);
    parent.removeEventListener("abort", abort);
    controller.abort();
  }
}
