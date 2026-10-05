import type { SubagentObservation, SubagentJournalEntry } from "./subagent-progress";
import { safeSubagentToolName } from "./subagents";
import "./subagent-progress.css";
export interface HermesSubagentProgressProps { children: SubagentObservation[]; selectedId: string; onSelect: (id:string) => void; onRetry?: () => void; error?: string }
export function subagentToolLabel(name: string | undefined): string {
  const labels: Record<string,string> = {read_file:"Чтение файла",search_files:"Поиск файлов",write_file:"Запись файла",patch:"Изменение файла",terminal:"Команда",execute_code:"Выполнение кода",browser_exec:"Браузер",web_search:"Поиск в интернете",web_extract:"Чтение страницы",web:"Интернет",skill_view:"Чтение инструкции",skill_manage:"Обновление инструкции"};
  return safeSubagentToolName(name) && typeof labels[name!] === "string" ? labels[name!] : "Инструмент Hermes";
}
export function subagentStatusLabel(child: SubagentObservation): string {
  if (child.terminalConfirmed) return child.status === "completed" ? "Завершён" : child.status === "failed" ? "Ошибка" : child.status === "cancelled" ? "Остановлен" : "Завершён · исход неизвестен";
  if (!child.present) return "Нет в текущем списке · завершение не подтверждено";
  return child.status === "running" ? ["pending","queued"].includes(child.rawStatus || "") ? "В очереди" : "Работает"
    : child.status === "failed" ? "Ошибка · статус из списка" : child.status === "cancelled" ? "Остановлен · статус из списка"
    : child.status === "completed" ? "Завершён · статус из списка" : "Статус неизвестен";
}
function instant(value: number | undefined) { return value === undefined ? "Не предоставлено" : new Date(value).toLocaleString("ru-RU"); }
function journalText(entry: SubagentJournalEntry) {
  return `${entry.kind === "tool_start" ? "Запуск" : "Результат"}: ${subagentToolLabel(entry.toolName)}${entry.kind === "tool_result" ? ` · ${entry.status === "error" ? "ошибка" : "успешно"}${entry.durationSeconds === undefined ? "" : ` · ${entry.durationSeconds} с`}` : ""}`;
}
export function HermesSubagentProgress({children,selectedId,onSelect,onRetry,error}: HermesSubagentProgressProps) {
  return <section id="hermes-subagent-progress" className="hermes-subagent-progress" aria-label="Фоновые помощники"><h3>Фоновые помощники</h3>
    {error && <p>{error}</p>}{error && onRetry && <button type="button" onClick={onRetry}>Повторить проверку помощников</button>}
    {!children.length && <p>Подтверждённых действий помощников пока нет.</p>}
    {children.map(child => {
      const expanded = selectedId === child.id;
      // Native IDs belong to a bounded window, not stable calls. Never accumulate or pair them.
      const native = child.nativeState === "supported" && child.nativeJournal.length > 0;
      const journal = native ? child.nativeJournal : child.journal;
      return <article key={child.id} data-subagent-id={child.id}>
        <button type="button" className="hermes-subagent-toggle" aria-expanded={expanded} onClick={()=>onSelect(expanded ? "" : child.id)}><span><b>{subagentStatusLabel(child)}</b><span>{child.goal || "Помощник Hermes"}</span><small>{child.lastTool ? `Последний запуск: ${subagentToolLabel(child.lastTool)}` : "Запуск инструмента пока не наблюдался"}</small></span><span aria-hidden="true">{expanded ? "−" : "+"}</span></button>
        {expanded && <div className="hermes-subagent-details">
          <h4>Текущая задача</h4><p>{child.goal || "Задача не предоставлена Hermes"}</p>
          <p>{child.lastTool ? `Последний запуск: ${subagentToolLabel(child.lastTool)}` : "Запуск инструмента пока не наблюдался"}. Запуск не подтверждает, что инструмент всё ещё работает.</p>
          <dl><div><dt>Начат</dt><dd>{instant(child.startedAt === undefined ? undefined : child.startedAt * 1000)}</dd></div><div><dt>Событие получено</dt><dd>{instant(child.receivedAt)}</dd></div><div><dt>Статус проверен</dt><dd>{instant(child.checkedAt)}</dd></div></dl>
          <p>Запусков по списку Hermes: {child.toolCount}. Получение события и проверка статуса — не время выполнения инструмента.</p>
          <h4>Журнал действий</h4>
          {child.nativeState === "unsupported" && <p>Результаты инструментов недоступны на этой версии компьютера. Показаны только полученные события запуска.</p>}
          {child.nativeState === "unavailable" && <p>Журнал компьютера недоступен для этого запуска. Завершение не предполагается.</p>}
          {child.nativeState === "error" && <p>Не удалось проверить журнал компьютера. Полученные события сохранены.</p>}
          {child.nativeState === "unchecked" && child.present && child.status === "running" && !child.terminalConfirmed && <p>Проверяем доступность результатов инструментов…</p>}
          {(child.partial || native) && <p>История неполная: только наблюдаемые события или ограниченное окно журнала.</p>}
          <p>Без мыслей, аргументов, содержимого файлов и вывода инструментов. Одноимённые запуски и результаты не связываются в пары.</p>
          {native && <p>Время из журнала компьютера, без даты. Часовой пояс компьютера неизвестен.</p>}
          {!journal.length && <p>События инструментов ещё не получены. История из счётчика не восстанавливается.</p>}
          <ol className="hermes-subagent-journal">{journal.map(entry=><li key={entry.id}><b>{journalText(entry)}</b><small>{entry.sourceTimeText ? `Журнал компьютера: ${entry.sourceTimeText}` : `Событие получено: ${instant(entry.receivedAt)}`}</small></li>)}</ol>
        </div>}
      </article>;
    })}
  </section>;
}
