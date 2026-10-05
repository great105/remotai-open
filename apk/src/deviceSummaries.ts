import type { PtySessionInfo, SystemStats } from "@tgcontrol/shared";
import type { CloudDevice } from "./cloud/api";
import { listDevices, requestDevice } from "./cloud/api";

export interface DeviceRuntimeSummary {
  deviceId: string;
  fetchedAt: number;
  sessions: PtySessionInfo[];
  terminalCount: number;
  waitingCount: number;
  workingCount: number;
  errorCount: number;
  cpuPercent?: number;
  memoryPercent?: number;
  hasDisplay?: boolean;
  /**
   * false — сводка собрана облегчённым запросом (только `/api/pty`): полей
   * cpuPercent/memoryPercent/hasDisplay в ней НЕТ и не будет. Экран, которому
   * нужны метрики, такую запись из кэша не берёт (см. getDeviceSummary).
   */
  full: boolean;
}

const TTL_MS = 10_000;
// Список устройств главная перечитывает не чаще раза в минуту: он меняется
// только когда человек добавляет/удаляет компьютер, а раньше уходил каждые
// 6 секунд вместе со сводкой (на мобильной сети это заметный расход).
const DEVICES_TTL_MS = 60_000;
const cache = new Map<string, DeviceRuntimeSummary>();
// Ключ — `${deviceId}:full` / `${deviceId}:pty`: лёгкий запрос может дождаться
// уже летящего полного (терминалы есть в обоих), обратное — нет.
const inflight = new Map<string, Promise<DeviceRuntimeSummary>>();
let devicesCache: { at: number; devices: CloudDevice[] } | null = null;
let devicesInflight: Promise<CloudDevice[]> | null = null;

function summarize(
  deviceId: string,
  sessions: PtySessionInfo[],
  full: boolean,
  stats?: SystemStats,
  service?: { has_display?: boolean; remote_desktop_supported?: boolean },
): DeviceRuntimeSummary {
  const alive = sessions.filter((session) => session.alive);
  return {
    deviceId,
    fetchedAt: Date.now(),
    sessions,
    terminalCount: alive.length,
    waitingCount: alive.filter((session) => session.status === "waiting" && !!session.hint).length,
    workingCount: alive.filter((session) => session.status === "working" || session.status === "stalled").length,
    errorCount: alive.filter((session) => session.status === "error").length,
    cpuPercent: stats?.cpu?.percent,
    memoryPercent: stats?.memory?.percent,
    hasDisplay: service
      ? service.has_display !== false && service.remote_desktop_supported !== false
      : undefined,
    full,
  };
}

/** Один запрос сводки: полный (терминалы + метрики + возможности) или лёгкий. */
function fetchSummary(deviceId: string, full: boolean): Promise<DeviceRuntimeSummary> {
  const key = `${deviceId}:${full ? "full" : "pty"}`;
  const pending = inflight.get(key) || (full ? undefined : inflight.get(`${deviceId}:full`));
  if (pending) return pending;
  const request = (async () => {
    if (!full) {
      const pty = await requestDevice<{ sessions?: PtySessionInfo[] }>(deviceId, "/api/pty");
      const value = summarize(deviceId, pty.sessions || [], false);
      cache.set(deviceId, value);
      return value;
    }
    const [pty, stats, service] = await Promise.allSettled([
      requestDevice<{ sessions?: PtySessionInfo[] }>(deviceId, "/api/pty"),
      requestDevice<SystemStats>(deviceId, "/api/system/stats"),
      requestDevice<{ has_display?: boolean; remote_desktop_supported?: boolean }>(
        deviceId,
        "/api/system/service",
      ),
    ]);
    // Без списка терминалов сводка не должна притворяться нулевой: это
    // основной сигнал online/offline и «ждёт ответа».
    if (pty.status === "rejected") throw pty.reason;
    const value = summarize(
      deviceId,
      pty.value.sessions || [],
      true,
      stats.status === "fulfilled" ? stats.value : undefined,
      service.status === "fulfilled" ? service.value : undefined,
    );
    cache.set(deviceId, value);
    return value;
  })().finally(() => inflight.delete(key));
  inflight.set(key, request);
  return request;
}

/**
 * Полная сводка одного компьютера (терминалы + CPU/RAM + наличие экрана) с
 * 10-секундным общим кэшем — для «Устройств», где эти цифры показаны.
 */
export function getDeviceSummary(
  device: CloudDevice,
  force = false,
): Promise<DeviceRuntimeSummary> {
  const cached = cache.get(device.id);
  // Лёгкая запись (full === false) полный запрос не заменяет: в ней нет метрик.
  if (!force && cached && cached.full && Date.now() - cached.fetchedAt < TTL_MS) {
    return Promise.resolve(cached);
  }
  return fetchSummary(device.id, true);
}

/**
 * Облегчённая сводка: только `GET /api/pty`.
 *
 * Главной нужен один факт — что на чужом ПК ждёт ответа или упало, — а полный
 * набор стоил трёх запросов на КАЖДУЮ машину, включая `/api/system/stats` с
 * блокирующим замером CPU (~500 мс работы агента) и `/api/system/service`,
 * который главная не читает вовсе.
 */
export function getDevicePtySummary(
  device: CloudDevice,
  force = false,
): Promise<DeviceRuntimeSummary> {
  const cached = cache.get(device.id);
  // Здесь годится и полная запись: список терминалов в ней тот же.
  if (!force && cached && Date.now() - cached.fetchedAt < TTL_MS) {
    return Promise.resolve(cached);
  }
  return fetchSummary(device.id, false);
}

async function summariesOf(
  devices: CloudDevice[],
  load: (device: CloudDevice) => Promise<DeviceRuntimeSummary>,
): Promise<Record<string, DeviceRuntimeSummary>> {
  const entries = await Promise.all(
    devices.filter((device) => device.online).map(async (device) => {
      try {
        return [device.id, await load(device)] as const;
      } catch {
        return null;
      }
    }),
  );
  return Object.fromEntries(entries.filter((entry): entry is readonly [string, DeviceRuntimeSummary] => !!entry));
}

export async function getDeviceSummaries(
  devices: CloudDevice[],
  force = false,
): Promise<Record<string, DeviceRuntimeSummary>> {
  return summariesOf(devices, (device) => getDeviceSummary(device, force));
}

/** Те же сводки, но одним запросом на машину — для живой сводки главной. */
export async function getDevicePtySummaries(
  devices: CloudDevice[],
  force = false,
): Promise<Record<string, DeviceRuntimeSummary>> {
  return summariesOf(devices, (device) => getDevicePtySummary(device, force));
}

/**
 * Список устройств аккаунта с минутным кэшем — для экранов, которым он нужен
 * только чтобы понять, у кого спрашивать сводку (главная). Экраны, которые сам
 * список и показывают («Устройства»), зовут listDevices напрямую: там человек
 * ждёт актуальных имён и статусов.
 */
export async function listDevicesCached(force = false): Promise<CloudDevice[]> {
  if (!force && devicesCache && Date.now() - devicesCache.at < DEVICES_TTL_MS) {
    return devicesCache.devices;
  }
  if (devicesInflight) return devicesInflight;
  devicesInflight = (async () => {
    const { devices } = await listDevices();
    devicesCache = { at: Date.now(), devices };
    return devices;
  })().finally(() => { devicesInflight = null; });
  return devicesInflight;
}

export function invalidateDeviceSummary(deviceId: string): void {
  cache.delete(deviceId);
}
