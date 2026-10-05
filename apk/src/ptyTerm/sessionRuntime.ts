import type { HistoryOwner } from "./altScroll";
import type { TerminalWriteGuard } from "./terminalWriter";
import type { PtyScrollMode } from "@tgcontrol/shared";

export type ScrollOverride = PtyScrollMode;

type RequestTicket = Readonly<{ route: string; generation: number; sequence: number }>;

/** Latest-started request wins; route invalidation rejects every old callback. */
export class LatestOnlyGate {
  private route: string;
  private generation = 1;
  private sequence = 0;

  constructor(route: string) {
    this.route = route;
  }

  begin(): RequestTicket {
    return { route: this.route, generation: this.generation, sequence: ++this.sequence };
  }

  accept(ticket: RequestTicket): boolean {
    return ticket.route === this.route
      && ticket.generation === this.generation
      && ticket.sequence === this.sequence;
  }

  invalidate(nextRoute = this.route): void {
    this.route = nextRoute;
    this.generation++;
    this.sequence = 0;
  }
}

export function effectiveHistoryOwner(auto: HistoryOwner, override: ScrollOverride): HistoryOwner {
  if (override === "terminal") return "terminal";
  if (override === "agent") return "application";
  return auto;
}

/** Only a missing/non-positive PID is transient. A known PID with no agent kind
 * is the foreground shell/other process and must form a real reset boundary. */
export function foregroundKey(input: { fgPid?: number; agentKind?: string }): string {
  const pid = input.fgPid ?? 0;
  if (pid <= 0) return "";
  const kind = input.agentKind?.trim() || "shell";
  return `${kind}:${pid}`;
}

/** Unknown/legacy server values must never invent a forced scroll route. */
export function normalizeScrollOverride(value?: string): ScrollOverride {
  return value === "terminal" || value === "agent" ? value : "auto";
}

/** The automatic classifier keeps its existing observational SSH lifecycle,
 * while local terminals use the authoritative process generation above.
 * These resets never change the person's saved output mode. */
export function scrollClassifierKey(input: {
  fgPid?: number;
  agentKind?: string;
  remote?: boolean;
}): string {
  if (!input.remote) return foregroundKey(input);
  const kind = (input.agentKind || "").trim().toLowerCase();
  const pid = Number.isFinite(input.fgPid) ? Math.trunc(input.fgPid ?? 0) : 0;
  return `remote-observed:${kind}:${pid}`;
}

/** Initial state only names the current generation; there is no older
 * classifier state to discard. Empty/transient observations are ignored. */
export function shouldResetScrollClassifier(previousKey: string, nextKey: string): boolean {
  return previousKey !== "" && nextKey !== "" && previousKey !== nextKey;
}

/** Parser epoch: unlike the resume epoch, every reset/resumed marker is unique. */
export function writerEpochKey(serverEpoch: string, sequence: number): string {
  return `stream:${serverEpoch}:${sequence}`;
}

export function sameRuntimeGuard(a: TerminalWriteGuard, b: TerminalWriteGuard): boolean {
  return a.generation === b.generation && a.epoch === b.epoch;
}

/** Old parser callbacks must not move an applied offset in the new epoch scale. */
export function advanceAppliedOffset(
  current: number,
  end: number,
  batch: TerminalWriteGuard,
  logical: TerminalWriteGuard,
): number {
  return sameRuntimeGuard(batch, logical) ? Math.max(current, end) : current;
}

export interface SyncRuntimeState {
  pageAccum: number;
  queuedProbeLines: number;
}

/**
 * State carried through a protocol marker; reset starts a new screen/app view.
 * `resetEvidence`: живые наблюдения навигации (якорь экрана, слабые ответы)
 * принадлежали прежнему экрану; устойчивые вернутся из памяти той же области.
 */
export function afterSyncMarker(
  marker: "reset" | "resumed",
  state: SyncRuntimeState,
): SyncRuntimeState & { resetEvidence: boolean } {
  if (marker === "resumed") return { ...state, queuedProbeLines: 0, resetEvidence: false };
  return { pageAccum: 0, queuedProbeLines: 0, resetEvidence: true };
}
