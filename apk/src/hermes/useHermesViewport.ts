import { useEffect, useState } from "react";
import { Capacitor } from "@capacitor/core";
import { Keyboard } from "@capacitor/keyboard";
import { hermesViewport } from "./viewport";

/** Local height owner; never adjusts document/body or other app routes. */
export function useHermesViewport() {
  const [viewport, setViewport] = useState(() => ({height:window.innerHeight,top:0,keyboard:false}));
  useEffect(() => {
    let alive = true;
    let nativeHeight = 0;
    let keyboardObserved = false;
    let baselineHeight = window.innerHeight;
    let baselineWidth = window.innerWidth;
    let previousLayoutHeight = window.innerHeight;
    let rotatedNativeResize = false;
    const visual = window.visualViewport;
    const measure = () => {
      const focused = typeof document !== "undefined" && /^(INPUT|TEXTAREA)$/.test(document.activeElement?.tagName || "");
      const scale = visual?.scale ?? 1;
      if (!nativeHeight && !focused && Math.abs(scale - 1) < .05) {
        baselineHeight = window.innerHeight; baselineWidth = window.innerWidth;
      } else if (Math.abs(window.innerWidth - baselineWidth) > 80) {
        // Preserve the established resize/overlay mode across orientation changes.
        // The new layout height is already available space in a resized WebView.
        rotatedNativeResize = nativeHeight > 0 && (rotatedNativeResize || baselineHeight - previousLayoutHeight > 120);
        baselineHeight = window.innerHeight; baselineWidth = window.innerWidth;
      }
      if (rotatedNativeResize && nativeHeight > 0) baselineHeight = window.innerHeight + nativeHeight;
      if (!nativeHeight) rotatedNativeResize = false;
      previousLayoutHeight = window.innerHeight;
      const next = hermesViewport({layoutHeight:window.innerHeight,baselineHeight,visualHeight:visual?.height ?? window.innerHeight,offsetTop:visual?.offsetTop ?? 0,scale,focused:focused || keyboardObserved,nativeHeight});
      keyboardObserved = next.keyboard;
      if (alive) setViewport(previous => previous.height === next.height && previous.top === next.top && previous.keyboard === next.keyboard ? previous : next);
    };
    const show = (event: {keyboardHeight:number}) => { nativeHeight = event.keyboardHeight; measure(); };
    const hide = () => { nativeHeight = 0; measure(); };
    window.addEventListener?.("resize", measure);
    visual?.addEventListener("resize", measure); visual?.addEventListener("scroll", measure);
    if (typeof document !== "undefined") { document.addEventListener("focusin",measure); document.addEventListener("focusout",measure); }
    const nativeListeners = Capacitor.isNativePlatform() ? [Keyboard.addListener("keyboardDidShow",show),Keyboard.addListener("keyboardDidHide",hide)] : [];
    measure();
    return () => {
      alive = false;
      window.removeEventListener?.("resize",measure);
      visual?.removeEventListener("resize",measure); visual?.removeEventListener("scroll",measure);
      if (typeof document !== "undefined") { document.removeEventListener("focusin",measure); document.removeEventListener("focusout",measure); }
      nativeListeners.forEach(listener => void listener.then(handle=>handle.remove()));
    };
  }, []);
  return viewport;
}
