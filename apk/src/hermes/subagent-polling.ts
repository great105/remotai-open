export interface SubagentPollOptions { fetch: (signal:AbortSignal)=>Promise<unknown>; accept:(value:unknown)=>boolean; error:(error:unknown)=>void; visible:()=>boolean; subscribe:(change:()=>void)=>(()=>void) }
export function startSubagentActivityPolling(options:SubagentPollOptions):()=>void {
  let disposed = false, stopped = false, busy = false;
  let timer: ReturnType<typeof setTimeout> | undefined;
  let controller: AbortController | undefined;
  async function tick() {
    if (disposed || stopped || busy || !options.visible()) return;
    busy = true; controller = new AbortController();
    const active = controller;
    try {
      const value = await options.fetch(active.signal);
      if (!disposed && !active.signal.aborted && options.visible()) stopped = !options.accept(value);
    } catch (error) {
      if (!disposed && !active.signal.aborted && options.visible()) options.error(error);
    } finally {
      busy = false;
      if (!disposed && !stopped && options.visible()) timer = setTimeout(()=>void tick(),2500);
    }
  }
  const unsubscribe = options.subscribe(()=> {
    clearTimeout(timer);
    if (!options.visible()) controller?.abort();
    else void tick();
  });
  void tick();
  return ()=>{ disposed=true;clearTimeout(timer);controller?.abort();unsubscribe(); };
}
