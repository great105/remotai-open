import { useEffect, useRef, useState } from "react";
import type { Terminal } from "@xterm/xterm";
import { SearchAddon } from "@xterm/addon-search";
import { useEscape } from "../hooks/useEscape";
import { t } from "../i18n";

interface Props {
  terminal: Terminal | null;
  onClose: () => void;
}

/**
 * Floating search bar over the xterm terminal. Owns its own SearchAddon
 * instance — created on mount, disposed on unmount.
 */
export function PtySearchBar({ terminal, onClose }: Props) {
  // Mounted only while open — Back/ESC closes the bar.
  useEscape(true, onClose);
  const [query, setQuery] = useState("");
  const [matchCount, setMatchCount] = useState<{ current: number; total: number } | null>(null);
  const [caseSensitive, setCaseSensitive] = useState(false);
  const addonRef = useRef<SearchAddon | null>(null);
  const inputRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    if (!terminal) return;
    const addon = new SearchAddon();
    terminal.loadAddon(addon);
    addonRef.current = addon;

    const off = addon.onDidChangeResults((res) => {
      if (!res || res.resultCount === 0) {
        setMatchCount(null);
      } else {
        setMatchCount({ current: res.resultIndex + 1, total: res.resultCount });
      }
    });
    inputRef.current?.focus();
    return () => {
      off?.dispose();
      addon.dispose();
      addonRef.current = null;
    };
  }, [terminal]);

  const findNext = () => {
    if (!query) return;
    addonRef.current?.findNext(query, { caseSensitive, decorations: defaultDecor() });
  };
  const findPrev = () => {
    if (!query) return;
    addonRef.current?.findPrevious(query, { caseSensitive, decorations: defaultDecor() });
  };

  const handleKeyDown = (e: React.KeyboardEvent<HTMLInputElement>) => {
    if (e.key === "Escape") {
      e.preventDefault();
      onClose();
    } else if (e.key === "Enter") {
      e.preventDefault();
      if (e.shiftKey) findPrev();
      else findNext();
    }
  };

  return (
    <div className="pty-search-bar">
      <input
        ref={inputRef}
        type="text"
        value={query}
        onChange={(e) => {
          setQuery(e.target.value);
          if (e.target.value) {
            addonRef.current?.findNext(e.target.value, { caseSensitive, decorations: defaultDecor() });
          }
        }}
        onKeyDown={handleKeyDown}
        placeholder={t("pty.searchPlaceholder")}
        className="pty-search-input"
        autoComplete="off"
        autoCorrect="off"
      />
      <button
        className={`pty-search-btn${caseSensitive ? " pty-search-btn-active" : ""}`}
        onClick={() => setCaseSensitive((v) => !v)}
        title={t("pty.searchCase")}
        aria-label={t("pty.searchCase")}
        aria-pressed={caseSensitive}
      >Aa</button>
      <button className="pty-search-btn" onClick={findPrev} disabled={!query} aria-label={t("pty.searchPrev")}>↑</button>
      <button className="pty-search-btn" onClick={findNext} disabled={!query} aria-label={t("pty.searchNext")}>↓</button>
      <span className="pty-search-count">
        {matchCount
          ? `${matchCount.current}/${matchCount.total}`
          : query
          ? t("pty.searchEmpty")
          : ""}
      </span>
      <button className="pty-search-btn pty-search-btn-close" onClick={onClose} aria-label={t("pty.searchClose")}>{"✕"}</button>
    </div>
  );
}

function defaultDecor() {
  return {
    matchBackground: "#3a4d6a",
    matchOverviewRuler: "#58a6ff",
    activeMatchBackground: "#1f6feb",
    activeMatchColorOverviewRuler: "#58a6ff",
  };
}
