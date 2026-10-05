import { useCallback, useEffect, useId, useMemo, useRef, useState } from "react";
import {
  addMcpServer, checkMcpForm, confirmDialog, deleteMcpServer, getMcpServers, mapApiError,
  mcpServerSummary, suggestMcpName, t, toggleMcpServer, useToast,
  type McpAddResult, type McpAgent, type McpCaps, type McpForm, type McpFormField, type McpServer, type McpType,
} from "@tgcontrol/shared";
import { haptic, hapticError, hapticSuccess } from "../telegram";
import "./McpPanel.css";

/**
 * «MCP-серверы» в разделе «Агенты»: подключения, которые дают агенту новые
 * умения (браузер, база, трекер задач), — без правки JSON и TOML руками.
 *
 * Пишет в конфиг сам CLI агента (internal/mcpmgr), здесь только форма и
 * список. Значений ключей экран не видит никогда: сервер отдаёт лишь имена.
 * Правила формы — в @tgcontrol/shared/mcp.ts, с тестами.
 */

/** Текст ошибки: у ответа компьютера он уже человеческий, у обрыва — нет. */
function errorText(e: unknown): string {
  const status = (e as { status?: number })?.status;
  const message = (e as { message?: string })?.message;
  if (status && status >= 400 && status < 500 && message) return message;
  return mapApiError(e);
}

const EMPTY_FORM: McpForm = { agents: [], type: "stdio", name: "", commandLine: "", url: "", envText: "", headersText: "" };

function TrashIcon() {
  return (
    <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" aria-hidden>
      <path d="M4 7h16M10 11v6M14 11v6M6 7l1 12a2 2 0 0 0 2 2h6a2 2 0 0 0 2-2l1-12M9 7V4h6v3" />
    </svg>
  );
}

function PlusIcon() {
  return (
    <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" aria-hidden>
      <path d="M12 5v14M5 12h14" />
    </svg>
  );
}

/** «3 ключа: API_KEY, TOKEN…» — только имена, значений здесь нет. */
function keysLine(s: McpServer): string {
  const env = s.env_keys || [];
  const hdr = s.header_keys || [];
  const parts: string[] = [];
  if (env.length) parts.push(t("mcp.envKeys", { n: env.length, names: env.join(", ") }));
  if (hdr.length) parts.push(t("mcp.headerKeys", { n: hdr.length, names: hdr.join(", ") }));
  return parts.join(" · ");
}

function noteText(s: McpServer): string {
  if (s.scope === "project") return t("mcp.noteProject", { path: s.project || "" });
  if (s.note === "extra_settings") return t("mcp.noteExtra");
  if (s.note === "disabled_by_agent") return t("mcp.noteDisabledByAgent");
  return "";
}

function ServerRow({ server, busy, onToggle, onDelete }: {
  server: McpServer;
  busy: boolean;
  onToggle: () => void;
  onDelete: () => void;
}) {
  const note = noteText(server);
  const keys = keysLine(server);
  const editable = !server.read_only && server.scope === "user";
  return (
    <li className={`mcp-server${server.enabled ? "" : " is-off"}`} data-mcp-server={server.name}>
      <div className="mcp-server-main">
        <div className="mcp-server-head">
          <span className="mcp-server-name">{server.name}</span>
          <span className="mcp-server-type">{["stdio", "http", "sse"].includes(server.type) ? t(`mcp.type.${server.type}`) : server.type}</span>
          {!server.enabled && <span className="mcp-server-state">{t("mcp.off")}</span>}
        </div>
        <code className="mcp-server-cmd">{mcpServerSummary(server)}</code>
        {keys && <div className="mcp-server-meta">{keys}</div>}
        {note && <div className="mcp-server-note">{note}</div>}
      </div>
      {editable && (
        <div className="mcp-server-actions">
          {server.can_toggle && (
            <button
              type="button"
              className="mcp-switch"
              role="switch"
              aria-checked={server.enabled}
              aria-label={t(server.enabled ? "mcp.turnOff" : "mcp.turnOn", { name: server.name })}
              disabled={busy}
              onClick={onToggle}
            >
              <span className={`advanced-toggle${server.enabled ? " on" : ""}`} aria-hidden>
                <span className="advanced-toggle-thumb" />
              </span>
            </button>
          )}
          <button
            type="button"
            className="mcp-icon-btn"
            aria-label={t("mcp.deleteAria", { name: server.name })}
            disabled={busy}
            onClick={onDelete}
          >
            <TrashIcon />
          </button>
        </div>
      )}
    </li>
  );
}

