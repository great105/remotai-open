import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";
import { planState, type PlanMe } from "../plan";
import { installedPairCode } from "../installedPair";
import { finishLocalPairing, localComputerFromStatus, type LocalComputer } from "../localComputer";
import {
  isPcOffline,
  mapApiError,
  plural,
  promptDialog,
  REMOTAI_SERVER_INSTALL_COMMAND,
  REMOTAI_SERVICE_START_COMMAND,
  SheetShell,
  t,
  useEscape,
  useToast,
} from "@tgcontrol/shared";
import {
  acceptWorkspaceInvite,
  archiveWorkspace,
  bulkUpdateDevices,
  createWorkspace,
  createWorkspaceInvite,
  createWorkspaceTag,
  createWorkspaceZone,
  deleteWorkspaceTag,
  deleteWorkspaceZone,
  deviceZone,
  getMe,
  listDevices,
  listWorkspaceAudit,
  listWorkspaceMembers,
  listWorkspaces,
  listWorkspaceTags,
  listWorkspaceZones,
  pairNative,
  pairWithTelegram,
  renameWorkspaceZone,
  removeWorkspaceMember,
  replaceDeviceTags,
  revokeDevice,
  setDeviceFavorite,
  updateDeviceInfrastructure,
  updateWorkspaceMember,
  type CloudAuditEvent,
  type CloudDevice,
  type CloudMe,
  type CloudTag,
  type CloudWorkspace,
  type CloudWorkspaceMember,
  type CloudZone,
  type DeviceType,
  type WorkspaceRole,
} from "../cloud/api";
import { startTelegramLogin } from "../cloud/tgLogin";
// Правила экрана (версия агента, «простой парк») живут отдельным модулем —
// иначе они вызываются только из React и в CI не проверяются. См. fleetRules.ts.
import {
  agentUpdateState,
  isSimpleFleet,
  populatedWorkspaceCount as countPopulatedWorkspaces,
} from "../fleetRules";
import { getDeviceSummaries, type DeviceRuntimeSummary } from "../deviceSummaries";
import { safeNextPath } from "../nextPath";
import {
  clearSelectedDevice,
  humanDeviceName,
  isLocalAddress,
  onSelectedDeviceChange,
  selectDevice,
} from "../devices";
import {
  getMode,
  getRelayBase,
  getSelectedDeviceId,
  getSelectedWorkspaceId,
  getServerUrl,
  isNativeApp,
  isOnPCPanel,
  saveConfig,
} from "../config";
import { getConfig, reconnectWS, type SshHost } from "../api";
import { listSshHostsCached, sshTargetLabel } from "../sshCommon";
import { useCapabilities } from "../hooks/useCapabilities";
import { useGoBack } from "../navBack";

/**
 * «←» в шапке «Моих компьютеров». Ведёт туда, откуда пришли (главная, чип
 * машины с рабочего экрана, настройки), а не жёстко на главную; системная
 * «Назад» на этом же экране идёт тем же правилом (navBack.ts). `hidden` —
 * когда экран и есть корень: в облаке без выбранной машины «←» вела на
 * главную, откуда DeviceGuard тут же возвращал обратно. Аудит ИА 02.09.2026,
 * P0-1/P0-2.
 */
function InfraBackButton({ hidden = false }: { hidden?: boolean }) {
  const goBack = useGoBack();
  if (hidden) return null;
  return <button className="infra-back" aria-label={t("generic.back")} onClick={goBack}>←</button>;
}
import type { AppConfig } from "../types";
import {
  getTelegram,
  haptic,
  hapticError,
  hapticSuccess,
  tgConfirm,
} from "../telegram";
import { deviceMark } from "../components/DeviceChip";
import { BottomNav } from "../components/BottomNav";
import { QrScanSheet } from "../components/QrScanSheet";
import { InstallTarget } from "../components/InstallTarget";
// Строку сервера рисуют два экрана — и рисовали по-разному. Облик у них теперь
// общий (см. ServerIdentity), различаются только действия рядом с ним.
import { ServerIdentity } from "../components/ServerIdentity";

type DeviceTab = "all" | DeviceType;
type FilterMode = "all" | "online" | "favorite";

const ROLE_RANK: Record<WorkspaceRole, number> = {
  viewer: 1,
  operator: 2,
  admin: 3,
  owner: 4,
};

const ROLE_LABEL: Record<WorkspaceRole, string> = {
  owner: "Владелец",
  admin: "Администратор",
  operator: "Оператор",
  viewer: "Наблюдатель",
};

const TYPE_LABEL: Record<DeviceType, string> = {
  computer: "Компьютер",
  server: "Сервер",
};

const TAG_COLORS = ["#2ee6b0", "#4aa3ff", "#a78bfa", "#f0b429", "#ff6b6b", "#8b95a7"];
const SHARED_WORKSPACE_ID = "__shared_devices__";
const UNASSIGNED_ZONE_ID = "__without_zone__";
// Готовые имена зон — подсказка о том, ЧТО тут вообще раскладывают. «Продакшен»
// и «ЦОД» подсказывали язык одного владельца из ста: у человека с ноутбуком и
// домашним мини-сервером нет ни продакшена, ни центра обработки данных, зато
// есть дом, работа и дача. Теми же словами зона объясняется в подписи «Создать
// первую зону» и в ключе infra.zones.hint — язык один на всё приложение.
const ZONE_PRESETS = ["Дом", "Работа", "Офис", "Дача"];

type InviteRole = Exclude<WorkspaceRole, "owner">;

const INVITE_ROLES: InviteRole[] = ["admin", "operator", "viewer"];

/** Что даёт каждая роль — одной строкой рядом с селектом (ключи словаря). */
const ROLE_HINT_KEY: Record<InviteRole, string> = {
  admin: "infra.invite.hintAdmin",
  operator: "infra.invite.hintOperator",
  viewer: "infra.invite.hintViewer",
};

/** Версия агента как её читает человек: пустое значение — честное «неизвестна». */
function agentVersionLabel(device: CloudDevice): string {
  const version = (device.agent_version || "").trim();
  return version ? t("infra.agentVersion", { version }) : t("infra.agentVersionUnknown");
}

type StoredInvite = { code: string; role: InviteRole; expires_at: string };

const INVITE_STORE_KEY = "infra.invite.v1";

function readInviteStore(): Record<string, StoredInvite> {
  try {
    const raw = localStorage.getItem(INVITE_STORE_KEY);
    const parsed = raw ? JSON.parse(raw) : null;
    return parsed && typeof parsed === "object" ? parsed as Record<string, StoredInvite> : {};
  } catch {
    return {};
  }
}

/**
 * Код приглашения обязан переживать закрытие шита: списка приглашений на релее
 * нет, и закрытая шторка раньше уносила код навсегда — приходилось создавать
 * новое приглашение. Держим последний код по пространству, пока он не истёк.
 */
function loadInvite(workspaceId: string): StoredInvite | null {
  const stored = readInviteStore()[workspaceId];
  if (!stored?.code) return null;
  const until = Date.parse(stored.expires_at);
  if (Number.isFinite(until) && until <= Date.now()) return null;
  return stored;
}

function saveInvite(workspaceId: string, invite: StoredInvite | null): void {
  const store = readInviteStore();
  if (invite) store[workspaceId] = invite;
  else delete store[workspaceId];
  try {
    localStorage.setItem(INVITE_STORE_KEY, JSON.stringify(store));
  } catch { /* приватный режим браузера: код просто не переживёт закрытие шита */ }
}

/**
 * Имя объекта события журнала. Строка «Обновлено устройство» без имени не
 * отвечает на вопрос «какое?». Имена приходят в metadata почти всегда; для
 * «Изменены теги устройства» релей кладёт только tag_ids — там имя достаём из
 * уже загруженного списка устройств по target_id.
 */
function auditTargetName(event: CloudAuditEvent, devices: CloudDevice[]): string {
  const metadata = event.metadata;
  if (metadata) {
    for (const key of ["name", "display", "username", "device_name", "workspace_name"]) {
      const value = metadata[key];
      if (typeof value === "string" && value.trim()) return value.trim();
    }
  }
  if (event.target_type === "device" && event.target_id) {
    const found = devices.find((device) => device.id === event.target_id);
    if (found) return found.name;
  }
  if (event.target_type === "user" && event.target_id) {
    return `участник ${event.target_id}`;
  }
  return "";
}

function sharedWorkspace(devices: CloudDevice[]): CloudWorkspace {
  return {
    id: SHARED_WORKSPACE_ID,
    kind: "company",
    name: "Общий доступ",
    // Роль здесь — служебная заглушка для гейтов (operator < admin, значит
    // «+ Устройство» и «Управление» в чужом псевдо-пространстве недоступны).
    // ЧЕЛОВЕКУ её показывать нельзя: у каждого расшаренного устройства своя
    // роль, и карточка «Оператор» спорила с «Только просмотр» на устройстве.
    role: "operator",
    owner_user_id: 0,
    member_count: 0,
    device_count: devices.length,
    online_count: devices.filter((device) => device.online).length,
    created_at: new Date(0).toISOString(),
  };
}

function selectInfrastructureDevice(device: CloudDevice, accessibleWorkspaces: CloudWorkspace[]): void {
  selectDevice(device.id, device);
  if (!accessibleWorkspaces.some((workspace) => workspace.id === device.workspace_id)) {
    const shared = sharedWorkspace([device]);
    saveConfig({
      selectedWorkspaceId: SHARED_WORKSPACE_ID,
      selectedWorkspaceName: shared.name,
      // Роль берём у самого устройства, а не у псевдо-пространства: зашитый
      // operator выдавал наблюдателю права оператора на других экранах.
      selectedWorkspaceRole: device.workspace_role,
    });
  }
}

function canAdmin(role: WorkspaceRole): boolean {
  return ROLE_RANK[role] >= ROLE_RANK.admin;
}

function timeAgo(iso: string | null | undefined): string {
  if (!iso) return "никогда";
  const ts = Date.parse(iso);
  if (!Number.isFinite(ts)) return "недавно";
  const sec = Math.max(0, Math.floor((Date.now() - ts) / 1000));
  if (sec < 60) return "только что";
  const min = Math.floor(sec / 60);
  if (min < 60) return `${min} мин назад`;
  const hours = Math.floor(min / 60);
  if (hours < 24) return `${hours} ч назад`;
  return `${Math.floor(hours / 24)} дн назад`;
}

/** «1 устройство» / «2 устройства» / «5 устройств» — через общий склонятор
 *  packages/shared (локальная копия правил больше не нужна). */
function deviceCountLabel(count: number): string {
  return `${count} ${plural(count, ["устройство", "устройства", "устройств"])}`;
}

function accountName(me: CloudMe): string {
  if (me.login_display) return me.login_display;
  if (me.username) return `@${me.username}`;
  return me.first_name || "Аккаунт Remotai";
}

function DeviceEditSheet({
  device,
  workspaces,
  zones,
  tags,
  latestVersion,
  onClose,
  onChanged,
}: {
  device: CloudDevice;
  workspaces: CloudWorkspace[];
  zones: CloudZone[];
  tags: CloudTag[];
  latestVersion: string;
  onClose: () => void;
  onChanged: () => Promise<void>;
}) {
  const { toastError, toastSuccess } = useToast();
  const [name, setName] = useState(device.name);
  const [deviceType, setDeviceType] = useState<DeviceType>(device.device_type);
  const [workspaceId, setWorkspaceId] = useState(device.workspace_id);
  const [zoneId, setZoneId] = useState(deviceZone(device)?.id || "");
  const [tagIds, setTagIds] = useState<string[]>(device.tags.map((tag) => tag.id));
  const [favorite, setFavorite] = useState(device.favorite);
  const [busy, setBusy] = useState(false);
  useEscape(true, onClose);

  const targetWorkspace = workspaces.find((workspace) => workspace.id === workspaceId);
  const editable = canAdmin(device.workspace_role);
  const update = agentUpdateState(device, latestVersion);

  const save = async () => {
    setBusy(true);
    try {
      await updateDeviceInfrastructure(device.id, {
        name: name.trim(),
        device_type: deviceType,
        workspace_id: workspaceId,
        zone_id: workspaceId === device.workspace_id ? zoneId : "",
      });
      await replaceDeviceTags(device.id, workspaceId === device.workspace_id ? tagIds : []);
      if (favorite !== device.favorite) await setDeviceFavorite(device.id, favorite);
      hapticSuccess();
      toastSuccess("Устройство обновлено");
      await onChanged();
      onClose();
    } catch (error) {
      hapticError();
      toastError(mapApiError(error));
    } finally {
      setBusy(false);
    }
  };

  const remove = async () => {
    const ok = await tgConfirm(
      `Удалить «${device.name}» из Remotai? Агент на устройстве потеряет доступ к аккаунту.`,
      { danger: true, confirmText: "Удалить устройство" },
    );
    if (!ok) return;
    setBusy(true);
    try {
      await revokeDevice(device.id);
      hapticSuccess();
      toastSuccess(t("infra.deviceRemoved", { name: device.name }));
      await onChanged();
      onClose();
    } catch (error) {
      hapticError();
      toastError(mapApiError(error));
    } finally {
      setBusy(false);
    }
  };

  return (
    <SheetShell open onClose={onClose} overlayClassName="infra-backdrop" className="infra-sheet" labelledBy="infra-device-edit">
        <div className="infra-sheet-handle" aria-hidden />
        <div className="infra-sheet-head">
          <div>
            <div className="infra-eyebrow">Устройство</div>
            <h2 id="infra-device-edit">{device.name}</h2>
            {/* Одно число — один раз: строки про обновление ниже сами называют
                текущую версию, и «Remotai 2.38.0» над ними было её вторым
                отпечатком подряд. */}
            {update !== "auto" && update !== "stuck" && <p>{agentVersionLabel(device)}</p>}
          </div>
          <button className="infra-close" aria-label="Закрыть" onClick={onClose}>×</button>
        </div>

        {!editable && (
          <div className="infra-note">Ваша роль — {ROLE_LABEL[device.workspace_role].toLowerCase()}. Настройки может менять администратор компании.</div>
        )}

        {/* Отставшая версия — не повод для красной плашки в настройках машины:
            обновление идёт само. Тревожный тон остаётся только для «застряло». */}
        {update === "auto" && (
          <p className="infra-agent-calm">
            {t("infra.agent.auto", { version: device.agent_version.trim(), latest: latestVersion })}
          </p>
        )}
        {update === "stuck" && (
          <div className="infra-note">
            {t("infra.agent.stuckNote", { version: device.agent_version.trim(), latest: latestVersion })}
          </div>
        )}

        <label className="infra-field">
          <span>Название</span>
          <input value={name} onChange={(event) => setName(event.target.value.slice(0, 64))} disabled={!editable} />
        </label>

        <div className="infra-field">
          <span>Тип устройства</span>
          <div className="infra-segmented">
            {(["computer", "server"] as DeviceType[]).map((type) => (
              <button key={type} className={deviceType === type ? "active" : ""} disabled={!editable} onClick={() => setDeviceType(type)}>
                {type === "computer" ? "▣" : "▰"} {TYPE_LABEL[type]}
              </button>
            ))}
          </div>
        </div>

        {editable ? (
          <label className="infra-field">
            <span>{t("infra.device.company")}</span>
            <select value={workspaceId} onChange={(event) => { setWorkspaceId(event.target.value); setZoneId(""); setTagIds([]); }}>
              {workspaces.filter((workspace) => canAdmin(workspace.role)).map((workspace) => (
                <option key={workspace.id} value={workspace.id}>{workspace.name}</option>
              ))}
            </select>
            {targetWorkspace?.id !== device.workspace_id && <small>При переносе зона и теги будут очищены.</small>}
          </label>
        ) : (
          /* Выключенный селект был заполнен ЧУЖИМ списком: в нём только те
             компании, где вы админ, поэтому машина компании, где вы оператор,
             показывалась лежащей в «Личном». Читаем то, что прислал релей. */
          <div className="infra-field">
            <span>{t("infra.device.company")}</span>
            <b className="infra-readonly">{device.workspace_name || "—"}</b>
          </div>
        )}

        {workspaceId === device.workspace_id && (editable ? (
          <>
            <label className="infra-field">
              <span>Зона</span>
              <select value={zoneId} onChange={(event) => setZoneId(event.target.value)}>
                <option value="">Без зоны</option>
                {zones.map((zone) => <option key={zone.id} value={zone.id}>{zone.name}</option>)}
              </select>
              {/* Одно объяснение зоны на всё приложение (infra.zones.hint):
                  «место или контур» — слова из мира админа, а машину человек
                  кладёт домой, на работу или на дачу. */}
              <small>{t("infra.zones.hint")}</small>
            </label>

            <div className="infra-field">
              <span>Теги</span>
              <div className="infra-check-grid">
                {tags.length === 0 && <small>{t("infra.device.noTags")}</small>}
                {tags.map((tag) => (
                  <label key={tag.id} className="infra-check">
                    <input
                      type="checkbox"
                      checked={tagIds.includes(tag.id)}
                      onChange={() => setTagIds((current) => current.includes(tag.id) ? current.filter((id) => id !== tag.id) : [...current, tag.id])}
                    />
                    <i style={{ background: tag.color }} />
                    {tag.name}
                  </label>
                ))}
              </div>
            </div>
          </>
        ) : (
          /* Зоны и теги грузятся только для активного пространства, поэтому у
             чужого устройства списки пустые — селект честно показывал «Без
             зоны» при заполненной зоне на карточке. Печатаем факт с устройства. */
          <>
            <div className="infra-field">
              <span>Зона</span>
              <b className="infra-readonly">{deviceZone(device)?.name || t("infra.device.zoneNone")}</b>
            </div>
            {device.tags.length > 0 && (
              <div className="infra-field">
                <span>Теги</span>
                <div className="infra-pill-list">
                  {device.tags.map((tag) => (
                    <span className="infra-manage-pill" key={tag.id}><i style={{ background: tag.color }} />{tag.name}</span>
                  ))}
                </div>
              </div>
            )}
          </>
        ))}

        <label className="infra-toggle-line">
          <span><b>Избранное</b><small>Показывать устройство в быстром фильтре</small></span>
          <input type="checkbox" checked={favorite} onChange={(event) => setFavorite(event.target.checked)} />
        </label>

        <div className="infra-sheet-actions">
          {editable && <button className="btn btn-primary" disabled={busy || !name.trim()} onClick={() => void save()}>{busy ? "Сохраняю…" : "Сохранить"}</button>}
          {editable && <button className="btn infra-danger" disabled={busy} onClick={() => void remove()}>Удалить устройство</button>}
        </div>
    </SheetShell>
  );
}

