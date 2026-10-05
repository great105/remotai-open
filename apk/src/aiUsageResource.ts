/** One result and one in-flight read per computer. No React or browser globals. */
export function createUsageResource<T>(initial: T, fetcher: (force: boolean, previous: T) => Promise<T>) {
  let value = initial, generation = 0, loadedAt = 0;
  let flight: { generation: number; promise: Promise<T> } | undefined;
  const listeners = new Set<() => void>();
  const publish = (next: T) => { value = next; for (const listener of listeners) listener(); };
  const load = async (force = false): Promise<T> => {
    if (flight) {
      const current = flight;
      await current.promise;
      return current.generation === generation ? value : load(force);
    }
    if (!force && loadedAt && Date.now() - loadedAt < 10_000) return value;
    const started = generation;
    const promise = Promise.resolve().then(() => fetcher(force, value)).then(next => {
      if (generation === started) { loadedAt = Date.now(); publish(next); }
      return value;
    }).finally(() => { if (flight?.promise === promise) flight = undefined; });
    flight = { generation: started, promise };
    await promise;
    return started === generation ? value : load(force);
  };
  return {
    get: () => value,
    subscribe: (listener: () => void) => { listeners.add(listener); return () => { listeners.delete(listener); }; },
    load,
    invalidate: () => { generation++; loadedAt = 0; publish(initial); },
  };
}
