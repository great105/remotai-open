import { getLocale } from "@tgcontrol/shared";
import { t } from "@tgcontrol/shared";
import type { SubagentObservation, SubagentJournalEntry } from "./subagent-progress";
import { safeSubagentToolName } from "./subagents";
import "./subagent-progress.css";
export interface HermesSubagentProgressProps { children: SubagentObservation[]; selectedId: string; onSelect: (id:string) => void; onRetry?: () => void; error?: string }
export function subagentToolLabel(name: string | undefined): string {
  const labels: Record<string,string> = {read_file:t("ui.hermessubagentprogress.m6f53376d61"),search_files:t("ui.hermessubagentprogress.m2800f69c37"),write_file:t("ui.hermessubagentprogress.mfd081547e4"),patch:t("ui.hermessubagentprogress.m517f5f46b6"),terminal:t("ui.hermessubagentprogress.m0fe2342c2e"),execute_code:t("ui.hermessubagentprogress.mcd1a9f56ec"),browser_exec:t("ui.hermessubagentprogress.m1b4034547a"),web_search:t("ui.hermessubagentprogress.m3bf33fa04c"),web_extract:t("ui.hermessubagentprogress.m800caff42c"),web:t("health.internet"),skill_view:t("ui.hermessubagentprogress.m6a79408b48"),skill_manage:t("ui.hermessubagentprogress.mc292ba7421")};
  return safeSubagentToolName(name) && typeof labels[name!] === "string" ? labels[name!] : t("ui.hermessubagentprogress.m69371bfec9");
}
export function subagentStatusLabel(child: SubagentObservation): string {
  if (child.terminalConfirmed) return child.status === "completed" ? t("ui.hermessubagentprogress.mac11729360") : child.status === "failed" ? t("error.label") : child.status === "cancelled" ? t("ui.hermessubagentprogress.maa0d25d0b3") : t("ui.hermessubagentprogress.m6ea2fa6d23");
  if (!child.present) return t("ui.hermessubagentprogress.md249aec2d4");
  return child.status === "running" ? ["pending","queued"].includes(child.rawStatus || "") ? t("ui.hermessubagentprogress.m307429dd84") : t("card.statusRunning")
    : child.status === "failed" ? t("ui.hermessubagentprogress.m1bee3c7f9a") : child.status === "cancelled" ? t("ui.hermessubagentprogress.m803ead836e")
    : child.status === "completed" ? t("ui.hermessubagentprogress.mc0391878c1") : t("ui.hermessubagentprogress.m713e4ad45b");
}
function instant(value: number | undefined) { return value === undefined ? t("ui.hermessubagentprogress.m667b573e80") : new Date(value).toLocaleString(getLocale()); }
function journalText(entry: SubagentJournalEntry) {
  return `${entry.kind === "tool_start" ? t("ui.hermessubagentprogress.mc001fc7717") : t("research.result")}: ${subagentToolLabel(entry.toolName)}${entry.kind === "tool_result" ? ` · ${entry.status === "error" ? t("settings.connectionFailShort") : t("ui.hermessubagentprogress.m62e346eb88")}${entry.durationSeconds === undefined ? "" : t("ui.hermessubagentprogress.ma98998075a", { p0: (entry.durationSeconds) })}` : ""}`;
}
export function HermesSubagentProgress({children,selectedId,onSelect,onRetry,error}: HermesSubagentProgressProps) {
  return <section id="hermes-subagent-progress" className="hermes-subagent-progress" aria-label={t("ui.hermessubagentprogress.mf331d84894")}><h3>{t("ui.hermessubagentprogress.mf331d84894")}</h3>
    {error && <p>{error}</p>}{error && onRetry && <button type="button" onClick={onRetry}>{t("ui.hermessubagentprogress.m9afac0607a")}</button>}
    {!children.length && <p>{t("ui.hermessubagentprogress.m0f0c8c6ef0")}</p>}
    {children.map(child => {
      const expanded = selectedId === child.id;
      // Native IDs belong to a bounded window, not stable calls. Never accumulate or pair them.
      const native = child.nativeState === "supported" && child.nativeJournal.length > 0;
      const journal = native ? child.nativeJournal : child.journal;
      return <article key={child.id} data-subagent-id={child.id}>
        <button type="button" className="hermes-subagent-toggle" aria-expanded={expanded} onClick={()=>onSelect(expanded ? "" : child.id)}><span><b>{subagentStatusLabel(child)}</b><span>{child.goal || t("ui.hermessubagentprogress.mca8073f6be")}</span><small>{child.lastTool ? t("ui.hermessubagentprogress.m5bf85ab4db", { p0: (subagentToolLabel(child.lastTool)) }) : t("ui.hermessubagentprogress.maea03b95f1")}</small></span><span aria-hidden="true">{expanded ? "−" : "+"}</span></button>
        {expanded && <div className="hermes-subagent-details">
          <h4>{t("ui.hermessubagentprogress.m23f80a709a")}</h4><p>{child.goal || t("ui.hermessubagentprogress.m8e3c5f9c82")}</p>
          <p>{child.lastTool ? t("ui.hermessubagentprogress.m5bf85ab4db", { p0: (subagentToolLabel(child.lastTool)) }) : t("ui.hermessubagentprogress.maea03b95f1")}{t("ui.hermessubagentprogress.m89635a4e0d")}</p>
          <dl><div><dt>{t("ui.hermessubagentprogress.mf73daec0c0")}</dt><dd>{instant(child.startedAt === undefined ? undefined : child.startedAt * 1000)}</dd></div><div><dt>{t("ui.hermessubagentprogress.m7270319243")}</dt><dd>{instant(child.receivedAt)}</dd></div><div><dt>{t("ui.hermessubagentprogress.m1aeef76489")}</dt><dd>{instant(child.checkedAt)}</dd></div></dl>
          <p>{t("ui.hermessubagentprogress.mf94007ce95")}{child.toolCount}{t("ui.hermessubagentprogress.m0c591c7702")}</p>
          <h4>{t("ui.hermessubagentprogress.m8fd2786347")}</h4>
          {child.nativeState === "unsupported" && <p>{t("ui.hermessubagentprogress.m801d6a6320")}</p>}
          {child.nativeState === "unavailable" && <p>{t("ui.hermessubagentprogress.ma06cdd0df2")}</p>}
          {child.nativeState === "error" && <p>{t("ui.hermessubagentprogress.m737798aa06")}</p>}
          {child.nativeState === "unchecked" && child.present && child.status === "running" && !child.terminalConfirmed && <p>{t("ui.hermessubagentprogress.m6481a3f9d7")}</p>}
          {(child.partial || native) && <p>{t("ui.hermessubagentprogress.m908677b00a")}</p>}
          <p>{t("ui.hermessubagentprogress.m4c07f05736")}</p>
          {native && <p>{t("ui.hermessubagentprogress.m128a7db34f")}</p>}
          {!journal.length && <p>{t("ui.hermessubagentprogress.md265ab2d00")}</p>}
          <ol className="hermes-subagent-journal">{journal.map(entry=><li key={entry.id}><b>{journalText(entry)}</b><small>{entry.sourceTimeText ? t("ui.hermessubagentprogress.m315019032c", { p0: (entry.sourceTimeText) }) : t("ui.hermessubagentprogress.mf37fd15cbe", { p0: (instant(entry.receivedAt)) })}</small></li>)}</ol>
        </div>}
      </article>;
    })}
  </section>;
}
