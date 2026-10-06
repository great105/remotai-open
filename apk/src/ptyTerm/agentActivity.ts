/** A repaint is output, not evidence that an agent started a new turn. */
export function agentActivityBusy(status: string | undefined, recentOutput: boolean): boolean {
  if (status === "working" || status === "stalled") return true;
  if (["ready", "idle", "waiting", "error", "dead"].includes(status ?? "")) return false;
  return recentOutput; // Compatibility with an agent that exposes no status.
}
