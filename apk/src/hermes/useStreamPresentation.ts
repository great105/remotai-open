import { useCallback, useEffect, useState, useSyncExternalStore } from "react";
import { createStreamPresentation } from "./streamPresentation";

const motionQuery = "(prefers-reduced-motion: reduce)";
const reducedMotion = () => typeof window !== "undefined" && typeof window.matchMedia === "function"
  && window.matchMedia(motionQuery).matches;
const serverMotion = () => true;

/** Smooth only explicitly live assistant text. History/SSR is always complete. */
export function useStreamPresentation(source: string, streaming = false, streamKey?: string) {
  const [presentation] = useState(() => createStreamPresentation(source, streamKey));
  const subscribeMotion = useCallback((changed: () => void) => {
    if (!streaming || typeof window === "undefined" || typeof window.matchMedia !== "function") return () => {};
    const media = window.matchMedia(motionQuery);
    media.addEventListener("change", changed);
    return () => media.removeEventListener("change", changed);
  }, [streaming]);
  const motion = useSyncExternalStore(subscribeMotion, reducedMotion, serverMotion);
  useSyncExternalStore(presentation.subscribe, presentation.getSnapshot, () => source);
  useEffect(() => {
    presentation.update(source, { streaming, reducedMotion: motion, streamKey });
  }, [presentation, source, streaming, motion, streamKey]);
  useEffect(() => () => presentation.dispose(), [presentation]);
  return presentation.getDisplay(source, { streaming, reducedMotion: motion, streamKey });
}
