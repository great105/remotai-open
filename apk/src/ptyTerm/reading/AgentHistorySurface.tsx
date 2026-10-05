import { useEffect, useMemo, useRef, useState } from "react";
import { t } from "../../i18n";
import type { AgentHistoryPage } from "./AgentHistoryClient";
import { ReadSurface } from "./ReadSurface";
import { nextDocumentVersion, type ReadDocument } from "./ReadDocument";
import {
  currentHistoryPage, historyErrorNotice, historyNeighbourEvicted, historyTarget, moveHistory,
  openHistoryStack, placeHistoryPage, type AgentHistoryStack,
} from "./agentHistoryPages";

export function AgentHistorySurface({ read, onClose, session }: {
  read(cursor: string): Promise<AgentHistoryPage>; onClose(): void; session: string;
}) {
  // Стопка прочитанных страниц (T-23): «старше» и «новее» без потери последнего ответа.
  const [stack, setStack] = useState<AgentHistoryStack | null>(null);
  const [pending, setPending] = useState(true);
  const [error, setError] = useState("");
  const generation = useRef(0);
  const surface = useRef<HTMLElement>(null);
  const closeButton = useRef<HTMLButtonElement>(null);
  const showError = (e: unknown) => {
    const notice = historyErrorNotice(e);
    // Смена источника сбрасывает стопку: страницы другого разговора не склеиваем.
    if (notice.key === "pty.readSourceChanged") setStack(null);
    setError(t(notice.key, notice.params));
  };
  // Одна явная просьба за раз; ответ устаревшей просьбы не применяется.
  const request = async (cursor: string, apply: (page: AgentHistoryPage) => void) => {
    const own = ++generation.current;
    setPending(true); setError("");
    try {
      const page = await read(cursor);
      if (generation.current === own) apply(page);
    } catch (e) {
      if (generation.current === own) showError(e);
    } finally { if (generation.current === own) setPending(false); }
  };
  const openLatest = () => request("", page => setStack(openHistoryStack(page)));
  const go = (direction: "older" | "newer") => {
    const from = stack;
    const target = from && historyTarget(from, direction);
    if (!from || !target) return;
    if (target.cached) { setStack(moveHistory(from, target.index)); setError(""); return; }
    void request(target.cursor, page => {
      const next = placeHistoryPage(from, target.index, target.cursor, page);
      if (next) setStack(next);
      else { setStack(null); setError(t("pty.readSourceChanged")); }
    });
  };
  useEffect(() => {
    const previous = document.activeElement as HTMLElement | null;
    closeButton.current?.focus(); void openLatest();
    return () => { generation.current++; previous?.focus?.({ preventScroll: true }); };
  }, []);
  const page = stack ? currentHistoryPage(stack) : null;
  const position = stack?.position ?? 0;
  const doc = useMemo<ReadDocument | null>(() => {
    if (!page) return null;
    const version = nextDocumentVersion();
    return Object.freeze({ id: `${session}:${page.source}:${version}`, version, session, epoch: page.source, offset: 0,
      geometryRevision: 0, capturedAt: Date.now(), source: "agent" as const, sourceDetail: `${page.agent} ${page.version}`,
      partial: page.partial, historySeq: 0, erasedBefore: false, evictedBefore: false,
      firstBufferRow: 0, truncatedBefore: !!page.next, truncatedAfter: position > 0,
      cols: 0, lines: Object.freeze([]), text: page.text });
  }, [page, position, session]);
  if (!stack || !doc) return <section ref={surface} className="pty-read-surface" role="dialog" aria-modal="true" aria-label={t("pty.readAgent")}
    onKeyDown={e => {
      if (e.key === "Escape") onClose();
      if (e.key !== "Tab") return;
      e.preventDefault();
      const buttons = Array.from(surface.current?.querySelectorAll<HTMLButtonElement>("button") ?? []);
      const index = buttons.indexOf(document.activeElement as HTMLButtonElement);
      buttons[(index + (e.shiftKey ? buttons.length - 1 : 1)) % Math.max(1, buttons.length)]?.focus();
    }}>
    <div className="pty-read-heading"><span>{t("pty.readAgent")}</span><button ref={closeButton} type="button" onClick={onClose}>{t("pty.readReturn")}</button></div>
    <div className="pty-read-notice" role="status">{pending ? t("pty.readLoading") : error}</div>
    {!pending && error && <div className="pty-read-actions">
      <button type="button" onClick={() => void openLatest()}>{t("pty.readHistoryReopen")}</button>
    </div>}
  </section>;
  const older = historyTarget(stack, "older"), newer = historyTarget(stack, "newer");
  return <ReadSurface key={doc.id} document={doc} initialRange={{ start: 0, end: 0 }} initialRow={0}
    cellWidth={9} cellHeight={24} fontFamily="monospace" fontSize={16} hasNewOutput={false} onClose={onClose}
    notice={error || (pending ? t("pty.readLoading") : historyNeighbourEvicted(stack) ? t("pty.readHistoryPagesTrimmed") : "")}
    older={older ? () => go("older") : undefined} newer={newer ? () => go("newer") : undefined} pending={pending} />;
}
