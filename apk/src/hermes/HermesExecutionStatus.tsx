import { memo, type ReactNode } from "react";
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

  if (restoring) return { phase: "restoring", title: "Восстанавливаем беседу" };
  if (error) return {
    phase: "error", title: "Запрос прерван",
    detail: running.length ? `Фоновые задачи продолжаются: ${running.length}. ${compactText(error)}` : compactText(error),
  };
  if (waiting) return { phase: "waiting", title: "Ждёт вашего ответа", detail: "Ответьте на запрос над полем сообщения." };
  if (props.observableActivity && !props.observableActivity.unconfirmed && running.length) return { phase: busy ? "working" : "background", ...props.observableActivity };
  if (busy) return {
    phase: "working", title: answerStarted ? "Hermes отвечает" : "Выполняется",
    ...(compactText(progress) ? { detail: compactText(progress) } : {}),
  };
  if (props.observableActivity?.unconfirmed) return { phase: "unknown", title: props.observableActivity.title, detail: props.observableActivity.detail };
  if (running.length) {
    const first = running[0];
    const goal = compactText(first.goal);
    const queued = first.rawStatus === "queued" || first.rawStatus === "pending";
    return {
      phase: "background", title: `Фоновые задачи выполняются: ${running.length}`,
      ...(goal ? { detail: queued ? `В очереди: ${goal}` : goal } : {}),
    };
  }

  // An idle, empty chat should not become a permanent status banner while the roster loads.
  if (!hasAnswer && !subagents?.length) return null;
  if (!rosterKnown || subagents.some(item => !isTerminalHermesSubagentStatus(item.status))) {
    return {
      phase: "unknown", title: hasAnswer ? "Ответ получен" : "Статус задач неизвестен",
      detail: "Фоновая работа не подтверждена.",
    };
  }
  const failed = subagents.filter(item => item.status === "failed").length;
  const cancelled = subagents.filter(item => item.status === "cancelled").length;
  const outcomes = [failed ? `С ошибкой: ${failed}` : "", cancelled ? `Отменено: ${cancelled}` : ""].filter(Boolean);
  return {
    phase: "complete", title: hasAnswer ? "Запрос завершён" : "Фоновые задачи завершены",
    ...(outcomes.length ? { detail: outcomes.join(" · ") } : {}),
  };
}

/** Gateway lifecycle and the session-owned roster are the only sources of completion. */
export const HermesExecutionStatus = memo(function HermesExecutionStatus(props: HermesExecutionStatusProps) {
  const summary = executionSummary(props);
  if (!summary || (props.quietCompleted && summary.phase === "complete" && !summary.detail) || (props.completionOnly && (summary.phase !== "complete" || !!summary.detail))) return null;
  return <div className="hermes-execution-status" data-phase={summary.phase}>
    <div className="hermes-execution-status-summary" role="status" aria-live="polite" aria-atomic="true">
      <strong className="hermes-execution-status-title">{summary.title}</strong>
      {summary.detail && <span className="hermes-execution-status-detail" title={summary.detail}>{summary.detail}</span>}
    </div>
    <div className="hermes-execution-status-actions">
      {props.stopAction}
      {summary.phase === "unknown" && props.onRetry && <button type="button" onClick={props.onRetry}>Проверить</button>}
      {props.onDetails && <button type="button" aria-label="Показать действия" onClick={props.onDetails}>Действия</button>}
    </div>
  </div>;
});