function ZoneEditSheet({
  workspace,
  zone,
  existingZones,
  deviceCount,
  onClose,
  onChanged,
  onSelect,
}: {
  workspace: CloudWorkspace;
  zone: CloudZone | null;
  existingZones: CloudZone[];
  deviceCount: number;
  onClose: () => void;
  onChanged: () => Promise<void>;
  onSelect: (zoneId: string) => void;
}) {
  const { toastError, toastSuccess } = useToast();
  const [name, setName] = useState(zone?.name || "");
  const [busy, setBusy] = useState(false);
  useEscape(true, onClose);
  const nameRef = useRef<HTMLInputElement>(null);
  // SheetShell сначала запоминает открывшую кнопку, затем переводим фокус
  // в поле: autoFocus сработал бы раньше и сломал возврат фокуса.
  useEffect(() => {
    const frame = requestAnimationFrame(() => nameRef.current?.focus());
    return () => cancelAnimationFrame(frame);
  }, []);

  const save = async () => {
    const nextName = name.trim();
    if (!nextName || busy) return;
    if (existingZones.some((item) => item.id !== zone?.id && item.name.toLocaleLowerCase("ru") === nextName.toLocaleLowerCase("ru"))) {
      toastError(`Зона «${nextName}» уже существует`);
      return;
    }
    setBusy(true);
    try {
      if (zone) {
        await renameWorkspaceZone(workspace.id, zone.id, nextName);
        onSelect(zone.id);
        toastSuccess("Зона переименована");
      } else {
        const result = await createWorkspaceZone(workspace.id, nextName);
        onSelect(result.zone.id);
        toastSuccess(`Зона «${result.zone.name}» создана`);
      }
      hapticSuccess();
      await onChanged();
      onClose();
    } catch (error) {
      hapticError();
      toastError(mapApiError(error));
    } finally {
      setBusy(false);
    }
  };

  const remove = async () => {
    if (!zone || busy) return;
    const ok = await tgConfirm(
      deviceCount > 0
        // Без согласования глагола с числом: «1 устройство останутся» ломалось
        // именно здесь, а формы «останется/останутся» пришлось бы склонять
        // отдельно (N70).
        ? `Удалить зону «${zone.name}»? Устройства (${deviceCount}) переедут в «Без зоны».`
        : `Удалить пустую зону «${zone.name}»?`,
      { danger: true, confirmText: "Удалить зону" },
    );
    if (!ok) return;
    setBusy(true);
    try {
      await deleteWorkspaceZone(workspace.id, zone.id);
      onSelect("");
      await onChanged();
      hapticSuccess();
      toastSuccess("Зона удалена");
      onClose();
    } catch (error) {
      hapticError();
      toastError(mapApiError(error));
    } finally {
      setBusy(false);
    }
  };

  return (
    <SheetShell open onClose={onClose} overlayClassName="infra-backdrop" className="infra-sheet infra-zone-sheet" labelledBy="infra-zone-editor">
        <div className="infra-sheet-handle" aria-hidden />
        <div className="infra-sheet-head">
          <div>
            <div className="infra-eyebrow">{workspace.name}</div>
            <h2 id="infra-zone-editor">{zone ? "Настроить зону" : "Новая зона"}</h2>
            <p>{zone ? `${deviceCountLabel(deviceCount)} в этой зоне` : "Объедините устройства по месту или назначению"}</p>
          </div>
          <button className="infra-close" aria-label="Закрыть" onClick={onClose}>×</button>
        </div>

        <div className="infra-zone-explainer">
          <span aria-hidden>⌖</span>
          <div><b>Компания → Зона → Устройство</b><p>Например: «Моя компания» → «Продакшен» → сервер API.</p></div>
        </div>

        <label className="infra-field">
          <span>Название зоны</span>
          <input
            ref={nameRef}
            value={name}
            onChange={(event) => setName(event.target.value.slice(0, 48))}
            onKeyDown={(event) => { if (event.key === "Enter") void save(); }}
            placeholder="Например, Офис Москва"
          />
        </label>

        {!zone && (
          <div className="infra-zone-presets" aria-label="Примеры зон">
            {ZONE_PRESETS.map((preset) => (
              <button key={preset} className={name === preset ? "active" : ""} onClick={() => setName(preset)}>{preset}</button>
            ))}
          </div>
        )}

        <div className="infra-sheet-actions">
          <button className="btn btn-primary" disabled={busy || !name.trim() || zone?.name === name.trim()} onClick={() => void save()}>
            {busy ? "Сохраняю…" : zone ? "Сохранить" : "Создать зону"}
          </button>
          {zone && <button className="btn infra-danger" disabled={busy} onClick={() => void remove()}>Удалить зону</button>}
        </div>
    </SheetShell>
  );
}

function WorkspaceManageSheet({
  workspace,
  devices,
  onClose,
  onChanged,
}: {
  workspace: CloudWorkspace;
  devices: CloudDevice[];
  onClose: () => void;
  onChanged: () => Promise<void>;
}) {
  const { toastError, toastSuccess } = useToast();
  const [section, setSection] = useState<"members" | "structure" | "audit">("members");
  const [members, setMembers] = useState<CloudWorkspaceMember[]>([]);
  const [zones, setZones] = useState<CloudZone[]>([]);
  const [tags, setTags] = useState<CloudTag[]>([]);
  const [audit, setAudit] = useState<CloudAuditEvent[]>([]);
  const [busy, setBusy] = useState(false);
  // Последний код приглашения переживает закрытие шита (см. loadInvite).
  const [invite, setInvite] = useState<StoredInvite | null>(() => loadInvite(workspace.id));
  const [inviteFormOpen, setInviteFormOpen] = useState(false);
  const [inviteRole, setInviteRole] = useState<InviteRole>("operator");
  const [zoneEditor, setZoneEditor] = useState<CloudZone | "new" | null>(null);
  // Пока сверху открыт лист зоны, Escape и системный «Назад» должны закрывать
  // только его: оба обработчика висят на document и иначе схлопнули бы всё.
  useEscape(!zoneEditor, onClose);

  const reload = useCallback(async () => {
    try {
      const [memberResult, zoneResult, tagResult, auditResult] = await Promise.all([
        listWorkspaceMembers(workspace.id),
        listWorkspaceZones(workspace.id),
        listWorkspaceTags(workspace.id),
        listWorkspaceAudit(workspace.id),
      ]);
      setMembers(memberResult.members);
      setZones(zoneResult.zones);
      setTags(tagResult.tags);
      setAudit(auditResult.events);
    } catch (error) {
      toastError(mapApiError(error));
    }
  }, [toastError, workspace.id]);

  useEffect(() => { void reload(); }, [reload]);

  /**
   * Роль выбирается селектом с русскими названиями — теми же, что в списке
   * участников строкой выше. Свободный ввод латиницей («admin/operator/viewer»)
   * был единственным местом продукта, где от человека требовали машинное слово.
   */
  const inviteMember = async () => {
    setBusy(true);
    try {
      const result = await createWorkspaceInvite(workspace.id, inviteRole);
      const created: StoredInvite = { code: result.code, role: inviteRole, expires_at: result.expires_at };
      setInvite(created);
      saveInvite(workspace.id, created);
      setInviteFormOpen(false);
      hapticSuccess();
    } catch (error) {
      hapticError();
      toastError(mapApiError(error));
    } finally {
      setBusy(false);
    }
  };

  /**
   * Тост «Код скопирован» раньше показывался независимо от результата записи в
   * буфер: промис не дожидались. При отказе честно показываем сам код —
   * по образцу copyServerInstall.
   */
  const copyInviteCode = async (code: string) => {
    try {
      await navigator.clipboard.writeText(code);
      hapticSuccess();
      toastSuccess(t("infra.invite.copied"));
    } catch {
      hapticError();
      toastError(t("infra.invite.copyFailed", { code }));
    }
  };

  const forgetInvite = () => {
    setInvite(null);
    saveInvite(workspace.id, null);
  };

  /**
   * Зона создаётся, переименовывается и удаляется ОДНИМ листом — тем же, что
   * открывается из ленты зон. Здесь раньше жила вторая, урезанная копия на
   * promptDialog: без объяснения «Компания → Зона → Устройство», без пресетов
   * и с другим текстом про судьбу устройств при удалении.
   */
  const zoneDeviceCount = (zone: CloudZone) => devices.filter((device) => deviceZone(device)?.id === zone.id).length;

  const addTag = async () => {
    const name = (await promptDialog("Название тега", { defaultValue: "" }))?.trim();
    if (!name) return;
    const color = TAG_COLORS[tags.length % TAG_COLORS.length];
    try {
      await createWorkspaceTag(workspace.id, name, color);
      hapticSuccess();
      await reload();
      await onChanged();
    } catch (error) { toastError(mapApiError(error)); }
  };

  const rename = async () => {
    const name = (await promptDialog("Название компании", { defaultValue: workspace.name }))?.trim();
    if (!name || name === workspace.name) return;
    const { renameWorkspace } = await import("../cloud/api");
    try {
      await renameWorkspace(workspace.id, name);
      toastSuccess("Название обновлено");
      await onChanged();
      onClose();
    } catch (error) { toastError(mapApiError(error)); }
  };

  const removeCompany = async () => {
    if (!(await tgConfirm(
      `Удалить компанию «${workspace.name}»? Сначала перенесите из неё все устройства.`,
      { danger: true, confirmText: "Удалить компанию" },
    ))) return;
    try {
      await archiveWorkspace(workspace.id);
      await onChanged();
      onClose();
    } catch (error) { toastError(mapApiError(error)); }
  };

  const changeMemberRole = async (member: CloudWorkspaceMember, role: WorkspaceRole) => {
    if (role === "owner") return;
    try {
      await updateWorkspaceMember(workspace.id, member.user_id, role);
      await reload();
    } catch (error) { toastError(mapApiError(error)); }
  };

  const removeMember = async (member: CloudWorkspaceMember) => {
    if (!(await tgConfirm(`Удалить участника «${member.display || member.username || member.first_name || member.user_id}»?`, { danger: true, confirmText: "Удалить" }))) return;
    try {
      await removeWorkspaceMember(workspace.id, member.user_id);
      await reload();
    } catch (error) { toastError(mapApiError(error)); }
  };

  const editable = canAdmin(workspace.role);
  const auditLabel: Record<string, string> = {
    "workspace.created": "Создана компания",
    "workspace.renamed": "Переименована компания",
    "member.invited": "Создано приглашение",
    "member.joined": "Добавлен участник",
    "member.role_changed": "Изменена роль",
    "member.removed": "Удалён участник",
    "device.updated": "Обновлено устройство",
    "device.moved": "Перенесено устройство",
    "device.removed": "Удалено устройство",
    "device.tags_changed": "Изменены теги устройства",
    "group.created": "Создана зона",
    "group.renamed": "Переименована зона",
    "group.deleted": "Удалена зона",
    "tag.created": "Создан тег",
    "tag.deleted": "Удалён тег",
  };

  return (
    <>
    <SheetShell open onClose={onClose} overlayClassName="infra-backdrop" className="infra-sheet infra-sheet-wide" labelledBy="infra-workspace-manage">
        <div className="infra-sheet-handle" aria-hidden />
        <div className="infra-sheet-head">
          <div>
            {/* «Рабочее пространство» из интерфейса убрано: у человека есть
                «Личное» и компании, третьего слова про то же быть не должно. */}
            <div className="infra-eyebrow">{workspace.kind === "personal" ? "Ваши устройства" : "Компания"}</div>
            <h2 id="infra-workspace-manage">{workspace.name}</h2>
            <p>{ROLE_LABEL[workspace.role]} · {deviceCountLabel(workspace.device_count)} · {workspace.online_count} в сети</p>
          </div>
          <button className="infra-close" aria-label="Закрыть" onClick={onClose}>×</button>
        </div>

        <div className="infra-modal-tabs">
          <button className={section === "members" ? "active" : ""} onClick={() => setSection("members")}>Участники</button>
          <button className={section === "structure" ? "active" : ""} onClick={() => setSection("structure")}>Зоны и теги</button>
          <button className={section === "audit" ? "active" : ""} onClick={() => setSection("audit")}>Журнал</button>
        </div>

        {section === "members" && (
          <div className="infra-manage-list">
            {members.map((member) => (
              <div className="infra-member" key={member.user_id}>
                <span className="infra-avatar">{(member.display || member.username || member.first_name || "?").slice(0, 1).toUpperCase()}</span>
                <span className="infra-member-main">
                  <b>{member.display || (member.username ? `@${member.username}` : member.first_name) || `Участник ${member.user_id}`}</b>
                  <small>С {new Date(member.joined_at).toLocaleDateString("ru-RU")}</small>
                </span>
                {editable && member.role !== "owner" ? (
                  <select value={member.role} onChange={(event) => void changeMemberRole(member, event.target.value as WorkspaceRole)}>
                    <option value="admin">Администратор</option>
                    <option value="operator">Оператор</option>
                    <option value="viewer">Наблюдатель</option>
                  </select>
                ) : <span className="infra-role">{ROLE_LABEL[member.role]}</span>}
                {editable && member.role !== "owner" && <button className="infra-icon-btn danger" aria-label="Удалить участника" onClick={() => void removeMember(member)}>×</button>}
              </div>
            ))}
            {editable && workspace.kind === "company" && (inviteFormOpen ? (
              <div className="infra-invite-form">
                <label className="infra-field">
                  <span>{t("infra.invite.roleLabel")}</span>
                  <select value={inviteRole} onChange={(event) => setInviteRole(event.target.value as InviteRole)}>
                    {INVITE_ROLES.map((role) => <option key={role} value={role}>{ROLE_LABEL[role]}</option>)}
                  </select>
                  <small>{t(ROLE_HINT_KEY[inviteRole])}</small>
                </label>
                <div className="infra-sheet-actions">
                  <button className="btn btn-primary" disabled={busy} onClick={() => void inviteMember()}>
                    {busy ? "Создаю…" : t("infra.invite.create")}
                  </button>
                  <button className="btn btn-secondary" disabled={busy} onClick={() => setInviteFormOpen(false)}>Отмена</button>
                </div>
              </div>
            ) : (
              <button className="btn btn-secondary infra-full" disabled={busy} onClick={() => setInviteFormOpen(true)}>+ Пригласить участника</button>
            ))}
            {invite && (
              <div className="infra-invite-result">
                <span>{t("infra.invite.codeFor", { role: ROLE_LABEL[invite.role] })}</span>
                <button onClick={() => void copyInviteCode(invite.code)}>{invite.code}</button>
                <small>Действует до {new Date(invite.expires_at).toLocaleString("ru-RU")}</small>
                <small>{t("infra.invite.kept")}</small>
                <button className="infra-invite-done" onClick={forgetInvite}>{t("infra.invite.done")}</button>
              </div>
            )}
          </div>
        )}

        {section === "structure" && (
          <div className="infra-structure">
            {/* Что такое зона, объясняем теми же словами, что и на самом экране
                машин (infra.zones.hint): «места и контуры» — это описание для
                админа, а человек раскладывает компьютеры по дому, работе и даче. */}
            <div className="infra-structure-head"><span><b>Зоны</b><small>{t("infra.zones.hint")}</small></span>{editable && <button onClick={() => setZoneEditor("new")}>+ Добавить</button>}</div>
            <div className="infra-pill-list">
              {zones.length === 0 && <span className="infra-muted">Зон пока нет. Создайте «Дом» или «Офис».</span>}
              {zones.map((zone) => (
                <span className="infra-manage-pill zone" key={zone.id}>⌖ {zone.name}{editable && (
                  /* Переименование и удаление живут внутри листа зоны — там же,
                     где человек видит, сколько устройств переедет в «Без зоны». */
                  <button aria-label={`Настроить зону ${zone.name}`} onClick={() => setZoneEditor(zone)}>•••</button>
                )}</span>
              ))}
            </div>
            <div className="infra-structure-head"><b>Теги</b>{editable && <button onClick={() => void addTag()}>+ Добавить</button>}</div>
            <div className="infra-pill-list">
              {tags.length === 0 && <span className="infra-muted">Пока нет тегов</span>}
              {tags.map((tag) => (
                <span className="infra-manage-pill" key={tag.id}><i style={{ background: tag.color }} />{tag.name}{editable && <button aria-label={`Удалить тег ${tag.name}`} onClick={async () => {
                  if (!(await tgConfirm(`Удалить тег «${tag.name}»?`, { danger: true, confirmText: "Удалить" }))) return;
                  try {
                    await deleteWorkspaceTag(workspace.id, tag.id);
                    await reload();
                    await onChanged();
                  } catch (error) { toastError(mapApiError(error)); }
                }}>×</button>}</span>
              ))}
            </div>
          </div>
        )}

        {section === "audit" && (
          <div className="infra-audit">
            {audit.length === 0 && <div className="infra-muted">Событий пока нет.</div>}
            {audit.map((event) => {
              // Заголовок — только человеческий текст: сырой ключ вида
              // «device.tags_changed» уходит в мелкую подпись, а неизвестное
              // событие называется «Другое действие», а не машинным именем.
              const label = auditLabel[event.action];
              const member = event.target_type === "user"
                ? members.find((item) => String(item.user_id) === event.target_id)
                : undefined;
              const target = member
                ? (member.display || (member.username ? `@${member.username}` : member.first_name) || `участник ${member.user_id}`)
                : auditTargetName(event, devices);
              const role = typeof event.metadata?.role === "string"
                ? ROLE_LABEL[event.metadata.role as WorkspaceRole]
                : "";
              return (
                <div className="infra-audit-row" key={event.id}>
                  <i />
                  <span>
                    <b>{label || t("infra.audit.unknown")}{target ? ` «${target}»` : ""}{role ? ` · ${role}` : ""}</b>
                    <small>
                      {event.actor || "Система"} · {new Date(event.created_at).toLocaleString("ru-RU")}
                      {label ? "" : ` · ${event.action}`}
                    </small>
                  </span>
                </div>
              );
            })}
          </div>
        )}

        {editable && workspace.kind === "company" && (
          <div className="infra-company-actions">
            <button onClick={() => void rename()}>Переименовать компанию</button>
            {workspace.role === "owner" && <button className="danger" onClick={() => void removeCompany()}>Удалить компанию</button>}
          </div>
        )}
    </SheetShell>
    {/* Лист зоны — сосед подложки, а не её потомок: так он рисуется поверх и
        его подложка не закрывает заодно «Управление». */}
    {zoneEditor && (
      <ZoneEditSheet
        workspace={workspace}
        zone={zoneEditor === "new" ? null : zoneEditor}
        existingZones={zones}
        deviceCount={zoneEditor === "new" ? 0 : zoneDeviceCount(zoneEditor)}
        onClose={() => setZoneEditor(null)}
        onChanged={async () => { await reload(); await onChanged(); }}
        /* Фильтра по зонам внутри «Управления» нет — выбирать нечего. */
        onSelect={() => {}}
      />
    )}
    </>
  );
}

