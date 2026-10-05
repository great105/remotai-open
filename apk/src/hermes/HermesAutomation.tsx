import { getLocale } from "@tgcontrol/shared";
import { t } from "@tgcontrol/shared";
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
  if ((error as { status?: number })?.status === 404) return t("ui.hermesautomation.ma405f40cf5");
  return mapApiError(error) || t("ui.hermesautomation.m8a0f6a9fbb");
}
function nextRun(job: Job): string {
  const value = job.next_run_at || job.next_run;
  if (!value) return t("ui.hermesautomation.m1ae26bafbb");
  const date = new Date(value);
  return Number.isNaN(date.valueOf()) ? value : date.toLocaleString(getLocale(), { dateStyle: "short", timeStyle: "short" });
}

export function HermesAutomation({ client, ready, busy, cwd, deviceName, sessionId, onChooseFolder, onChooseTaskFolder, styleDraft, onStyleDraft, onPreparePrompt, onInspectContext, onOpenResult, contextAvailable = false, contextUnavailableReason = t("ui.hermesautomation.m65fb9e31e4") }: Props) {
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
      if (section === "jobs") { if (!Array.isArray(result)) throw new Error(t("ui.hermesautomation.m82702ad1ac")); setJobs(result); }
      if (section === "memory") setMemory(result as MemoryInfo);
      if (section === "skills") { if (!Array.isArray(result)) throw new Error(t("ui.hermesautomation.md942e72833")); setSkills(result); }
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
      setNotice(editingId ? t("ui.hermesautomation.m481def9b56") : t("ui.hermesautomation.m5beefe3f4f")); setRevision(value => value + 1);
    });
  }
  const disabled = busy || !!pending || !ready || loading;
  const heartbeat = jobs.find(job => typeof job.scheduler_heartbeat_age_s === "number")?.scheduler_heartbeat_age_s;
  const titles: Array<[Section, string, string]> = [
    ["jobs", t("ui.hermesautomation.m4851f53930"), t("ui.hermesautomation.mb32da2e08f")],
    ["memory", t("ui.hermesautomation.m12fe220e55"), t("ui.hermesautomation.m190a741a41")],
    ["skills", t("ui.hermesautomation.m822b985513"), t("ui.hermesautomation.md599763970")],
  ];

  return <div className="hermes-automation">
    {titles.map(([key, title, description]) => <div key={key} className="hermes-automation-section">
      <button type="button" className="hermes-setting-category" aria-expanded={section === key} aria-controls={`hermes-settings-${key}`} disabled={!!pending} onClick={() => open(key)}><span><b>{title}</b><small>{description}</small></span><IconChevron size={18} dir={section === key ? "up" : "down"} /></button>
      {section === key && <section id={`hermes-settings-${key}`} aria-label={title} className="hermes-setting-body">
        {!ready ? <p>{t("ui.hermesautomation.m6f7b3172a7")}</p> : <>
          {loading && <p role="status">{t("ui.hermesautomation.mb00e2401eb")}</p>}
          {error && <div role="alert"><p>{error}</p><button type="button" className="hermes-context-button" disabled={!!pending} onClick={() => setRevision(value => value + 1)}>{t("ui.hermesautomation.mfc95f94b15")}</button></div>}
          {notice && <p role="status">{notice}</p>}
          {key === "jobs" && <>
            <p>{t("ui.hermesautomation.mb8d08c9c99")}</p>
            {jobs.length > 0 && !loading && <p role="status">{typeof heartbeat === "number" && heartbeat < 120 ? t("ui.hermesautomation.mac969d0681") : typeof heartbeat === "number" ? t("ui.hermesautomation.m43de5b434f") : t("ui.hermesautomation.m6db02c170a")}</p>}
            <p className="hermes-automation-host">{deviceName || t("ui.hermesautomation.md1f6e8d034")}</p>
            <button type="button" className="hermes-context-button" disabled={disabled} onClick={() => { setEditingId(""); setName(""); setPrompt(""); setWhen(""); setRepeat("once"); setContinuity(false); setTimeError(""); setWorkdir(cwd); setNewJob(value => !value); }}>{newJob ? t("ui.hermesautomation.m47e63cdce9") : t("ui.hermesautomation.mca5f8743a2")}</button>
            {newJob && <form className="hermes-automation-form" onSubmit={event => void createJob(event)}>
              <div className="hermes-automation-field"><label htmlFor={`${fieldId}-name`}>{t("ui.hermesautomation.m7a1fc2cbc5")}</label><input id={`${fieldId}-name`} value={name} disabled={!!pending || busy} onChange={event => setName(event.target.value)} maxLength={120} /></div>
              <div className="hermes-automation-field"><label htmlFor={`${fieldId}-task`}>{t("ui.hermesautomation.m978ab9d160")}</label><textarea id={`${fieldId}-task`} required value={prompt} disabled={!!pending || busy} onChange={event => setPrompt(event.target.value)} placeholder={t("ui.hermesautomation.m748b744b32")} rows={3} maxLength={16000} /></div>
              {!editingId && <><div className="hermes-automation-field"><label htmlFor={`${fieldId}-repeat`}>{t("ui.hermesautomation.m725347e425")}</label><select id={`${fieldId}-repeat`} value={repeat} disabled={!!pending || busy} onChange={event => { setRepeat(event.target.value); setTimeError(""); }}><option value="once">{t("ui.hermesautomation.mbfe9562ad7")}</option><option value="every 1h">{t("ui.hermesautomation.mb3276918fc")}</option><option value="every 24h">{t("ui.hermesautomation.m15e3619f0c")}</option><option value="every 7d">{t("ui.hermesautomation.m4cd9d058ed")}</option></select></div>
              {repeat === "once" ? <div className="hermes-automation-field"><label htmlFor={`${fieldId}-when`}>{t("ui.hermesautomation.md1910f293e")}</label><input id={`${fieldId}-when`} type="datetime-local" disabled={!!pending || busy} required value={when} aria-invalid={!!timeError} aria-describedby={timeError ? `${fieldId}-time-error` : undefined} onChange={event => { setWhen(event.target.value); setTimeError(""); }} />{timeError && <p id={`${fieldId}-time-error`} className="hermes-error-text" role="alert">{timeError}</p>}</div> : <><p className="hermes-muted">{t("ui.hermesautomation.m74327ea6c6")}</p><label className="hermes-automation-checkbox"><input type="checkbox" disabled={!!pending || busy} checked={continuity} onChange={event => setContinuity(event.target.checked)} />{t("ui.hermesautomation.mf139581688")}</label></>}
              <p className="hermes-task-folder">{workdir ? <><b>{t("ui.hermesautomation.m2022d63efc")}</b><br /><span title={workdir}>{workdir}</span></> : t("ui.hermesautomation.mb6905f1d2b")}</p>
              <p className="hermes-muted">{t("ui.hermesautomation.m7523161bdf")}</p>
              <button type="button" className="hermes-context-button" disabled={disabled} onClick={() => onChooseTaskFolder(workdir, setWorkdir)}><IconFolder size={18} />{workdir ? t("ui.hermesautomation.m748881e8f8") : t("ui.hermesautomation.m1dadaf3bee")}</button>
              </>}
              <button className="btn btn-primary" type="submit" disabled={disabled || !prompt.trim()}>{pending === "create" ? t("ui.hermesautomation.m73bfb5e424") : editingId ? t("ui.hermesautomation.mdcff19f0eb") : t("ui.hermesautomation.m0feddb32ec")}</button>
            </form>}
            {!loading && !error && !jobs.length && <p className="hermes-muted">{t("ui.hermesautomation.mdf0b430729")}</p>}
            <ul className="hermes-automation-list">{jobs.map(job => { const completed = job.state === "completed"; const paused = !completed && (job.enabled === false || job.state === "paused"); return <li key={job.id}>
              <b>{job.name || job.prompt?.slice(0, 80) || t("ui.hermesautomation.mb1a8fecbc6")}</b>
              {job.prompt && <p>{job.prompt}</p>}
              <small>{completed ? t("ui.hermesautomation.mc5ff181c46") : paused ? t("ui.hermesautomation.m2d3af95509") : job.state === "running" ? t("ui.hermesautomation.m37acc42d76") : t("ui.hermesautomation.med15b19b9f", { p0: (nextRun(job)) })}</small>
              {job.workdir && <small title={job.workdir}>{job.workdir}</small>}
              {job.last_error && <p role="alert">{t("ui.hermesautomation.md07b7e3543")}{job.last_error}</p>}
              <div className="hermes-automation-actions">{!completed && <button type="button" className="hermes-context-button" disabled={disabled} onClick={() => void act(job.id, async () => { await client.backendRequest(`/cron/jobs/${encodeURIComponent(job.id)}/${paused ? "resume" : "pause"}?profile=default`, "POST", {}); if (alive.current) setRevision(value => value + 1); })}>{paused ? t("agentSessions.continue") : t("ui.hermesautomation.m4205b1307e")}</button>}
                <button type="button" className="hermes-context-button" disabled={disabled} onClick={() => { setEditingId(job.id); setName(job.name || ""); setPrompt(job.prompt || ""); setTimeError(""); setNewJob(true); }}>{t("ui.hermesautomation.mdd77cd566f")}</button>
                <button type="button" className="hermes-context-button" disabled={!!pending} onClick={() => void act(`runs:${job.id}`, async () => { const result = await client.backendRequest<{ runs: Array<Record<string, unknown>> }>(`/cron/jobs/${encodeURIComponent(job.id)}/runs?profile=default&limit=20`); if (alive.current) setRuns({ id: job.id, entries: result.runs || [] }); })}>{t("ui.hermesautomation.m154741f5cc")}</button>
                <button type="button" className="hermes-context-button" disabled={disabled} onClick={() => setDeleteId(job.id)}>{t("ui.hermesautomation.mbaf47b08bc")}</button></div>
              {deleteId === job.id && <div className="hermes-automation-confirm"><p>{t("ui.hermesautomation.m121bc3bb7a")}</p><button type="button" className="btn btn-secondary" onClick={() => setDeleteId("")}>{t("ui.hermesautomation.ma90b7cbc92")}</button><button type="button" className="btn btn-danger" disabled={disabled} onClick={() => void act(`delete:${job.id}`, async () => { await client.backendRequest(`/cron/jobs/${encodeURIComponent(job.id)}?profile=default`, "DELETE"); if (alive.current) { setDeleteId(""); setRevision(value => value + 1); } })}>{t("ui.hermesautomation.m99e0d97ae9")}</button></div>}
              {runs?.id === job.id && <div className="hermes-automation-results"><h4>{t("ui.hermesautomation.mdcc8ba48fe")}</h4>{!runs.entries.length ? <p>{t("ui.hermesautomation.m232a205e5d")}</p> : runs.entries.map((run, index) => <article key={String(run.id || index)}><b>{String(run.title || t("ui.hermesautomation.m22f094cc07"))}</b><p>{String(run.preview || t("ui.hermesautomation.mff699d38ae"))}</p>{run.source === "cron" && typeof run.id === "string" && <button type="button" className="hermes-context-button" disabled={disabled} onClick={() => onOpenResult(run.id as string)}>{t("ui.hermesautomation.m8c9362f5b5")}</button>}</article>)}</div>}
            </li>; })}</ul>
          </>}
          {key === "memory" && <>
            <p>{t("ui.hermesautomation.mba50704681")}</p>
            <p className="hermes-automation-host">{cwd || t("ui.hermesautomation.m7c486b3300")}</p>
            <button type="button" className="hermes-context-button" disabled={disabled} onClick={onChooseFolder}><IconFolder size={18} />{cwd ? t("ui.hermesautomation.md02c129dbe") : t("ui.hermesautomation.m7bd26add53")}</button>
            <p>{t("ui.hermesautomation.mbc639ea3c5")}</p>
            <button type="button" className="hermes-context-button" disabled={disabled || !cwd} onClick={() => onPreparePrompt(t("ui.hermesautomation.mcb3d1eb744"))}>{t("ui.hermesautomation.m1280f61c26")}</button>
            {memory && <p className="hermes-muted">{memory.active === "builtin" ? ((memory.builtin_files?.memory || 0) + (memory.builtin_files?.user || 0) > 0 ? t("ui.hermesautomation.m8522904fa1") : t("ui.hermesautomation.m551cfa6000")) : memory.active ? t("ui.hermesautomation.m3bd8260681", { p0: (memory.active) }) : t("ui.hermesautomation.m141719c445")}</p>}
            <button type="button" className="hermes-context-button" disabled={disabled || !sessionId || !contextAvailable} title={!contextAvailable ? contextUnavailableReason : undefined} onClick={() => { if (contextAvailable) void act("context", async () => { const result = await onInspectContext(); if (alive.current) setContext(result); }); }}>{t("ui.hermesautomation.m7a35348836")}</button>
            {!contextAvailable && <p className="hermes-muted">{contextUnavailableReason}</p>}
            {!sessionId && <p className="hermes-muted">{t("ui.hermesautomation.m7c48c447b4")}</p>}
            {context && <pre className="hermes-automation-content">{context}</pre>}
            <div className="hermes-memory-preference"><label htmlFor={`${fieldId}-preference`}>{t("ui.hermesautomation.m1f91b9d4ef")}</label><textarea id={`${fieldId}-preference`} value={preference} onChange={event => setPreference(event.target.value)} placeholder={t("ui.hermesautomation.mbc55642d5f")} rows={3} maxLength={8000} /></div>
            <p className="hermes-muted">{t("ui.hermesautomation.m1ccb2e982d")}</p>
            <button type="button" className="hermes-context-button" disabled={disabled || !preference.trim()} onClick={() => onPreparePrompt(t("ui.hermesautomation.m6e6d6d5957", { p0: (preference.trim()) }))}>{t("ui.hermesautomation.m8a8b60f849")}</button>
            <HermesStyleSettings client={client} disabled={disabled} draft={styleDraft} onDraft={onStyleDraft} />
          </>}
          {key === "skills" && <>
            <p>{t("ui.hermesautomation.mb5362e3fdd")}</p>
            {!loading && !error && !skills.length && <p className="hermes-muted">{t("ui.hermesautomation.m7109442407")}</p>}
            <ul className="hermes-automation-list">{skills.map(skill => <li key={skill.name}><div className="hermes-skill-heading"><b>{skill.name}</b><button type="button" role="switch" aria-label={t("ui.hermesautomation.m8c184c1d94", { p0: (skill.name) })} aria-checked={skill.enabled} className="hermes-context-button" disabled={disabled} onClick={() => void act(`skill:${skill.name}`, async () => { await client.backendRequest("/skills/toggle?profile=default", "PUT", { name: skill.name, enabled: !skill.enabled, profile: "default" }); if (alive.current) setRevision(value => value + 1); })}>{skill.enabled ? t("ui.hermesautomation.m12b6f10396") : t("sys.autostartMethodNone")}</button></div>{skill.description && <p>{skill.description}</p>}
              <button type="button" className="hermes-context-button" disabled={!!pending} onClick={() => void act(`content:${skill.name}`, async () => { const result = await client.backendRequest<{ name: string; content: string }>(`/skills/content?profile=default&name=${encodeURIComponent(skill.name)}`); if (alive.current) setSkillContent(result); })}>{t("ui.hermesautomation.m172d395e83")}</button>
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
      if (alive.current) { onDraft({ content, dirty: false }); setNotice(t("ui.hermesautomation.m51c541d1da")); }
    } catch (e) { if (alive.current) setError(failure(e)); }
    finally { if (alive.current) setPending(false); }
  }
  return <form className="hermes-automation-form hermes-style-form" onSubmit={event => void save(event)}>
    <h4>{t("ui.hermesautomation.m6848775545")}</h4><p>{t("ui.hermesautomation.mc15eba38d4")}</p>
    {error && <div role="alert"><p>{error}</p><button type="button" className="hermes-context-button" disabled={pending} onClick={() => setAttempt(value => value + 1)}>{t("ui.hermesautomation.me20f7e15a3")}</button></div>}
    {!loaded && !error && <p role="status">{t("ui.hermesautomation.m5c398b4394")}</p>}
    {loaded && <><div className="hermes-automation-field"><label htmlFor={fieldId}>{t("ui.hermesautomation.m2a1e543dbf")}</label><textarea id={fieldId} value={content} disabled={pending} onChange={event => { onDraft({ content: event.target.value, dirty: true }); setNotice(""); }} rows={5} maxLength={32000} /></div>{dirty && <p className="hermes-muted">{t("ui.hermesautomation.m6f6e87622c")}</p>}<button type="submit" className="btn btn-primary" disabled={disabled || pending || !dirty}>{pending ? t("ui.hermesautomation.m73bfb5e424") : t("ui.hermesautomation.m3a03c1e8a4")}</button></>}
    {notice && <p role="status">{notice}</p>}
  </form>;
}
