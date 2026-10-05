import { useCallback, useLayoutEffect, useRef, useState, type RefObject } from "react";

/** Follow rendered layout, not source tokens. Detached readers retain their offset. */
export function useChatScroll(area: RefObject<HTMLDivElement | null>, key: string, enabled: boolean) {
  const positions = useRef(new Map<string, { top: number; follow: boolean }>());
  const follow = useRef(true);
  const [detached, setDetached] = useState(false);
  const jumpToLatest = useCallback(() => {
    const element = area.current;
    if (!element) return;
    follow.current = true;
    element.scrollTop = element.scrollHeight;
    setDetached(false);
  }, [area]);

  useLayoutEffect(() => {
    const element = area.current;
    if (!element || !enabled) return;
    const saved = positions.current.get(key);
    follow.current = saved?.follow ?? true;
    element.scrollTop = follow.current ? element.scrollHeight : saved!.top;
    let lastTop = element.scrollTop;
    let frame = 0;
    const update = () => {
      frame = 0;
      if (follow.current) element.scrollTop = element.scrollHeight;
      lastTop = element.scrollTop;
      setDetached(!follow.current && element.scrollHeight - element.clientHeight - element.scrollTop > 2);
    };
    const schedule = () => { if (!frame) frame = requestAnimationFrame(update); };
    const onScroll = () => {
      const gap = element.scrollHeight - element.clientHeight - element.scrollTop;
      // Layout-induced scroll events are not a reader opting out. An actual
      // upward movement is, even if it happens during presentation animation.
      if (gap <= 2) follow.current = true;
      else if (element.scrollTop < lastTop - 1) follow.current = false;
      lastTop = element.scrollTop;
      setDetached(!follow.current && gap > 2);
    };
    element.addEventListener("scroll", onScroll, { passive: true });
    const resize = new ResizeObserver(schedule);
    resize.observe(element);
    if (element.firstElementChild) resize.observe(element.firstElementChild);
    const mutations = new MutationObserver(schedule);
    mutations.observe(element, { childList: true, subtree: true, characterData: true, attributes: true });
    update();
    return () => {
      // The commit may already have replaced this DOM with a shorter chat.
      // Save the last observed offset, not its newly clamped scrollTop.
      positions.current.set(key, { top: lastTop, follow: follow.current });
      element.removeEventListener("scroll", onScroll);
      resize.disconnect(); mutations.disconnect(); cancelAnimationFrame(frame);
    };
  }, [area, key, enabled]);
  return { detached: enabled && detached, jumpToLatest };
}
