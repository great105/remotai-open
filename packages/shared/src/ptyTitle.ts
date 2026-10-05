import type { PtySessionInfo } from "./types";
import { agentDisplayName } from "./agentRegistry";

// Форматирование времени карточки живёт в отдельном модуле, но выходит наружу
// вместе с заголовком: барель `index.ts` правится другими пакетами задач,
// поэтому реэкспорт сделан здесь. Как только `export * from "./timeAgo"`
// появится в index.ts, эту строку можно убрать.
export { formatAgoValue, formatAgo, formatDurationMs } from "./timeAgo";

/**
 * Канонический заголовок терминала для списка и главной.
 * SSH никогда не выглядит как сырой `ssh · ssh:root@host`.
 */
export function ptyDisplayTitle(
  session: Pick<
    PtySessionInfo,
    "name" | "kind" | "shell" | "ssh_host" | "ssh_user" | "cwd" | "agent_kind"
  >,
  options: { preferAgent?: boolean } = {},
): string {
  if (session.name) return session.name;
  if (session.kind === "ssh" || session.shell === "ssh") {
    const target = session.ssh_host
      ? `${session.ssh_user ? `${session.ssh_user}@` : ""}${session.ssh_host}`
      : (session.cwd || "").replace(/^ssh:/i, "");
    return target ? `SSH · ${target}` : "SSH";
  }
  const shell = (session.shell || "terminal")
    .split(/[\\/]/)
    .pop()!
    .replace(/\.exe$/i, "");
  const agent = options.preferAgent && session.agent_kind
    && session.agent_kind !== "shell" && session.agent_kind !== "other"
    ? agentDisplayName(session.agent_kind)
    : "";
  const tail = (session.cwd || "").split(/[\\/]/).filter(Boolean).pop();
  const head = agent || shell;
  return tail ? `${head} · ${tail}` : head;
}
