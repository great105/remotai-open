import { useCallback, useEffect, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import { ProcessBadge, SheetShell, isAgentKind, mapApiError, ptyDisplayTitle, t, useEscape } from "@tgcontrol/shared";
import type { AgentKind, PtySessionInfo } from "@tgcontrol/shared";
import { listDevices, requestDevice } from "../cloud/api";
import type { CloudDevice } from "../cloud/api";
import { selectDevice } from "../devices";
import { getSelectedDeviceId } from "../config";
import { onWSEvent } from "../api";
import { IconChevron, IconClose, IconComputer, IconRobot, IconSearch } from "../components/icons";
import { ptyStatusIcon } from "../components/PtyStatusIcon";
import { ptyListStatus } from "../ptyTerm/listStatus";
import { computerListHome, computerName, computerTerminalKey, filterComputerTerminals, loadComputerTerminals,
  matchingComputerTerminals, orderComputerTerminals, readComputerFolders, saveComputerFolders } from "../ptyTerm/computerList";
import type { ComputerTerminalFilter } from "../ptyTerm/computerList";
import { usePolling } from "../hooks/usePolling";

interface ComputerResult { sessions: PtySessionInfo[]; fetchedAt: number; error?: string }
interface Props {
  pinned: string[];
  onPinnedChange: (ids: string[]) => void;
  pickerOpen: boolean;
  onPickerClose: () => void;
  query: string;
  statusFilter: ComputerTerminalFilter;
}

/** Added computers share the terminal list; reading them never changes its PC. */
export function ComputerTerminalFolders(props: Props) {
  const navigate = useNavigate();
  const [devices, setDevices] = useState<CloudDevice[]>([]);
  const [results, setResults] = useState<Record<string, ComputerResult>>({});
  const [collapsed, setCollapsed] = useState(readComputerFolders);
  const [pickerQuery, setPickerQuery] = useState("");
  const [loading, setLoading] = useState(true);
  const [refreshing, setRefreshing] = useState(false);
  const [error, setError] = useState("");
  const [nowMs, setNowMs] = useState(Date.now());
  const request = useRef<AbortController | null>(null);
  const queued = useRef(false);
  const mounted = useRef(true);
  const eventTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const searching = !!props.query.trim();
  const options = useRef({ pinned: props.pinned, collapsed, pickerOpen: props.pickerOpen, searching });
  options.current = { pinned: props.pinned, collapsed, pickerOpen: props.pickerOpen, searching };

  const refresh = useCallback(async () => {
    if (!mounted.current || (!options.current.pinned.length && !options.current.pickerOpen)) return;
    if (request.current) { queued.current = true; return; }
    const controller = new AbortController();
    request.current = controller;
    setRefreshing(true);
    try {
      const list = await listDevices();
      if (controller.signal.aborted) return;
      const current = (list.devices || []).slice().sort((a, b) => computerName(a).localeCompare(computerName(b)) || a.id.localeCompare(b.id));
      setDevices(current);
      setError("");
      setLoading(false);
      const open = current.filter(device => device.id !== getSelectedDeviceId()
        && options.current.pinned.includes(device.id) && (options.current.searching || options.current.collapsed[device.id] === false));
      await loadComputerTerminals(open,
        device => requestDevice<{ sessions?: PtySessionInfo[] }>(device.id, "/api/pty", { signal: controller.signal }),
        (device, result) => {
          if (!options.current.pinned.includes(device.id)) return;
          setResults(previous => ({ ...previous, [device.id]: result.status === "fulfilled"
            ? { sessions: orderComputerTerminals(result.value.sessions || []), fetchedAt: Date.now() }
            : { sessions: previous[device.id]?.sessions || [], fetchedAt: previous[device.id]?.fetchedAt || 0, error: mapApiError(result.reason) } }));
        }, controller.signal);
    } catch (failure) {
      if (!controller.signal.aborted) setError(mapApiError(failure));
    } finally {
      if (request.current === controller) request.current = null;
      if (!controller.signal.aborted && mounted.current) { setLoading(false); setRefreshing(false); setNowMs(Date.now()); }
      if (queued.current && mounted.current) { queued.current = false; void refresh(); }
    }
  }, []);

  usePolling(refresh, 20_000, { enabled: props.pinned.length > 0 || props.pickerOpen, immediate: false });
  useEffect(() => { void refresh(); }, [refresh, props.pinned, collapsed, props.pickerOpen, searching]);
  useEffect(() => { if (props.pickerOpen) setPickerQuery(""); }, [props.pickerOpen]);
  useEscape(props.pickerOpen, props.onPickerClose);
  useEffect(() => {
    mounted.current = true;
    const clock = window.setInterval(() => setNowMs(Date.now()), 30_000);
    const off = onWSEvent(ev => {
      if (!["pty_event", "pty_list_changed", "agent_status"].includes(ev.type) || document.hidden || eventTimer.current) return;
      eventTimer.current = setTimeout(() => { eventTimer.current = null; void refresh(); }, 400);
    });
    return () => {
      mounted.current = false; queued.current = false;
      off(); window.clearInterval(clock); request.current?.abort(); request.current = null;
      if (eventTimer.current) clearTimeout(eventTimer.current);
    };
  }, [refresh]);

  const changeCollapsed = (id: string, value: boolean) => setCollapsed(previous => {
    const next = { ...previous, [id]: value }; saveComputerFolders(next); return next;
  });
  const add = (device: CloudDevice) => {
    props.onPinnedChange([...props.pinned, device.id]);
    changeCollapsed(device.id, false);
    props.onPickerClose();
  };
  const remove = (id: string) => {
    props.onPinnedChange(props.pinned.filter(value => value !== id));
    setResults(previous => Object.fromEntries(Object.entries(previous).filter(([key]) => key !== id)));
  };
  const open = (device: CloudDevice, sessionId?: string, agent = false) => {
    const home = computerListHome(getSelectedDeviceId());
    const origin = devices.find(item => item.id === getSelectedDeviceId());
    // The browser Back entry and the terminal's explicit return agree on its PC.
    navigate(home, { replace: true, state: { terminalComputer: origin } });
    selectDevice(device.id, device);
    const params = new URLSearchParams({ from: home });
    if (!sessionId) { params.set("new", "1"); if (agent) params.set("agent", "1"); }
    navigate(sessionId ? `/pty/${encodeURIComponent(sessionId)}?${params}` : `/pty?${params}`);
  };
  const pickerMatches = devices.filter(device => device.id !== getSelectedDeviceId() && !props.pinned.includes(device.id)
    && [device.name, device.hostname, device.workspace_name].some(value => value?.toLocaleLowerCase().includes(pickerQuery.trim().toLocaleLowerCase())));

  return <>
    <div className="pty-computer-folders">
      {error && props.pinned.length > 0 && <div className="pty-computers-message" role="status">{error} {devices.length > 0 && t("pty.computers.stale")} <button className="btn btn-secondary" disabled={refreshing} onClick={() => void refresh()}>{t("pty.computers.retry")}</button></div>}
      {props.pinned.filter(id => id !== getSelectedDeviceId()).map(id => {
        const device = devices.find(item => item.id === id), result = results[id];
        if (!device) return <div key={id} className="pty-computer-missing" role="status">
          <span>{t(loading ? "infra.loading" : "pty.computers.missing")}</span>
          {!loading && <button className="btn btn-secondary" onClick={() => remove(id)}>{t("pty.computers.remove")}</button>}
        </div>;
        const sessions = filterComputerTerminals(matchingComputerTerminals(device, result?.sessions || [], props.query), props.statusFilter);
        const query = props.query.trim();
        if (query && (result || !device.online) && !sessions.length && ![device.name, device.hostname, device.workspace_name].some(value => value?.toLocaleLowerCase().includes(query.toLocaleLowerCase()))) return null;
        const isCollapsed = !query && collapsed[id] !== false, unavailable = !device.online || !!result?.error || !!error;
        const panelId = `pty-computer-${encodeURIComponent(id)}`;
        return <section key={id} className="pty-computer-folder" data-device-id={id}>
          <div className="pty-computer-head-row">
            <button type="button" className="pty-computer-heading" aria-expanded={!isCollapsed} aria-controls={panelId} onClick={() => changeCollapsed(id, !isCollapsed)}>
              <IconChevron size={14} dir={isCollapsed ? "right" : "down"} /><IconComputer size={20} />
              <span className="pty-computer-name">{computerName(device)}<span className="pty-computer-workspace">{device.workspace_name}</span></span>
              <span className="pty-computer-summary"><span className={device.online && !result?.error ? "online" : "offline"}>{t(!device.online ? "pty.computers.offline" : result?.error ? "pty.computers.unreachable" : "pty.computers.online")}</span>
                {!!result?.fetchedAt && <span>{t("pty.computers.count", { n: result.sessions.length })}</span>}</span>
            </button>
            <button type="button" className="pty-computer-remove" aria-label={t("pty.computers.removeNamed", { name: computerName(device) })} title={t("pty.computers.remove")} onClick={() => remove(id)}><IconClose size={16} /></button>
          </div>
          <div id={panelId} hidden={isCollapsed}>
            {!device.online && <p className="pty-computers-message">{t("pty.computers.offline")}{result?.sessions.length ? ` · ${t("pty.computers.stale")}` : ""}</p>}
            {device.online && result?.error && <div className="pty-computers-message" role="status">{t("pty.computers.loadError")} {result.error}<button className="btn btn-secondary" disabled={refreshing} onClick={() => void refresh()}>{t("pty.computers.retry")}</button></div>}
            {device.online && !result && <p className="pty-computers-message" role="status">{t("pty.computers.loading")}</p>}
            {device.online && result && !result.error && !sessions.length && <p className="pty-computers-message">{t(result.sessions.length ? "pty.computers.none" : "pty.computers.empty")}</p>}
            <div className="cards-grid">
              {sessions.map(session => {
                const status = ptyListStatus(session, nowMs), title = ptyDisplayTitle(session, { preferAgent: true });
                return <button key={computerTerminalKey(id, session.id)} type="button" data-pty-id={session.id} className={`card pty-card pty-computer-card status-${status.cls}`}
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
            {device.online && device.workspace_role !== "viewer" && <div className="pty-computer-actions">
              <button type="button" className="btn btn-secondary" disabled={unavailable} onClick={() => open(device, undefined, true)}><IconRobot size={16} />{t("pty.launchAgent")}</button>
              <button type="button" className="btn btn-secondary" disabled={unavailable} onClick={() => open(device)}>{t("pty.newTerminal")}</button>
            </div>}
          </div>
        </section>;
      })}
    </div>
    <SheetShell open={props.pickerOpen} onClose={props.onPickerClose} overlayClassName="folder-sheet-overlay" className="folder-sheet pty-computer-picker" labelledBy="pty-computer-picker-title">
      <div className="folder-sheet-header"><div className="folder-sheet-title" id="pty-computer-picker-title">{t("pty.computers.choose")}</div><button className="folder-sheet-close" aria-label={t("modal.close")} onClick={props.onPickerClose}><IconClose size={20} /></button></div>
      <div className="folder-sheet-search"><span className="folder-sheet-search-icon"><IconSearch size={18} /></span><input type="search" value={pickerQuery} onChange={e => setPickerQuery(e.target.value)} placeholder={t("pty.computers.searchComputer")} aria-label={t("pty.computers.searchComputer")} /></div>
      <div className="folder-sheet-body">
        {loading && <p className="pty-computers-message" role="status">{t("infra.loading")}</p>}
        {error && <div className="pty-computers-message" role="status">{error}<button className="btn btn-secondary" disabled={refreshing} onClick={() => void refresh()}>{t("pty.computers.retry")}</button></div>}
        {pickerMatches.map(device => <button key={device.id} type="button" className="pty-computer-choice" disabled={!!error} onClick={() => add(device)}>
          <IconComputer size={22} /><span>{computerName(device)}<small>{device.workspace_name}</small></span><small>{t(device.online ? "pty.computers.online" : "pty.computers.offline")}</small>
        </button>)}
        {!loading && !error && !pickerMatches.length && <p className="pty-computers-message">{t(pickerQuery.trim() ? "pty.computers.none" : "pty.computers.allAdded")}</p>}
      </div>
      <div className="folder-browse-actions"><button type="button" className="btn btn-secondary" onClick={() => { props.onPickerClose(); navigate("/infrastructure?back=%2Fpty"); }}>{t("pty.computers.add")}</button></div>
    </SheetShell>
  </>;
}
