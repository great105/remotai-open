// Экран «Беседы» (/agents/sessions): все прошлые беседы Claude Code и Codex на
// этом компьютере по всем аккаунтам, с поиском. Нажатие на беседу, которая уже
// идёт или спит в терминале Remotai, открывает ЕГО; остальные — подтверждение
// прямо в строке и новый терминал в папке беседы, тем же аккаунтом.
//
// Правила (куда ведёт нажатие, команда, дни) — в agentSessions/rules.ts.

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import {
  ApiError, getAgentSessions, mapApiError, t,
  type AgentSessionItem, type AgentSessionsPage,
} from "@tgcontrol/shared";
import { createPtySession, getAgentAccounts, getAgents, ptyInput, setPtyAccount } from "../api";
import type { AgentInfo } from "../types";
import { BottomNav } from "../components/BottomNav";
import { useGoBack } from "../navBack";
import { haptic, hapticError, hapticSuccess } from "../telegram";
import { useCapabilities } from "../hooks/useCapabilities";
import {
  launchAccountForAgent, loadPrefs, recordedResumeAccount, type LaunchAccount,
} from "../ptyTerm/agentLaunch";
import {
  folderName, groupByDay, isPosixShell, mergePages, resumeSessionCommand, sessionAction,
  sessionKey, skipPermissionsFlag, timeLabel,
} from "../agentSessions/rules";
import "../agentSessions/agentSessions.css";

const PAGE = 50;
// Компьютер не успел разобрать все файлы (первый заход) — дочитываем сами.
const PARTIAL_RETRY_MS = 2500;
const PARTIAL_RETRIES = 6;

type LoadState = "loading" | "ready" | "error";
type Filter = "" | "claude" | "codex";

function isOldAgent(e: unknown): boolean {
  // Старый Remotai на ПК: маршрута нет (404) или вместо JSON пришла страница.
  return (e instanceof ApiError && e.status === 404) || e instanceof SyntaxError;
}

function SearchIcon() {
  return (
    <svg viewBox="0 0 24 24" width="18" height="18" aria-hidden="true" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round">
      <circle cx="11" cy="11" r="6.5" />
      <path d="m16 16 4.5 4.5" />
    </svg>
  );
}

function Chevron({ open }: { open?: boolean }) {
  return (
    <svg className={`as-chev${open ? " is-open" : ""}`} viewBox="0 0 24 24" width="18" height="18" aria-hidden="true" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
      <path d="m9 6 6 6-6 6" />
    </svg>
  );
}

