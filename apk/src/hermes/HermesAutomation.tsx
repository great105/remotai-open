import { useEffect, useId, useRef, useState, type FormEvent } from "react";
import { mapApiError } from "@tgcontrol/shared";
import { IconChevron, IconFolder } from "../components/icons";
import { scheduledTaskTime, type HermesClient } from "./client";
import "./automation.css";

interface Job {
  id: string; name?: string; prompt?: string; enabled?: boolean; state?: string;
  next_run_at?: string; next_run?: string; last_status?: string;
  last_error?: string;
  scheduler_heartbeat_age_s?: number | null; workdir?: string;
  schedule?: string | { display?: string; kind?: string; value?: string };
}
interface MemoryInfo { active?: string; builtin_files?: { memory?: number; user?: number } }
interface Skill { name: string; description?: string; enabled: boolean }
export interface HermesStyleDraft { content: string; dirty: boolean }
interface Props {
  client: HermesClient; ready: boolean; busy: boolean; cwd: string; deviceName: string;
  sessionId: string; onChooseFolder: () => void; onPreparePrompt: (text: string) => void;
  onChooseTaskFolder: (cwd: string, onPick: (path: string) => void) => void;
  styleDraft: HermesStyleDraft | null; onStyleDraft: (draft: HermesStyleDraft) => void;
  onInspectContext: () => Promise<string>;
  contextAvailable?: boolean; contextUnavailableReason?: string;
  onOpenResult: (id: string) => void;
}
type Section = "jobs" | "memory" | "skills";

function failure(error: unknown): string {
  if ((error as { status?: number })?.status === 404) return "Эта версия Hermes ещё не открывает этот раздел. Обновите Hermes и попробуйте снова.";
  return mapApiError(error) || "Не удалось загрузить данные. Проверьте подключение и попробуйте снова.";
}
function nextRun(job: Job): string {
  const value = job.next_run_at || job.next_run;
  if (!value) return "Следующий запуск пока не назначен";
  const date = new Date(value);
  return Number.isNaN(date.valueOf()) ? value : date.toLocaleString("ru", { dateStyle: "short", timeStyle: "short" });
}