/**
 * Действия над машиной — листом из «⋯» на карточке.
 *
 * Зачем отдельный лист: карточка теперь ПЕРЕКЛЮЧАЕТ управление по тапу, и
 * держать на ней же ряд «Терминалы · Файлы · Экран», звезду избранного и
 * шестерёнку настроек значило бы, что один жест делает разное в зависимости от
 * того, куда попал палец. Здесь собрано всё, что можно сделать С машиной, — и
 * каждая строка ростом с палец, а не 12-пиксельная надпись в три колонки.
 */
function DeviceActionsSheet({ device, isCurrent, hasScreen, onClose, onSwitch, onOpen, onFavorite, onSettings }: {
  device: CloudDevice;
  isCurrent: boolean;
  /** У сервера без монитора экрана нет — кнопку не рисуем вовсе. */
  hasScreen: boolean;
  onClose: () => void;
  onSwitch: () => void;
  onOpen: (path: string) => void;
  onFavorite: () => void;
  onSettings: () => void;
}) {
  useEscape(true, onClose);
  const viewer = device.workspace_role === "viewer";
  const canWork = device.online && !viewer;

  return (
    <SheetShell open onClose={onClose} overlayClassName="infra-backdrop" className="infra-sheet infra-menu-sheet" labelledBy="infra-device-menu">
        <div className="infra-sheet-handle" aria-hidden />
        <div className="infra-sheet-head">
          <div>
            <div className="infra-eyebrow">{TYPE_LABEL[device.device_type]}</div>
            <h2 id="infra-device-menu">{device.name}</h2>
            <p>{t("infra.menu.title")}</p>
          </div>
          <button className="infra-close" aria-label="Закрыть" onClick={onClose}>×</button>
        </div>

        {viewer && <div className="infra-note">{t("infra.menu.viewer")}</div>}
        {!device.online && !viewer && <div className="infra-note">{t("infra.menu.offline")}</div>}

        <div className="infra-menu-list">
          {!isCurrent && (
            <button className="infra-menu-item primary" onClick={() => { onSwitch(); onClose(); }}>
              <i aria-hidden>◉</i><span>{t("infra.menu.switch")}</span>
            </button>
          )}
          <button className="infra-menu-item" disabled={!canWork} onClick={() => { onOpen("/pty"); onClose(); }}>
            <i aria-hidden>⌁</i><span>{t("infra.menu.terminals")}</span>
          </button>
          <button className="infra-menu-item" disabled={!canWork} onClick={() => { onOpen("/files"); onClose(); }}>
            <i aria-hidden>▤</i><span>{t("infra.menu.files")}</span>
          </button>
          {hasScreen && (
            <button className="infra-menu-item" disabled={!canWork} onClick={() => { onOpen("/remote"); onClose(); }}>
              <i aria-hidden>▣</i><span>{t("infra.menu.screen")}</span>
            </button>
          )}
          {/* «Питание» в два нажатия (аудит ИА 02.09.2026, волна 3): выключить,
              перезагрузить или усыпить машину раньше можно было только через
              переключение на неё → «Система» → вкладка «Питание». Вкладка
              берётся из адреса (SystemView читает ?tab=), а onOpen у родителя
              переключает машину и зовёт navigate(path) со строкой запроса как
              есть. */}
          <button className="infra-menu-item" disabled={!canWork} onClick={() => { onOpen("/system?tab=power"); onClose(); }}>
            <i aria-hidden>⏻</i><span>{t("infra.menu.power")}</span>
          </button>
          <button className="infra-menu-item" onClick={() => { onFavorite(); onClose(); }}>
            <i aria-hidden>★</i><span>{device.favorite ? t("infra.menu.favoriteRemove") : t("infra.menu.favoriteAdd")}</span>
          </button>
          {/* Переименование, зона и удаление живут в одном листе настроек —
              заводить им три отдельные строки значило бы три разных места, где
              человек ищет одно и то же. */}
          <button className="infra-menu-item" onClick={() => { onSettings(); onClose(); }}>
            <i aria-hidden>⚙</i><span>{t("infra.menu.settings")}</span>
          </button>
        </div>
    </SheetShell>
  );
}

/**
 * Серверы по SSH — в том же списке, что и машины аккаунта.
 *
 * Владелец продукта попросил один список всего, чем он управляет: «нажимаю на
 * сервер — и на сервере тоже могу создавать терминалы». Раньше SSH жил
 * исключительно в своём разделе, и человек, у которого два компьютера и прод по
 * SSH, искал их в двух местах.
 *
 * Слияние ЧЕСТНОЕ, а не косметическое. Сервер по SSH — не такая же машина:
 *  • он работает ЧЕРЕЗ выбранный компьютер (соединение, пароли и ключи держит
 *    агент на нём, телефон к серверу не ходит) — поэтому имя посредника
 *    написано прямо в подзаголовке;
 *  • его нет в аккаунте Remotai: он лежит в `~/.ssh/config` и `ssh_hosts.json`
 *    того компьютера, и на другом компьютере списка будет другой;
 *  • экрана у него нет — только терминалы и файлы.
 * Отсюда же следует поведение при выключенном посреднике: список не исчезает
 * молча, а объясняет, ЧЕЙ компьютер надо включить.
 *
 * Слияние живёт только на этом экране: раздел «Серверы» (/ssh) остаётся на
 * месте со всеми возможностями (создание, теги, пробросы, файлы, установка
 * Remotai), сюда вынесен лишь вход в терминалы одним тапом.
 */
function SshServersSection({ viaName, deviceId, offlineVia }: {
  /** Имя компьютера-посредника: через него агент и ходит на серверы. */
  viaName: string;
  /** Смена машины обязана перечитать список: у другого ПК он другой. */
  deviceId: string;
  /** Посредник заведомо не в сети — запрос не нужен, ответ уже известен. */
  offlineVia?: boolean;
}) {
  const navigate = useNavigate();
  const [hosts, setHosts] = useState<SshHost[] | null>(null);
  const [pcOffline, setPcOffline] = useState(false);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  const load = useCallback(async (force?: boolean) => {
    if (offlineVia) {
      setHosts([]);
      setPcOffline(true);
      setError("");
      return;
    }
    setBusy(true);
    try {
      const list = await listSshHostsCached(force);
      setHosts(list);
      setPcOffline(false);
      setError("");
    } catch (e) {
      // «Серверов нет» и «компьютер не в сети» — разные ответы: во втором
      // случае человеку надо включить машину, а не заводить сервер.
      setHosts([]);
      setPcOffline(isPcOffline(e));
      setError(isPcOffline(e) ? "" : mapApiError(e));
    } finally {
      setBusy(false);
    }
  }, [offlineVia]);

  useEffect(() => { void load(); }, [load, deviceId]);

  return (
    <section className="infra-ssh">
      <div className="infra-section-line">
        <span>{t("infra.ssh.title")}</span>
        <div>
          <button onClick={() => { haptic(); navigate("/ssh"); }}>{t("infra.ssh.openAll")}</button>
        </div>
      </div>
      <p className="infra-ssh-note">{t("infra.ssh.via", { name: viaName })}</p>

      {pcOffline ? (
        <div className="infra-ssh-state">
          <b>{t("infra.ssh.offlineTitle", { name: viaName })}</b>
          <span>{t("infra.ssh.offlineText")}</span>
          <button disabled={busy} onClick={() => void load(true)}>{t("infra.ssh.retry")}</button>
        </div>
      ) : error ? (
        <div className="infra-ssh-state">
          <b>{error}</b>
          <button disabled={busy} onClick={() => void load(true)}>{t("infra.ssh.retry")}</button>
        </div>
      ) : hosts == null ? (
        <div className="infra-ssh-state"><span>{t("infra.ssh.loading")}</span></div>
      ) : hosts.length === 0 ? (
        <div className="infra-ssh-state"><span>{t("infra.ssh.empty")}</span></div>
      ) : (
        <>
          <p className="infra-ssh-tap">{t("infra.ssh.tap")}</p>
          <div className="infra-ssh-list">
            {hosts.map((host) => (
              <button
                key={host.id}
                type="button"
                className="infra-ssh-item"
                onClick={() => { haptic("light"); navigate(`/ssh/${encodeURIComponent(host.id)}`); }}
              >
                {/* Имя берём тем же правилом, что и раздел «SSH-серверы»:
                    сервер без своего имени зовётся адресом. Раньше здесь стоял
                    `host.name || host.host` — и один сервер назывался «prod» в
                    списке машин и «root@prod» в разделе. */}
                <ServerIdentity name={host.name || sshTargetLabel(host)} address={sshTargetLabel(host)} />
                <span className="infra-ssh-go" aria-hidden>{"→"}</span>
              </button>
            ))}
          </div>
        </>
      )}
    </section>
  );
}

/**
 * «Мои компьютеры» без облака.
 *
 * Телефон, подключённый к своему ПК по QR, и окно exe живут в self_hosted:
 * облачного инвентаря там нет вовсе, поэтому запросы getMe()/listDevices()
 * встречали человека красным «Нет связи с Remotai. Доступ не подтверждён» —
 * ошибкой в ответ на вопрос «где мои компьютеры». Отвечаем тем, что реально
 * известно: этот компьютер (имя, адрес, связь, версия), его серверы по SSH и
 * честное «остальные машины — после входа в аккаунт».
 *
 * Список здесь ОДИН, как и в облаке: этот компьютер плюс серверы, которые
 * работают через него. Прежнее правило «SSH не сводить с парком машин» отменено
 * владельцем продукта — но сводим честно, разницу двух сущностей называем
 * словами (см. SshServersSection).
 */