function AgentGroup({ agent, busyKey, onAccount, onToggle, onDelete }: {
  agent: McpAgent;
  busyKey: string;
  onAccount: (agentID: string, accountID: string) => void;
  onToggle: (agent: McpAgent, s: McpServer) => void;
  onDelete: (agent: McpAgent, s: McpServer) => void;
}) {
  const own = agent.servers.filter((s) => s.scope === "user");
  const project = agent.servers.filter((s) => s.scope !== "user");
  const [showProject, setShowProject] = useState(false);
  const onCount = own.filter((s) => s.enabled).length;
  return (
    <section className="mcp-agent" data-mcp-agent={agent.id} aria-label={agent.name}>
      <header className="mcp-agent-head">
        <h3 className="mcp-agent-name">{agent.name}</h3>
        {agent.installed && !agent.error && (
          <span className="mcp-agent-count">{t("mcp.count", { n: onCount, total: own.length })}</span>
        )}
      </header>
      {agent.installed && agent.accounts.length > 1 && (
        <div className="mcp-accounts" role="radiogroup" aria-label={t("mcp.accountLabel")}>
          {agent.accounts.map((a) => (
            <button
              key={a.id}
              type="button"
              role="radio"
              aria-checked={a.id === agent.account}
              className={`mcp-chip${a.id === agent.account ? " is-on" : ""}`}
              onClick={() => { haptic(); onAccount(agent.id, a.id); }}
            >
              {a.is_default ? t("mcp.accountMain") : (a.label || a.id)}
            </button>
          ))}
        </div>
      )}
      {!agent.installed ? (
        <p className="mcp-muted">{t("mcp.notInstalled")}</p>
      ) : agent.error ? (
        <p className="mcp-error" role="alert">{agent.error}</p>
      ) : own.length === 0 ? (
        <p className="mcp-muted">{t("mcp.emptyAgent")}</p>
      ) : (
        <ul className="mcp-list">
          {own.map((s) => (
            <ServerRow
              key={s.name}
              server={s}
              busy={busyKey === `${agent.id}/${s.name}`}
              onToggle={() => onToggle(agent, s)}
              onDelete={() => onDelete(agent, s)}
            />
          ))}
        </ul>
      )}
      {project.length > 0 && (
        <>
          <button type="button" className="mcp-link" aria-expanded={showProject} onClick={() => setShowProject((v) => !v)}>
            {t(showProject ? "mcp.projectHide" : "mcp.projectShow", { n: project.length })}
          </button>
          {showProject && (
            <ul className="mcp-list">
              {project.map((s) => (
                <ServerRow key={`${s.project}/${s.name}`} server={s} busy={false} onToggle={() => {}} onDelete={() => {}} />
              ))}
            </ul>
          )}
        </>
      )}
    </section>
  );
}

function FieldError({ id, err }: { id: string; err?: { key: string; line?: number } }) {
  if (!err) return null;
  return <p id={id} className="mcp-field-error" role="alert">{t(err.key, { line: err.line ?? "" })}</p>;
}

