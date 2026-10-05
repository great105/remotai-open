import { t } from "../i18n";
import { agentDisplayName } from "../agentRegistry";

export type AgentKind =
  | "claude" | "codex" | "gemini" | "kimi" | "aider" | "opencode"
  | "copilot" | "cursor-agent" | "cline" | "kilo" | "amazon-q" | "grok"
  | "git" | "node" | "go" | "python" | "docker"
  | "shell" | "other" | "";

const ICONS: Record<string, string> = {
  claude:         "🤖",
  codex:          "🧠",
  gemini:         "✨",
  kimi:           "🌙",
  aider:          "🔵",
  opencode:       "🟣",
  copilot:        "⚫",
  "cursor-agent": "⬛",
  cline:          "🤎",
  kilo:           "🔴",
  "amazon-q":     "🟡",
  grok:           "✖️",
  git:    "🔀",
  node:   "📦",
  go:     "🐹",
  python: "🐍",
  docker: "🐳",
  shell:  ">_",
  other:  "▶",
};

/** isAgentKind reports whether kind is an interactive AI coding agent
 *  (mirrors pty.IsAgentKind on the backend). */
export function isAgentKind(kind: string | undefined | null): boolean {
  switch (kind) {
    case "claude": case "codex": case "gemini": case "kimi": case "aider":
    case "opencode": case "copilot": case "cursor-agent": case "cline":
    case "kilo": case "amazon-q": case "grok":
      return true;
  }
  return false;
}

interface Props {
  kind: AgentKind;
  name?: string;
  status?: "idle" | "running" | "waiting";
  /** When true, render only the icon (compact form for list cards). */
  compact?: boolean;
  /**
   * Аккаунт нейросети, под которым запущен агент («дом», «работа»).
   *
   * Без него два терминала с одним и тем же Codex в списке неразличимы: бейдж
   * у обоих одинаковый, а лимиты и история у аккаунтов разные. Показываем рядом
   * с названием агента, а не вместо него — важны оба факта.
   */
  account?: string;
}

export function ProcessBadge({ kind, name, status, compact, account }: Props) {
  if (!kind || kind === "shell") return null;
  const icon = ICONS[kind] || ICONS.other;
  const label =
    kind === "other"
      ? t("pty.kind.other", { name: name || "—" })
      : isAgentKind(kind) ? agentDisplayName(kind) : t(`pty.kind.${kind}`);
  const cls =
    `process-badge process-badge-${kind}` +
    (status ? ` process-badge-${status}` : "");

  // В подсказке аккаунт нужен и компактному бейджу: иконка одинаковая у обоих
  // Codex, и без подписи различить их нельзя вовсе.
  const fullLabel = account ? `${label} · ${account}` : label;

  if (compact) {
    return (
      <span className={cls} title={fullLabel}>
        <span className="process-badge-icon">{icon}</span>
      </span>
    );
  }
  return (
    <span className={cls} title={fullLabel}>
      <span className="process-badge-icon">{icon}</span>
      <span className="process-badge-label">{label}</span>
      {account && <span className="process-badge-account">{account}</span>}
      {status && (
        <span className="process-badge-status">{t(`pty.fg.${status}`)}</span>
      )}
    </span>
  );
}
