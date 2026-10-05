import { useEffect, useRef } from "react";

/**
 * Central registry for the Android system Back button (and edge-swipe back).
 *
 * Handlers are invoked LIFO — the most recently registered (the topmost
 * modal/sheet) gets the event first. A handler returns true when it consumed
 * the press (e.g. closed its sheet); false lets the next handler try. When no
 * handler consumes it, the global fallback in App.tsx walks up the route
 * hierarchy and finally minimizes the app at the root (never just exits).
 */
type BackHandler = () => boolean;

const handlers: BackHandler[] = [];
const changeListeners = new Set<() => void>();

function notifyChange() {
  for (const listener of changeListeners) listener();
}

/** Register a back handler; returns an unregister function. */
export function pushBackHandler(h: BackHandler): () => void {
  handlers.push(h);
  notifyChange();
  return () => {
    const i = handlers.indexOf(h);
    if (i >= 0) {
      handlers.splice(i, 1);
      notifyChange();
    }
  };
}

/** Run handlers LIFO until one consumes the press. */
export function runBackHandlers(): boolean {
  for (let i = handlers.length - 1; i >= 0; i--) {
    if (handlers[i]()) return true;
  }
  return false;
}

export function hasBackHandlers(): boolean {
  return handlers.length > 0;
}

export function onBackHandlersChange(listener: () => void): () => void {
  changeListeners.add(listener);
  return () => changeListeners.delete(listener);
}

/**
 * Page-level back handling (e.g. Files: close context → go up a folder).
 * `handler` is read through a ref so callers can pass a fresh closure every
 * render without re-registering (registration order = mount order, which
 * keeps page handlers below later-opened modal handlers).
 */
export function useBackHandler(handler: () => boolean) {
  const ref = useRef(handler);
  ref.current = handler;
  useEffect(() => pushBackHandler(() => ref.current()), []);
}
