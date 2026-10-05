/** Sections that may be opened by a link to /agents?focus=… . */
export const AGENT_SECTION_IDS = [
  "accounts", "openrouter", "limits", "tokens", "skills", "mcp", "installed", "behaviour",
] as const;

export type AgentSectionId = typeof AGENT_SECTION_IDS[number];

const SECTION_IDS = new Set<string>(AGENT_SECTION_IDS);

export function agentSectionFocus(value: unknown): AgentSectionId | "" {
  return typeof value === "string" && SECTION_IDS.has(value) ? value as AgentSectionId : "";
}

export function agentFocusFromSearch(search: string): AgentSectionId | "" {
  return agentSectionFocus(new URLSearchParams(search).get("focus"));
}