function LocalInfrastructureView() {
  const navigate = useNavigate();
  const { platform, hasDisplay } = useCapabilities();
  const [info, setInfo] = useState<AppConfig | null>(null);
  const [link, setLink] = useState<"checking" | "online" | "offline">("checking");
  const [linkError, setLinkError] = useState("");
  const [busy, setBusy] = useState(false);
  /**
   * «+ Компьютер» — кнопка, которой здесь не было вовсе.
   *
   * Гид звал нажать её («кнопка „+ Компьютер“ в „Моих компьютерах“ проводит по
   * шагам»), а в локальном режиме добавить вторую машину было нечем: кнопка
   * живёт только в облачной версии экрана. Человек искал её на пустом месте.
   *
   * Отвечаем честно: второй компьютер существует только в аккаунте — прямое
   * подключение знает ровно одну машину. Поэтому кнопка не заглушка и не
   * обещание: она объясняет, что произойдёт и что для этого нужно, и ведёт ко
   * входу. Шторкой, а не переходом: уводить с экрана в ответ на «добавить» —
   * значит потерять и вопрос, и ответ.
   */
  const [addOpen, setAddOpen] = useState(false);

  /** Один запрос отвечает сразу на всё: имя машины, версия агента и жив ли он. */
  const probe = useCallback(async () => {
    setBusy(true);
    try {
      const config = await getConfig();
      setInfo(config);
      setLink("online");
      setLinkError("");
    } catch (error) {
      setLink("offline");
      setLinkError(mapApiError(error));
    } finally {
      setBusy(false);
    }
  }, []);

  useEffect(() => { void probe(); }, [probe]);
  useEscape(addOpen, () => setAddOpen(false));

  const address = getServerUrl();
  let hostLabel = address;
  try { hostLabel = address ? new URL(address).host : ""; } catch { hostLabel = address; }
  /**
   * Под именем машины стояла строка «127.0.0.1:59999» — адрес, по которому
   * клиент до неё дошёл. На вопрос «что это за компьютер» он не отвечает: это
   * адрес самого себя, и человек, открывший окно на этом же ПК, читает его как
   * непонятное имя. Loopback заменяем словами; чужой адрес в локальной сети
   * оставляем — там он хотя бы говорит, к какой машине подключён телефон.
   */
  const localAddress = isLocalAddress(address || hostLabel);
  // Имя машины — общим фильтром: агент подставляет именем hostname, а он на
  // части машин приходит адресом, и тогда «Компьютер» честнее, чем «192.168.1.5».
  const name = humanDeviceName(info?.hostname, t("infra.local.thisPcName"));
  const version = (info?.version || "").trim();
  const icon = deviceMark(info?.platform || platform, "computer");
  const linkLabel = link === "online"
    ? t("infra.local.online")
    : link === "offline" ? t("infra.local.offline") : t("infra.local.checking");

  return (
    <div className="infra-page infra-page-with-nav">
      <header className="infra-header">
        <InfraBackButton />
        <div>
          <h1>{t("infra.title")}</h1>
          <p>{t("infra.local.subtitle")}</p>
        </div>
      </header>

      <div className="infra-content">
        {/* Кнопка стоит там же, где в облачной версии экрана, — в шапке списка:
            человек, которого сюда прислал гид, ищет её именно над машинами. */}
        <div className="infra-section-line">
          <span>{t("infra.local.thisPc")}</span>
          <div>
            <button onClick={() => { haptic(); setAddOpen(true); }}>{t("infra.local.addBtn")}</button>
          </div>
        </div>
        <div className="infra-local-list">
          <section className="infra-local-card">
            <div className="infra-local-head">
              <span aria-hidden>{icon}</span>
              <span>
                <b>{name}</b>
                {localAddress
                  ? <small>{t("devices.thisPcLanHint")}</small>
                  : hostLabel ? <small>{hostLabel}</small> : null}
              </span>
            </div>
            <div className="infra-local-facts">
              <span className={link === "offline" ? "offline" : link === "checking" ? "checking" : ""}>
                <i />{linkLabel}
              </span>
              <span>{version ? t("infra.agentVersion", { version }) : t("infra.agentVersionUnknown")}</span>
            </div>
            {link === "offline" && linkError && <p className="infra-local-text">{linkError}</p>}
            <div className="infra-local-actions">
              {link === "offline" ? (
                <button className="primary" disabled={busy} onClick={() => void probe()}>
                  {busy ? t("infra.local.checking") : t("infra.local.retry")}
                </button>
              ) : (
                <>
                  <button onClick={() => navigate("/pty")}>{t("infra.local.openTerminals")}</button>
                  <button onClick={() => navigate("/files")}>{t("infra.local.openFiles")}</button>
                  {hasDisplay && <button onClick={() => navigate("/remote")}>{t("infra.local.openScreen")}</button>}
                </>
              )}
            </div>
          </section>
        </div>

        {/* Второй пункт того же списка: серверы, которые работают ЧЕРЕЗ этот
            компьютер. Нажатие открывает их терминалы — ради этого сюда и
            приходят; полный раздел с настройками остаётся по ссылке. */}
        <SshServersSection viaName={name} deviceId="local" offlineVia={link === "offline"} />

        <div className="infra-local-list">
          <section className="infra-local-card">
            <div className="infra-local-head">
              <span aria-hidden>◎</span>
              <span><b>{t("infra.local.othersTitle")}</b></span>
            </div>
            <p className="infra-local-text">{t("infra.local.othersText")}</p>
            {/* Дверь одна и зовётся одним словом. Раньше отсюда вёл «Войти в
                аккаунт Remotai», а гид звал «+ Компьютер» — два имени одного
                действия человек связать не мог. Вход остался: он внутри
                шторки, под объяснением, зачем он нужен. */}
            <div className="infra-local-actions">
              <button className="primary" onClick={() => { haptic(); setAddOpen(true); }}>{t("infra.local.addBtn")}</button>
            </div>
          </section>
        </div>
      </div>

      {/* Что произойдёт и что для этого нужно — до входа, а не после. Шагов
          ровно три, и они те же, что в гиде: поставить, войти, подтвердить. */}
      {addOpen && (
        <SheetShell
          open={addOpen}
          onClose={() => setAddOpen(false)}
          overlayClassName="infra-backdrop"
          className="infra-sheet"
          labelledBy="infra-local-add"
        >
            <div className="infra-sheet-handle" aria-hidden />
            <div className="infra-sheet-head">
              <div><h2 id="infra-local-add">{t("infra.local.addTitle")}</h2></div>
              <button className="infra-close" aria-label={t("modal.close")} onClick={() => setAddOpen(false)}>×</button>
            </div>
            <p className="infra-local-text">{t("infra.local.addText")}</p>
            <ol className="infra-pair-steps">
              <li><b>1</b><span>{t("infra.local.addStep1")}</span></li>
              <li><b>2</b><span>{t("infra.local.addStep2")}</span></li>
              <li><b>3</b><span>{t("infra.local.addStep3")}</span></li>
            </ol>
            <button className="btn btn-primary infra-full" onClick={() => { haptic(); navigate("/cloud-login"); }}>
              {t("infra.local.othersBtn")}
            </button>
            {/* Аккаунт не навязываем: с одним компьютером всё уже работает, и
                человек имеет право закрыть шторку, ничего не потеряв. */}
            <p className="infra-local-text">{t("infra.local.addNoAccount")}</p>
        </SheetShell>
      )}
      <BottomNav active="devices" />
    </div>
  );
}

/**
 * Экран парка машин двух режимов. Режим фиксируем на монтировании: сменить его
 * можно только через вход в аккаунт или пейринг, а те уводят на другой экран.
 */
export function InfrastructureView() {
  const [cloud] = useState(() => getMode() === "cloud");
  return cloud ? <CloudInfrastructureView /> : <LocalInfrastructureView />;
}