function AddForm({ agents, onDone, onChanged, onCancel }: {
  agents: McpAgent[];
  onDone: () => void;
  /** Часть агентов уже приняла сервер — обновить список, форму не закрывать. */
  onChanged: () => void;
  onCancel: () => void;
}) {
  const uid = useId();
  const { toastSuccess } = useToast();
  const installed = agents.filter((a) => a.installed && !a.error);
  const caps = useMemo(() => {
    const m: Record<string, McpCaps> = {};
    for (const a of agents) m[a.id] = a.caps;
    return m;
  }, [agents]);
  const [form, setForm] = useState<McpForm>(() => ({ ...EMPTY_FORM, agents: installed.slice(0, 1).map((a) => a.id) }));
  const [nameTouched, setNameTouched] = useState(false);
  const [showSecrets, setShowSecrets] = useState(false);
  const [errors, setErrors] = useState<Partial<Record<McpFormField, { key: string; line?: number }>>>({});
  const [saving, setSaving] = useState(false);
  const [results, setResults] = useState<McpAddResult[]>([]);
  const firstRef = useRef<HTMLInputElement>(null);

  useEffect(() => { firstRef.current?.focus({ preventScroll: true }); }, [form.type]);

  // Какие типы вообще предлагать: объединение того, что умеют выбранные
  // агенты. SSE есть только у Claude — при выбранном Codex его не будет.
  const allTypes: McpType[] = ["stdio", "http", "sse"];
  const typeAllowed = (tp: McpType) => form.agents.length === 0
    ? installed.some((a) => a.caps.types.includes(tp))
    : form.agents.every((id) => caps[id]?.types.includes(tp));
  const headersAllowed = form.agents.every((id) => caps[id]?.headers);

  const patch = (p: Partial<McpForm>) => {
    setForm((prev) => {
      const next = { ...prev, ...p };
      if (!nameTouched && (p.commandLine !== undefined || p.url !== undefined || p.type !== undefined)) {
        next.name = suggestMcpName(next.type, next.commandLine, next.url);
      }
      // Выбрали агента, который не умеет текущий тип, — откатываемся на
      // «программу», а не оставляем форму в невыполнимом состоянии.
      if (p.agents && !p.agents.every((id) => caps[id]?.types.includes(next.type))) next.type = "stdio";
      return next;
    });
    setErrors({});
    setResults([]);
  };

  const toggleAgent = (id: string) => {
    haptic();
    patch({ agents: form.agents.includes(id) ? form.agents.filter((a) => a !== id) : [...form.agents, id] });
  };

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    const check = checkMcpForm(form, caps);
    setErrors(check.errors);
    if (!check.spec) { hapticError(); return; }
    setSaving(true);
    try {
      const targets = form.agents.map((id) => ({ agent: id, account: agents.find((a) => a.id === id)?.account || "default" }));
      const res = await addMcpServer(targets, check.spec);
      const failed = res.results.filter((r) => !r.ok);
      if (failed.length === 0) {
        hapticSuccess();
        toastSuccess(t("mcp.added", { name: check.spec.name }));
        onDone();
        return;
      }
      // Частичный успех: у кого получилось — уже добавлено, форму не
      // сбрасываем, чтобы было видно, где отказ и почему.
      setResults(res.results);
      // Повторная отправка должна идти только туда, где не получилось: у
      // принявшего агента она упёрлась бы в «уже есть».
      const okIDs = res.results.filter((r) => r.ok).map((r) => r.agent);
      setForm((prev) => ({ ...prev, agents: prev.agents.filter((id) => !okIDs.includes(id)) }));
      hapticError();
      if (res.results.some((r) => r.ok)) onChanged();
    } catch (err) {
      hapticError();
      const payload = (err as { results?: McpAddResult[] })?.results;
      setResults(payload && payload.length ? payload : form.agents.map((agent) => ({ agent, ok: false, error: errorText(err) })));
    } finally {
      setSaving(false);
    }
  };

  const errId = (f: string) => `${uid}-${f}-err`;
  const agentName = (id: string) => agents.find((a) => a.id === id)?.name || id;
  const isStdio = form.type === "stdio";

  if (installed.length === 0) {
    return (
      <div className="mcp-form">
        <p className="mcp-muted">{t("mcp.noAgents")}</p>
        <button type="button" className="btn btn-secondary" onClick={onCancel}>{t("mcp.cancel")}</button>
      </div>
    );
  }

  return (
    <form className="mcp-form" onSubmit={submit} noValidate>
      <h3 className="mcp-form-title">{t("mcp.formTitle")}</h3>

      <fieldset className="mcp-field">
        <legend className="mcp-label">{t("mcp.fieldAgents")}</legend>
        <div className="mcp-chips">
          {installed.map((a) => (
            <button
              key={a.id}
              type="button"
              role="checkbox"
              aria-checked={form.agents.includes(a.id)}
              className={`mcp-chip${form.agents.includes(a.id) ? " is-on" : ""}`}
              onClick={() => toggleAgent(a.id)}
            >
              {a.name}
            </button>
          ))}
        </div>
        <FieldError id={errId("agents")} err={errors.agents} />
      </fieldset>

      <fieldset className="mcp-field">
        <legend className="mcp-label">{t("mcp.fieldType")}</legend>
        <div className="mcp-segment" role="radiogroup">
          {allTypes.filter(typeAllowed).map((tp) => (
            <button
              key={tp}
              type="button"
              role="radio"
              aria-checked={form.type === tp}
              className={`mcp-segment-btn${form.type === tp ? " is-on" : ""}`}
              onClick={() => { haptic(); patch({ type: tp }); }}
            >
              <span className="mcp-segment-title">{t(`mcp.typeTitle.${tp}`)}</span>
              <span className="mcp-segment-hint">{t(`mcp.typeHint.${tp}`)}</span>
            </button>
          ))}
        </div>
      </fieldset>

      {isStdio ? (
        <div className="mcp-field">
          <label className="mcp-label" htmlFor={`${uid}-cmd`}>{t("mcp.fieldCommand")}</label>
          <input
            ref={firstRef}
            id={`${uid}-cmd`}
            className="mcp-input mcp-input-code"
            value={form.commandLine}
            onChange={(e) => patch({ commandLine: e.target.value })}
            placeholder="npx -y @modelcontextprotocol/server-filesystem C:\work"
            autoCapitalize="off" autoCorrect="off" spellCheck={false} inputMode="text"
            aria-invalid={!!errors.commandLine}
            aria-describedby={errors.commandLine ? errId("commandLine") : `${uid}-cmd-hint`}
          />
          <p id={`${uid}-cmd-hint`} className="mcp-hint">{t("mcp.hintCommand")}</p>
          <FieldError id={errId("commandLine")} err={errors.commandLine} />
        </div>
      ) : (
        <div className="mcp-field">
          <label className="mcp-label" htmlFor={`${uid}-url`}>{t("mcp.fieldUrl")}</label>
          <input
            ref={firstRef}
            id={`${uid}-url`}
            className="mcp-input mcp-input-code"
            type="url"
            value={form.url}
            onChange={(e) => patch({ url: e.target.value })}
            placeholder="https://mcp.example.com/mcp"
            autoCapitalize="off" autoCorrect="off" spellCheck={false} inputMode="url"
            aria-invalid={!!errors.url}
            aria-describedby={errors.url ? errId("url") : undefined}
          />
          <FieldError id={errId("url")} err={errors.url} />
        </div>
      )}

      <div className="mcp-field">
        <label className="mcp-label" htmlFor={`${uid}-name`}>{t("mcp.fieldName")}</label>
        <input
          id={`${uid}-name`}
          className="mcp-input"
          value={form.name}
          onChange={(e) => { setNameTouched(true); patch({ name: e.target.value }); }}
          placeholder="filesystem"
          autoCapitalize="off" autoCorrect="off" spellCheck={false}
          maxLength={64}
          aria-invalid={!!errors.name}
          aria-describedby={errors.name ? errId("name") : `${uid}-name-hint`}
        />
        <p id={`${uid}-name-hint`} className="mcp-hint">{t("mcp.hintName")}</p>
        <FieldError id={errId("name")} err={errors.name} />
      </div>

      {/* Заголовки уже введены, а потом выбрали Codex — поле не прячем: иначе
          ошибка «Codex не принимает заголовки» ссылалась бы на невидимое. */}
      {(isStdio || headersAllowed || form.headersText.trim() !== "") && (
        <div className="mcp-field">
          <button
            type="button"
            className="mcp-link mcp-disclosure"
            aria-expanded={showSecrets}
            onClick={() => setShowSecrets((v) => !v)}
          >
            {t(isStdio ? "mcp.envToggle" : "mcp.headersToggle")}
            <span aria-hidden className="mcp-disclosure-chev">{showSecrets ? "−" : "+"}</span>
          </button>
          {(showSecrets || (isStdio ? errors.envText : errors.headersText)) && (
            <>
              <textarea
                id={`${uid}-secrets`}
                className="mcp-input mcp-textarea mcp-input-code"
                aria-label={t(isStdio ? "mcp.fieldEnv" : "mcp.fieldHeaders")}
                value={isStdio ? form.envText : form.headersText}
                onChange={(e) => patch(isStdio ? { envText: e.target.value } : { headersText: e.target.value })}
                placeholder={isStdio ? "API_KEY=ваш-ключ" : "Authorization: Bearer ваш-ключ"}
                rows={3}
                autoCapitalize="off" autoCorrect="off" spellCheck={false}
                aria-invalid={!!(isStdio ? errors.envText : errors.headersText)}
                aria-describedby={`${uid}-secrets-hint`}
              />
              <p id={`${uid}-secrets-hint`} className="mcp-hint">{t(isStdio ? "mcp.hintEnv" : "mcp.hintHeaders")}</p>
              <FieldError id={errId("secrets")} err={isStdio ? errors.envText : errors.headersText} />
            </>
          )}
        </div>
      )}

      {results.length > 0 && (
        <ul className="mcp-results" aria-live="polite">
          {results.map((r) => (
            <li key={r.agent} className={r.ok ? "is-ok" : "is-fail"}>
              <strong>{agentName(r.agent)}:</strong> {r.ok ? t("mcp.resultOk") : (r.error || t("mcp.resultFail"))}
            </li>
          ))}
        </ul>
      )}

      <div className="mcp-form-actions">
        <button type="submit" className="btn btn-primary mcp-grow" disabled={saving}>
          {saving ? t("mcp.saving") : t("mcp.addSubmit")}
        </button>
        <button type="button" className="btn btn-secondary" onClick={onCancel} disabled={saving}>{t("mcp.cancel")}</button>
      </div>
    </form>
  );
}