export function AgentSessionsView() {
  const navigate = useNavigate();
  const goBack = useGoBack();
  const { platform } = useCapabilities();

  const [input, setInput] = useState("");
  const [q, setQ] = useState("");
  const [filter, setFilter] = useState<Filter>("");
  const [items, setItems] = useState<AgentSessionItem[]>([]);
  const [meta, setMeta] = useState<Omit<AgentSessionsPage, "sessions"> | null>(null);
  const [state, setState] = useState<LoadState>("loading");
  const [error, setError] = useState("");
  const [listError, setListError] = useState("");
  const [oldAgent, setOldAgent] = useState(false);
  const [loadingMore, setLoadingMore] = useState(false);
  const [agents, setAgents] = useState<AgentInfo[] | null>(null);
  const [agentsError, setAgentsError] = useState("");
  const [partialTick, setPartialTick] = useState(0);

  const [expanded, setExpanded] = useState("");
  const [skip, setSkip] = useState(false);
  const [launching, setLaunching] = useState(false);
  const [launchError, setLaunchError] = useState("");

  const requestRef = useRef(0);
  const retriesRef = useRef(0);

  // Поиск — после паузы в наборе: каждый символ с телефона не должен
  // становиться запросом через облако.
  useEffect(() => {
    const timer = window.setTimeout(() => setQ(input.trim()), 250);
    return () => window.clearTimeout(timer);
  }, [input]);

  const loadAgents = useCallback(async () => {
    setAgents(null);
    setAgentsError("");
    try {
      const data = await getAgents();
      setAgents(data.agents || []);
    } catch (e) {
      setAgentsError(mapApiError(e));
    }
  }, []);
  useEffect(() => { void loadAgents(); }, [loadAgents]);

  const load = useCallback(async (mode: "reset" | "quiet") => {
    const req = ++requestRef.current;
    if (mode === "reset") {
      setState("loading");
      setListError("");
      retriesRef.current = 0;
    }
    try {
      const page = await getAgentSessions({ q, agent: filter, limit: PAGE });
      if (req !== requestRef.current) return;
      const { sessions, ...rest } = page;
      setItems(sessions || []);
      setMeta(rest);
      setError("");
      setListError("");
      setState("ready");
    } catch (e) {
      if (req !== requestRef.current) return;
      const old = isOldAgent(e);
      if (mode === "quiet") {
        setListError(mapApiError(e));
        return;
      }
      setOldAgent(old);
      setError(old ? t("agentSessions.oldAgent") : mapApiError(e));
      setState("error");
    } finally {
      // Even a failed background read must advance the retry timer; otherwise
      // the first transient error leaves the "will update soon" state forever.
      if (mode === "quiet" && req === requestRef.current) setPartialTick((tick) => tick + 1);
    }
  }, [q, filter]);

  useEffect(() => { void load("reset"); }, [load]);

  // Первый заход на компьютер с сотнями бесед: сервер отдал, что успел
  // разобрать за свой бюджет. Дочитываем тихо, не сбрасывая экран.
  useEffect(() => {
    if (state !== "ready" || !meta?.partial || retriesRef.current >= PARTIAL_RETRIES) return;
    const timer = window.setTimeout(() => {
      retriesRef.current += 1;
      void load("quiet");
    }, PARTIAL_RETRY_MS);
    return () => window.clearTimeout(timer);
  }, [state, meta, load, partialTick]);

  const loadMore = async () => {
    if (!meta?.next || loadingMore) return;
    haptic();
    setLoadingMore(true);
    setListError("");
    const req = requestRef.current;
    try {
      const page = await getAgentSessions({ q, agent: filter, limit: PAGE, cursor: meta.next });
      if (req !== requestRef.current) return;
      const { sessions, ...rest } = page;
      setItems((prev) => mergePages(prev, sessions || []));
      setMeta(rest);
      setListError("");
    } catch (e) {
      if (req === requestRef.current) setListError(mapApiError(e));
    } finally {
      setLoadingMore(false);
    }
  };

  const agentById = useMemo(() => {
    const map: Record<string, AgentInfo> = {};
    for (const a of agents || []) map[a.id] = a;
    return map;
  }, [agents]);

  const agentName = (id: string) => agentById[id]?.name || (id === "claude" ? "Claude Code" : id === "codex" ? "Codex" : id);

  const groups = useMemo(
    () => groupByDay(items, Date.now(), t("agentSessions.today"), t("agentSessions.yesterday")),
    [items],
  );

  const onPick = (item: AgentSessionItem) => {
    const action = sessionAction(item, agentById[item.agent]);
    if (!agents && action.kind !== "open") return;
    haptic();
    if (action.kind === "open") {
      navigate(`/pty/${encodeURIComponent(action.ptyId)}`, { state: { from: "/agents/sessions" } });
      return;
    }
    const key = sessionKey(item);
    if (expanded === key) {
      setExpanded("");
      return;
    }
    const agent = agentById[item.agent];
    const flag = skipPermissionsFlag(agent);
    // «Без подтверждений» — как человек запускал этого агента в прошлый раз
    // (память шторки запуска), а не всегда выключено.
    setSkip(Boolean(flag && agent && loadPrefs(agent.id).flags.includes(flag)));
    setLaunchError("");
    setExpanded(key);
  };

  const onContinue = async (item: AgentSessionItem) => {
    const agent = agentById[item.agent];
    if (launching || !agent) return;
    haptic();
    setLaunching(true);
    setLaunchError("");
    try {
      // Аккаунт перечитываем перед запуском, как «Продолжить» в терминале:
      // другая шторка могла сменить прокси или профиль. Ошибка — стоп, а не
      // запуск основным аккаунтом мимо прокси.
      let launchAccount: LaunchAccount | undefined;
      let version = 0;
      let account: ReturnType<typeof recordedResumeAccount> = null;
      if (agent.account_env) {
        const payload = await getAgentAccounts(undefined, item.cwd || ".");
        version = Number.isSafeInteger(payload.proxy_contract_version) ? Number(payload.proxy_contract_version) : 0;
        const accounts = payload.accounts || [];
        account = recordedResumeAccount(accounts, item.agent, item.account_id);
        launchAccount = launchAccountForAgent(agent, { accounts, proxyContractVersion: version, accountsReady: true }, account);
        if (launchAccount?.blockedReason) throw new Error(launchAccount.blockedReason);
      }
      const command = resumeSessionCommand(agent, item, skip, {
        agentID: agent.id,
        accountEnvName: agent.account_env || "",
        proxyContractVersion: version,
        blockedReason: launchAccount?.blockedReason,
        account: launchAccount,
        posix: isPosixShell(platform, item.cwd),
        launchArgs: agent.launch_args,
      });
      if (!command) throw new Error(t("agentSessions.unavailable", { name: agent.name }));
      const res = await createPtySession(item.cwd || ".", "", 80, 24);
      await setPtyAccount(
        res.id,
        account?.is_default ? "" : (account?.id || ""),
        account && !account.is_default ? account.label : "",
      );
      await ptyInput(res.id, { data: command + "\r" });
      try { localStorage.setItem(`pty.lastAgent.${res.id}`, agent.id); } catch { /* ignore */ }
      hapticSuccess();
      navigate(`/pty/${encodeURIComponent(res.id)}`, { state: { from: "/agents/sessions" } });
    } catch (e) {
      hapticError();
      setLaunchError(mapApiError(e));
    } finally {
      setLaunching(false);
    }
  };

  const counts = meta?.agents || {};
  const allCount = (counts.claude || 0) + (counts.codex || 0);
  const filters: Array<{ id: Filter; label: string; n: number }> = [
    { id: "", label: t("agentSessions.filterAll"), n: allCount },
    // Первое слово имени («Claude Code» → «Claude»): на 320 px три
    // переключателя с полными именами обрезались до «Claude …».
    { id: "claude", label: agentName("claude").split(" ")[0], n: counts.claude || 0 },
    { id: "codex", label: agentName("codex").split(" ")[0], n: counts.codex || 0 },
  ];

  const renderStatus = (item: AgentSessionItem) => {
    if (item.open_pty_id) {
      return (
        <span className={`as-status ${item.sleeping ? "is-sleeping" : "is-open"}`}>
          {t(item.sleeping ? "agentSessions.statusSleeping" : "agentSessions.statusOpen")}
        </span>
      );
    }
    if (item.running) return <span className="as-status is-running">{t("agentSessions.statusRunning")}</span>;
    return null;
  };

  const renderConfirm = (item: AgentSessionItem) => {
    const agent = agentById[item.agent];
    const flag = skipPermissionsFlag(agent);
    const accountLabel = !item.account_id || item.account_id === "default"
      ? t("agentSessions.accountDefault")
      : (item.account_label || item.account_id);
    return (
      <div className="as-confirm" id={`as-confirm-${sessionKey(item)}`}>
        <h3>{t("agentSessions.confirmTitle")}</h3>
        <dl className="as-facts">
          <div><dt>{t("agentSessions.confirmFolder")}</dt><dd className="as-path">{item.cwd || "—"}</dd></div>
          <div><dt>{t("agentSessions.confirmAccount")}</dt><dd>{agentName(item.agent)} · {accountLabel}</dd></div>
        </dl>
        {item.running && <p className="as-warn" role="note">{t("agentSessions.runningWarn")}</p>}
        {flag && (
          <label className="as-switch">
            <span>
              <b>{t("agentSessions.skipPermissions")}</b>
              <small>{t("agentSessions.skipPermissionsHint")}</small>
            </span>
            <input
              type="checkbox"
              role="switch"
              checked={skip}
              onChange={(e) => { haptic(); setSkip(e.target.checked); }}
            />
            <i aria-hidden="true" />
          </label>
        )}
        {launchError && <p className="as-error" role="alert">{launchError}</p>}
        <div className="as-confirm-actions">
          <button type="button" className="btn btn-secondary" disabled={launching} onClick={() => { haptic(); setExpanded(""); }}>
            {t("agentSessions.cancel")}
          </button>
          <button type="button" className="btn btn-primary as-continue" disabled={launching} onClick={() => void onContinue(item)}>
            {launching ? t("agentSessions.starting") : t("agentSessions.continue")}
          </button>
        </div>
      </div>
    );
  };

  const renderRow = (item: AgentSessionItem) => {
    const key = sessionKey(item);
    const open = expanded === key;
    const action = sessionAction(item, agentById[item.agent]);
    const unavailable = agents !== null && action.kind === "unavailable";
    const accountNote = item.account_id && item.account_id !== "default" ? (item.account_label || item.account_id) : "";
    return (
      <li key={key} className={`as-item${open ? " is-expanded" : ""}`} data-session={item.session_id} data-agent={item.agent}>
        <button
          type="button"
          className="as-row"
          aria-expanded={action.kind === "open" ? undefined : open}
          aria-controls={open ? `as-confirm-${key}` : undefined}
          aria-label={action.kind === "open" ? `${item.title || t("agentSessions.untitled")}. ${t("agentSessions.opening")}` : undefined}
          disabled={(agents === null && action.kind !== "open") || unavailable}
          onClick={() => onPick(item)}
        >
          <span className="as-row-main">
            <span className={`as-title${item.title ? "" : " is-untitled"}`}>{item.title || t("agentSessions.untitled")}</span>
            <span className="as-meta">
              {renderStatus(item)}
              <span className={`as-agent as-agent-${item.agent}`}>{agentName(item.agent)}</span>
              {item.cwd && <span className="as-folder" title={item.cwd}>{folderName(item.cwd)}</span>}
              <span>{timeLabel(item.updated_at)}</span>
              {item.messages !== undefined && <span>{t("agentSessions.messages", { n: item.messages })}</span>}
              {accountNote && <span className="as-account">{accountNote}</span>}
            </span>
            {unavailable && <span className="as-unavailable">{t("agentSessions.unavailable", { name: agentName(item.agent) })}</span>}
          </span>
          {(agents !== null || action.kind === "open") && !unavailable && <Chevron open={open} />}
        </button>
        {open && renderConfirm(item)}
      </li>
    );
  };

  const searching = q !== "" || filter !== "";

  return (
    <div className="usage-page as-page">
      <header className="usage-header">
        <button className="infra-back" aria-label={t("agentSessions.back")} onClick={goBack}>←</button>
        <div>
          <h1>{t("agentSessions.title")}</h1>
          <p>{t("agentSessions.subtitle")}</p>
        </div>
      </header>

      <main className="usage-content as-content">
        <div className="as-toolbar">
          <label className="as-search">
            <SearchIcon />
            <input
              type="search"
              inputMode="search"
              enterKeyHint="search"
              aria-label={t("agentSessions.searchLabel")}
              placeholder={t("agentSessions.searchPlaceholder")}
              value={input}
              onChange={(e) => setInput(e.target.value)}
            />
            {input && (
              <button type="button" className="as-search-clear" aria-label={t("agentSessions.clearSearch")} onClick={() => { haptic(); setInput(""); }}>
                <svg viewBox="0 0 24 24" width="16" height="16" aria-hidden="true" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round"><path d="M6 6l12 12M18 6 6 18" /></svg>
              </button>
            )}
          </label>
          <div className="as-filters" role="radiogroup" aria-label={t("agentSessions.filterAria")}>
            {filters.map((f) => (
              <button
                key={f.id || "all"}
                type="button"
                role="radio"
                aria-checked={filter === f.id}
                className={`as-filter${filter === f.id ? " is-active" : ""}`}
                onClick={() => { haptic(); setFilter(f.id); setExpanded(""); }}
              >
                <span>{f.label}</span>
                {meta && <small>{f.n}</small>}
              </button>
            ))}
          </div>
        </div>

        {state === "ready" && agents === null && (
          <div className="as-agent-status" role={agentsError ? "alert" : "status"}>
            <p>{agentsError
              ? t("agentSessions.agentsLoadFailed", { error: agentsError })
              : t("agentSessions.agentsLoading")}</p>
            {agentsError && <button type="button" className="btn btn-secondary" onClick={() => { haptic(); void loadAgents(); }}>
              {t("agentSessions.retry")}
            </button>}
          </div>
        )}

        {state === "loading" && (
          <div className="as-skeleton" role="status" aria-label={t("agentSessions.loading")}>
            <p>{t("agentSessions.loading")}</p>
            {[0, 1, 2, 3, 4].map((i) => <div key={i} className="as-skeleton-row"><i /><i /></div>)}
          </div>
        )}

        {state === "error" && (
          <div className="usage-empty as-state" role="alert">
            <h2>{t(oldAgent ? "agentSessions.oldAgentTitle" : "agentSessions.errorTitle")}</h2>
            <p>{error}</p>
            <button type="button" className="btn btn-primary" onClick={() => { haptic(); void load("reset"); }}>{t("agentSessions.retry")}</button>
          </div>
        )}

        {state === "ready" && items.length === 0 && !meta?.partial && (
          <div className="usage-empty as-state">
            <h2>{t(searching ? "agentSessions.nothingFound" : "agentSessions.emptyTitle")}</h2>
            <p>{t(searching ? "agentSessions.nothingFoundText" : "agentSessions.emptyText")}</p>
          </div>
        )}

        {state === "ready" && meta?.partial && (
          <div className="as-partial" role="status">
            <p className="as-note">{t(retriesRef.current >= PARTIAL_RETRIES
              ? "agentSessions.partialStopped" : "agentSessions.partial")}</p>
            {retriesRef.current >= PARTIAL_RETRIES && (
              <button type="button" className="btn btn-secondary" onClick={() => { haptic(); void load("reset"); }}>
                {t("agentSessions.retry")}
              </button>
            )}
          </div>
        )}

        {state === "ready" && groups.map((g) => (
          <section key={g.key} className="as-day" aria-label={g.label}>
            <h2 className="as-day-title">{g.label}</h2>
            <ul className="as-list">{g.items.map(renderRow)}</ul>
          </section>
        ))}

        {state === "ready" && listError && <p className="as-error as-list-error" role="alert">{listError}</p>}

        {state === "ready" && items.length > 0 && (
          <div className="as-more">
            <small>{t("agentSessions.shown", { shown: items.length, total: meta?.total ?? items.length })}</small>
            {meta?.next && (
              <button type="button" className="btn btn-secondary" disabled={loadingMore} onClick={() => void loadMore()}>
                {loadingMore ? t("agentSessions.loading") : t("agentSessions.more")}
              </button>
            )}
          </div>
        )}
      </main>
      <BottomNav active="usage" />
    </div>
  );
}

export default AgentSessionsView;