function CloudInfrastructureView() {
  const navigate = useNavigate();
  const [searchParams] = useSearchParams();
  const { toastError, toastSuccess } = useToast();
  const [devices, setDevices] = useState<CloudDevice[] | null>(null);
  const [workspaces, setWorkspaces] = useState<CloudWorkspace[]>([]);
  const [me, setMe] = useState<CloudMe | null>(null);
  const [summaries, setSummaries] = useState<Record<string, DeviceRuntimeSummary>>({});
  const [zones, setZones] = useState<CloudZone[]>([]);
  const [tags, setTags] = useState<CloudTag[]>([]);
  const [activeWorkspaceId, setActiveWorkspaceId] = useState(getSelectedWorkspaceId);
  const [latestVersion, setLatestVersion] = useState("");
  // Пространства/зоны — надстройка над ответом «где мой компьютер», поэтому
  // раскрываются по требованию (или сами, когда структура реально есть).
  const [structureOpen, setStructureOpen] = useState(false);
  const [tab, setTab] = useState<DeviceTab>("all");
  const [filter, setFilter] = useState<FilterMode>("all");
  const [zoneFilter, setZoneFilter] = useState("");
  const [tagFilter, setTagFilter] = useState("");
  const [query, setQuery] = useState("");
  const [selected, setSelected] = useState<string[]>([]);
  const [editDevice, setEditDevice] = useState<CloudDevice | null>(null);
  const [manageWorkspace, setManageWorkspace] = useState<CloudWorkspace | null>(null);
  const [zoneEditor, setZoneEditor] = useState<CloudZone | "new" | null>(null);
  const [addOpen, setAddOpen] = useState(false);
  const [addType, setAddType] = useState<DeviceType>("computer");
  const [addZoneId, setAddZoneId] = useState("");
  const [pairCode, setPairCode] = useState("");
  const [fromInstalledApp, setFromInstalledApp] = useState(false);
  const [pairBusy, setPairBusy] = useState(false);
  const [localComputer, setLocalComputer] = useState<LocalComputer | null | undefined>(() => isOnPCPanel() ? undefined : null);
  const [localCodeBusy, setLocalCodeBusy] = useState(false);
  const [localRequestedCode, setLocalRequestedCode] = useState("");
  const [localFinishFailed, setLocalFinishFailed] = useState(false);
  useEffect(() => {
    if (!isOnPCPanel()) return;
    const controller = new AbortController();
    let active = true;
    const timeout = setTimeout(() => controller.abort(), 3000);
    void fetch("/api/setup/status", { signal: controller.signal, cache: "no-store" })
      .then(response => response.ok ? response.json() : null)
      .then(status => { if (active) setLocalComputer(localComputerFromStatus(status)); })
      .catch(() => { if (active) setLocalComputer(null); })
      .finally(() => clearTimeout(timeout));
    return () => { active = false; controller.abort(); clearTimeout(timeout); };
  }, []);
  const getLocalPairCode = async () => {
    if (localCodeBusy) return;
    setLocalCodeBusy(true);
    try {
      const response = await fetch("/api/setup/cloud/pair-request", { method: "POST", headers: { "Content-Type": "application/json" }, body: "{}" });
      if (!response.ok) throw new Error();
      const result = await response.json();
      const code = installedPairCode("/infrastructure?add=1&code=" + encodeURIComponent(result.code || ""));
      if (!code) throw new Error();
      setAddType("computer");
      setAddZoneId("");
      setPairCode(code);
      setLocalRequestedCode(code);
      toastSuccess("Код этого компьютера подставлен. Нажмите «Подключить».");
    } catch {
      toastError("Не удалось получить код этого компьютера. Откройте «Панель ПК» и попробуйте ещё раз.");
    } finally { setLocalCodeBusy(false); }
  };
  const [tgBusy, setTgBusy] = useState(false);
  const [err, setErr] = useState("");
  const [bulkAction, setBulkAction] = useState<"favorite" | "device_type" | "zone" | "move" | "tags">("favorite");
  const [bulkValue, setBulkValue] = useState("true");
  const [bulkBusy, setBulkBusy] = useState(false);
  // Сканер живёт ШТОРКОЙ над этим экраном: маршрут не меняется, список машин и
  // поле кода остаются на месте (раньше кнопка уводила на /scan).
  const [scanOpen, setScanOpen] = useState(false);
  const [scanHint, setScanHint] = useState("");
  // Действия машины (терминалы, файлы, экран, избранное, настройки) — в «⋯»,
  // чтобы тап по карточке значил ровно одно: «управлять этой машиной».
  const [menuDevice, setMenuDevice] = useState<CloudDevice | null>(null);
  // Только что подключённая машина: она уже в списке, а управление переключаем
  // не молча, а по нажатию «Управлять этим компьютером».
  const [justAdded, setJustAdded] = useState<CloudDevice | null>(null);
  const tg = getTelegram();
  const inTg = !!tg?.initData;
  // Управляемая машина — состояние, а не разовое чтение конфига: её меняет тап
  // по карточке прямо здесь, и пометка «Управляете сейчас», список серверов
  // выбранного ПК и предложение переключиться обязаны переехать сразу.
  const [selectedDeviceId, setSelectedDeviceId] = useState(getSelectedDeviceId);
  useEffect(() => onSelectedDeviceChange(setSelectedDeviceId), []);

  // Пока сверху открыт сканер, Escape и системный «Назад» закрывают только его:
  // оба обработчика висят на document и иначе схлопнули бы и шторку добавления.
  useEscape(addOpen && !scanOpen, () => setAddOpen(false));

  const reload = useCallback(async () => {
    setErr("");
    try {
      const [meResult, deviceResult, workspaceResult] = await Promise.all([
        getMe(),
        listDevices(),
        listWorkspaces(),
      ]);
      const workspaceIDs = new Set(workspaceResult.workspaces.map((workspace) => workspace.id));
      const sharedDevices = deviceResult.devices.filter((device) => !workspaceIDs.has(device.workspace_id));
      const effectiveWorkspaces = sharedDevices.length
        ? [...workspaceResult.workspaces, sharedWorkspace(sharedDevices)]
        : workspaceResult.workspaces;
      setMe(meResult);
      setDevices(deviceResult.devices);
      setWorkspaces(effectiveWorkspaces);
      // Порядок важен: сначала решаем, какой ПК управляется, и только потом —
      // какое пространство активно. Иначе экран открывался активной карточкой
      // пустого «Личного», а чип в шапке (он берёт пространство выбранной
      // машины) — «Общим доступом»: два разных ответа на один вопрос.
      const current = deviceResult.devices.find((device) => device.id === getSelectedDeviceId());
      const preferred = current || deviceResult.devices.find((device) => device.online) || deviceResult.devices[0];
      if (preferred && preferred.id !== getSelectedDeviceId()) {
        selectInfrastructureDevice(preferred, effectiveWorkspaces);
      } else if (!preferred) {
        // Удалили последнее устройство: подставить нечего, поэтому забываем
        // выбранную машину — иначе чип в шапке и рабочие экраны продолжают
        // жить с ПК, которого в аккаунте больше нет.
        clearSelectedDevice();
      }
      let workspaceId = getSelectedWorkspaceId();
      if (!effectiveWorkspaces.some((workspace) => workspace.id === workspaceId)) {
        // Сохранённого выбора нет (первый заход) — открываем пространство той
        // машины, которая только что стала выбранной; личное — лишь когда
        // машин нет вовсе.
        const deviceHome = preferred
          ? effectiveWorkspaces.find((workspace) => workspace.id === preferred.workspace_id)?.id
            || (effectiveWorkspaces.some((workspace) => workspace.id === SHARED_WORKSPACE_ID) ? SHARED_WORKSPACE_ID : "")
          : "";
        workspaceId = deviceHome
          || effectiveWorkspaces.find((workspace) => workspace.kind === "personal")?.id
          || effectiveWorkspaces[0]?.id
          || "";
      }
      if (workspaceId) {
        const workspace = effectiveWorkspaces.find((item) => item.id === workspaceId);
        setActiveWorkspaceId(workspaceId);
        saveConfig({
          selectedWorkspaceId: workspaceId,
          selectedWorkspaceName: workspace?.name,
          selectedWorkspaceRole: workspace?.role,
        });
      }
      void getDeviceSummaries(deviceResult.devices).then(setSummaries);
    } catch (error) {
      setErr(mapApiError(error));
      setDevices([]);
    }
  }, []);

  useEffect(() => { void reload(); }, [reload]);

  /**
   * Опубликованная версия релиза — чтобы честно помечать устаревшие агенты.
   * Манифест публичный и кэшируется релеем на 5 минут; если он недоступен,
   * пометки просто не будет (ложных «устарел» быть не должно).
   */
  useEffect(() => {
    let alive = true;
    void (async () => {
      try {
        const response = await fetch(`${getRelayBase().replace(/\/+$/, "")}/v1/latest`, {
          headers: { Accept: "application/json" },
        });
        if (!response.ok) return;
        const manifest = await response.json() as { version?: string };
        const version = String(manifest?.version || "").trim();
        if (alive && /^\d+\.\d+/.test(version)) setLatestVersion(version);
      } catch { /* без манифеста бейдж «устарел» не показываем */ }
    })();
    return () => { alive = false; };
  }, []);

  const reloadActiveStructure = useCallback(async () => {
    if (!activeWorkspaceId) return;
    if (activeWorkspaceId === SHARED_WORKSPACE_ID) {
      setZones([]);
      setTags([]);
      return;
    }
    const [zoneResult, tagResult] = await Promise.all([
      listWorkspaceZones(activeWorkspaceId),
      listWorkspaceTags(activeWorkspaceId),
    ]);
    setZones(zoneResult.zones);
    setTags(tagResult.tags);
  }, [activeWorkspaceId]);

  useEffect(() => {
    void reloadActiveStructure().catch(() => { setZones([]); setTags([]); });
  }, [reloadActiveStructure]);

  useEffect(() => {
    if (!devices?.length) return;
    const timer = window.setInterval(async () => {
      if (document.hidden) return;
      try {
        const result = await listDevices();
        setDevices(result.devices);
        // Устройство могли удалить с другого телефона: без этого чип в шапке
        // остался бы с машиной, которой в аккаунте уже нет.
        if (!result.devices.length) clearSelectedDevice();
        setSummaries(await getDeviceSummaries(result.devices, true));
      } catch { /* retain the last confirmed inventory */ }
    }, 15_000);
    return () => window.clearInterval(timer);
  }, [devices?.length]);

  useEffect(() => {
    if (!devices) return;
    if (searchParams.get("add") === "1") {
      const suppliedCode = installedPairCode(`/infrastructure?${searchParams}`);
      setPairCode(suppliedCode);
      setFromInstalledApp(!!suppliedCode);
      const requestedWorkspace = searchParams.get("workspace");
      const requestedType = searchParams.get("type");
      const requested = requestedWorkspace ? workspaces.find((item) => item.id === requestedWorkspace) : undefined;
      if (requested) {
        switchWorkspace(requested);
      } else {
        // Ссылкой «поставил Remotai на сервер — введите код» сюда приходят с
        // любого экрана, в том числе когда активен «Общий доступ»: добавлять
        // туда некуда, и лист открылся бы мёртвым (пейринг ушёл бы в
        // несуществующее пространство). Переводим туда, где человек админ.
        const here = workspaces.find((item) => item.id === getSelectedWorkspaceId());
        if (!here || here.id === SHARED_WORKSPACE_ID || !canAdmin(here.role)) {
          const fallback = workspaces.find((item) => item.kind === "personal" && canAdmin(item.role))
            || workspaces.find((item) => item.id !== SHARED_WORKSPACE_ID && canAdmin(item.role));
          if (fallback) switchWorkspace(fallback);
        }
      }
      if (requestedType === "server" || requestedType === "computer") setAddType(requestedType);
      const requestedZone = searchParams.get("zone");
      if (requestedZone) setAddZoneId(requestedZone);
      setAddOpen(true);
      const back = searchParams.get("back");
      navigate(back ? `/infrastructure?back=${encodeURIComponent(safeNextPath(back))}` : "/infrastructure", { replace: true });
      return;
    }
    const focused = searchParams.get("focus");
    if (focused) {
      const found = devices.find((device) => device.id === focused);
      if (found) {
        const workspace = workspaces.find((item) => item.id === found.workspace_id)
          || workspaces.find((item) => item.id === SHARED_WORKSPACE_ID);
        if (workspace) {
          setActiveWorkspaceId(workspace.id);
          setTab("all");
          setFilter("all");
          setQuery("");
          saveConfig({
            selectedWorkspaceId: workspace.id,
            selectedWorkspaceName: workspace.name,
            selectedWorkspaceRole: workspace.role,
          });
        }
        window.setTimeout(() => {
          document.getElementById(`infra-device-${found.id}`)?.scrollIntoView({
            behavior: "smooth",
            block: "center",
          });
        }, 120);
      } else {
        toastError("Устройство из сводки больше недоступно.");
      }
      navigate("/infrastructure", { replace: true });
      return;
    }
    // Два вида диплинка: из бота — `?select=<ПК>&next=<экран>`; из гарда
    // «сначала выберите компьютер» (App.tsx) — только `?next=<экран>`, ПК там
    // берётся автовыбором чуть выше (reload). Второй случай раньше терялся:
    // цель лежала в памяти и срабатывала позже, при тапе по карточке.
    const target = searchParams.get("select");
    const next = searchParams.get("next");
    if (!target && !next) return;
    if (target) {
      const found = devices.find((device) => device.id === target);
      if (!found) {
        toastError("Устройство из ссылки больше недоступно.");
        navigate("/infrastructure", { replace: true });
        return;
      }
      selectInfrastructureDevice(found, workspaces);
    } else if (!getSelectedDeviceId()) {
      // Выбирать нечего (устройств нет) — цель забываем: пустое состояние
      // списка само объясняет, что нужно подключить компьютер. Уходить на цель
      // без выбранного ПК нельзя, гард вернёт обратно (петля).
      navigate("/infrastructure", { replace: true });
      return;
    }
    // Куда пускать по адресу из ссылки — общее правило клиента (nextPath.ts):
    // свои экраны с запросом, всё прочее на главную. Раньше выражение жило
    // здесь, но понадобилось второй раз — для возврата по `?back=`.
    navigate(safeNextPath(next), { replace: true });
  }, [devices, navigate, searchParams, toastError, workspaces]);

  const activeWorkspace = workspaces.find((workspace) => workspace.id === activeWorkspaceId) || null;
  const directWorkspaceIDs = useMemo(
    () => new Set(workspaces.filter((workspace) => workspace.id !== SHARED_WORKSPACE_ID).map((workspace) => workspace.id)),
    [workspaces],
  );
  const inActiveWorkspace = useCallback((device: CloudDevice) => (
    activeWorkspaceId === SHARED_WORKSPACE_ID
      ? !directWorkspaceIDs.has(device.workspace_id)
      : device.workspace_id === activeWorkspaceId
  ), [activeWorkspaceId, directWorkspaceIDs]);
  const visibleDevices = useMemo(() => {
    const needle = query.trim().toLocaleLowerCase("ru");
    return (devices || []).filter((device) => {
      if (activeWorkspaceId && !inActiveWorkspace(device)) return false;
      if (tab !== "all" && device.device_type !== tab) return false;
      if (filter === "online" && !device.online) return false;
      if (filter === "favorite" && !device.favorite) return false;
      const zone = deviceZone(device);
      if (zoneFilter === UNASSIGNED_ZONE_ID && zone) return false;
      if (zoneFilter && zoneFilter !== UNASSIGNED_ZONE_ID && zone?.id !== zoneFilter) return false;
      if (tagFilter && !device.tags.some((tag) => tag.id === tagFilter)) return false;
      if (needle && ![
        device.name, device.hostname, device.workspace_name, zone?.name,
        ...device.tags.map((tag) => tag.name),
      ].filter(Boolean).some((value) => String(value).toLocaleLowerCase("ru").includes(needle))) return false;
      return true;
    });
  }, [activeWorkspaceId, devices, filter, inActiveWorkspace, query, tab, tagFilter, zoneFilter]);

  /**
   * Групповое действие обязано касаться только того, что человек видит. Смена
   * вкладки, поиска, статуса или тега раньше выбор не трогала — «Применить»
   * молча меняло машины из другого фильтра. Теперь выбор сужается до видимых
   * (те, что остались на экране, сохраняются — в отличие от полного сброса).
   */
  useEffect(() => {
    if (!selected.length) return;
    const visibleIDs = new Set(visibleDevices.map((device) => device.id));
    const next = selected.filter((id) => visibleIDs.has(id));
    if (next.length !== selected.length) setSelected(next);
  }, [selected, visibleDevices]);

  const switchWorkspace = (workspace: CloudWorkspace) => {
    haptic();
    setActiveWorkspaceId(workspace.id);
    setZoneFilter("");
    setTagFilter("");
    setSelected([]);
    saveConfig({
      selectedWorkspaceId: workspace.id,
      selectedWorkspaceName: workspace.name,
      selectedWorkspaceRole: workspace.role,
    });
  };

  const switchZone = (zoneId: string) => {
    haptic("light");
    setZoneFilter(zoneId);
    setSelected([]);
  };

  /**
   * Тап по машине = переключение управления, и человек остаётся в списке.
   *
   * Раньше карточка уводила на главную, а офлайн-машина сначала спрашивала
   * «Всё равно открыть?» — вопрос без пользы: подготовиться к включению машины
   * (выбрать её, чтобы всё приложение смотрело туда) — нормальное желание, и
   * запрещать его нечем. Вместо подтверждения ДО — «Отменить» ПОСЛЕ: человек
   * уже сказал, куда хочет, а вернуться одним нажатием после промаха пальцем
   * важнее, чем лишний вопрос (та же модель, что у плиток на главной).
   */
  const switchToDevice = (device: CloudDevice) => {
    if (device.id === getSelectedDeviceId()) {
      // Тап по машине, которой уже управляешь, — это «пойдём работать», а не
      // «ничего не делать». На первом входе с ОДНОЙ машиной другого хода из
      // списка не было вовсе: карточка не кнопка, в меню «•••» пункта на
      // главную нет (аудит путей 29.08.2026).
      haptic();
      navigate(safeNextPath(searchParams.get("back")));
      return;
    }
    const from = (devices || []).find((item) => item.id === getSelectedDeviceId()) || null;
    haptic();
    selectInfrastructureDevice(device, workspaces);
    hapticSuccess();
    setJustAdded((current) => (current && current.id === device.id ? null : current));
    toastSuccess(
      t("devices.switcher.switched", { name: device.name || device.hostname }),
      from
        ? {
          action: {
            label: t("devices.switcher.undo"),
            onClick: () => selectInfrastructureDevice(from, workspaces),
          },
        }
        : undefined,
    );
    // Пришли сюда С РАБОЧЕГО ЭКРАНА (чип машины передал `?back=`) — туда и
    // возвращаемся: машину меняют ради работы, а не ради списка. Раньше человек
    // оставался в списке, и возврат к делу стоил третьего нажатия.
    const back = searchParams.get("back");
    if (back) navigate(safeNextPath(back));
  };

  const openOnDevice = (device: CloudDevice, path: string) => {
    selectInfrastructureDevice(device, workspaces);
    haptic("light");
    navigate(path);
  };

  const createCompany = async () => {
    const name = (await promptDialog("Название компании", { defaultValue: "" }))?.trim();
    if (!name) return;
    try {
      const result = await createWorkspace(name);
      await reload();
      switchWorkspace(result.workspace);
      toastSuccess("Компания создана");
    } catch (error) { toastError(mapApiError(error)); }
  };

  const joinCompany = async () => {
    const code = (await promptDialog("Код приглашения в компанию", { defaultValue: "" }))?.trim();
    if (!code) return;
    try {
      const result = await acceptWorkspaceInvite(code);
      await reload();
      switchWorkspace(result.workspace);
      toastSuccess(`Вы присоединились к «${result.workspace.name}»`);
    } catch (error) { toastError(mapApiError(error)); }
  };

  const pair = async (raw?: string) => {
    const code = (raw ?? pairCode).trim().toUpperCase();
    if (code.replace("-", "").length < 8 || pairBusy || localCodeBusy || !activeWorkspace) return;
    setPairBusy(true);
    try {
      let deviceId = "";
      if (inTg) {
        const result = await pairWithTelegram(code, { workspaceId: activeWorkspace.id, deviceType: addType, zoneId: addZoneId });
        deviceId = result.device_id;
      } else {
        const result = await pairNative(getRelayBase(), code, { workspaceId: activeWorkspace.id, deviceType: addType, zoneId: addZoneId });
        saveConfig({
          mode: "cloud",
          relayBase: getRelayBase(),
          jwt: result.user_jwt,
          jwtExpiresAt: Date.parse(result.expires_at),
          selectedDeviceType: result.device_type,
          selectedWorkspaceId: result.workspace_id,
        });
        deviceId = result.device_id;
      }
      if (localComputer && code === localRequestedCode) {
        try { await finishLocalPairing(code); setLocalFinishFailed(false); }
        catch { setLocalFinishFailed(true); }
        setLocalRequestedCode("");
      }
      hapticSuccess();
      setPairCode("");
      setFromInstalledApp(false);
      setScanOpen(false);
      setAddOpen(false);
      await reload();
      const device = (await listDevices()).devices.find((item) => item.id === deviceId);
      if (device) {
        if (!workspaces.some((workspace) => workspace.id === device.workspace_id)) {
          setActiveWorkspaceId(SHARED_WORKSPACE_ID);
        }
        if (!getSelectedDeviceId() || getSelectedDeviceId() === device.id) {
          // Первая машина в аккаунте: управлять больше нечем, спрашивать не о
          // чем. reload мог уже выбрать её: это тот же путь первого запуска.
          selectInfrastructureDevice(device, workspaces);
          const back = searchParams.get("back");
          if (back) navigate(safeNextPath(back));
        } else {
          // Машин уже несколько: молча уводить управление с той, где идёт
          // работа, нельзя. Новая уже в списке — предлагаем переключиться.
          setJustAdded(device);
        }
      }
      toastSuccess(`${TYPE_LABEL[addType]} подключён`);
    } catch (error) {
      hapticError();
      const text = mapApiError(error);
      // Сканер открыт — ошибку показываем ПОД видоискателем: тост на чёрном
      // экране камеры читают не все.
      setScanHint(text);
      toastError(text);
    } finally {
      setPairBusy(false);
    }
  };

  /**
   * Камера — шторкой над этим же экраном. Раньше здесь был
   * `navigate("/scan?…")`: человек терял из виду и список машин, и поле кода,
   * а Telegram уходил в свой сканер отдельной веткой. Обе ветки (камера
   * браузера и штатный сканер Telegram) теперь внутри QrScanSheet.
   */
  const scan = () => {
    haptic();
    setScanHint("");
    setScanOpen(true);
  };

  const copyServerInstall = async () => {
    try {
      await navigator.clipboard.writeText(REMOTAI_SERVER_INSTALL_COMMAND);
      hapticSuccess();
      toastSuccess("Команда установки скопирована");
    } catch {
      toastError("Не удалось скопировать. Выделите команду вручную.");
    }
  };

  const toggleFavorite = async (device: CloudDevice) => {
    try {
      await setDeviceFavorite(device.id, !device.favorite);
      setDevices((current) => current?.map((item) => item.id === device.id ? { ...item, favorite: !item.favorite } : item) || []);
      hapticSuccess();
    } catch (error) { toastError(mapApiError(error)); }
  };

  const applyBulk = async () => {
    if (!selected.length) return;
    // Страховка к сужению выбора: под действие попадают только устройства,
    // которые прямо сейчас на экране.
    const targets = selected.filter((id) => visibleDevices.some((device) => device.id === id));
    if (!targets.length) {
      toastError(t("infra.bulk.noneVisible"));
      setSelected([]);
      return;
    }
    let value: boolean | string | string[] = bulkValue;
    if (bulkAction === "favorite") value = bulkValue === "true";
    if (bulkAction === "tags") value = bulkValue ? [bulkValue] : [];
    if (bulkAction === "move") {
      // Перенос необратимо чистит зону и все теги (релей делает это на сервере),
      // поэтому спрашиваем так же явно, как в настройках одного устройства.
      const target = workspaces.find((workspace) => workspace.id === bulkValue);
      const ok = await tgConfirm(
        t("infra.bulk.moveConfirm", {
          count: deviceCountLabel(targets.length),
          workspace: target?.name || t("infra.bulk.moveTarget"),
        }),
        { danger: true, confirmText: t("infra.bulk.moveConfirmBtn") },
      );
      if (!ok) return;
    }
    setBulkBusy(true);
    try {
      const result = await bulkUpdateDevices(targets, bulkAction, value);
      if (Object.keys(result.failures).length) {
        toastError(`Изменено ${result.succeeded.length} из ${targets.length}. Проверьте права на остальные устройства.`);
      } else {
        toastSuccess(`Обновлено устройств: ${result.succeeded.length}`);
      }
      setSelected([]);
      await reload();
    } catch (error) { toastError(mapApiError(error)); } finally { setBulkBusy(false); }
  };

  // Выход из аккаунта, список входов и их завершение отсюда убраны (аудит ИА
  // 02.09.2026, P1-20/V11): всё это живёт в Настройках, а здесь остаётся одна
  // дверь туда из карточки аккаунта ниже. switchTelegram остаётся ради баннера
  // гостевого аккаунта («Подключить Telegram»).
  const switchTelegram = async () => {
    if (inTg) {
      toastSuccess("Закрываем Mini App. Переключите активный аккаунт Telegram и откройте Remotai снова.");
      tg?.close();
      return;
    }
    setTgBusy(true);
    try {
      const handle = await startTelegramLogin();
      const result = await handle.done;
      if (result.ok) {
        reconnectWS();
        await reload();
        toastSuccess("Telegram-аккаунт подключён");
      }
    } catch (error) { toastError(mapApiError(error)); } finally { setTgBusy(false); }
  };

  const permanent = !!me && (me.permanent ?? ((me.telegram_id ?? 0) > 0));
  const activeDevices = (devices || []).filter(inActiveWorkspace);
  /**
   * Предложение «Управлять этим компьютером» после подключения. Живёт, пока
   * машина есть в списке и ею ещё не управляют: переключились (отсюда, с
   * главной или из бота) — предлагать нечего.
   */
  const addedDevice = justAdded
    && justAdded.id !== selectedDeviceId
    && (devices || []).some((device) => device.id === justAdded.id)
    ? justAdded
    : null;
  /** Машина, через которую работают SSH-серверы: она же управляемая сейчас. */
  const currentDevice = (devices || []).find((device) => device.id === selectedDeviceId) || null;
  const unassignedDevices = activeDevices.filter((device) => !deviceZone(device));
  const zoneStats = zones.map((zone) => {
    const members = activeDevices.filter((device) => deviceZone(device)?.id === zone.id);
    return {
      zone,
      total: members.length,
      online: members.filter((device) => device.online).length,
    };
  });
  /**
   * Зона существует для человека тогда, когда в ней ЛЕЖАТ машины, — то же самое
   * правило, по которому считаются пространства (см. simpleFleet ниже).
   * Заведённая и ещё не наполненная зона не раскладывает ничего: одной такой
   * хватало, чтобы на экран вернулся весь организационный слой («Личное и
   * компании», «+ Компания», «По приглашению», «Зоны», «Выбрать все»), а путь до
   * карточки своей машины вырос с 698 до 987 px.
   *
   * Исключение ровно одно — зона, ВЫБРАННАЯ сейчас: её либо только что создали
   * (лист зоны сразу делает новую активным фильтром — иначе на «Создать зону»
   * экран отвечал бы полным молчанием), либо из неё унесли последнюю машину.
   * Спрятать выбранную — значит оставить включённый фильтр без единого следа на
   * экране и без кнопки «Сбросить».
   */
  const populatedZoneCount = zoneStats.filter(({ total }) => total > 0).length;
  const visibleZoneStats = zoneStats.filter(({ zone, total }) => total > 0 || zone.id === zoneFilter);
  const activeZone = zones.find((zone) => zone.id === zoneFilter) || null;
  const counters = {
    all: activeDevices.length,
    computer: activeDevices.filter((device) => device.device_type === "computer").length,
    server: activeDevices.filter((device) => device.device_type === "server").length,
  };
  const openAddDevice = (type?: DeviceType) => {
    setFromInstalledApp(false);
    if (type) setAddType(type);
    setAddZoneId(zones.some((zone) => zone.id === zoneFilter) ? zoneFilter : "");
    setAddOpen(true);
  };

  /**
   * «+ Устройство» в шапке — из «Общего доступа» и из чужой компании.
   *
   * Раньше кнопка там просто гасла: подключать устройства можно только туда,
   * где ты хозяин, а в расшаренном пространстве — некуда. Но серая кнопка без
   * объяснения читается как «сломалось», и человек с расшаренным компьютером
   * упирался в неё на пустом месте — при том что своё пространство у него
   * есть, просто сейчас открыто не оно.
   *
   * Намерение однозначно: «хочу подключить свой компьютер». Поэтому вместо
   * запрета — переход туда, где это возможно.
   */
  const addDeviceHome = workspaces.find((workspace) => workspace.kind === "personal" && canAdmin(workspace.role))
    || workspaces.find((workspace) => canAdmin(workspace.role))
    || null;

  const addDeviceFromHeader = () => {
    if (activeWorkspace && canAdmin(activeWorkspace.role)) {
      openAddDevice();
      return;
    }
    if (addDeviceHome) {
      switchWorkspace(addDeviceHome);
      openAddDevice();
      return;
    }
    // Своего пространства нет вовсе (аккаунт только с расшаренными машинами) —
    // тогда честный текст вместо немого отказа.
    toastError(t("infra.addNeedsOwnWorkspace"));
  };
  const refreshInfrastructure = async () => {
    await Promise.all([reload(), reloadActiveStructure()]);
  };

  /**
   * Нулевое состояние — онбординг первого компьютера, а не панель управления
   * парком машин: пространства, зоны, теги, фильтры и групповые действия
   * описывают то, чего у человека ещё нет.
   */
  const onboarding = !!devices && devices.length === 0
    && workspaces.length <= 1
    && (workspaces.length === 0 || workspaces[0]?.kind === "personal");
  /** «Настроить пространства и зоны» из онбординга открывает обычный экран. */
  const showOnboarding = onboarding && !structureOpen && !!activeWorkspace;

  /**
   * Прямая ссылка на .exe уместна только там, где файл можно тут же запустить:
   * окно exe и настольный браузер. В Telegram, APK и мобильном браузере
   * установщик Windows скачивается В ТЕЛЕФОН и бесполезен — там нужен адрес,
   * который человек откроет на компьютере.
   */
  const mobileSurface = inTg || isNativeApp
    || (typeof window !== "undefined" && !!window.matchMedia?.("(pointer: coarse)")?.matches);
  const relayOrigin = getRelayBase().replace(/\/+$/, "");
  const quotaMax = me?.max_devices ?? 0;
  const quotaUsed = me?.devices_count ?? 0;
  const quotaReached = quotaMax > 0 && quotaUsed >= quotaMax;
  // Имя полки — как оно звучит на витрине: «Локально», «Про», «Флит».
  // Раньше сюда печаталось сырое значение из базы, и человек читал «тариф pro».
  // Пустой tier ничего не печатает: выдумывать человеку тариф нельзя.
  const planName = me ? planState(me as PlanMe).planName : "";

  /**
   * ПРИНЦИП ЭКРАНА (его держатся все условия ниже):
   *   • экран показывает ровно то, что у человека ЕСТЬ;
   *   • слой появляется в тот момент, когда в нём появляется ВТОРОЙ объект,
   *     и ни секундой раньше;
   *   • одно и то же число печатается на экране ОДИН раз.
   *
   * «Простой парк» считается по СОДЕРЖИМОМУ — и пространства, и зоны по одному
   * правилу, а не по двум разным. Признак «пространств больше одного» на живом
   * телефоне владельца не сработал: их было два, но во втором ноль машин, и
   * человек с ОДНИМ компьютером получал первый экран из пустого «Личное · 0 в
   * сети · 0 всего», служебной зоны «Все устройства» и пяти счётчиков «1 в
   * сети». Ровно то же самое делала одна пустая зона — она считалась структурой
   * парка, хотя не раскладывала ни одной машины. Выбор существует, только если
   * машины лежат больше чем в одном пространстве или хотя бы в одной зоне;
   * пустое пространство и пустая зона в выборе не участвуют.
   */
  const populatedWorkspaceCount = useMemo(
    () => countPopulatedWorkspaces(devices, directWorkspaceIDs, SHARED_WORKSPACE_ID),
    [devices, directWorkspaceIDs],
  );
  const simpleFleet = isSimpleFleet(populatedWorkspaceCount, populatedZoneCount);
  // Слой раскрывается сам ровно тогда, когда в нём появился выбор; закрывать
  // его сам не имеет права — открыл человек, значит ему туда надо.
  useEffect(() => {
    if (!simpleFleet) setStructureOpen(true);
  }, [simpleFleet]);
  /**
   * Фильтры показываем, когда их есть зачем применять: до четырёх устройств все
   * видны на экране целиком, а поиск, статус и теги над списком из одной
   * карточки — просто ещё один блок между человеком и его компьютером.
   */
  const filtersActive = !!zoneFilter || !!tagFilter || filter !== "all" || tab !== "all" || !!query.trim();
  // Зона сбрасывается своей кнопкой в строке контекста, поэтому zoneFilter сюда
  // не входит — иначе выбор зоны возвращал бы на экран весь блок фильтров.
  const showFilters = activeDevices.length > 3
    || !!query.trim() || filter !== "all" || !!tagFilter || tab !== "all";

  /**
   * Счётчик машин живёт в ЗАГОЛОВКЕ СПИСКА и больше нигде. В строке контекста он
   * печатается, только если отвечает на другой вопрос: фильтр или зона спрятали
   * часть машин пространства, и «сколько всего» перестало совпадать с «сколько
   * на экране». Совпало — второй раз то же число не печатаем.
   */
  const visibleOnlineCount = visibleDevices.filter((device) => device.online).length;
  const contextCountAdds = !!activeWorkspace
    && (activeWorkspace.device_count !== visibleDevices.length
      || activeWorkspace.online_count !== visibleOnlineCount);

  /**
   * Своих машин нет, а расшаренные есть. Это ровно то, что видел владелец:
   * телефон вошёл в ДРУГОЙ аккаунт Remotai, чем тот, за которым числится
   * компьютер, поэтому «Личное» пусто, а компьютер лежит в общем доступе — и
   * ничего из этого экран не объяснял. Считаем по правам: «своё» — то, что
   * лежит в пространстве, где человек владелец или администратор.
   */
  const ownWorkspaces = workspaces.filter((workspace) => workspace.id !== SHARED_WORKSPACE_ID && canAdmin(workspace.role));
  const ownDeviceCount = (devices || []).filter((device) => ownWorkspaces.some((workspace) => workspace.id === device.workspace_id)).length;
  /**
   * Ровно ОДНА машина, и та чужая. Условие «своих нет» само по себе ловило и
   * штатного оператора компании: человеку с десятью рабочими серверами экран
   * писал «Этот компьютер открыт вам другим аккаунтом» — единственным числом
   * над списком из десяти и с обвинением в перепутанном входе там, где его не
   * было. Спутать свой аккаунт с чужим можно, когда машина одна.
   */
  const sharedOnly = !!devices && devices.length === 1 && ownDeviceCount === 0;
  const ownHome = ownWorkspaces.find((workspace) => workspace.kind === "personal") || ownWorkspaces[0] || null;
  /** «Добавить свой компьютер» — сразу туда, где человек имеет право добавлять. */
  const addOwnDevice = () => {
    setFromInstalledApp(false);
    haptic();
    if (ownHome && ownHome.id !== activeWorkspaceId) switchWorkspace(ownHome);
    setAddType("computer");
    setAddZoneId("");
    setAddOpen(true);
  };

  const copyPcLink = async () => {
    try {
      await navigator.clipboard.writeText(relayOrigin);
      hapticSuccess();
      toastSuccess(t("infra.onboarding.linkCopied"));
    } catch {
      toastError(t("infra.onboarding.linkCopyFailed", { url: relayOrigin }));
    }
  };

  const openOnPcBlock = (
    <div className="infra-install-command">
      <span>
        <b>{t("infra.onboarding.openOnPcTitle")}</b>
        <small>{t("infra.onboarding.openOnPcHint")}</small>
      </span>
      <code>{relayOrigin}</code>
      <button onClick={() => void copyPcLink()}>{t("infra.onboarding.copyLink")}</button>
    </div>
  );

  const quotaBlock = me && quotaMax > 0 && (
    quotaReached ? (
      // Дверь «Агенты» отсюда убрана (аудит ИА 02.09.2026, P1-6): на экране
      // она одна — в шапке (infra-header-usage), и вторая рядом с лимитом
      // устройств вела туда же.
      <div className="infra-note">
        {t("infra.quotaReached", { used: quotaUsed, max: quotaMax })}
      </div>
    ) : (
      // Тариф Remotai переехал сюда со страницы лимитов: там он стоял рядом с
      // подписками на нейросети, и две совершенно разные подписки на одном
      // экране путались. Компьютеры — это про устройства, а устройства живут
      // здесь; число печатается один раз, тариф стоит рядом как подпись.
      <p className="infra-quota">
        {t("infra.quota", { used: quotaUsed, max: quotaMax })}
        {planName && <span className="infra-quota-plan"> · {t("infra.planNamed", { plan: planName })}</span>}
      </p>
    )
  );

  return (
    <div className="infra-page infra-page-with-nav">
      <header className="infra-header">
        <InfraBackButton hidden={!selectedDeviceId} />
        <div>
          {/* Одно имя раздела на всё приложение: вкладка, заголовок, гид на
              главной и плашка офлайна зовут его «Мои компьютеры». Слово
              «Инфраструктура» — жаргон и в навигации его нет вовсе. */}
          <h1>{t("infra.title")}</h1>
          <p>{t("infra.subtitle")}</p>
        </div>
        <div className="infra-header-actions">
          <button className="infra-header-usage" onClick={() => navigate("/agents")}><span>◔</span><b>{t("agents.title")}</b></button>
          {/* Одно имя у всех дверей подключения (аудит ИА 02.09.2026,
              P1-25/V9): «+ Компьютер» здесь и «Подключить компьютер» в
              Настройках и на главной — одна и та же дверь. */}
          <button className="infra-header-add" aria-label={t("infra.addComputer")} onClick={addDeviceFromHeader}>{t("infra.addComputer")}</button>
        </div>
      </header>

      <div className="infra-content">
        {localFinishFailed && <div className="infra-empty" role="status">
          <p>Компьютер добавлен в аккаунт. Осталось включить доступ через интернет в «Панели ПК».</p>
          <button className="btn btn-primary" onClick={() => navigate("/panel")}>Открыть «Панель ПК»</button>
        </div>}
        {err ? (
          <div className="infra-empty">
            <span>⌁</span><h2>Нет связи с Remotai</h2><p>{err}</p>
            <button className="btn btn-primary" onClick={() => void reload()}>Повторить</button>
          </div>
        ) : devices == null ? (
          <div className="route-loading">{t("infra.loading")}</div>
        ) : (
          <>
            {/* Нулевое состояние: экран отвечает на «где мой компьютер», а не
                предлагает настроить компании и зоны для пустого списка. */}
            {showOnboarding && (
              <section className="infra-onboarding">
                <div className="infra-onboarding-head">
                  <span aria-hidden>▣</span>
                  <div>
                    <h2>{t("infra.onboarding.title")}</h2>
                    <p>{localComputer ? "Подключите этот компьютер к вашему аккаунту Remotai." : t("infra.onboarding.subtitle")}</p>
                  </div>
                </div>
                {localComputer === undefined ? <p role="status">Проверяем Remotai на этом компьютере…</p> : localComputer ? <div className="infra-local-connect">
                  <p>Remotai уже запущен на этом {localComputer.platform === "darwin" ? "Mac" : "компьютере"}. Получите код и нажмите «Подключить», чтобы добавить его в текущий аккаунт и управлять через интернет.</p>
                  <button className="btn btn-primary" disabled={localCodeBusy || pairBusy} onClick={() => void getLocalPairCode()}>
                    {localCodeBusy ? "Получаем код…" : `Получить код этого ${localComputer.platform === "darwin" ? "Mac" : "компьютера"}`}
                  </button>
                  <p className="onb-hint">Для другой машины введите её код ниже.</p>
                </div> : <>
                  <p className="onb-hint">Здесь вы будете работать с агентами. Remotai нужно установить на машину с проектами. Если она уже была в аккаунте, проверьте способ входа в «Личном кабинете».</p>
                  {mobileSurface ? openOnPcBlock : <InstallTarget />}
                </>}
                {localComputer === null && <ol className="infra-pair-steps">
                  <li><b>1</b><span>{t("infra.onboarding.step1", { url: relayOrigin })}</span></li>
                  <li><b>2</b><span>{t("infra.onboarding.step2")}</span></li>
                  <li><b>3</b><span>{t("infra.onboarding.step3")}</span></li>
                </ol>}
                <div className="device-pair-form infra-pair-form">
                  <input
                    className="device-pair-input"
                    value={pairCode}
                    disabled={localCodeBusy}
                    onChange={(event) => setPairCode(event.target.value.toUpperCase().slice(0, 9))}
                    onKeyDown={(event) => { if (event.key === "Enter") void pair(); }}
                    placeholder="FX42-9KQ7"
                    autoCapitalize="characters"
                    autoCorrect="off"
                    spellCheck={false}
                    aria-label="Код подключения"
                  />
                  <button className="btn btn-primary" disabled={pairBusy || localCodeBusy || pairCode.replace("-", "").length < 8} onClick={() => void pair()}>
                    {pairBusy ? "Подключаю…" : "Подключить"}
                  </button>
                </div>
                <button className="btn btn-secondary infra-full" onClick={scan}>{t("infra.add.scanBtn")}</button>
                <small className="infra-add-scan-note">{t("infra.add.scanNote")}</small>
                {quotaBlock}
                <div className="infra-onboarding-more">
                  <button onClick={() => openAddDevice("server")}>{t("infra.onboarding.serverLink")}</button>
                  <button onClick={() => void joinCompany()}>{t("infra.onboarding.haveInvite")}</button>
                  <button onClick={() => navigate("/start")}>Помочь выбрать способ подключения</button>
                  <details><summary>Для нескольких пользователей и компьютеров</summary><button onClick={() => setStructureOpen(true)}>{t("infra.onboarding.structure")}</button></details>
                </div>
              </section>
            )}

            {/* Контекст одной строкой: чьё пространство, сколько машин в сети и
                где лежат подключение, «Управление» и раскрытие структуры. */}
            {!showOnboarding && activeWorkspace && (!simpleFleet || structureOpen) && (
              <section className="infra-context-bar">
                <span className="infra-context-icon" aria-hidden>{activeWorkspace.kind === "personal" ? "◆" : activeWorkspace.id === SHARED_WORKSPACE_ID ? "⌁" : "▦"}</span>
                <span className="infra-context-main">
                  <b>{activeWorkspace.name}</b>
                  {/* Счётчик — только когда он не повторяет заголовок списка. */}
                  {contextCountAdds && (
                    <small>{activeWorkspace.online_count} из {activeWorkspace.device_count} в сети</small>
                  )}
                </span>
                {activeWorkspace.id !== SHARED_WORKSPACE_ID && (
                  <button onClick={() => setManageWorkspace(activeWorkspace)}>Управление</button>
                )}
                <div className="infra-context-actions">
                  {canAdmin(activeWorkspace.role) && (
                    <button className="primary" onClick={() => openAddDevice()}>{t("infra.addComputer")}</button>
                  )}
                  <button aria-expanded={structureOpen} onClick={() => setStructureOpen((open) => !open)}>
                    {structureOpen ? t("infra.structure.hide") : t("infra.structure.show")}
                  </button>
                </div>
              </section>
            )}

            {/* Имя пространства на экране ровно одно — в строке контекста выше.
                Раньше «Личное» повторялось четырежды подряд: строка контекста,
                единственная плитка-переключатель, карточка-описание под ней и
                подпись ленты зон. Плитки нужны только когда есть между чем
                переключаться, карточка-описание дублировала строку контекста
                целиком (имя, счётчик, «Управление») и удалена. */}
            {!showOnboarding && structureOpen && (
            <section className="infra-workspaces">
              <div className="infra-section-line">
                <span>{t("infra.workspaces.title")}</span>
                <div>
                  <button onClick={() => void joinCompany()}>По приглашению</button>
                  <button onClick={() => void createCompany()}>+ Компания</button>
                </div>
              </div>
              {workspaces.length > 1 && (
              <div className="infra-workspace-strip">
                {workspaces.map((workspace) => (
                  <button
                    key={workspace.id}
                    className={`infra-workspace-card${workspace.id === activeWorkspaceId ? " active" : ""}`}
                    onClick={() => switchWorkspace(workspace)}
                  >
                    <span className="infra-workspace-icon">{workspace.kind === "personal" ? "◆" : workspace.id === SHARED_WORKSPACE_ID ? "⌁" : "▦"}</span>
                    <span>
                      <b>{workspace.name}</b>
                      {/* Счётчик — только у тех пространств, которых сейчас не
                          видно: у открытого то же число стоит в заголовке
                          списка машин. А «0 в сети · 0 всего» — это два нуля
                          вместо ответа: пустое говорит о себе словом. */}
                      {workspace.id !== activeWorkspaceId && (
                        <small>
                          {workspace.device_count === 0
                            ? t("infra.workspaces.empty")
                            : `${workspace.online_count} в сети · ${workspace.device_count} всего`}
                        </small>
                      )}
                    </span>
                    {/* У «Общего доступа» своей роли нет: она у каждого
                        устройства своя и написана на его карточке. */}
                    {workspace.id !== SHARED_WORKSPACE_ID && <i>{ROLE_LABEL[workspace.role]}</i>}
                  </button>
                ))}
              </div>
              )}
              {/* Про «чужие против своих» рассказываем, только когда машины
                  правда лежат в разных пространствах. Когда всё, что есть у
                  человека, — это одна расшаренная машина, объяснение другое и
                  стоит оно у самого списка (см. infra-shared-note). */}
              {activeWorkspace?.id === SHARED_WORKSPACE_ID && populatedWorkspaceCount > 1 && (
                <p className="infra-workspace-hint">{t("infra.shared.hint")}</p>
              )}
            </section>
            )}

            {/* Свёрнутая структура всё равно показывает выбранную зону — иначе
                фильтр остался бы включённым без единого следа на экране. */}
            {/* Заголовок «Зоны» без единой зоны и без права их заводить (чужое
                пространство) — пустая полка: в общем доступе зоны хозяйские, и
                показывать нечего. Пустые зоны на экране не живут (см.
                visibleZoneStats), поэтому и полку открываем, только когда на ней
                что-то есть; предложение завести первую — когда зон нет вовсе.
                Заведённая впрок пустая зона не пропадает: она остаётся в выборе
                зоны у самой машины (её настройки и групповое действие) и в
                «Управлении» — стоит положить в неё машину, как она появится на
                ленте. */}
            {!showOnboarding && activeWorkspace && (structureOpen || !!zoneFilter) && (visibleZoneStats.length > 0 || (canAdmin(activeWorkspace.role) && zones.length === 0)) && (
              <section className="infra-zones" aria-labelledby="infra-zones-title">
                {structureOpen && (
                <div className="infra-section-line infra-zones-head">
                  {/* Имя пространства здесь было четвёртым повтором подряд —
                      оно уже стоит в строке контекста над лентой. */}
                  {/* Что такое зона, объясняем только там, где зоны есть: пока
                      их нет, ровно те же примеры стоят на карточке «Создать
                      первую зону» строкой ниже. */}
                  <span id="infra-zones-title">Зоны {visibleZoneStats.length > 0 && <small>{t("infra.zones.hint")}</small>}</span>
                  {activeWorkspace.id !== SHARED_WORKSPACE_ID && canAdmin(activeWorkspace.role) && (
                    <button onClick={() => setZoneEditor("new")}>+ Зона</button>
                  )}
                </div>
                )}
                {structureOpen && (
                <div className="infra-zone-rail" role="tablist" aria-label="Фильтр по зонам">
                  {/* «Все устройства» — сброс фильтра по зонам. Пока на ленте
                      нет ни одной зоны, сбрасывать нечего: карточка повторяла
                      список машин целиком и печатала его счётчик третий раз
                      подряд. */}
                  {visibleZoneStats.length > 0 && (
                  <article className={`infra-zone-card all${zoneFilter === "" ? " active" : ""}`}>
                    <button className="infra-zone-main" role="tab" aria-selected={zoneFilter === ""} onClick={() => switchZone("")}>
                      <span className="infra-zone-icon" aria-hidden>⌘</span>
                      {/* Счётчик тут был третьим отпечатком одного и того же
                          числа: «Все устройства» — это отсутствие фильтра, и
                          сколько их всего, написано в заголовке списка. Числа
                          остаются у самих зон — их иначе не сравнить. */}
                      <span><b>Все устройства</b></span>
                    </button>
                  </article>
                  )}
                  {visibleZoneStats.map(({ zone, total, online }) => (
                    <article className={`infra-zone-card${zoneFilter === zone.id ? " active" : ""}`} key={zone.id}>
                      <button className="infra-zone-main" role="tab" aria-selected={zoneFilter === zone.id} onClick={() => switchZone(zone.id)}>
                        <span className="infra-zone-icon" aria-hidden>⌖</span>
                        <span><b>{zone.name}</b><small>{online} в сети · {total} всего</small></span>
                        <em>{total}</em>
                      </button>
                      {canAdmin(activeWorkspace.role) && (
                        <button className="infra-zone-edit" aria-label={`Настроить зону ${zone.name}`} onClick={() => setZoneEditor(zone)}>•••</button>
                      )}
                    </article>
                  ))}
                  {/* Пока ни в одной зоне нет машин, «Без зоны» — это те же
                      самые «Все устройства»: две карточки с одинаковыми числами
                      читаются как непонятный включённый фильтр. Поэтому считаем
                      НЕПУСТЫЕ зоны: только когда часть машин разложена, вторая
                      часть становится отдельным ответом. */}
                  {/* …и когда все машины разложены по зонам, «Без зоны» — пустой
                      фильтр с двумя нулями («0 в сети», «0»). Показывать его
                      незачем: выбрать там нечего, а нули на экране читаются как
                      «что-то потерялось». */}
                  {populatedZoneCount > 0 && unassignedDevices.length > 0 && (
                    <article className={`infra-zone-card unassigned${zoneFilter === UNASSIGNED_ZONE_ID ? " active" : ""}`}>
                      <button className="infra-zone-main" role="tab" aria-selected={zoneFilter === UNASSIGNED_ZONE_ID} onClick={() => switchZone(UNASSIGNED_ZONE_ID)}>
                        <span className="infra-zone-icon" aria-hidden>○</span>
                        <span><b>Без зоны</b><small>{unassignedDevices.filter((device) => device.online).length} в сети</small></span>
                        <em>{unassignedDevices.length}</em>
                      </button>
                    </article>
                  )}
                  {activeWorkspace.id !== SHARED_WORKSPACE_ID && canAdmin(activeWorkspace.role) && zones.length === 0 && (
                    <button className="infra-zone-create-card" onClick={() => setZoneEditor("new")}><span>＋</span><b>Создать первую зону</b><small>Дом, работа, офис или дача</small></button>
                  )}
                </div>
                )}
                {/* Строка «что сейчас показано» нужна, только когда показано НЕ
                    всё: без включённого фильтра она печатала то же число, что
                    заголовок списка, и называла его «Все зоны» при нулях зон. */}
                {filtersActive && (
                <div className="infra-zone-context" role="status">
                  <span aria-hidden>{zoneFilter ? "⌖" : "⌘"}</span>
                  <b>{activeZone ? activeZone.name : zoneFilter === UNASSIGNED_ZONE_ID ? "Без зоны" : "Все зоны"}</b>
                  <small>{`${deviceCountLabel(visibleDevices.length)} после всех фильтров`}</small>
                  {zoneFilter && <button onClick={() => switchZone("")}>Сбросить</button>}
                </div>
                )}
              </section>
            )}

            {!permanent && me && (
              <section className="infra-account-banner">
                <span>◎</span>
                <div><b>{t("infra.guestBanner.title")}</b><p>Сейчас это гостевой аккаунт. Подключите Telegram или другой способ входа.</p></div>
                <button disabled={tgBusy} onClick={() => void switchTelegram()}>{tgBusy ? "Открываю…" : "Подключить Telegram"}</button>
              </section>
            )}

            {/* Поиск, статус, теги и разделение на компьютеры/серверы — только
                когда список действительно велик или фильтр уже включён. */}
            {!showOnboarding && showFilters && (
            <section className="infra-toolbar">
              {((counters.computer > 0 && counters.server > 0) || tab !== "all") && (
              <div className="infra-tabs" role="tablist">
                {([
                  ["all", "Все", counters.all],
                  ["computer", "Компьютеры", counters.computer],
                  ["server", "Серверы", counters.server],
                ] as [DeviceTab, string, number][]).map(([key, label, count]) => (
                  <button key={key} className={tab === key ? "active" : ""} onClick={() => setTab(key)}>{label}<i>{count}</i></button>
                ))}
              </div>
              )}
              <div className="infra-search-row">
                <label className="infra-search"><span>⌕</span><input value={query} onChange={(event) => setQuery(event.target.value)} placeholder="Имя, хост, зона или тег" /></label>
                <select value={filter} onChange={(event) => setFilter(event.target.value as FilterMode)} aria-label="Состояние">
                  <option value="all">Любой статус</option>
                  <option value="online">Только в сети</option>
                  <option value="favorite">Избранные</option>
                </select>
                <select value={tagFilter} onChange={(event) => setTagFilter(event.target.value)} aria-label="Тег">
                  <option value="">Все теги</option>
                  {tags.map((tag) => <option key={tag.id} value={tag.id}>{tag.name}</option>)}
                </select>
              </div>
            </section>
            )}

            {selected.length > 0 && (
              <section className="infra-bulk">
                <b>{t("infra.bulk.selected", { count: selected.length, visible: visibleDevices.length })}</b>
                <select value={bulkAction} onChange={(event) => { setBulkAction(event.target.value as typeof bulkAction); setBulkValue(event.target.value === "favorite" ? "true" : ""); }}>
                  <option value="favorite">Избранное</option>
                  <option value="device_type">Тип устройства</option>
                  <option value="zone">Зона</option>
                  <option value="move">Перенести</option>
                  <option value="tags">Установить тег</option>
                </select>
                {bulkAction === "favorite" && <select value={bulkValue} onChange={(event) => setBulkValue(event.target.value)}><option value="true">Добавить</option><option value="false">Убрать</option></select>}
                {bulkAction === "device_type" && <select value={bulkValue} onChange={(event) => setBulkValue(event.target.value)}><option value="">Выберите тип</option><option value="computer">Компьютер</option><option value="server">Сервер</option></select>}
                {bulkAction === "zone" && <select value={bulkValue} onChange={(event) => setBulkValue(event.target.value)}><option value="">Без зоны</option>{zones.map((zone) => <option key={zone.id} value={zone.id}>{zone.name}</option>)}</select>}
                {bulkAction === "move" && <select value={bulkValue} onChange={(event) => setBulkValue(event.target.value)}><option value="">{t("infra.bulk.moveSelect")}</option>{workspaces.filter((workspace) => canAdmin(workspace.role)).map((workspace) => <option key={workspace.id} value={workspace.id}>{workspace.name}</option>)}</select>}
                {bulkAction === "tags" && <select value={bulkValue} onChange={(event) => setBulkValue(event.target.value)}><option value="">Очистить теги</option>{tags.map((tag) => <option key={tag.id} value={tag.id}>{tag.name}</option>)}</select>}
                <button className="btn btn-primary btn-sm" disabled={bulkBusy || ((bulkAction === "device_type" || bulkAction === "move") && !bulkValue)} onClick={() => void applyBulk()}>{bulkBusy ? "Применяю…" : "Применить"}</button>
                <button className="infra-bulk-clear" onClick={() => setSelected([])}>Отмена</button>
                {bulkAction === "move" && <p className="infra-bulk-warn">⚠ {t("infra.bulk.moveWarning")}</p>}
              </section>
            )}

            {!showOnboarding && (
            <section className="infra-inventory">
              {/* Первая половина ответа «кто подключён»: машины аккаунта. Раньше
                  список начинался безымянным счётчиком «2 устройства», и понять,
                  что это и есть все подключённые компьютеры, было неоткуда. */}
              <div className="infra-section-line">
                <span>{t("infra.connected.devices")}</span>
                {/* ЕДИНСТВЕННЫЙ счётчик машин на экране — и тот молчит, когда
                    машина одна: её состояние написано словом на карточке, а
                    «1 из 1 в сети» только повторяет это цифрами. */}
                {visibleDevices.length > 1 && (
                  <span>{t("infra.connected.devicesCount", {
                    online: visibleOnlineCount,
                    total: visibleDevices.length,
                  })}</span>
                )}
              </div>
              {/* Что делает тап — словами и один раз. Карточка сама этого не
                  скажет: имя машины со строкой состояния одинаково похоже и на
                  «переключиться», и на «открыть подробности». */}
              {visibleDevices.length > 1 && (
                <p className="infra-tap-hint">{t("infra.card.tapHint")}</p>
              )}
              {/* Машина подключилась — она уже в списке. Управление переключаем
                  не молча: у человека мог идти агент на прежней машине. */}
              {addedDevice && (
                <section className="infra-just-added">
                  <span aria-hidden>✓</span>
                  <div>
                    <b>{t("infra.added.title", { name: addedDevice.name })}</b>
                    <p>{t("infra.added.text")}</p>
                  </div>
                  <button className="primary" onClick={() => switchToDevice(addedDevice)}>
                    {addedDevice.device_type === "server" ? t("infra.added.manageServer") : t("infra.added.manage")}
                  </button>
                  <button onClick={() => setJustAdded(null)}>{t("infra.added.later")}</button>
                </section>
              )}
              {/* Групповые операции — только там, где группа возможна: с одной
                  машиной чекбокс и подсказка ничего не дают. */}
              {visibleDevices.length > 1 && (
                /* Счётчик переехал в заголовок секции выше — здесь остаётся
                   только то, ради чего строка нужна: выбор нескольких машин. */
                <div className="infra-inventory-head">
                  <label><input type="checkbox" checked={visibleDevices.every((device) => selected.includes(device.id))} onChange={(event) => setSelected(event.target.checked ? visibleDevices.map((device) => device.id) : [])} /> <span>{t("infra.bulk.selectAll")}</span></label>
                  <span>Выберите несколько для группового управления</span>
                </div>
              )}

              {visibleDevices.length === 0 ? (
                <div className="infra-empty">
                  <span>{devices.length === 0 ? "▣" : activeZone ? "⌖" : "⌕"}</span>
                  <h2>
                    {devices.length === 0
                      ? "Подключите первое устройство"
                      : activeZone
                        ? `В зоне «${activeZone.name}» пока пусто`
                        : zoneFilter === UNASSIGNED_ZONE_ID
                          ? "Все устройства распределены по зонам"
                          : "Ничего не найдено"}
                  </h2>
                  <p>
                    {devices.length === 0
                      ? "Установите Remotai на компьютер или сервер и введите код подключения."
                      : activeZone
                        ? "Подключите новое устройство — эта зона уже выбрана. Существующее можно перенести через его настройки."
                        : zoneFilter === UNASSIGNED_ZONE_ID
                          ? "Откройте любую зону или сбросьте фильтр."
                          : "Измените поиск или фильтры."}
                  </p>
                  {activeWorkspace && canAdmin(activeWorkspace.role) && (devices.length === 0 || !!activeZone) && (
                    <button className="btn btn-primary" onClick={() => openAddDevice()}>{activeZone ? `${t("infra.addComputer")} в «${activeZone.name}»` : t("infra.addComputer")}</button>
                  )}
                </div>
              ) : (
                <div className="infra-device-grid">
                  {visibleDevices.map((device) => {
                    const summary = summaries[device.id];
                    const checked = selected.includes(device.id);
                    const current = device.id === selectedDeviceId;
                    /* Имя и hostname почти всегда одно и то же слово (агент
                       подставляет hostname именем) — на карточке оно печаталось
                       дважды подряд. Вторая строка живёт, только когда она
                       что-то ДОБАВЛЯЕТ. */
                    const hostname = (device.hostname || "").trim();
                    const title = (device.name || "").trim() || hostname || device.id;
                    const nameBlock = (
                      <span>
                        <b>{title}</b>
                        {hostname && hostname.toLocaleLowerCase("ru") !== title.toLocaleLowerCase("ru") && (
                          <small>{hostname}</small>
                        )}
                      </span>
                    );
                    const update = agentUpdateState(device, latestVersion);
                    const agentVersion = (device.agent_version || "").trim();
                    /* Плитки состояния: пустая ячейка сетки рисовалась рамкой и
                       читалась как «данные не пришли». Сетка идёт по числу
                       метрик, которые реально есть. */
                    const metrics = [
                      /* «1 терминалов» на карточке единственной машины читалось
                         как ошибка программы — склоняем тем же общим
                         склонятором, что и счётчик устройств. */
                      summary ? <span key="pty"><b>{summary.terminalCount}</b> {plural(summary.terminalCount, ["терминал", "терминала", "терминалов"])}</span> : null,
                      summary && Number.isFinite(summary.cpuPercent) ? <span key="cpu"><b>{Math.round(summary.cpuPercent!)}%</b> CPU</span> : null,
                      summary && Number.isFinite(summary.memoryPercent) ? <span key="ram"><b>{Math.round(summary.memoryPercent!)}%</b> RAM</span> : null,
                      /* И «1 ждут ответа» — та же беда: склоняем сам глагол. */
                      summary && summary.waitingCount > 0 ? <span key="wait" className="attention"><b>{summary.waitingCount}</b> {plural(summary.waitingCount, ["ждёт ответа", "ждут ответа", "ждут ответа"])}</span> : null,
                    ];
                    const metricCount = metrics.filter(Boolean).length;
                    return (
                      <article id={`infra-device-${device.id}`} key={device.id} className={`infra-device${current ? " current" : ""}${checked ? " checked" : ""}`}>
                        <div className={`infra-device-top${visibleDevices.length > 1 ? "" : " no-select"}`}>
                          {/* Флажок — часть группового действия: с единственной
                              машиной группы нет, и он только занимал место
                              рядом с её именем (колонку под него снимает
                              модификатор no-select). */}
                          {visibleDevices.length > 1 && (
                            <label className="infra-select-device"><input type="checkbox" checked={checked} onChange={() => setSelected((current) => current.includes(device.id) ? current.filter((id) => id !== device.id) : [...current, device.id])} /></label>
                          )}
                          {/* Управляемая машина — не кнопка: нажимать «я и так
                              здесь» нечего, а disabled-кнопка читалась бы как
                              «машина недоступна» (та же логика, что у плитки
                              «Управляете сейчас» на главной). */}
                          {current ? (
                            <div className="infra-device-open as-current" aria-current="true">
                              <span className={`infra-device-icon ${device.device_type}`}>{deviceMark(device.platform, device.device_type)}</span>
                              {nameBlock}
                            </div>
                          ) : (
                            <button className="infra-device-open" onClick={() => switchToDevice(device)}>
                              <span className={`infra-device-icon ${device.device_type}`}>{deviceMark(device.platform, device.device_type)}</span>
                              {nameBlock}
                            </button>
                          )}
                          {/* Всё, что можно сделать С машиной (терминалы,
                              файлы, экран, избранное, переименовать, зона,
                              удалить), уехало в «⋯»: рядом с тапом-переключением
                              им не место — иначе один жест значит два разных
                              действия в зависимости от того, куда попал палец. */}
                          <button className="infra-more" aria-label={t("infra.card.menu", { name: device.name })} onClick={() => { haptic(); setMenuDevice(device); }}>•••</button>
                        </div>
                        <div className="infra-device-status">
                          {/* «Не в сети · никогда» — это не то же самое, что
                              «выключен»: машину привязали, но её агент ни разу
                              не выходил на связь. У сервера причина почти всегда
                              одна — служба не запущена (после `remotai pair`
                              нужен ещё `systemctl enable --now remotai`), и
                              сказать об этом надо здесь, а не оставлять человека
                              с вопросом «почему сервер, что я подключил, не в
                              сети». */}
                          <span className={device.online ? "online" : "offline"}>
                            <i />
                            {device.online
                              ? "В сети"
                              : device.last_seen_at
                                ? `Не в сети · ${timeAgo(device.last_seen_at)}`
                                : t("infra.neverOnline")}
                          </span>
                          <em>{TYPE_LABEL[device.device_type]}</em>
                          {/* Версия агента: часть возможностей релей гейтит по
                              ней, и без этой строки старая машина выглядит как
                              «функции пропали». Отстала — та же плитка говорит,
                              ЧТО БУДЕТ, вместо тревожного «Агент устарел» рядом
                              с числом версии (одно число — один раз). */}
                          {update === "auto" ? (
                            <em className="updating">{t("infra.agent.auto", { version: agentVersion, latest: latestVersion })}</em>
                          ) : update === "stuck" ? (
                            <em className="stale">{t("infra.agent.stuck", { version: agentVersion, latest: latestVersion })}</em>
                          ) : (
                            <em>{agentVersionLabel(device)}</em>
                          )}
                          {device.workspace_role === "viewer" && <em className="viewer">Только просмотр</em>}
                        </div>
                        {/* Машина привязана, но ни разу не выходила на связь —
                            называем самую частую причину и даём команду, а не
                            оставляем человека гадать. */}
                        {!device.online && !device.last_seen_at && (
                          <div className="infra-never-hint">
                            {t(device.device_type === "server" ? "infra.neverOnlineServer" : "infra.neverOnlinePc")}
                            {device.device_type === "server" && (
                              <code className="infra-never-cmd">{REMOTAI_SERVICE_START_COMMAND}</code>
                            )}
                          </div>
                        )}
                        {(deviceZone(device) || device.tags.length > 0) && (
                          <div className="infra-device-labels">
                            {deviceZone(device) && <span className="zone">⌖ {deviceZone(device)!.name}</span>}
                            {device.tags.map((tag) => <span key={tag.id}><i style={{ background: tag.color }} />{tag.name}</span>)}
                          </div>
                        )}
                        {metricCount > 0 && (
                          <div className={`infra-runtime cols-${metricCount}`}>{metrics}</div>
                        )}
                        {/* Единственное место, где версия говорит тревожно, —
                            и здесь же лежит действие: без него человек видит
                            беду и не может ничего сделать. */}
                        {update === "stuck" && (
                          <div className="infra-agent-stuck">
                            <span>{t("infra.agent.stuckNote", { version: agentVersion, latest: latestVersion })}</span>
                            <button onClick={() => openOnDevice(device, "/pty")}>{t("infra.agent.stuckAction")}</button>
                          </div>
                        )}
                        {device.workspace_role === "viewer" && (
                          <div className="infra-device-footnote">{t("infra.menu.viewer")}</div>
                        )}
                        {/* Офлайн-машину переключать МОЖНО: человек готовится к
                            её включению. Но врать, что всё заработает сразу,
                            нельзя — говорим прямо на карточке. */}
                        {!device.online && !current && (
                          <div className="infra-device-footnote">{t("infra.card.offlineNote")}</div>
                        )}
                        {current ? (
                          <div className="infra-device-switch current">
                            <span aria-hidden>✓</span>{t("devices.switcher.current")}
                          </div>
                        ) : (
                          <button className="infra-device-switch" onClick={() => switchToDevice(device)}>
                            {!device.online
                              ? t("infra.card.switchOffline")
                              : device.device_type === "server"
                                ? t("infra.card.switchServer")
                                : t("infra.card.switchComputer")}
                          </button>
                        )}
                      </article>
                    );
                  })}
                </div>
              )}

              {/* Своих машин нет, а расшаренные есть. Человек видит свой же
                  компьютер в «общем доступе» и пустое «Личное» — объяснение
                  стоит прямо под списком, а не спрятано в справке. Без
                  обвинений: аккаунты просто разные. */}
              {sharedOnly && visibleDevices.length > 0 && (
                <section className="infra-shared-note">
                  <span aria-hidden>◎</span>
                  <div>
                    <b>{t("infra.sharedOnly.title")}</b>
                    <p>{t("infra.sharedOnly.text")}</p>
                  </div>
                  {ownHome && (
                    <button onClick={addOwnDevice}>{t("infra.sharedOnly.add")}</button>
                  )}
                </section>
              )}
            </section>
            )}

            {/* Продолжение того же списка: серверы по SSH. Владелец попросил
                видеть их рядом с машинами и открывать тапом — «нажимаю на
                сервер, и на сервере тоже могу создавать терминалы». Разница
                сущностей не спрятана: подзаголовок говорит, через какой
                компьютер они работают и чего у них нет. */}
            {!showOnboarding && selectedDeviceId && (
              <SshServersSection
                viaName={currentDevice?.name || currentDevice?.hostname || t("devices.chipThisPc")}
                deviceId={selectedDeviceId}
                offlineVia={!!currentDevice && !currentDevice.online}
              />
            )}

            {/* Слой компаний и зон свёрнут (одно пространство, зон нет) — вход в
                него остаётся одной строкой, иначе завести компанию или зону
                стало бы негде. */}
            {!showOnboarding && simpleFleet && !structureOpen && activeWorkspace && (
              <div className="infra-structure-more">
                <button onClick={() => { haptic(); setStructureOpen(true); }}>{t("infra.structure.show")}</button>
              </div>
            )}

            {/* Карточка аккаунта — ОДНА дверь в Настройки, а не второй орган
                управления аккаунтом (аудит ИА 02.09.2026, P1-20/V11/P1-6).
                Раньше здесь стоял список входов с «Завершить» и ряд кнопок
                «Агенты · Безопасность · Сменить Telegram · Выйти» — всё это
                дословно повторяло Настройки, и одно и то же действие жило в
                двух местах. «Агенты» на этом экране зовут из шапки
                (infra-header-usage); «Подключить Telegram» для гостя — баннер
                выше. focus: "logins" — чтобы Настройки открылись на входах. */}
            {me && (
              <section className="infra-account">
                <span className="infra-avatar">{accountName(me).slice(0, 1).toUpperCase()}</span>
                <span><b>{permanent ? accountName(me) : "Гостевой аккаунт"}</b><small>{t("infra.account.securityHint")}</small></span>
                <button onClick={() => navigate("/settings", { state: { focus: "logins" } })}>{t("infra.account.manage")}</button>
              </section>
            )}
          </>
        )}
      </div>

      {addOpen && activeWorkspace && (
        <SheetShell
          open={addOpen}
          onClose={() => setAddOpen(false)}
          overlayClassName="infra-backdrop"
          className="infra-sheet infra-add-sheet"
          labelledBy="infra-add-device"
        >
            <div className="infra-sheet-handle" aria-hidden />
            <div className="infra-sheet-head">
              <div><div className="infra-eyebrow">{activeWorkspace.name}</div><h2 id="infra-add-device">{addType === "server" ? "Подключить сервер" : "Подключить компьютер"}</h2></div>
              <button className="infra-close" aria-label="Закрыть" onClick={() => setAddOpen(false)}>×</button>
            </div>
            {/* Лимит виден до установки агента: раньше про исчерпанную квоту
                человек узнавал из отказа, уже поставив Remotai на третий ПК. */}
            {quotaBlock}
            <div className="infra-field">
              <span>Что подключаем?</span>
              <div className="infra-type-cards">
                <button className={addType === "computer" ? "active" : ""} onClick={() => setAddType("computer")}><i>▣</i><b>Компьютер</b><small>Windows, macOS или Linux</small></button>
                <button className={addType === "server" ? "active" : ""} onClick={() => setAddType("server")}><i>▰</i><b>Сервер</b><small>Linux, VPS или машина без монитора</small></button>
              </div>
            </div>
            <details>
              <summary>Размещение по зонам (необязательно)</summary>
            <label className="infra-field">
              <span>Куда добавить?</span>
              <select value={addZoneId} onChange={(event) => setAddZoneId(event.target.value)}>
                <option value="">Без зоны</option>
                {zones.map((zone) => <option key={zone.id} value={zone.id}>{zone.name}</option>)}
              </select>
              <small>Зону можно изменить позже в настройках устройства.</small>
            </label>
            {zones.length === 0 && activeWorkspace.id !== SHARED_WORKSPACE_ID && (
              <button className="infra-inline-zone-create" onClick={() => { setAddOpen(false); setZoneEditor("new"); }}>＋ Сначала создать зону</button>
            )}
            </details>
            {addType === "server" && (
              <div className="infra-install-command">
                <span><b>Установка на Linux одной командой</b><small>Вставьте в SSH-терминал сервера. После установки там появится код подключения.</small></span>
                <code>{REMOTAI_SERVER_INSTALL_COMMAND}</code>
                <button onClick={() => void copyServerInstall()}>Скопировать</button>
              </div>
            )}
            {/* На телефоне .exe скачался бы В телефон: там нужен адрес, который
                человек откроет на самом компьютере. */}
            {addType === "computer" && !fromInstalledApp && <details>
              <summary>Remotai ещё не установлен?</summary>
              {mobileSurface ? openOnPcBlock : <InstallTarget />}
            </details>}
            {fromInstalledApp ? <p>Код из установленного Remotai уже подставлен. Нажмите «Подключить», чтобы добавить компьютер в «{activeWorkspace.name}»{me ? ` (аккаунт: ${accountName(me)})` : ""}.</p> : <ol className="infra-pair-steps">
              <li><b>1</b><span>Откройте Remotai на {addType === "server" ? "сервере" : "компьютере, которым будете управлять"}. Если приложение уже установлено, скачивать его повторно не нужно.</span></li>
              <li><b>2</b><span>Введите здесь код с той машины — она появится в «{activeWorkspace.name}»{addZoneId ? `, зона «${zones.find((zone) => zone.id === addZoneId)?.name || "выбранная"}»` : ""}.</span></li>
              <li><b>3</b><span>Нажмите «Управление» у добавленной машины. Её имя будет видно вверху приложения.</span></li>
            </ol>}
            {!quotaReached && (
              /* Два способа ввести один и тот же код стоят рядом и оба
                 заканчиваются подключением ЗДЕСЬ: камера открывается шторкой
                 поверх этой же, маршрут не меняется. */
              <div className="infra-add-ways">
                <div className="device-pair-form infra-pair-form">
                  <input className="device-pair-input" value={pairCode} onChange={(event) => setPairCode(event.target.value.toUpperCase().slice(0, 9))} onKeyDown={(event) => { if (event.key === "Enter") void pair(); }} placeholder="FX42-9KQ7" autoCapitalize="characters" autoCorrect="off" spellCheck={false} aria-label="Код подключения" />
                  <button className="btn btn-primary" disabled={pairBusy || pairCode.replace("-", "").length < 8} onClick={() => void pair()}>{pairBusy ? "Подключаю…" : "Подключить"}</button>
                </div>
                <button className="btn btn-secondary infra-full" onClick={scan}>{t("infra.add.scanBtn")}</button>
                <small className="infra-add-scan-note">{t("infra.add.scanNote")}</small>
              </div>
            )}
        </SheetShell>
      )}

      {/* Сканер — поверх шторки добавления, а не вместо экрана. Закрывает его
          вызывающий: успех гасит обе шторки в pair(), ошибка возвращается
          подписью под видоискателем. */}
      <QrScanSheet
        open={scanOpen}
        onClose={() => setScanOpen(false)}
        hint={scanHint || t("infra.scan.hint")}
        onCode={(code, match) => {
          if (match.kind !== "cloud") {
            // QR режима «по локальной сети» аккаунту не подходит: там нет кода,
            // который принимает релей. Молча игнорировать нельзя — человек
            // будет водить камерой по тому же QR.
            setScanHint(t("infra.scan.lan"));
            return;
          }
          void pair(code);
        }}
      />

      {menuDevice && (
        <DeviceActionsSheet
          device={menuDevice}
          isCurrent={menuDevice.id === selectedDeviceId}
          hasScreen={summaries[menuDevice.id]?.hasDisplay !== false}
          onClose={() => setMenuDevice(null)}
          onSwitch={() => switchToDevice(menuDevice)}
          onOpen={(path) => openOnDevice(menuDevice, path)}
          onFavorite={() => void toggleFavorite(menuDevice)}
          onSettings={() => setEditDevice(menuDevice)}
        />
      )}
      {editDevice && <DeviceEditSheet device={editDevice} workspaces={workspaces} zones={zones} tags={tags} latestVersion={latestVersion} onClose={() => setEditDevice(null)} onChanged={refreshInfrastructure} />}
      {manageWorkspace && <WorkspaceManageSheet workspace={manageWorkspace} devices={devices || []} onClose={() => setManageWorkspace(null)} onChanged={refreshInfrastructure} />}
      {zoneEditor && activeWorkspace && activeWorkspace.id !== SHARED_WORKSPACE_ID && (
        <ZoneEditSheet
          workspace={activeWorkspace}
          zone={zoneEditor === "new" ? null : zoneEditor}
          existingZones={zones}
          deviceCount={zoneEditor === "new" ? 0 : activeDevices.filter((device) => deviceZone(device)?.id === zoneEditor.id).length}
          onClose={() => setZoneEditor(null)}
          onChanged={refreshInfrastructure}
          onSelect={switchZone}
        />
      )}
      <BottomNav active="devices" />
    </div>
  );
}
