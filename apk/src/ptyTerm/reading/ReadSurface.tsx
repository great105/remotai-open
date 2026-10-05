import { useEffect, useLayoutEffect, useMemo, useRef, useState, type KeyboardEvent } from "react";
import { t } from "../../i18n";
import { terminalClipboard } from "../input/ClipboardService";
import { saveBlob } from "../../saveFile";
import { completenessNotice, documentCell, documentPosition, documentRuns, type ReadDocument } from "./ReadDocument";
import { decayVelocity, edgeDirection, releaseVelocity } from "../gestures/GestureController";
import { findInText, revealRange } from "./revealRange";
import {
  captureAction, consumeAction, moveBoundary, selectionRange, wordRange, type ActionSnapshot, type TextRange,
} from "../selection/SelectionController";
import "./readSurface.css";

export interface ReadSurfaceProps {
  document: ReadDocument;
  initialRange: TextRange;
  initialRow: number;
  cellWidth: number;
  cellHeight: number;
  fontFamily: string;
  fontSize: number;
  hasNewOutput: boolean;
  notice?: string;
  older?(): void;
  newer?(): void;
  pending?: boolean;
  onClose(): void;
}

/** One owner for this frozen surface. The live xterm keeps parsing behind it. */
export function ReadSurface(props: ReadSurfaceProps) {
  const doc = props.document;
  const [range, setRange] = useState(props.initialRange);
  const rangeRef = useRef(range);
  // released — кнопку уже отпустили, и click вот-вот придёт (T-16).
  const actionSelection = useRef<{ snap: ActionSnapshot<"copy" | "export">; released: boolean } | null>(null);
  const [textMode, setTextMode] = useState(doc.source === "agent");
  const [scrollTop, setScrollTop] = useState(0);
  const [scrollLeft, setScrollLeft] = useState(0);
  const [width, setWidth] = useState(400);
  const [height, setHeight] = useState(400);
  const [message, setMessage] = useState("");
  const [query, setQuery] = useState("");
  const [searchOpen, setSearchOpen] = useState(false);
  const scroll = useRef<HTMLDivElement>(null);
  const textElement = useRef<HTMLPreElement>(null);
  const surface = useRef<HTMLElement>(null);
  const glyphScale = useMemo(() => {
    const canvas = document.createElement("canvas"), context = canvas.getContext("2d");
    if (context) context.font = `${props.fontSize}px ${props.fontFamily}`;
    const widths = new Map<string, number>();
    return (text: string, available: number) => {
      let natural = widths.get(text);
      if (natural == null) {
        natural = context?.measureText(text).width ?? available;
        if (widths.size < 1024) widths.set(text, natural);
      }
      return natural > available ? available / natural : 1;
    };
  }, [props.fontFamily, props.fontSize]);
  const raf = useRef<number | null>(null);
  const drag = useRef<{ pointer: number; end: "start" | "end"; anchor: number; x: number; y: number; grabX: number; grabY: number; rect: DOMRect } | null>(null);
  const edgeTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const pan = useRef<{ pointer: number; x: number; y: number; moved: boolean; stopped: boolean;
    samples: { dx: number; dy: number; t: number }[] } | null>(null);
  const inertia = useRef<number | null>(null);
  const longPress = useRef<ReturnType<typeof setTimeout> | null>(null);
  const cancelDrag = () => {
    if (inertia.current != null) cancelAnimationFrame(inertia.current);
    inertia.current = null;
    drag.current = null;
    pan.current = null;
    if (longPress.current != null) clearTimeout(longPress.current);
    longPress.current = null;
    if (edgeTimer.current != null) clearTimeout(edgeTimer.current);
    edgeTimer.current = null;
  };
  const publish = () => {
    if (raf.current != null) return;
    raf.current = requestAnimationFrame(() => {
      raf.current = null;
      setRange({ ...rangeRef.current });
      setScrollTop(scroll.current?.scrollTop ?? 0);
      setScrollLeft(scroll.current?.scrollLeft ?? 0);
    });
  };
  const update = (next: TextRange) => { rangeRef.current = next; publish(); };
  useLayoutEffect(() => {
    const el = scroll.current;
    if (!el) return;
    if (!textMode) el.scrollTop = Math.max(0, props.initialRow * props.cellHeight);
    setScrollTop(el.scrollTop);
    const observer = new ResizeObserver(() => { cancelDrag(); setHeight(el.clientHeight); setWidth(el.clientWidth); });
    observer.observe(el);
    return () => observer.disconnect();
  }, [textMode]);
  useEffect(() => () => { if (raf.current != null) cancelAnimationFrame(raf.current); }, []);
  useEffect(() => { surface.current?.querySelector<HTMLButtonElement>("button")?.focus({ preventScroll: true }); }, []);
  useLayoutEffect(() => {
    const node = textElement.current?.firstChild;
    if (!textMode || !node) return;
    const native = window.getSelection();
    const selected = window.document.createRange();
    selected.setStart(node, rangeRef.current.start);
    selected.setEnd(node, rangeRef.current.end);
    native?.removeAllRanges(); native?.addRange(selected);
    const changed = () => {
      const selection = window.getSelection();
      if (selection?.anchorNode !== node || selection.focusNode !== node) {
        update({ start: 0, end: 0 });
        return;
      }
      // A caret means no selection, including a caret inside a combining cell.
      update(selection.isCollapsed ? { start: selection.anchorOffset, end: selection.anchorOffset }
        : selectionRange(doc, selection.anchorOffset, selection.focusOffset));
    };
    document.addEventListener("selectionchange", changed);
    return () => {
      document.removeEventListener("selectionchange", changed);
      if (window.getSelection()?.anchorNode === node) window.getSelection()?.removeAllRanges();
    };
  }, [textMode, doc]);

  const at = (x: number, y: number, rect: DOMRect) => documentCell(doc,
    Math.floor((y - rect.top + (scroll.current?.scrollTop ?? 0)) / props.cellHeight),
    Math.floor((x - rect.left + (scroll.current?.scrollLeft ?? 0)) / props.cellWidth));
  const extend = () => {
    const d = drag.current;
    if (!d) return;
    const offset = at(d.x - d.grabX, d.y - d.grabY, d.rect);
    update(selectionRange(doc, d.anchor, offset));
  };
  const releasePan = () => {
    const p = pan.current;
    cancelDrag();
    if (!p?.moved || p.stopped) return;
    const released = Date.now();
    let vx = releaseVelocity(p.samples.map(s => ({ dy: s.dx, t: s.t })), released);
    let vy = releaseVelocity(p.samples, released);
    if (Math.abs(vx) < 0.4) vx = 0;
    if (Math.abs(vy) < 0.4) vy = 0;
    if (!vx && !vy) return;
    let previous = performance.now();
    const tick = (now: number) => {
      const el = scroll.current;
      if (!el || document.hidden) { cancelDrag(); return; }
      const elapsed = Math.min(50, now - previous);
      previous = now;
      const x = el.scrollLeft, y = el.scrollTop;
      el.scrollLeft -= vx * elapsed; el.scrollTop -= vy * elapsed;
      vx = el.scrollLeft === x ? 0 : decayVelocity(vx, elapsed);
      vy = el.scrollTop === y ? 0 : decayVelocity(vy, elapsed);
      if (Math.max(Math.abs(vx), Math.abs(vy)) > 0.05) inertia.current = requestAnimationFrame(tick);
      else inertia.current = null;
    };
    inertia.current = requestAnimationFrame(tick);
  };
  const edgeTick = () => {
    edgeTimer.current = null;
    const d = drag.current, el = scroll.current;
    if (!d || !el || document.hidden) return;
    // T-12: по обеим осям — документ на 240 колонок едет вбок за ручкой.
    const { dx, dy } = edgeDirection({ x: d.x, y: d.y }, d.rect);
    if (!dx && !dy) return;
    const top = el.scrollTop, left = el.scrollLeft;
    el.scrollTop += dy * props.cellHeight;
    el.scrollLeft += dx * props.cellWidth;
    // Упёрлись в край документа: ехать некуда, таймер не держим. Следующее
    // движение ручки запустит тик заново.
    if (el.scrollTop === top && el.scrollLeft === left) return;
    extend();
    edgeTimer.current = setTimeout(edgeTick, 100);
  };
  useEffect(() => {
    // No polling in idle/background. Edge ticks exist only during an edge drag.
    const cancel = cancelDrag;
    window.addEventListener("blur", cancel);
    document.addEventListener("visibilitychange", cancel);
    return () => {
      cancel();
      window.removeEventListener("blur", cancel);
      document.removeEventListener("visibilitychange", cancel);
    };
  }, [doc, props.cellHeight]);

  const selectedText = () => {
    if (textMode) {
      const selected = window.getSelection();
      return selected && textElement.current?.contains(selected.anchorNode) && textElement.current.contains(selected.focusNode)
        && !selected.isCollapsed ? selected.toString() : "";
    }
    return doc.text.slice(rangeRef.current.start, rangeRef.current.end);
  };
  // Native focus changes caused by this action may collapse Selection. Capture
  // before that change, and consume the snapshot only for this button activation.
  // T-16: снимок живёт одну операцию. Палец или мышь, ушедшие с НАЖАТОЙ
  // кнопки, отменяют его: click не будет. После pointerup touch-указатель
  // тоже получает pointerleave и lostpointercapture, но click ещё впереди —
  // такой снимок остаётся, а срок отсчитывается от отпускания.
  const dropPressed = (action: "copy" | "export") => {
    const current = actionSelection.current;
    if (current?.snap.action === action && !current.released) actionSelection.current = null;
  };
  const selectionAction = (action: "copy" | "export") => ({
    onPointerDown: () => { actionSelection.current = { snap: captureAction(action, selectedText(), Date.now()), released: false }; },
    onPointerUp: () => {
      const current = actionSelection.current;
      if (current?.snap.action === action && !current.released) {
        actionSelection.current = { snap: captureAction(action, current.snap.text, Date.now()), released: true };
      }
    },
    onPointerLeave: () => dropPressed(action),
    onLostPointerCapture: () => dropPressed(action),
    onPointerCancel: () => { actionSelection.current = null; },
    onBlur: () => { if (actionSelection.current?.snap.action === action) actionSelection.current = null; },
    onKeyDown: (e: KeyboardEvent<HTMLButtonElement>) => {
      if (e.key === "Enter" || e.key === " ") {
        actionSelection.current = { snap: captureAction(action, selectedText(), Date.now()), released: true };
      }
    },
  });
  const takeSelection = (action: "copy" | "export") => {
    const captured = actionSelection.current;
    actionSelection.current = null;
    return consumeAction(captured?.snap, action, Date.now()) ?? selectedText();
  };
  const copy = async (text: string) => {
    setMessage(text ? t(await terminalClipboard.write(text) ? "pty.copied" : "pty.copyFailed") : t("pty.nothingToCopy"));
    // Keep the frozen document open; the next action reads its current selection.
  };
  const exportSelection = async (text: string) => {
    if (!text) { setMessage(t("pty.nothingToCopy")); return; }
    try {
      const outcome = await saveBlob(new Blob([text], { type: "text/plain;charset=utf-8" }), "terminal-selection.txt");
      setMessage(t(outcome === "failed" ? "pty.readExportFailed" : "pty.readExported"));
    } catch { setMessage(t("pty.readExportFailed")); }
  };
  const find = (direction: -1 | 1) => {
    if (!query) return;
    const index = findInText(doc.text, query, rangeRef.current, direction);
    if (index < 0) { setMessage(t("pty.readNotFound")); return; }
    const next = selectionRange(doc, index, index + query.length);
    update(next);
    if (textMode && textElement.current?.firstChild) {
      const found = window.document.createRange();
      found.setStart(textElement.current.firstChild, next.start);
      found.setEnd(textElement.current.firstChild, next.end);
      const selection = window.getSelection();
      selection?.removeAllRanges(); selection?.addRange(found);
      const rect = found.getBoundingClientRect(), el = scroll.current;
      if (el) el.scrollTop += rect.top - el.getBoundingClientRect().top - 30;
    } else if (scroll.current) {
      const el = scroll.current;
      const target = revealRange(doc, next, { left: el.scrollLeft, width: el.clientWidth,
        cellWidth: props.cellWidth, cellHeight: props.cellHeight });
      el.scrollLeft = target.left; el.scrollTop = target.top;
    }
    setMessage("");
  };
  const first = Math.max(0, Math.floor(scrollTop / props.cellHeight) - 2);
  const last = Math.min(doc.lines.length, first + Math.ceil(height / props.cellHeight) + 5);
  const handle = (end: "start" | "end") => {
    const pos = documentPosition(doc, range[end]);
    return <button type="button" key={end} className={`pty-read-handle ${end}`}
      aria-label={t(end === "start" ? "pty.readSelectionStart" : "pty.readSelectionEnd")}
      style={{ left: Math.max(22, pos.col * props.cellWidth), top: (pos.row + 1) * props.cellHeight }}
      data-boundary={end}
      onKeyDown={e => {
        if (e.key !== "ArrowLeft" && e.key !== "ArrowRight") return;
        e.preventDefault(); update(moveBoundary(doc, rangeRef.current, end, e.key === "ArrowLeft" ? -1 : 1));
      }}><span /></button>;
  };

  return <section ref={surface} className="pty-read-surface" role="dialog" aria-modal="true" aria-label={t("pty.readMode")}
    // Новое касание где угодно начинает новую операцию: старый снимок кнопки
    // (T-16) не доживает до неё. Собственный pointerdown кнопки сработает
    // позже, в фазе цели, и снимет новый.
    onPointerDownCapture={() => { actionSelection.current = null; }}
    onKeyDown={e => {
      if (e.key !== "Tab") return;
      const controls = Array.from(surface.current?.querySelectorAll<HTMLElement>('button,input,[tabindex="0"]') ?? [])
        .filter(el => el.getClientRects().length > 0);
      const index = controls.indexOf(window.document.activeElement as HTMLElement);
      const next = e.shiftKey ? index - 1 : index + 1;
      if (next < 0 || next >= controls.length) { e.preventDefault(); controls[next < 0 ? controls.length - 1 : 0]?.focus(); }
    }}>
    <div className="pty-read-heading">
      <span>{t(doc.source === "screen" ? "pty.readScreen" : doc.source === "agent" ? "pty.readAgent" : "pty.readTerminal")}</span>
      {doc.sourceDetail && <small>{doc.sourceDetail}</small>}
      <button type="button" onClick={props.onClose}>{t(props.hasNewOutput ? "pty.readNewOutput" : "pty.readReturn")}</button>
    </div>
    <div className="pty-read-actions">
      <button type="button" {...selectionAction("copy")} onClick={() => void copy(takeSelection("copy"))}>{t("pty.copy")}</button>
      <button type="button" onClick={() => { update({ start: 0, end: doc.text.length });
        if (textMode && textElement.current) { const selection = window.getSelection(), r = window.document.createRange();
          r.selectNodeContents(textElement.current); selection?.removeAllRanges(); selection?.addRange(r); }
      }}>{t("pty.selectAll")}</button>
      {doc.source !== "agent" && <button type="button" onClick={() => setTextMode(v => !v)} aria-pressed={textMode}>{t("pty.readTextMode")}</button>}
      <button type="button" onClick={() => setSearchOpen(v => !v)} aria-expanded={searchOpen}>{t("pty.readSearch")}</button>
      <button type="button" {...selectionAction("export")} onClick={() => void exportSelection(takeSelection("export"))}>{t("pty.readExport")}</button>
      {props.older && <button type="button" disabled={props.pending} onClick={props.older}>{t("pty.readOlderPage")}</button>}
      {props.newer && <button type="button" disabled={props.pending} onClick={props.newer}>{t("pty.readNewerPage")}</button>}
    </div>
    {searchOpen && <form className="pty-read-search" onSubmit={e => { e.preventDefault(); find(1); }}>
      <input aria-label={t("pty.readSearch")} value={query} onChange={e => setQuery(e.target.value)} />
      <button type="button" aria-label={t("pty.readPrevious")} onClick={() => find(-1)}>↑</button>
      <button type="submit" aria-label={t("pty.readNext")}>↓</button>
    </form>}
    <div className="pty-read-notice" role="status">{props.notice || message || t(completenessNotice(doc))}</div>
    <div ref={scroll} className={`pty-read-scroll ${textMode ? "text-mode" : "grid-mode"}`}
      onScroll={publish}
      onPointerDown={e => {
        if (textMode || e.button !== 0) return;
        const el = scroll.current!;
        if (drag.current || pan.current) { cancelDrag(); return; }
        const stopping = inertia.current != null;
        cancelDrag();
        const rect = el.getBoundingClientRect();
        const boundary = (e.target as HTMLElement).closest<HTMLElement>("[data-boundary]")?.dataset.boundary as "start" | "end" | undefined;
        const offset = at(e.clientX, e.clientY, rect);
        if (e.pointerType === "touch" && !boundary) {
          const pointer = e.pointerId, x = e.clientX, y = e.clientY;
          pan.current = { pointer, x, y, moved: false, stopped: stopping, samples: [{ dx: 0, dy: 0, t: Date.now() }] };
          if (!stopping) longPress.current = setTimeout(() => {
            longPress.current = null;
            if (!pan.current || pan.current.moved) return;
            pan.current = null;
            const next = wordRange(doc, offset);
            update(next);
            drag.current = { pointer, end: "end", anchor: next.start, x, y, grabX: 0, grabY: 0, rect };
          }, 500);
          el.setPointerCapture(pointer);
          e.preventDefault(); e.stopPropagation();
          return;
        }
        const next = boundary ? rangeRef.current : wordRange(doc, offset);
        update(next);
        const end = boundary ?? "end";
        const pos = documentPosition(doc, next[end]);
        // A 44px hit target can be grabbed off-centre. Preserve that distance
        // so the boundary does not jump to the finger on the first movement.
        const grabX = boundary ? e.clientX - rect.left + el.scrollLeft - pos.col * props.cellWidth : 0;
        const grabY = boundary ? e.clientY - rect.top + el.scrollTop - (pos.row + 0.5) * props.cellHeight : 0;
        drag.current = { pointer: e.pointerId, end, anchor: end === "start" ? next.end : next.start,
          x: e.clientX, y: e.clientY, grabX, grabY, rect };
        el.setPointerCapture(e.pointerId);
        e.preventDefault(); e.stopPropagation();
      }}
      onPointerMove={e => {
        const p = pan.current;
        if (p?.pointer === e.pointerId) {
          const dx = e.clientX - p.x, dy = e.clientY - p.y;
          if (!p.moved && Math.max(Math.abs(dx), Math.abs(dy)) < 8) return;
          // A tap only stops inertia; crossing the drag threshold starts a new pan.
          p.stopped = false;
          p.moved = true;
          if (longPress.current != null) clearTimeout(longPress.current);
          longPress.current = null;
          scroll.current!.scrollTop -= dy; scroll.current!.scrollLeft -= dx;
          const now = Date.now();
          p.samples = p.samples.filter(s => s.t >= now - 100);
          p.samples.push({ dx, dy, t: now });
          p.x = e.clientX; p.y = e.clientY;
          return;
        }
        if (drag.current?.pointer !== e.pointerId) return;
        drag.current.x = e.clientX; drag.current.y = e.clientY; extend();
        if (edgeTimer.current == null) edgeTick();
      }}
      onPointerUp={releasePan}
      onPointerCancel={cancelDrag}
      onLostPointerCapture={e => {
        if (pan.current?.pointer === e.pointerId || drag.current?.pointer === e.pointerId) cancelDrag();
      }}>
      {textMode ? <pre ref={textElement} className="pty-read-text" tabIndex={0}>{doc.text}</pre> :
        <div className="pty-read-grid" style={{ height: doc.lines.length * props.cellHeight + 44, width: doc.cols * props.cellWidth,
          fontFamily: props.fontFamily, fontSize: props.fontSize, lineHeight: `${props.cellHeight}px` }}>
          {doc.lines.slice(first, last).map((line, index) => {
            const runs = documentRuns(line, Math.max(0, Math.floor(scrollLeft / props.cellWidth) - 2),
              Math.ceil((scrollLeft + width) / props.cellWidth) + 2);
            return <div key={first + index} className="pty-read-line"
              style={{ top: (first + index) * props.cellHeight, width: doc.cols * props.cellWidth, height: props.cellHeight }}>
              {runs.map(run => {
                // Fallback emoji can be wider than xterm's declared cell.
                // Fit the entire glyph into its box without shifting its peers.
                const glyph = <span className="pty-read-glyph"
                  style={{ transform: `scaleX(${glyphScale(run.text, run.width * props.cellWidth)})` }}>{run.text}</span>;
                return <span key={run.col} className="pty-read-cell" data-col={run.col}
                  style={{ left: run.col * props.cellWidth, width: run.width * props.cellWidth, height: props.cellHeight }}>
                  {run.start < range.end && run.end > range.start ? <mark>{glyph}</mark> : glyph}
                </span>;
              })}
            </div>;
          })}
          {handle("start")}{handle("end")}
        </div>}
    </div>
    {!textMode && <div className="pty-read-adjust">
      {(["start", "end"] as const).map(end => <span key={end}>
        <span>{t(end === "start" ? "pty.readSelectionStart" : "pty.readSelectionEnd")}</span>
        {([-1, 1] as const).map(dir => <button type="button" key={dir}
          aria-label={`${t(end === "start" ? "pty.readSelectionStart" : "pty.readSelectionEnd")} ${dir < 0 ? "←" : "→"}`}
          onClick={() => update(moveBoundary(doc, rangeRef.current, end, dir))}>{dir < 0 ? "←" : "→"}</button>)}
      </span>)}
    </div>}
  </section>;
}
