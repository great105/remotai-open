import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import { HermesExecutionStatus, type HermesExecutionStatusProps } from "./HermesExecutionStatus";
import type { HermesSubagent } from "./subagents";

const defaults: HermesExecutionStatusProps = {
  busy: false, waiting: false, progress: "", answerStarted: false,
  hasAnswer: false, restoring: false, subagents: null, subagentError: "",
  onDetails: () => {}, onRetry: () => {},
};
const child = (status: HermesSubagent["status"], goal = "Проверить тесты"): HermesSubagent => ({ id: "worker-1", goal, status, toolCount: 2 });
const render = (props: Partial<HermesExecutionStatusProps> = {}) => renderToStaticMarkup(createElement(HermesExecutionStatus, { ...defaults, ...props }));

describe("Hermes execution lifecycle", () => {
  const observableActivity = { title: "Помощник работает", detail: "Последний запуск: Чтение файла" };
  it.each(["completed", "failed", "cancelled"] as const)("terminal %s roster defeats retained compact activity", status => {
    const html = render({ hasAnswer: true, subagents: [child(status)], observableActivity });
    expect(html).toContain('data-phase="complete"');
    expect(html).toContain("Запрос завершён");
    expect(html).not.toContain(observableActivity.title);
    expect(html).not.toContain(observableActivity.detail);
  });
  it.each([
    { subagents: null, subagentError: "" },
    { subagents: [child("unknown")], subagentError: "" },
    { subagents: [child("running")], subagentError: "unavailable" },
    { subagents: [], subagentError: "malformed" },
  ])("unknown roster defeats stale compact activity %j", partial => {
    const html = render({ hasAnswer: true, ...partial, observableActivity });
    expect(html).toContain('data-phase="unknown"');
    expect(html).not.toContain(observableActivity.title);
    expect(html).not.toContain("Запрос завершён");
  });
  it("does not turn disappearance into background activity or completion", () => {
    const html = render({ hasAnswer: true, subagents: [], observableActivity: { title: "Завершение помощника не подтверждено", detail: "Последний запуск: Чтение файла", unconfirmed: true } });
    expect(html).toContain('data-phase="unknown"');
    expect(html).toContain("Завершение помощника не подтверждено");
    expect(html).not.toContain("Запрос завершён");
  });
  it("does not promote retained child activity while only the main request is busy", () => {
    const html = render({ busy: true, subagents: [child("completed")], observableActivity });
    expect(html).toContain('data-phase="working"');
    expect(html).not.toContain(observableActivity.title);
    expect(html).not.toContain(observableActivity.detail);
  });
  it("keeps compact tool evidence for a currently running helper", () => {
    expect(render({ hasAnswer: true, subagents: [child("running")], observableActivity })).toContain(observableActivity.detail);
  });
  it.each([{ subagents: null }, { subagents: [] }] as Array<{ subagents: HermesSubagent[] | null }>)("keeps the empty new chat quiet with roster %j", ({ subagents }) => {
    expect(render({ subagents, subagentError: "unsupported" })).toBe("");
  });

  it("prioritizes restoring over stale completion, errors and waiting", () => {
    const html = render({ restoring: true, hasAnswer: true, subagents: [], waiting: true, error: "old error" });
    expect(html).toContain('data-phase="restoring"');
    expect(html).toContain("Восстанавливаем беседу");
    expect(html).not.toContain("Запрос завершён");
    expect(html).not.toContain("old error");
  });

  it("shows the native human-input wait rather than declaring background completion", () => {
    const html = render({ busy: true, waiting: true, subagents: [], hasAnswer: true });
    expect(html).toContain('data-phase="waiting"');
    expect(html).toContain("Ждёт вашего ответа");
    expect(html).not.toContain("Запрос завершён");
  });

  it("shows real main progress and only uses answerStarted for the response phase", () => {
    const running = render({ busy: true, progress: "Читаю\n package.json" });
    expect(running).toContain('data-phase="working"');
    expect(running).toContain("Выполняется");
    expect(running).toContain("Читаю package.json");
    expect(render({ busy: true, answerStarted: true })).toContain("Hermes отвечает");
  });

  it("does not infer activity from diagnostic prose when main and children are confirmed idle", () => {
    const html = render({ hasAnswer: true, subagents: [], progress: "Я всё ещё работаю" });
    expect(html).toContain('data-phase="complete"');
    expect(html).not.toContain("Я всё ещё работаю");
  });

  it("keeps background work visible after the main answer has finished", () => {
    const html = render({ hasAnswer: true, subagents: [child("running"), { ...child("completed"), id: "worker-2" }] });
    expect(html).toContain('data-phase="background"');
    expect(html).toContain("Фоновые задачи выполняются: 1");
    expect(html).toContain("Проверить тесты");
    expect(html).not.toContain("Запрос завершён");
  });

  it("distinguishes a native queued helper from one already executing", () => {
    expect(render({ subagents: [{ ...child("running"), rawStatus: "queued" }] })).toContain("В очереди: Проверить тесты");
  });

  it.each([
    { subagents: null, subagentError: "" },
    { subagents: [], subagentError: "unsupported" },
    { subagents: [child("unknown")], subagentError: "" },
    { subagents: [child("completed")], subagentError: "network error" },
  ])("does not claim completion for an unconfirmed roster %j", partial => {
    const html = render({ ...partial, hasAnswer: true });
    expect(html).toContain('data-phase="unknown"');
    expect(html).toContain("Ответ получен");
    expect(html).toContain("Фоновая работа не подтверждена.");
    expect(html).toContain(">Проверить</button>");
    expect(html).not.toContain("Запрос завершён");
  });

  it("does not present a cached running row as current after a roster error", () => {
    const html = render({ hasAnswer: true, subagents: [child("running")], subagentError: "unavailable" });
    expect(html).toContain('data-phase="unknown"');
    expect(html).not.toContain("Фоновые задачи выполняются");
  });

  it.each([{ subagents: [] }, { subagents: [child("completed")] }])("confirms completion only from a known terminal roster %j", ({ subagents }) => {
    const html = render({ subagents, hasAnswer: true });
    expect(html).toContain('data-phase="complete"');
    expect(html).toContain("Запрос завершён");
    expect(html).not.toContain(">Проверить</button>");
  });

  it("discloses failed and cancelled child outcomes alongside completion", () => {
    const html = render({ hasAnswer: true, subagents: [child("failed"), { ...child("cancelled"), id: "worker-2" }] });
    expect(html).toContain("Запрос завершён");
    expect(html).toContain("С ошибкой: 1 · Отменено: 1");
  });

  it("does not claim a failed main request succeeded even with an empty confirmed roster", () => {
    const html = render({ hasAnswer: true, subagents: [], error: "Соединение прервано" });
    expect(html).toContain('data-phase="error"');
    expect(html).toContain("Запрос прерван");
    expect(html).toContain("Соединение прервано");
    expect(html).not.toContain("Запрос завершён");
  });

  it("does not erase known background work when the main request fails", () => {
    expect(render({ error: "Main failed", subagents: [child("running")] })).toContain("Фоновые задачи продолжаются: 1.");
  });

  it("renders native goal and progress as text, and keeps action controls outside the live region", () => {
    const html = render({ subagents: [child("running", "<script>alert(1)</script>")] });
    expect(html).toContain("&lt;script&gt;alert(1)&lt;/script&gt;");
    expect(html).not.toContain("<script>");
    expect(html).toContain('role="status" aria-live="polite" aria-atomic="true"');
    expect(html).toMatch(/<\/div><div class="hermes-execution-status-actions">/);
    expect(html).toContain('type="button" aria-label="Показать действия"');
  });

  it("keeps confirmed completion in history while preserving failed-child visibility", () => {
    expect(render({hasAnswer:true,subagents:[],quietCompleted:true})).toBe("");
    expect(render({hasAnswer:true,subagents:[child("failed")],quietCompleted:true})).toContain("С ошибкой: 1");
    expect(render({hasAnswer:true,subagents:null,quietCompleted:true})).toContain('data-phase="unknown"');
  });
  it("does not offer an inert retry when the parent cannot refresh the roster", () => {
    expect(render({ hasAnswer: true, onRetry: undefined })).not.toContain(">Проверить</button>");
  });
});
