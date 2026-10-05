import { getAIUsage, mapApiError, t } from "@tgcontrol/shared";
import type { CloudDevice } from "./cloud/api";
import { requestDevice } from "./cloud/api";
import { getMode, getServerConfig, getServerUrl, getRelayBase, getSelectedDeviceId } from "./config";
import { createUsageResource } from "./aiUsageResource";
import { usageTime } from "./aiUsagePolicy";

export interface AIUsageWindow {
  limit_id?: string;
  id: string;
  label: string;
  used_percent: number;
  duration_minutes?: number;
  resets_at?: number;
}

export interface AIExtraUsage {
  enabled: boolean;
  used: number;
  limit: number;
  used_percent: number;
  currency?: string;
  decimal_places?: number;
  reached?: boolean;
}

export interface AIProviderUsage {
  captured_at?: string;
  checked_at?: string;
  next_retry_at?: string;
  stale?: boolean;
  message_code?: string;
  id: string;
  name: string;
  installed: boolean;
  status: "available" | "signed_out" | "unavailable" | "unsupported" | string;
  account?: string;
  plan?: string;
  /** Какой из аккаунтов машины это (см. api_accounts.go). При двух подписках
   *  на одного вендора провайдер приезжает дважды — по строке на аккаунт. */
  account_id?: string;
  account_label?: string;
  /** Этим аккаунтом агент запускается сейчас. */
  account_active?: boolean;
  windows: AIUsageWindow[];
  extra_usage?: AIExtraUsage;
  message?: string;
  source?: string;
}

export interface AIUsageSnapshot {
  captured_at: string;
  providers: AIProviderUsage[];
}

export interface DeviceAIUsage {
  device: CloudDevice;
  snapshot?: AIUsageSnapshot;
  /** Человеческая причина отказа: сырой message агента/релея сюда не попадает. */
  error?: string;
  /** Агент старее версии с `GET /api/ai-usage` — лечится обновлением на ПК. */
  needsUpdate?: boolean;
}

/** Версия агента, в которой появился маршрут `GET /api/ai-usage`. */
const AI_USAGE_SINCE = [2, 33, 0] as const;

/**
 * Старый агент на неизвестный путь отвечает простым текстом «404 page not
 * found» — он не JSON, поэтому доезжал до интерфейса как есть. Версию машины
 * знаем заранее из инвентаря, так что такие устройства даже не опрашиваем.
 * Непонятную версию (dev-сборка, пусто) считаем свежей и всё-таки спрашиваем.
 */
function agentTooOld(version?: string): boolean {
  const parsed = /^v?(\d+)\.(\d+)\.(\d+)/.exec((version || "").trim());
  if (!parsed) return false;
  for (let i = 0; i < AI_USAGE_SINCE.length; i++) {
    const part = Number(parsed[i + 1]);
    if (part !== AI_USAGE_SINCE[i]) return part < AI_USAGE_SINCE[i];
  }
  return false;
}

/** Отказ «этот агент ещё не умеет лимиты» — общий для 404 и для старой версии. */
function needsUpdateResult(device: CloudDevice): DeviceAIUsage {
  const version = (device.agent_version || "").trim();
  return {
    device,
    error: version ? t("usage.needsUpdateVer", { version }) : t("usage.needsUpdate"),
    needsUpdate: true,
  };
}

/** Лимиты компьютера, с которым клиент говорит напрямую (без облачного списка). */
export interface LocalAIUsage {
  snapshot?: AIUsageSnapshot;
  /** Человеческая причина отказа — сырой текст агента сюда не попадает. */
  error?: string;
  /** Агент старее версии с `GET /api/ai-usage`. */
  needsUpdate?: boolean;
}

/**
 * Лимиты в режиме LAN / окна exe: облачного инвентаря там нет вовсе, а данные
 * отдаёт агент этого же компьютера — тот самый, на котором человек и запускает
 * Claude Code и Codex. Раньше экран умел только облачный путь (getMe +
 * listDevices + релей-прокси), поэтому вне облака не работал в принципе.
 */
function normalizeUsage(snapshot: AIUsageSnapshot): AIUsageSnapshot {
  return { ...snapshot, providers: (snapshot.providers || []).map(p => ({
    ...p, windows: p.windows || [],
    captured_at: usageTime(p.captured_at) ? p.captured_at : snapshot.captured_at,
  })) };
}

