/** Copy the completed mouse selection, including releases outside the host. */
export function attachCopyOnSelect(
  host: HTMLElement,
  selection: () => string,
  copy: (text: string) => void,
  enabled: () => boolean = () => true,
): () => void {
  const doc = host.ownerDocument;
  const releaseTarget = doc.defaultView ?? doc;
  let pressed = false;
  let disposed = false;
  const start = (event: MouseEvent) => {
    pressed = event.button === 0 && enabled();
  };
  const finish = (event: MouseEvent) => {
    if (event.button !== 0 || !pressed) return;
    pressed = false;
    // Window bubbling follows xterm's document mouseup listeners, including
    // ones installed during this drag. A capture-listener microtask runs BEFORE
    // those listeners on native Chromium input. Read here, still in the same
    // user activation, without a timer or render frame.
    queueMicrotask(() => {
      if (disposed || !enabled()) return;
      const text = selection();
      if (text.trim()) copy(text);
    });
  };
  const cancel = () => { pressed = false; };
  host.addEventListener("mousedown", start, { capture: true });
  releaseTarget.addEventListener("mouseup", finish as EventListener);
  doc.defaultView?.addEventListener("blur", cancel);
  return () => {
    disposed = true;
    cancel();
    host.removeEventListener("mousedown", start, { capture: true });
    releaseTarget.removeEventListener("mouseup", finish as EventListener);
    doc.defaultView?.removeEventListener("blur", cancel);
  };
}
