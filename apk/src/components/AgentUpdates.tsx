/**
 * Обновление CLI-агента из строки «Что установлено» на экране «Агенты».
 *
 * Что видит человек: под именем агента — «Есть обновление 0.37.2 → 2.1.1 · npm»
 * и кнопка «Обновить». Кнопка НЕ обновляет молча: она открывает новый терминал
 * и печатает туда команду того же менеджера, которым агент поставлен (её
 * собирает компьютер, internal/agentupdate). Так же устроена установка —
 * процесс видно целиком, и видно, чем он кончился. Когда команда закончилась,
 * список агентов перепроверяется сам, а строка говорит, какая версия стала.
 *
 * Данные одни на все строки: модульное хранилище ниже, один запрос на экран.
 * Правила текста — в @tgcontrol/shared/agentUpdates (проверяются тестом).
 */
import { useEffect, useState, useSyncExternalStore } from "react";
import { useNavigate } from "react-router-dom";
import {
  getCliAgentUpdates, getCliAgentVersions, updateLine, runningWarning, versionEventText,
  updateFinished, compareVersions, mapApiError, t,
  type CliAgentUpdate, type CliAgentVersionEvent,
} from "@tgcontrol/shared";
import { createPtySession, getQuickPaths, listPtySessions, ptyInput, rescanAgents } from "../api";
import { haptic, tgConfirm } from "../telegram";
import "./AgentUpdates.css";

// ── Хранилище ────────────────────────────────────────────────────────────

interface Pending {
  agentId: string;
  ptyId: string;
  from: string;
  startedAt: number;
}

interface State {
  items: Record<string, CliAgentUpdate> | null;
  loading: boolean;
  at: number;
  pending: Pending | null;
  /** Итог последнего обновления — строкой под агентом. */
  done: { agentId: string; text: string } | null;
}

const PENDING_KEY = "agentUpdate.pending";
/** Сколько верить списку без перепроверки: реестр npm на сервере и так в кэше. */
const FRESH_MS = 5 * 60_000;
const WATCH_EVERY_MS = 4000;
/** Дольше обновление не идёт; дальше перестаём следить, чтобы не опрашивать вечно. */
const WATCH_LIMIT_MS = 20 * 60_000;

function readPending(): Pending | null {
  try {
    const raw = sessionStorage.getItem(PENDING_KEY);
    return raw ? (JSON.parse(raw) as Pending) : null;
  } catch { return null; }
}

function writePending(p: Pending | null) {
  try {
    if (p) sessionStorage.setItem(PENDING_KEY, JSON.stringify(p));
    else sessionStorage.removeItem(PENDING_KEY);
  } catch { /* приватный режим: следим только в памяти */ }
}

let state: State = { items: null, loading: false, at: 0, pending: readPending(), done: null };
const listeners = new Set<() => void>();
let inflight: Promise<void> | null = null;
let watchTimer: number | null = null;

function setState(patch: Partial<State>) {
  state = { ...state, ...patch };
  listeners.forEach((l) => l());
}

function subscribe(l: () => void) {
  listeners.add(l);
  return () => { listeners.delete(l); };
}

function load(refresh: boolean): Promise<void> {
  if (inflight && !refresh) return inflight;
  setState({ loading: true });
  const p = getCliAgentUpdates(refresh)
    .then((r) => {
      const items: Record<string, CliAgentUpdate> = {};
      for (const a of r.agents || []) items[a.id] = a;
      setState({ items, at: Date.now() });
    })
    // Старый агент без маршрута (404) или сеть: строк обновления просто нет —
    // экран «Агенты» от этого не должен ломаться.
    .catch(() => { setState({ at: Date.now() }); })
    .finally(() => {
      if (inflight === p) inflight = null;
      setState({ loading: false });
    });
  inflight = p;
  return p;
}

function ensureFresh() {
  if (!state.loading && Date.now() - state.at > FRESH_MS) void load(false);
}

async function finishWatch(p: Pending) {
  stopWatch();
  writePending(null);
  // Агент мог «пропасть» на секунды обновления (npm переименовывает обёртки) —
  // перепроверяем список, как кнопка «Проверить снова».
  await rescanAgents().catch(() => undefined);
  await load(true);
  const now = state.items?.[p.agentId];
  const name = now?.name || p.agentId;
  const text = now?.version && compareVersions(now.version, p.from) !== 0
    ? t("agentUpdate.done", { name, version: now.version })
    : t("agentUpdate.doneSame", { name });
  setState({ pending: null, done: { agentId: p.agentId, text } });
}

function stopWatch() {
  if (watchTimer !== null) window.clearInterval(watchTimer);
  watchTimer = null;
}

function startWatch() {
  if (watchTimer !== null || !state.pending) return;
  watchTimer = window.setInterval(() => {
    const p = state.pending;
    if (!p) { stopWatch(); return; }
    const elapsed = Date.now() - p.startedAt;
    if (elapsed > WATCH_LIMIT_MS) {
      stopWatch();
      writePending(null);
      setState({ pending: null });
      return;
    }
    listPtySessions()
      .then(({ sessions }) => {
        const s = sessions.find((x) => x.id === p.ptyId);
        if (!s || updateFinished(s.status, s.alive, elapsed)) void finishWatch(p);
      })
      .catch(() => { /* нет связи — спросим на следующем круге */ });
  }, WATCH_EVERY_MS);
}

