/**
 * Только порядок показа. Команды установки и имена приходят из Go-реестра
 * `/api/agents`; клиент больше не хранит второй расходящийся каталог.
 */
export const AGENT_INSTALL_ORDER = [
  "claude", "codex", "kimi", "gemini", "opencode", "kilo", "copilot", "aider",
  "cline", "cursor-agent", "amazon-q",
] as const;

/** Node.js is needed for both the POSIX npm command and its Windows launcher. */
export function usesNpm(command: string): boolean {
  return /(^|\s)npm(?:\.cmd)?\s/i.test(command);
}