async function readUsage(force: boolean, previous: LocalAIUsage, deviceID?: string): Promise<LocalAIUsage> {
  try {
    const snapshot = deviceID === undefined
      ? await getAIUsage<AIUsageSnapshot>(force)
      : await requestDevice<AIUsageSnapshot>(deviceID, force ? "/api/ai-usage?refresh=1" : "/api/ai-usage");
    return { snapshot: normalizeUsage(snapshot) };
  } catch (error) {
    // 404 — не «не найдено», а агент без маршрута лимитов (старая версия);
    // остальное (ПК не отвечает, таймаут) уже переводит mapApiError.
    if ((error as { status?: unknown })?.status === 404) {
      return { error: t("usage.needsUpdate"), needsUpdate: true };
    }
    const message = mapApiError(error);
    const status = (error as { status?: number })?.status;
    const snapshot = status === 401 || status === 403 ? undefined : previous.snapshot;
    return { error: message, snapshot: snapshot ? { ...snapshot, providers: snapshot.providers.map(p => ({
      ...p, stale: true, message, message_code: "connection_error",
    })) } : undefined };
  }
}

type UsageResource = ReturnType<typeof createUsageResource<LocalAIUsage>>;
const resources = new Map<string, UsageResource>();
const listeners = new Set<() => void>();
let revision = 0, identity = "";
export const usageRevision = () => revision;
export const subscribeUsage = (listener: () => void) => { listeners.add(listener); return () => { listeners.delete(listener); }; };
function resourceFor(deviceID?: string): UsageResource {
  const config = getServerConfig();
  // Kept only in memory to invalidate on logout/account changes, never in a URL or log.
  const currentIdentity = `${config?.jwt || ""}|${config?.token || ""}`;
  if (identity !== currentIdentity) {
    identity = currentIdentity;
    resources.clear();
  }
  const key = deviceID === undefined ? `local:${getServerUrl()}` : `cloud:${getRelayBase()}:${deviceID}`;
  let resource = resources.get(key);
  if (!resource) {
    const owner = identity, url = getServerUrl();
    resource = createUsageResource<LocalAIUsage>({}, (force, previous) => {
      const current = getServerConfig();
      if (`${current?.jwt || ""}|${current?.token || ""}` !== owner ||
          (deviceID === undefined && (getMode() !== "self_hosted" || getServerUrl() !== url))) return Promise.resolve({});
      return readUsage(force, previous, deviceID);
    });
    resource.subscribe(() => { revision++; listeners.forEach(listener => listener()); });
    resources.set(key, resource);
  }
  return resource;
}
export function selectedUsageResource() {
  return resourceFor(getMode() === "cloud" ? getSelectedDeviceId() : undefined);
}
export function fetchLocalAIUsage(force = false): Promise<LocalAIUsage> { return selectedUsageResource().load(force); }
export function invalidateSelectedAIUsage(): void { selectedUsageResource().invalidate(); }
export function latestDeviceUsage(device: CloudDevice): LocalAIUsage {
  const result = resourceFor(device.id).get();
  if (device.online || !result.snapshot) return result;
  return { ...result, snapshot: { ...result.snapshot, providers: result.snapshot.providers.map(p => ({ ...p, stale: true })) } };
}

/**
 * Запрашивает лимиты на каждой online-машине, не переключая текущий контекст
 * приложения. Четыре параллельных worker-а не создают шторм из 25 тяжёлых
 * provider-запросов на большом аккаунте.
 */
export async function collectInfrastructureAIUsage(
  devices: CloudDevice[],
  force = false,
): Promise<DeviceAIUsage[]> {
  const online = devices.filter((device) => device.online);
  const results: DeviceAIUsage[] = new Array(online.length);
  let cursor = 0;

  const worker = async () => {
    while (cursor < online.length) {
      const index = cursor++;
      const device = online[index];
      if (agentTooOld(device.agent_version)) {
        results[index] = needsUpdateResult(device);
        continue;
      }
      try {
        results[index] = {
          device,
          ...await resourceFor(device.id).load(force),
        };
      } catch (error) {
        // 404 здесь — не «не найдено», а агент без маршрута лимитов; остальное
        // (pc_offline, таймаут, потеря доступа) уже переводит mapApiError.
        results[index] = (error as { status?: unknown })?.status === 404
          ? needsUpdateResult(device)
          : { device, error: mapApiError(error) };
      }
    }
  };

  await Promise.all(Array.from(
    { length: Math.min(4, online.length) },
    () => worker(),
  ));
  return [...results, ...devices.filter(device => !device.online).map(device => ({ device, ...latestDeviceUsage(device) }))];
}
