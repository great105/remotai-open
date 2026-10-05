export const AGENT_LAUNCH_WAIT_MS = 12_000;

export interface AgentLaunchAttempt {
  sessionId: string;
  name: string;
  startedAt: number;
  expired?: boolean;
}

export type AgentEntryMode = "unknown" | "shell" | "starting" | "unconfirmed" | "agent";

/** Sending a command is not proof that its process started. */
export function agentEntryMode(input: {
  sessionId: string;
  alive: boolean;
  connected: boolean;
  agentKind?: string;
  agentRunning: boolean;
  attempt: AgentLaunchAttempt | null;
}): AgentEntryMode {
  if (!input.alive || !input.connected || !input.agentKind) return "unknown";
  if (input.agentRunning) return "agent";
  if (input.attempt?.sessionId === input.sessionId) {
    return input.attempt.expired ? "unconfirmed" : "starting";
  }
  return input.agentKind === "shell" ? "shell" : "unknown";
}
