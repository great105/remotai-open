import type { PtySessionInfo } from "@tgcontrol/shared";
import type { CloudDevice } from "../cloud/api";

const PINNED_KEY = "remotai.pty.computers.pinned";
const FOLDERS_KEY = "remotai.pty.computers.collapsed";

export function computerIds(value: unknown): string[] {
  return Array.isArray(value) ? [...new Set(value.filter((id): id is string => typeof id === "string" && id.trim() !== ""))] : [];
}
export function readPinnedComputers(): string[] {
  try { return computerIds(JSON.parse(localStorage.getItem(PINNED_KEY) || "[]")); } catch { return []; }
}
export function savePinnedComputers(ids: string[]): void {
  try { localStorage.setItem(PINNED_KEY, JSON.stringify(computerIds(ids))); } catch { /* private storage */ }
}
export function readComputerFolders(): Record<string, boolean> {
  try {
    const value = JSON.parse(localStorage.getItem(FOLDERS_KEY) || "{}");
    return value && typeof value === "object" && !Array.isArray(value)
      ? Object.fromEntries(Object.entries(value).filter((entry): entry is [string, boolean] => typeof entry[1] === "boolean")) : {};
  } catch { return {}; }
}
export function saveComputerFolders(value: Record<string, boolean>): void {
  try { localStorage.setItem(FOLDERS_KEY, JSON.stringify(value)); } catch { /* private storage */ }
}

/** PTY ids are local to a computer and may collide across devices. */
export function computerTerminalKey(deviceId: string, terminalId: string): string {
  return JSON.stringify([deviceId, terminalId]);
}
export function computerName(device: Pick<CloudDevice, "name" | "hostname" | "id">): string {
  return device.name || device.hostname || device.id;
}
export function orderComputerTerminals(sessions: PtySessionInfo[]): PtySessionInfo[] {
  const sort = (s: PtySessionInfo) => s.sort || -s.created;
  return sessions.slice().sort((a, b) => (a.group || "").localeCompare(b.group || "")
    || sort(a) - sort(b) || a.id.localeCompare(b.id));
}
export function matchingComputerTerminals(device: CloudDevice, sessions: PtySessionInfo[], query: string): PtySessionInfo[] {
  const q = query.trim().toLocaleLowerCase();
  if (!q || [device.name, device.hostname, device.workspace_name].some(s => s?.toLocaleLowerCase().includes(q))) return sessions;
  return sessions.filter(s => [s.name, s.cwd, s.shell, s.agent_kind, s.group, s.hint]
    .some(value => value?.toLocaleLowerCase().includes(q)));
}

export type ComputerTerminalFilter = "all" | "waiting" | "working" | "error" | "dead";
export function filterComputerTerminals(sessions: PtySessionInfo[], filter: ComputerTerminalFilter): PtySessionInfo[] {
  return filter === "all" ? sessions : sessions.filter(session => {
    const status = session.status || (session.alive ? "working" : "dead");
    return (status === "stalled" ? "working" : status) === filter;
  });
}

/** Keep the home computer in browser history when opening a remote terminal. */
export function computerListHome(deviceId: string): string {
  return `/pty?${new URLSearchParams({ computer: deviceId })}`;
}

/** Bound fan-out and deliver each PC immediately; one timeout never holds the others. */
export async function loadComputerTerminals<T>(
  devices: CloudDevice[], load: (device: CloudDevice) => Promise<T>,
  onResult: (device: CloudDevice, result: PromiseSettledResult<T>) => void,
  signal: AbortSignal,
): Promise<void> {
  let cursor = 0;
  const online = devices.filter(d => d.online);
  const worker = async () => {
    while (!signal.aborted && cursor < online.length) {
      const device = online[cursor++];
      let result: PromiseSettledResult<T>;
      try { result = { status: "fulfilled", value: await load(device) }; }
      catch (reason) { result = { status: "rejected", reason }; }
      if (!signal.aborted) onResult(device, result);
    }
  };
  await Promise.all(Array.from({ length: Math.min(4, online.length) }, worker));
}