export function McpPanel() {
  const { toastError, toastSuccess } = useToast();
  const [agents, setAgents] = useState<McpAgent[] | null>(null);
  const [loadError, setLoadError] = useState("");
  const [picked, setPicked] = useState<Record<string, string>>({});
  const [busyKey, setBusyKey] = useState("");
  const [adding, setAdding] = useState(false);

  // Номер последнего запроса: ответ Codex идёт секунду-две, и перечитывание
  // после удаления могло прийти ПОЗЖЕ ответа на смену аккаунта — экран
  // показал бы список не того аккаунта (поймано пробой probe-mcp).
  const seq = useRef(0);
  // Выбор аккаунтов — через ref: перечитывание после действия обязано взять
  // ТЕКУЩИЙ выбор, а не тот, что был на момент нажатия.
  const pickedRef = useRef(picked);
  pickedRef.current = picked;
  const load = useCallback(async () => {
    const my = ++seq.current;
    try {
      const res = await getMcpServers(Object.values(pickedRef.current));
      if (my !== seq.current) return;
      setAgents(res.agents || []);
      setLoadError("");
    } catch (e) {
      if (my !== seq.current) return;
      setLoadError(errorText(e));
    }
  }, []);

  useEffect(() => { void load(); }, [load, picked]);

  const target = (a: McpAgent) => ({ agent: a.id, account: a.account });

  const onToggle = async (a: McpAgent, s: McpServer) => {
    haptic();
    setBusyKey(`${a.id}/${s.name}`);
    // Сразу показываем новое положение: CLI отвечает секунду-две, и тумблер,
    // который не двигается под пальцем, выглядит сломанным.
    setAgents((prev) => prev && prev.map((g) => g.id !== a.id ? g : {
      ...g, servers: g.servers.map((x) => x.name === s.name && x.scope === "user" ? { ...x, enabled: !s.enabled } : x),
    }));
    try {
      await toggleMcpServer(target(a), s.name, !s.enabled);
      toastSuccess(t(s.enabled ? "mcp.turnedOff" : "mcp.turnedOn", { name: s.name }));
    } catch (e) {
      hapticError();
      toastError(errorText(e));
    } finally {
      setBusyKey("");
      await load();
    }
  };

  const onDelete = async (a: McpAgent, s: McpServer) => {
    haptic();
    const ok = await confirmDialog(t("mcp.deleteConfirm", { name: s.name, agent: a.name }), {
      title: t("mcp.deleteTitle"), danger: true, confirmText: t("mcp.delete"),
    });
    if (!ok) return;
    setBusyKey(`${a.id}/${s.name}`);
    try {
      await deleteMcpServer(target(a), s.name);
      hapticSuccess();
      toastSuccess(t("mcp.deleted", { name: s.name }));
    } catch (e) {
      hapticError();
      toastError(errorText(e));
    } finally {
      setBusyKey("");
      await load();
    }
  };

  const onAccount = (agentID: string, accountID: string) => {
    setPicked((prev) => ({ ...prev, [agentID]: accountID }));
  };

  return (
    <div className="mcp-panel">
      <p className="mcp-intro">{t("mcp.intro")}</p>
      {loadError && (
        <div className="mcp-error" role="alert">
          {loadError}{" "}
          <button type="button" className="mcp-link" onClick={() => void load()}>{t("mcp.retry")}</button>
        </div>
      )}
      {agents === null && !loadError && <p className="mcp-muted" aria-busy="true">{t("mcp.loading")}</p>}
      {agents && agents.map((a) => (
        <AgentGroup key={a.id} agent={a} busyKey={busyKey} onAccount={onAccount} onToggle={onToggle} onDelete={onDelete} />
      ))}
      {agents && (adding ? (
        <AddForm
          agents={agents}
          onDone={() => { setAdding(false); void load(); }}
          onChanged={() => void load()}
          onCancel={() => setAdding(false)}
        />
      ) : (
        <button type="button" className="btn btn-primary mcp-add" onClick={() => { haptic(); setAdding(true); }}>
          <PlusIcon />{t("mcp.add")}
        </button>
      ))}
    </div>
  );
}
