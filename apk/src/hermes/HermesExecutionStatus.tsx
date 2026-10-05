import { t } from "@tgcontrol/shared";
import { memo, type ReactNode } from "react";
import { useLanguage } from "@tgcontrol/shared";
import { isTerminalHermesSubagentStatus, type HermesSubagent } from "./subagents";
import "./activity-status.css";

export interface HermesExecutionStatusProps {
  quietCompleted?: boolean;
  completionOnly?: boolean;
  busy: boolean;
  waiting: boolean;
  progress: string;
  answerStarted: boolean;
  hasAnswer: boolean;
  restoring: boolean;
  subagents: HermesSubagent[] | null;
  subagentError: string;
  error?: string;
  observableActivity?: { title: string; detail: string; unconfirmed?: boolean };
  stopAction?: ReactNode;
  onDetails?: () => void;
  onRetry?: () => void;
}

type Phase = "restoring" | "waiting" | "working" | "background" | "unknown" | "complete" | "error";
interface Summary { phase: Phase; title: string; detail?: string }

const compactText = (text: string) => text.replace(/\s+/g, " ").trim();

function executionSummary(props: HermesExecutionStatusProps): Summary | null {
  const { busy, waiting, progress, answerStarted, hasAnswer, restoring, subagents, subagentError, error } = props;
  const rosterKnown = subagents !== null && !subagentError;
  const running = rosterKnown ? subagents.filter(item => item.status === "running") : [];

  if (restoring) return { phase: "restoring", title: t("ui.hermesexecutionstatus.mc1c065dfaa") };
  if (error) return {
    phase: "error", title: t("ui.hermesexecutionstatus.mbef4376849"),
    detail: running.length ? t("ui.hermesexecutionstatus.m1da48288f1", { p0: (running.length), p1: (compactText(error)) }) : compactText(error),
  };
  if (waiting) return { phase: "waiting", title: t("ui.hermesexecutionstatus.mb86ec46f37"), detail: t("ui.hermesexecutionstatus.m57a4f45935") };
  if (props.observableActivity && !props.observableActivity.unconfirmed && running.length) return { phase: busy ? "working" : "background", ...props.observableActivity };
  if (busy) return {
    phase: "working", title: answerStarted ? t("ui.hermesexecutionstatus.maf865dfe62") : t("ui.hermesexecutionstatus.meff79c40e2"),
    ...(compactText(progress) ? { detail: compactText(progress) } : {}),
  };
  if (props.observableActivity?.unconfirmed) return { phase: "unknown", title: props.observableActivity.title, detail: props.observableActivity.detail };
  if (running.length) {
    const first = running[0];
    const goal = compactText(first.goal);
    const queued = first.rawStatus === "queued" || first.rawStatus === "pending";
    return {
      phase: "background", title: t("ui.hermesexecutionstatus.m091747214a", { p0: (running.length) }),
      ...(goal ? { detail: queued ? t("ui.hermesexecutionstatus.m8f38f3c577", { p0: (goal) }) : goal } : {}),
    };
  }

  // An idle, empty chat should not become a permanent status banner while the roster loads.
  if (!hasAnswer && !subagents?.length) return null;
  if (!rosterKnown || subagents.some(item => !isTerminalHermesSubagentStatus(item.status))) {
    return {
      phase: "unknown", title: hasAnswer ? t("ui.hermesexecutionstatus.mb4ff61a71c") : t("ui.hermesexecutionstatus.me0b3f9b95c"),
      detail: t("ui.hermesexecutionstatus.mf9fd6c5afa"),
    };
  }
  const failed = subagents.filter(item => item.status === "failed").length;
  const cancelled = subagents.filter(item => item.status === "cancelled").length;
  const outcomes = [failed ? t("ui.hermesexecutionstatus.m318d432c63", { p0: (failed) }) : "", cancelled ? t("ui.hermesexecutionstatus.mea96e2d89d", { p0: (cancelled) }) : ""].filter(Boolean);
  return {
    phase: "complete", title: hasAnswer ? t("ui.hermesexecutionstatus.m08fcf154eb") : t("ui.hermesexecutionstatus.m2810b83ea2"),
    ...(outcomes.length ? { detail: outcomes.join(" · ") } : {}),
  };
}

/** Gateway lifecycle and the session-owned roster are the only sources of completion. */
export const HermesExecutionStatus = memo(function HermesExecutionStatus(props: HermesExecutionStatusProps) {
  useLanguage();
  const summary = executionSummary(props);
  if (!summary || (props.quietCompleted && summary.phase === "complete" && !summary.detail) || (props.completionOnly && (summary.phase !== "complete" || !!summary.detail))) return null;
  return <div className="hermes-execution-status" data-phase={summary.phase}>
    <div className="hermes-execution-status-summary" role="status" aria-live="polite" aria-atomic="true">
      <strong className="hermes-execution-status-title">{summary.title}</strong>
      {summary.detail && <span className="hermes-execution-status-detail" title={summary.detail}>{summary.detail}</span>}
    </div>
    <div className="hermes-execution-status-actions">
      {props.stopAction}
      {summary.phase === "unknown" && props.onRetry && <button type="button" onClick={props.onRetry}>{t("agentCheck.check")}</button>}
      {props.onDetails && <button type="button" aria-label={t("ui.hermesexecutionstatus.mcb36a536c6")} onClick={props.onDetails}>{t("remote.sectionActions")}</button>}
    </div>
  </div>;
});
