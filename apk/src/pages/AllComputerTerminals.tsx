import { useCallback, useEffect, useRef, useState } from "react";
import type { ReactNode } from "react";
import { useNavigate } from "react-router-dom";
import { ProcessBadge, isAgentKind, mapApiError, ptyDisplayTitle, t } from "@tgcontrol/shared";
import type { AgentKind, PtySessionInfo } from "@tgcontrol/shared";
import { listDevices, requestDevice } from "../cloud/api";
import type { CloudDevice } from "../cloud/api";
import { selectDevice } from "../devices";
import { onWSEvent } from "../api";
import { BottomNav } from "../components/BottomNav";
import { IconChevron, IconFolder, IconRefresh, IconSearch } from "../components/icons";
import { ptyStatusIcon } from "../components/PtyStatusIcon";
import { ptyListStatus } from "../ptyTerm/listStatus";
import { computerName, computerTerminalKey, loadComputerTerminals, matchingComputerTerminals,
  orderComputerTerminals, readComputerFolders, saveComputerFolders } from "../ptyTerm/computerList";
import { usePolling } from "../hooks/usePolling";

interface ComputerResult { sessions: PtySessionInfo[]; fetchedAt: number; error?: string }

export function AllComputerTerminals({ scopeControl }: { scopeControl: ReactNode }) {
  const navigate = useNavigate();
  const [devices, setDevices] = useState<CloudDevice[]>([]);
  const [results, setResults] = useState<Record<string, ComputerResult>>({});
  const [collapsed, setCollapsed] = useState(readComputerFolders);
  const [query, setQuery] = useState("");
  const [loading, setLoading] = useState(true);
  const [refreshing, setRefreshing] = useState(false);
  const [error, setError] = useState("");
  const [nowMs, setNowMs] = useState(Date.now());
  const request = useRef<AbortController | null>(null);
  const eventTimer = useRef<ReturnType<typeof setTimeout> | null>(null);

  const refresh = useCallback(async () => {
    if (request.current) return;
    const controller = new AbortController();
    request.current = controller;
    setRefreshing(true);
    try {
      const list = await listDevices();
      if (controller.signal.aborted) return;
      const current = list.devices || [];
      // Keep positions stable when status changes; search never changes their order.
      current.sort((a, b) => computerName(a).localeCompare(computerName(b)) || a.id.localeCompare(b.id));
      setDevices(current);
      setResults(previous => Object.fromEntries(current.filter(d => previous[d.id]).map(d => [d.id, previous[d.id]])));
      setError("");
      setLoading(false);
      await loadComputerTerminals(current,
        device => requestDevice<{ sessions?: PtySessionInfo[] }>(device.id, "/api/pty", { signal: controller.signal }),
        (device, result) => setResults(previous => ({ ...previous, [device.id]: result.status === "fulfilled"
          ? { sessions: orderComputerTerminals(result.value.sessions || []), fetchedAt: Date.now() }
          : { sessions: previous[device.id]?.sessions || [], fetchedAt: previous[device.id]?.fetchedAt || 0, error: mapApiError(result.reason) } })),
        controller.signal);
    } catch (failure) {
      if (!controller.signal.aborted) setError(mapApiError(failure));
    } finally {
      if (request.current === controller) request.current = null;
      if (!controller.signal.aborted) { setLoading(false); setRefreshing(false); setNowMs(Date.now()); }
    }
  }, []);

  usePolling(refresh, 20_000);
  useEffect(() => {
    const clock = window.setInterval(() => setNowMs(Date.now()), 30_000);
    const off = onWSEvent(ev => {
      if (!["pty_event", "pty_list_changed", "agent_status"].includes(ev.type) || document.hidden || eventTimer.current) return;
      eventTimer.current = setTimeout(() => { eventTimer.current = null; void refresh(); }, 400);
    });
    return () => {
      off(); window.clearInterval(clock); request.current?.abort(); request.current = null;
      if (eventTimer.current) clearTimeout(eventTimer.current);
    };
  }, [refresh]);

  const toggle = (id: string) => setCollapsed(previous => {
    const next = { ...previous, [id]: !previous[id] }; saveComputerFolders(next); return next;
  });
  const open = (device: CloudDevice, id?: string) => {
    selectDevice(device.id, device);
    navigate(id ? `/pty/${encodeURIComponent(id)}?from=${encodeURIComponent("/pty")}` : "/");
  };
  const visible = devices.map(device => ({ device, sessions: matchingComputerTerminals(device, results[device.id]?.sessions || [], query) }))
    .filter(({ device, sessions }) => !query.trim() || sessions.length ||
      [device.name, device.hostname, device.workspace_name].some(v => v?.toLocaleLowerCase().includes(query.trim().toLocaleLowerCase())));

  return <div className="page pty-computers-page">
    <div className="page-header"><div className="page-header-context"><h1>{t("pty.title")}</h1></div>
      <button type="button" className="header-action" disabled={refreshing} onClick={() => void refresh()} aria-label={t("pty.computers.retry")}><IconRefresh size={20} /></button>
    </div>
    <div className="page-content">
      {scopeControl}
      <p className="pty-computers-hint">{t("pty.computers.hint")}</p>
      <label className="pty-computers-search"><IconSearch size={18} /><input className="pty-search-input" type="search" value={query} onChange={e => setQuery(e.target.value)} placeholder={t("pty.computers.search")} aria-label={t("pty.computers.search")} /></label>
      {error && <div className="pty-computers-message" role="status">{error} {devices.length > 0 && t("pty.computers.stale")} <button className="btn btn-secondary" disabled={refreshing} onClick={() => void refresh()}>{t("pty.computers.retry")}</button></div>}
      {loading && <p role="status">{t("infra.loading")}</p>}
      {!loading && !error && !devices.length && <div className="pty-computers-message"><p>{t("pty.computers.noDevices")}</p><button className="btn btn-secondary" onClick={() => navigate("/infrastructure")}>{t("pty.computers.add")}</button></div>}
      {!loading && devices.length > 0 && !visible.length && <p role="status">{t("pty.computers.none")}</p>}
      {visible.map(({ device, sessions }) => {
        const result = results[device.id], unavailable = !device.online || !!result?.error || !!error;
        const isCollapsed = !query.trim() && !!collapsed[device.id];
        const panelId = `pty-computer-${encodeURIComponent(device.id)}`;
        return <section key={device.id} className="pty-computer-folder" data-device-id={device.id}>
          <button type="button" className="pty-computer-heading" aria-expanded={!isCollapsed} aria-controls={panelId} onClick={() => toggle(device.id)}>
            <IconChevron size={14} dir={isCollapsed ? "right" : "down"} /><IconFolder size={20} />
            <span className="pty-computer-name">{computerName(device)}<span className="pty-computer-workspace">{device.workspace_name}</span></span>
            <span className="pty-computer-summary"><span className={device.online && !result?.error ? "online" : "offline"}>{t(!device.online ? "pty.computers.offline" : result?.error ? "pty.computers.unreachable" : "pty.computers.online")}</span>
              {!!result?.fetchedAt && <span>{t("pty.computers.count", { n: result.sessions.length })}</span>}</span>
          </button>
          <div id={panelId} hidden={isCollapsed}>
            {!device.online && <p className="pty-computers-message">{t("pty.computers.offline")}{result?.sessions.length ? ` · ${t("pty.computers.stale")}` : ""}</p>}
            {device.online && result?.error && <div className="pty-computers-message" role="status">{t("pty.computers.loadError")} {result.error}<button className="btn btn-secondary" disabled={refreshing} onClick={() => void refresh()}>{t("pty.computers.retry")}</button></div>}
            {device.online && !result && <p className="pty-computers-message" role="status">{t("pty.computers.loading")}</p>}
            {device.online && result && !result.error && !result.sessions.length && <p className="pty-computers-message">{t("pty.computers.empty")}</p>}
            <div className="cards-grid">
              {sessions.map(session => {
                const status = ptyListStatus(session, nowMs), title = ptyDisplayTitle(session, { preferAgent: true });
                return <button key={computerTerminalKey(device.id, session.id)} type="button" data-pty-id={session.id} className={`card pty-card pty-computer-card status-${status.cls}`}
                  disabled={unavailable} aria-label={t("pty.openCard", { name: title, state: status.text })} onClick={() => open(device, session.id)}>
                  <div className="pty-card-top"><div className="pty-card-identity"><span className={`dash-dot ${session.alive ? "alive" : "dead"}`} /><span className="pty-card-shell">{title}</span></div>
                    <div className="pty-card-meta">{isAgentKind(session.agent_kind) && <ProcessBadge kind={session.agent_kind as AgentKind} name={session.fg_process} account={session.account_label} />}
                      <span className={`pty-status ${status.cls}`} title={status.title}><span className="pty-status-icon">{ptyStatusIcon(status.icon)}</span><span className="pty-status-text">{status.text}</span></span>
                    </div></div>
                  <div className="pty-card-path">{session.cwd}</div>
                  {session.group && <div className="pty-computer-group">{t("pty.computers.group", { name: session.group })}</div>}
                  {session.kind === "ssh" && <span className="pty-ssh-badge">SSH{session.ssh_host ? ` · ${session.ssh_host}` : ""}</span>}
                  {session.hint && (session.status === "waiting" || session.status === "error" || !session.alive) && <div className="pty-card-hint">{session.hint}</div>}
                </button>;
              })}
            </div>
            {device.online && <button type="button" className="btn btn-secondary pty-computer-open" onClick={() => open(device)}>{t("pty.computers.manage")}</button>}
          </div>
        </section>;
      })}
    </div><BottomNav active="terminal" />
  </div>;
}
