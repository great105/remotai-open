import { createContext, useContext, useLayoutEffect, useState, type ReactNode } from "react";
import { useLocation } from "react-router-dom";

type MoreMenuState = {
  moreOpen: boolean;
  moreQuery: string;
  setMoreOpen: (open: boolean) => void;
  setMoreQuery: (query: string) => void;
};

const MoreMenuContext = createContext<MoreMenuState | null>(null);

/** The loading fallback and ready route share the same menu for one visit. */
export function MoreMenuProvider({ children }: { children: ReactNode }) {
  const { key } = useLocation();
  const [moreOpen, setMoreOpen] = useState(false);
  const [moreQuery, setMoreQuery] = useState("");
  // A real navigation dismisses the menu before paint. Resolving a lazy
  // route keeps the location key and must preserve the user's open search.
  useLayoutEffect(() => { setMoreOpen(false); setMoreQuery(""); }, [key]);
  return <MoreMenuContext.Provider value={{ moreOpen, moreQuery, setMoreOpen, setMoreQuery }}>
    {children}
  </MoreMenuContext.Provider>;
}

export function useMoreMenuState() {
  const state = useContext(MoreMenuContext);
  if (!state) throw new Error("More navigation requires MoreMenuProvider");
  return state;
}
