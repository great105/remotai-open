import { useEffect } from "react";
import { platform } from "../platform";

/**
 * Close a modal/overlay on Escape (desktop/external keyboard) AND on the
 * platform back gesture where the platform provides one (Android system Back
 * in apk via PlatformAdapter.pushBackHandler; no-op в miniapp/браузере).
 * One hook → consistent dismissal everywhere a modal registers itself.
 */
export function useEscape(isOpen: boolean, onClose: () => void) {
  useEffect(() => {
    if (!isOpen) return;
    const handler = (e: KeyboardEvent) => {
      if (e.key === "Escape") { e.stopPropagation(); onClose(); }
    };
    document.addEventListener("keydown", handler);
    const unpush = platform().pushBackHandler(() => { onClose(); return true; });
    return () => {
      document.removeEventListener("keydown", handler);
      unpush();
    };
  }, [isOpen, onClose]);
}