/** Команда в новый терминал: создать, напечатать, открыть. */
async function runUpdate(u: CliAgentUpdate): Promise<string> {
  // Домашняя папка, как у «Установить» (PtyListView, P1-29): глобальной
  // установке папка не важна, а спрашивать её — лишний шаг.
  const home = await getQuickPaths()
    .then((d) => (d.paths || []).find((p) => p.name === "Home")?.path || "")
    .catch(() => "");
  const res = await createPtySession(home || ".", "", 80, 24);
  // Шелл поднимается не мгновенно; ввод в PTY он всё равно прочтёт, но с паузой
  // команда не смешивается с его приветствием.
  await new Promise((r) => window.setTimeout(r, 600));
  await ptyInput(res.id, { data: u.update_command, enter: true });
  const pending: Pending = { agentId: u.id, ptyId: res.id, from: u.version, startedAt: Date.now() };
  writePending(pending);
  setState({ pending, done: null });
  startWatch();
  return res.id;
}

export function useAgentUpdates(): State {
  const snap = useSyncExternalStore(subscribe, () => state, () => state);
  useEffect(() => {
    ensureFresh();
    startWatch();
  }, []);
  return snap;
}

// ── Строка агента ────────────────────────────────────────────────────────

/** Вставляется в `.agent-row-info` обнаруженного агента. */
export function AgentUpdateLine({ agentId }: { agentId: string }) {
  const { items, pending, done } = useAgentUpdates();
  const navigate = useNavigate();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [historyOpen, setHistoryOpen] = useState(false);
  const [history, setHistory] = useState<CliAgentVersionEvent[] | null>(null);
  const [historyError, setHistoryError] = useState(false);

  const u = items?.[agentId];
  if (!u) return null;
  const line = updateLine(u);
  const mine = pending?.agentId === agentId ? pending : null;
  const otherBusy = !!pending && !mine;

  const onUpdate = async () => {
    haptic();
    setError("");
    const warn = runningWarning(u);
    if (warn) {
      const ok = await tgConfirm(warn, {
        confirmText: t("agentUpdate.runningConfirm"),
        cancelText: t("modal.cancel"),
      });
      if (!ok) return;
    }
    setBusy(true);
    try {
      const id = await runUpdate(u);
      navigate(`/pty/${encodeURIComponent(id)}`);
    } catch (e: any) {
      setError(`${t("agentUpdate.error")}: ${mapApiError(e)}`);
    } finally {
      setBusy(false);
    }
  };

  const toggleHistory = () => {
    haptic();
    const next = !historyOpen;
    setHistoryOpen(next);
    if (next) {
      setHistoryError(false);
      getCliAgentVersions(agentId)
        .then((r) => setHistory(r.events || []))
        .catch(() => setHistoryError(true));
    }
  };

  return (
    <div className="agent-update" data-kind={line.kind} data-agent-update={agentId}>
      <div className="agent-update-line">{line.text}</div>
      {line.hint && <div className="agent-update-hint">{line.hint}</div>}
      {line.canUpdate && u.running_sessions > 0 && !mine && (
        <div className="agent-update-warn">{t("agentUpdate.runningShort", { n: u.running_sessions })}</div>
      )}
      {mine && (
        <button
          type="button"
          className="agent-update-progress"
          onClick={() => navigate(`/pty/${encodeURIComponent(mine.ptyId)}`)}
        >
          <span className="agent-update-spinner" aria-hidden="true" />
          {t("agentUpdate.inProgress")}
        </button>
      )}
      {done?.agentId === agentId && !mine && <div className="agent-update-done" role="status">{done.text}</div>}
      {error && <div className="agent-update-error" role="alert">{error}</div>}
      <div className="agent-update-actions">
        {line.canUpdate && !mine && (
          <button
            type="button"
            className={`btn ${line.kind === "update" ? "btn-primary" : "btn-secondary"} agent-update-btn`}
            disabled={busy || otherBusy}
            onClick={() => void onUpdate()}
          >
            {busy ? t("agentUpdate.opening") : line.kind === "update" ? t("agentUpdate.update") : t("agentUpdate.checkUpdate")}
          </button>
        )}
        <button
          type="button"
          className="agent-update-history-toggle"
          aria-expanded={historyOpen}
          onClick={toggleHistory}
        >
          {t("agentUpdate.history")}
          <svg viewBox="0 0 16 16" width="14" height="14" aria-hidden="true">
            <path d="M4 6l4 4 4-4" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" />
          </svg>
        </button>
      </div>
      {historyOpen && (
        <div className="agent-update-history">
          {historyError ? (
            <div className="agent-update-hint">{t("agentUpdate.historyError")}</div>
          ) : history === null ? null : history.length === 0 ? (
            <div className="agent-update-hint">{t("agentUpdate.historyEmpty")}</div>
          ) : (
            <ol>
              {history.map((e) => <li key={`${e.at}-${e.to}`}>{versionEventText(e)}</li>)}
            </ol>
          )}
        </div>
      )}
    </div>
  );
}
