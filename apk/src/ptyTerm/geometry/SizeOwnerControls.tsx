import { useEffect, useRef } from "react";
import { t } from "@tgcontrol/shared";
import type { TerminalControls } from "../runtime/TerminalControls";

export function SizeOwnerControls(props: {
  state: TerminalControls; pending: boolean;
  onAction(op: "claim" | "release" | "transfer", target?: string): void;
  onClose(): void;
}) {
  const { state } = props;
  const root = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const previous = document.activeElement as HTMLElement | null;
    root.current?.querySelector<HTMLButtonElement>("button")?.focus();
    return () => previous?.focus?.({ preventScroll: true });
  }, []);
  const mine = state.owner === state.you;
  const owner = state.viewers.find(v => v.id === state.owner);
  const limited = owner && state.viewers.some(v => v.cols > 0 && v.rows > 0 && (v.cols < owner.cols || v.rows < owner.rows));
  return <div className="modal-overlay" onClick={props.onClose}>
    <div ref={root} className="modal-sheet" role="dialog" aria-modal="true" aria-label={t("pty.sizeControl")}
      onClick={e => e.stopPropagation()} onKeyDown={e => {
        if (e.key === "Escape") { e.stopPropagation(); props.onClose(); }
        if (e.key !== "Tab") return;
        const controls = [...root.current!.querySelectorAll<HTMLButtonElement>("button:not(:disabled)")];
        const first = controls[0], last = controls[controls.length - 1];
        if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last?.focus(); }
        else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first?.focus(); }
      }}>
      <div className="modal-title">{t("pty.sizeControl")}</div>
      <p>{t(!state.owner ? "pty.sizeAutomatic" : mine ? "pty.sizeOwnedHere" : "pty.sizeOwnedElsewhere")}</p>
      {limited && <p role="status">{t("pty.sizeSharedLimit")}</p>}
      {state.error && <p role="status">{t("pty.sizeChanged")}</p>}
      <div style={{ display: "grid", gap: 8, marginBlock: 12 }}>
        {!state.owner && <button className="btn btn-primary" disabled={props.pending}
          onClick={() => props.onAction("claim")}>{t("pty.sizeClaim")}</button>}
        {mine && <>
          <button className="btn btn-secondary" disabled={props.pending}
            onClick={() => props.onAction("release")}>{t("pty.sizeRelease")}</button>
          {state.viewers.filter(v => v.id !== state.you && v.canOwn && v.cols > 1 && v.rows > 1).map(v =>
            <button key={v.id} className="btn btn-secondary" disabled={props.pending}
              onClick={() => props.onAction("transfer", v.id)}>
              {t("pty.sizeTransfer", { id: v.id, cols: v.cols, rows: v.rows })}
            </button>)}
        </>}
      </div>
      <p>{t("pty.sizeLeaseHint")}</p>
      <button className="btn btn-secondary" onClick={props.onClose}>{t("modal.close")}</button>
    </div>
  </div>;
}
