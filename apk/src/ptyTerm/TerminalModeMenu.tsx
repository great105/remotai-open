import { useEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { useEscape } from "@tgcontrol/shared";
import { IconCheck, IconChevron, IconClose, IconTerminal } from "../components/icons";
import { t } from "../i18n";
import type { ScrollOverride } from "./sessionRuntime";
import "./modeMenu.css";

const modes = [
  { value: "auto", label: "pty.scrollModeAuto", hint: "pty.outputModeAutoHint" },
  { value: "terminal", label: "pty.scrollModeTerminal", hint: "pty.outputModeTerminalHint" },
  { value: "agent", label: "pty.scrollModeAgent", hint: "pty.outputModeAgentHint" },
] as const;

/** Changes only scroll ownership; opening the menu never resizes the PTY. */
export function TerminalModeMenu({ value, onChange }: {
  value: ScrollOverride;
  onChange: (value: ScrollOverride) => void;
}) {
  const [open, setOpen] = useState(false);
  const triggerRef = useRef<HTMLButtonElement>(null);
  const sheetRef = useRef<HTMLDivElement>(null);
  const label = t(modes.find(mode => mode.value === value)!.label);
  useEscape(open, () => setOpen(false));

  useEffect(() => {
    if (!open) return;
    const selected = sheetRef.current?.querySelector<HTMLButtonElement>('[aria-pressed="true"]');
    selected?.focus({ preventScroll: true });
    return () => { triggerRef.current?.focus({ preventScroll: true }); };
  }, [open]);

  return <>
    <button
      ref={triggerRef}
      type="button"
      className="pty-header-action pty-mode-btn"
      aria-label={t("pty.outputModeCurrent", { mode: label })}
      title={t("pty.outputModeCurrent", { mode: label })}
      aria-haspopup="dialog"
      aria-expanded={open}
      onClick={() => setOpen(true)}
    >
      <span className="pty-mode-caption"><IconTerminal size={16} />{t("pty.scrollModeTerminal")}</span>
      <span className="pty-mode-value">{label}<IconChevron size={12} /></span>
    </button>
    {open && createPortal(
      <div className="modal-overlay" onClick={event => {
        if (event.target === event.currentTarget) setOpen(false);
      }}>
        <div
          ref={sheetRef}
          className="modal-sheet pty-mode-sheet"
          role="dialog"
          aria-modal="true"
          aria-label={t("pty.outputModeTitle")}
          onKeyDown={event => {
            // Keep keyboard navigation inside the picker, including Shift+Tab.
            event.stopPropagation();
            if (event.key === "Escape") { event.preventDefault(); setOpen(false); return; }
            if (event.key !== "Tab") return;
            const buttons = Array.from(sheetRef.current?.querySelectorAll<HTMLButtonElement>("button") || []);
            const first = buttons[0], last = buttons[buttons.length - 1];
            if (event.shiftKey && document.activeElement === first) {
              event.preventDefault(); last?.focus();
            } else if (!event.shiftKey && document.activeElement === last) {
              event.preventDefault(); first?.focus();
            }
          }}
        >
          <div className="pty-mode-sheet-heading">
            <h2 className="modal-title">{t("pty.outputModeTitle")}</h2>
            <button type="button" className="icon-btn" aria-label={t("modal.close")} onClick={() => setOpen(false)}>
              <IconClose size={20} />
            </button>
          </div>
          <div role="group" aria-label={t("pty.scrollMode")}>
            {modes.map(mode => <button
              key={mode.value}
              type="button"
              className="pty-mode-option"
              aria-label={`${t("pty.scrollMode")}: ${t(mode.label)}`}
              aria-pressed={value === mode.value}
              onClick={() => { onChange(mode.value); setOpen(false); }}
            >
              <span className="pty-mode-option-copy">
                <span className="pty-mode-option-name">{t(mode.label)}</span>
                <span className="pty-mode-option-hint">{t(mode.hint)}</span>
              </span>
              <span className="pty-mode-option-check" aria-hidden="true">
                {value === mode.value && <IconCheck size={20} />}
              </span>
            </button>)}
          </div>
        </div>
      </div>, document.body,
    )}
  </>;
}