export function HermesAutomation({ client, ready, busy, cwd, deviceName, sessionId, onChooseFolder, onChooseTaskFolder, styleDraft, onStyleDraft, onPreparePrompt, onInspectContext, onOpenResult, contextAvailable = false, contextUnavailableReason = "Контекст недоступен через защищённый HTTP-интерфейс Remotai; используйте нативный Hermes." }: Props) {
  const fieldId = useId();
  const [section, setSection] = useState<Section | null>(null);
  const [revision, setRevision] = useState(0);
  const [loading, setLoading] = useState(false);
  const [pending, setPending] = useState("");
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [jobs, setJobs] = useState<Job[]>([]);
  const [memory, setMemory] = useState<MemoryInfo | null>(null);
  const [skills, setSkills] = useState<Skill[]>([]);
  const [newJob, setNewJob] = useState(false);
  const [editingId, setEditingId] = useState("");
  const [name, setName] = useState("");
  const [prompt, setPrompt] = useState("");
  const [repeat, setRepeat] = useState("once");
  const [when, setWhen] = useState("");
  const [timeError, setTimeError] = useState("");
  const [workdir, setWorkdir] = useState(cwd);
  const [continuity, setContinuity] = useState(false);
  const [preference, setPreference] = useState("");
  const [context, setContext] = useState("");
  const [skillContent, setSkillContent] = useState<{ name: string; content: string } | null>(null);
  const [runs, setRuns] = useState<{ id: string; entries: Array<Record<string, unknown>> } | null>(null);
  const [deleteId, setDeleteId] = useState("");
  const alive = useRef(true);
  const actionBusy = useRef(false);
  useEffect(() => { alive.current = true; return () => { alive.current = false; }; }, []);
  useEffect(() => {
    if (section !== "jobs" || !ready) return;
    const timer = window.setInterval(() => { if (!actionBusy.current) setRevision(value => value + 1); }, 15000);
    return () => window.clearInterval(timer);
  }, [section, ready]);

  useEffect(() => {
    if (!section || !ready) return;
    const controller = new AbortController();
    let current = true;
    setLoading(true); setError("");
    const path = section === "jobs" ? "/cron/jobs?profile=default" : `/${section}?profile=default`;
    void client.backendRequest<unknown>(path, "GET", undefined, controller.signal).then(result => {
      if (!current) return;
      if (section === "jobs") { if (!Array.isArray(result)) throw new Error("Hermes вернул неизвестный список задач."); setJobs(result); }
      if (section === "memory") setMemory(result as MemoryInfo);
      if (section === "skills") { if (!Array.isArray(result)) throw new Error("Hermes вернул неизвестный список навыков."); setSkills(result); }
    }).catch(e => { if (current) setError(failure(e)); }).finally(() => { if (current) setLoading(false); });
    return () => { current = false; controller.abort(); };
  }, [client, ready, section, revision]);

  async function act(key: string, action: () => Promise<void>) {
    if (actionBusy.current) return;
    actionBusy.current = true; setPending(key); setError(""); setNotice("");
    try { await action(); } catch (e) { if (alive.current) setError(failure(e)); }
    finally { actionBusy.current = false; if (alive.current) setPending(""); }
  }
  function open(next: Section) {
    if (pending) return;
    setSection(current => current === next ? null : next);
    setError(""); setNotice(""); setDeleteId("");
  }
  async function createJob(event: FormEvent) {
    event.preventDefault();
    if (!prompt.trim()) return;
    let schedule = repeat;
    if (!editingId) {
      try { schedule = scheduledTaskTime(repeat, when); }
      catch (error) { setTimeError((error as Error).message); return; }
    }
    setTimeError("");
    await act("create", async () => {
      if (editingId) await client.backendRequest(`/cron/jobs/${encodeURIComponent(editingId)}?profile=default`, "PUT", { updates: { name: name.trim(), prompt: prompt.trim() } });
      else await client.backendRequest("/cron/jobs?profile=default", "POST", {
        name: name.trim(), prompt: prompt.trim(), schedule, deliver: "local", ...(workdir ? { workdir } : {}),
        ...(repeat !== "once" && continuity ? { context_from: ["self"] } : {}),
      });
      if (!alive.current) return;
      setName(""); setPrompt(""); setWhen(""); setNewJob(false); setEditingId("");
      setNotice(editingId ? "Задача изменена." : "Задача добавлена в расписание."); setRevision(value => value + 1);
    });
  }
  const disabled = busy || !!pending || !ready || loading;
  const heartbeat = jobs.find(job => typeof job.scheduler_heartbeat_age_s === "number")?.scheduler_heartbeat_age_s;
  const titles: Array<[Section, string, string]> = [
    ["jobs", "Задачи по расписанию", "Выполнить позже или повторять"],
    ["memory", "Память и контекст", "Что Hermes помнит и с чем работает"],
    ["skills", "Навыки", "Готовые умения для ваших задач"],
  ];

  return <div className="hermes-automation">
    {titles.map(([key, title, description]) => <div key={key} className="hermes-automation-section">
      <button type="button" className="hermes-setting-category" aria-expanded={section === key} aria-controls={`hermes-settings-${key}`} disabled={!!pending} onClick={() => open(key)}><span><b>{title}</b><small>{description}</small></span><IconChevron size={18} dir={section === key ? "up" : "down"} /></button>
      {section === key && <section id={`hermes-settings-${key}`} aria-label={title} className="hermes-setting-body">
        {!ready ? <p>Сначала запустите Hermes на этом компьютере.</p> : <>
          {loading && <p role="status">Загружаем…</p>}
          {error && <div role="alert"><p>{error}</p><button type="button" className="hermes-context-button" disabled={!!pending} onClick={() => setRevision(value => value + 1)}>Попробовать снова</button></div>}
          {notice && <p role="status">{notice}</p>}
          {key === "jobs" && <>
            <p>Компьютер, Remotai и Hermes должны работать в назначенное время. Закрыть эту страницу можно. Результаты сохраняются на компьютере.</p>
            {jobs.length > 0 && !loading && <p role="status">{typeof heartbeat === "number" && heartbeat < 120 ? "Планировщик на связи." : typeof heartbeat === "number" ? "Планировщик давно не подтверждал работу. Проверьте, запущен ли Hermes." : "Планировщик ещё не подтвердил работу. Если задача не запускается, обновите и запустите Remotai и Hermes."}</p>}
            <p className="hermes-automation-host">{deviceName || "Выбранный компьютер"}</p>
            <button type="button" className="hermes-context-button" disabled={disabled} onClick={() => { setEditingId(""); setName(""); setPrompt(""); setWhen(""); setRepeat("once"); setContinuity(false); setTimeError(""); setWorkdir(cwd); setNewJob(value => !value); }}>{newJob ? "Закрыть форму" : "Добавить задачу"}</button>
            {newJob && <form className="hermes-automation-form" onSubmit={event => void createJob(event)}>
              <div className="hermes-automation-field"><label htmlFor={`${fieldId}-name`}>Название · необязательно</label><input id={`${fieldId}-name`} value={name} disabled={!!pending || busy} onChange={event => setName(event.target.value)} maxLength={120} /></div>
              <div className="hermes-automation-field"><label htmlFor={`${fieldId}-task`}>Что сделать</label><textarea id={`${fieldId}-task`} required value={prompt} disabled={!!pending || busy} onChange={event => setPrompt(event.target.value)} placeholder="Например, подготовить отчёт по проекту" rows={3} maxLength={16000} /></div>
              {!editingId && <><div className="hermes-automation-field"><label htmlFor={`${fieldId}-repeat`}>Когда</label><select id={`${fieldId}-repeat`} value={repeat} disabled={!!pending || busy} onChange={event => { setRepeat(event.target.value); setTimeError(""); }}><option value="once">Один раз</option><option value="every 1h">Каждый час</option><option value="every 24h">Каждые 24 часа</option><option value="every 7d">Каждые 7 дней</option></select></div>
              {repeat === "once" ? <div className="hermes-automation-field"><label htmlFor={`${fieldId}-when`}>Дата и время на вашем устройстве</label><input id={`${fieldId}-when`} type="datetime-local" disabled={!!pending || busy} required value={when} aria-invalid={!!timeError} aria-describedby={timeError ? `${fieldId}-time-error` : undefined} onChange={event => { setWhen(event.target.value); setTimeError(""); }} />{timeError && <p id={`${fieldId}-time-error`} className="hermes-error-text" role="alert">{timeError}</p>}</div> : <><p className="hermes-muted">Первый запуск — через выбранный интервал после сохранения.</p><label className="hermes-automation-checkbox"><input type="checkbox" disabled={!!pending || busy} checked={continuity} onChange={event => setContinuity(event.target.checked)} />Учитывать предыдущий результат этой задачи</label></>}
              <p className="hermes-task-folder">{workdir ? <><b>Папка этой задачи</b><br /><span title={workdir}>{workdir}</span></> : "Папка не выбрана. Если нужны файлы проекта, выберите её перед сохранением."}</p>
              <p className="hermes-muted">Задача начнёт отдельный разговор. Текущая переписка в неё не копируется.</p>
              <button type="button" className="hermes-context-button" disabled={disabled} onClick={() => onChooseTaskFolder(workdir, setWorkdir)}><IconFolder size={18} />{workdir ? "Изменить папку задачи" : "Выбрать папку задачи"}</button>
              </>}
              <button className="btn btn-primary" type="submit" disabled={disabled || !prompt.trim()}>{pending === "create" ? "Сохраняем…" : editingId ? "Сохранить изменения задачи" : "Добавить в расписание"}</button>
            </form>}
            {!loading && !error && !jobs.length && <p className="hermes-muted">Задач пока нет. Добавьте первую.</p>}
            <ul className="hermes-automation-list">{jobs.map(job => { const completed = job.state === "completed"; const paused = !completed && (job.enabled === false || job.state === "paused"); return <li key={job.id}>
              <b>{job.name || job.prompt?.slice(0, 80) || "Задача Hermes"}</b>
              {job.prompt && <p>{job.prompt}</p>}
              <small>{completed ? "Задача завершена" : paused ? "Приостановлена" : job.state === "running" ? "Задача выполняется" : `Следующий запуск: ${nextRun(job)}`}</small>
              {job.workdir && <small title={job.workdir}>{job.workdir}</small>}
              {job.last_error && <p role="alert">Последний запуск: {job.last_error}</p>}
              <div className="hermes-automation-actions">{!completed && <button type="button" className="hermes-context-button" disabled={disabled} onClick={() => void act(job.id, async () => { await client.backendRequest(`/cron/jobs/${encodeURIComponent(job.id)}/${paused ? "resume" : "pause"}?profile=default`, "POST", {}); if (alive.current) setRevision(value => value + 1); })}>{paused ? "Продолжить" : "Приостановить"}</button>}
                <button type="button" className="hermes-context-button" disabled={disabled} onClick={() => { setEditingId(job.id); setName(job.name || ""); setPrompt(job.prompt || ""); setTimeError(""); setNewJob(true); }}>Изменить задачу</button>
                <button type="button" className="hermes-context-button" disabled={!!pending} onClick={() => void act(`runs:${job.id}`, async () => { const result = await client.backendRequest<{ runs: Array<Record<string, unknown>> }>(`/cron/jobs/${encodeURIComponent(job.id)}/runs?profile=default&limit=20`); if (alive.current) setRuns({ id: job.id, entries: result.runs || [] }); })}>Результаты</button>
                <button type="button" className="hermes-context-button" disabled={disabled} onClick={() => setDeleteId(job.id)}>Удалить задачу</button></div>
              {deleteId === job.id && <div className="hermes-automation-confirm"><p>Убрать эту задачу из расписания?</p><button type="button" className="btn btn-secondary" onClick={() => setDeleteId("")}>Оставить</button><button type="button" className="btn btn-danger" disabled={disabled} onClick={() => void act(`delete:${job.id}`, async () => { await client.backendRequest(`/cron/jobs/${encodeURIComponent(job.id)}?profile=default`, "DELETE"); if (alive.current) { setDeleteId(""); setRevision(value => value + 1); } })}>Да, удалить задачу</button></div>}
              {runs?.id === job.id && <div className="hermes-automation-results"><h4>Последние результаты</h4>{!runs.entries.length ? <p>Запусков ещё не было.</p> : runs.entries.map((run, index) => <article key={String(run.id || index)}><b>{String(run.title || "Запуск задачи")}</b><p>{String(run.preview || "Результат сохранён на компьютере.")}</p>{run.source === "cron" && typeof run.id === "string" && <button type="button" className="hermes-context-button" disabled={disabled} onClick={() => onOpenResult(run.id as string)}>Открыть разговор с результатом</button>}</article>)}</div>}
            </li>; })}</ul>
          </>}
          {key === "memory" && <>
            <p>Память сохраняет ваши предпочтения между чатами. Папка проекта задаёт файлы, с которыми Hermes работает в этом чате.</p>
            <p className="hermes-automation-host">{cwd || "Папка проекта пока не выбрана"}</p>
            <button type="button" className="hermes-context-button" disabled={disabled} onClick={onChooseFolder}><IconFolder size={18} />{cwd ? "Изменить папку проекта" : "Выбрать папку проекта"}</button>
            <p>Описание и правила проекта Hermes читает из выбранной папки. Можно попросить его подготовить такое описание для следующих задач.</p>
            <button type="button" className="hermes-context-button" disabled={disabled || !cwd} onClick={() => onPreparePrompt("Изучи проект в выбранной папке и подготовь инструкции для следующих задач Hermes. Учти существующие правила проекта и предложи содержание файла HERMES.md.")}>Подготовить описание проекта</button>
            {memory && <p className="hermes-muted">{memory.active === "builtin" ? ((memory.builtin_files?.memory || 0) + (memory.builtin_files?.user || 0) > 0 ? "В памяти есть сохранённые заметки." : "Встроенная память пока пуста.") : memory.active ? `Подключена память: ${memory.active}` : "Состояние памяти получено"}</p>}
            <button type="button" className="hermes-context-button" disabled={disabled || !sessionId || !contextAvailable} title={!contextAvailable ? contextUnavailableReason : undefined} onClick={() => { if (contextAvailable) void act("context", async () => { const result = await onInspectContext(); if (alive.current) setContext(result); }); }}>Посмотреть контекст чата</button>
            {!contextAvailable && <p className="hermes-muted">{contextUnavailableReason}</p>}
            {!sessionId && <p className="hermes-muted">Сведения о контексте появятся после первого сообщения.</p>}
            {context && <pre className="hermes-automation-content">{context}</pre>}
            <div className="hermes-memory-preference"><label htmlFor={`${fieldId}-preference`}>Что Hermes стоит запомнить</label><textarea id={`${fieldId}-preference`} value={preference} onChange={event => setPreference(event.target.value)} placeholder="Например: отвечай по-русски и сначала показывай результат" rows={3} maxLength={8000} /></div>
            <p className="hermes-muted">Подготовим сообщение в чате. Вы сможете проверить его и отправить.</p>
            <button type="button" className="hermes-context-button" disabled={disabled || !preference.trim()} onClick={() => onPreparePrompt(`Запомни это предпочтение в постоянной памяти: ${preference.trim()}`)}>Подготовить сообщение</button>
            <HermesStyleSettings client={client} disabled={disabled} draft={styleDraft} onDraft={onStyleDraft} />
          </>}
          {key === "skills" && <>
            <p>Навыки дают Hermes инструкции для определённых задач. Включённые навыки доступны в следующих сообщениях.</p>
            {!loading && !error && !skills.length && <p className="hermes-muted">Установленных навыков пока нет.</p>}
            <ul className="hermes-automation-list">{skills.map(skill => <li key={skill.name}><div className="hermes-skill-heading"><b>{skill.name}</b><button type="button" role="switch" aria-label={`Навык ${skill.name}`} aria-checked={skill.enabled} className="hermes-context-button" disabled={disabled} onClick={() => void act(`skill:${skill.name}`, async () => { await client.backendRequest("/skills/toggle?profile=default", "PUT", { name: skill.name, enabled: !skill.enabled, profile: "default" }); if (alive.current) setRevision(value => value + 1); })}>{skill.enabled ? "Включён" : "Выключен"}</button></div>{skill.description && <p>{skill.description}</p>}
              <button type="button" className="hermes-context-button" disabled={!!pending} onClick={() => void act(`content:${skill.name}`, async () => { const result = await client.backendRequest<{ name: string; content: string }>(`/skills/content?profile=default&name=${encodeURIComponent(skill.name)}`); if (alive.current) setSkillContent(result); })}>Посмотреть инструкции</button>
              {skillContent?.name === skill.name && <pre className="hermes-automation-content">{skillContent.content}</pre>}
            </li>)}</ul>
          </>}
        </>}
      </section>}
    </div>)}
  </div>;
}

function HermesStyleSettings({ client, disabled, draft, onDraft }: { client: HermesClient; disabled: boolean; draft: HermesStyleDraft | null; onDraft: (draft: HermesStyleDraft) => void }) {
  const fieldId = useId();
  const content = draft?.content || "";
  const dirty = draft?.dirty || false;
  const draftRef = useRef(draft);
  draftRef.current = draft;
  const [loaded, setLoaded] = useState(false);
  const [pending, setPending] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [attempt, setAttempt] = useState(0);
  const alive = useRef(true);
  useEffect(() => { alive.current = true; return () => { alive.current = false; }; }, []);
  useEffect(() => {
    const controller = new AbortController(); let current = true;
    setLoaded(false); setError("");
    void client.backendRequest<{ content: string }>("/profiles/default/soul", "GET", undefined, controller.signal).then(result => {
      if (current) { if (!draftRef.current?.dirty) onDraft({ content: result.content || "", dirty: false }); setLoaded(true); }
    }).catch(e => { if (current) setError(failure(e)); });
    return () => { current = false; controller.abort(); };
  }, [client, attempt, onDraft]);
  async function save(event: FormEvent) {
    event.preventDefault(); if (!loaded || pending || disabled || !dirty) return;
    setPending(true); setError(""); setNotice("");
    try {
      await client.backendRequest("/profiles/default/soul", "PUT", { content });
      if (alive.current) { onDraft({ content, dirty: false }); setNotice("Сохранено. Эти инструкции применяются к новым чатам на этом компьютере."); }
    } catch (e) { if (alive.current) setError(failure(e)); }
    finally { if (alive.current) setPending(false); }
  }
  return <form className="hermes-automation-form hermes-style-form" onSubmit={event => void save(event)}>
    <h4>Как Hermes должен общаться</h4><p>Общие инструкции для новых чатов: стиль ответа, язык и правила работы.</p>
    {error && <div role="alert"><p>{error}</p><button type="button" className="hermes-context-button" disabled={pending} onClick={() => setAttempt(value => value + 1)}>Загрузить снова</button></div>}
    {!loaded && !error && <p role="status">Загружаем инструкции…</p>}
    {loaded && <><div className="hermes-automation-field"><label htmlFor={fieldId}>Ваши инструкции</label><textarea id={fieldId} value={content} disabled={pending} onChange={event => { onDraft({ content: event.target.value, dirty: true }); setNotice(""); }} rows={5} maxLength={32000} /></div>{dirty && <p className="hermes-muted">Ваши изменения ещё не сохранены.</p>}<button type="submit" className="btn btn-primary" disabled={disabled || pending || !dirty}>{pending ? "Сохраняем…" : "Сохранить инструкции"}</button></>}
    {notice && <p role="status">{notice}</p>}
  </form>;
}
