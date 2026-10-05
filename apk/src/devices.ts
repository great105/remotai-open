/**
 * Переключение управляемой машины.
 *
 * Смена ПК — это смена ВСЕГО контекста: REST-запросы, live-события и
 * возможности машины (есть ли дисплей, поднимается ли виртуальный браузер).
 * Раньше каждый вызывающий делал только saveConfig, поэтому после выбора другого
 * компьютера телефон продолжал слушать сокет прежней машины, а capabilities
 * оставались закэшированными от неё: вкладка «Экран» могла быть видна на
 * headless-сервере и наоборот.
 */
import { t } from "@tgcontrol/shared";
import { saveConfig, getMode, getSelectedDeviceId, getSelectedDeviceAgentVersion } from "./config";
import { resetCapabilities } from "./capabilities";
import { disconnectWS, reconnectWS } from "./api";

/**
 * Подписка на смену управляемой машины.
 *
 * Зачем: конфиг и канал переключаются мгновенно, а ДАННЫЕ прежней машины
 * остаются на экране до первого ответа новой (реконнект 0,5–2 с). Живая сводка
 * главной успевала показать карточки вопросов СТАРОЙ машины уже под новым
 * именем, причём с активными кнопками «Да»/«Нет»: нажатие уходило на новую
 * машину с чужим id терминала и возвращало «Терминал не найден» — сообщение,
 * по которому невозможно догадаться, что произошло.
 */
type DeviceChangeListener = (deviceId: string) => void;
const deviceListeners = new Set<DeviceChangeListener>();

export function onSelectedDeviceChange(fn: DeviceChangeListener): () => void {
  deviceListeners.add(fn);
  return () => { deviceListeners.delete(fn); };
}

function emitDeviceChange(deviceId: string): void {
  for (const fn of deviceListeners) fn(deviceId);
}

/** Выбрать облачный ПК: конфиг → сброс возможностей → пересоздание WS. */
export function selectDevice(
  deviceId: string,
  device?: {
    name?: string;
    hostname?: string;
    platform?: string;
    device_type?: "computer" | "server";
    workspace_id?: string;
    workspace_name?: string;
    workspace_role?: "owner" | "admin" | "operator" | "viewer";
    // Версия агента выбранной машины — плашке обновления на главной и в
    // терминале сравнивать её нужно с манифестом релея, а список устройств
    // они не грузят.
    agent_version?: string | null;
  },
): void {
  if (!deviceId) return;
  const prev = getSelectedDeviceId();
  // Версию НЕ обнуляем, если это та же машина, а объект устройства не передали:
  // saveConfig раскладывает поля спредом, поэтому явный undefined затирал уже
  // известную версию. А от неё зависит, шлём ли мы агенту `resume` — то есть
  // одно «выбрать эту же машину» без объекта стоило человеку истории на экране
  // при каждом следующем возврате в терминал (см. getSelectedDeviceAgentVersion).
  // При переходе на ДРУГУЮ машину сброс правилен: версия там своя.
  const sameDevice = prev === deviceId;
  saveConfig({
    mode: "cloud",
    selectedDeviceId: deviceId,
    selectedDeviceName: device?.name || device?.hostname || undefined,
    selectedDevicePlatform: device?.platform || undefined,
    selectedDeviceType: device?.device_type || undefined,
    selectedDeviceAgentVersion: device?.agent_version
      || (sameDevice ? getSelectedDeviceAgentVersion() || undefined : undefined),
    selectedWorkspaceId: device?.workspace_id || undefined,
    selectedWorkspaceName: device?.workspace_name || undefined,
    selectedWorkspaceRole: device?.workspace_role || undefined,
  });
  if (prev !== deviceId) {
    resetCapabilities();
    reconnectWS();
    // Слушателей зовём ПОСЛЕ сброса возможностей и канала: подписчик очищает
    // свои данные и уходит в скелетон, а первый ответ уже придёт от новой
    // машины.
    emitDeviceChange(deviceId);
  }
}

/**
 * Забыть управляемую машину: её больше нет в аккаунте (удалили запись, отозвали
 * доступ). Без этого сброса чип в шапке продолжал показывать удалённый ПК, а
 * рабочие экраны — стучаться к несуществующему устройству. После вызова
 * DeviceGuard сам вернёт человека к выбору машины.
 */
export function clearSelectedDevice(): void {
  // Понятие «выбранная машина» есть только в облаке: в LAN-режиме адрес ПК
  // задан в конфиге, и сбрасывать там нечего (а saveConfig проверял бы url).
  if (getMode() !== "cloud" || !getSelectedDeviceId()) return;
  saveConfig({
    selectedDeviceId: undefined,
    selectedDeviceName: undefined,
    selectedDevicePlatform: undefined,
    selectedDeviceType: undefined,
    selectedDeviceAgentVersion: undefined,
  });
  resetCapabilities();
  disconnectWS();
  emitDeviceChange("");
}

/**
 * IPv4 целиком: «192.168.1.5», «127.0.0.1». Точность до октета не нужна —
 * имени такого вида у компьютера не бывает, и ошибиться тут нечем.
 */
const IPV4_RE = /^\d{1,3}(?:\.\d{1,3}){3}$/;

/** IPv6-литерал в любом виде: «::1», «fe80::1», «2001:db8::42». */
const IPV6_RE = /^[0-9a-f]{0,4}(?::[0-9a-f]{0,4}){2,7}$/i;

/**
 * Хост из того, что пришло вместо имени.
 *
 * Порт и скобки IPv6 снимаем ДО любого сравнения: «127.0.0.1:59999» и
 * «[::1]:8080» — тот же адрес. Одно двоеточие считаем портом, несколько — IPv6
 * без скобок, поэтому машина по имени «mypc:8080» именем и останется.
 */
function hostOf(raw: string): string {
  let host = raw;
  const bracketed = /^\[(.+?)\](?::\d+)?$/.exec(host);
  if (bracketed) {
    host = bracketed[1];
  } else {
    const colon = host.lastIndexOf(":");
    if (colon > 0 && host.indexOf(":") === colon && /^\d+$/.test(host.slice(colon + 1))) {
      host = host.slice(0, colon);
    }
  }
  return host.trim().toLowerCase();
}

/** Похоже ли «имя» на сетевой адрес, а не на имя машины. */
function looksLikeAddress(raw: string): boolean {
  const host = hostOf(raw);
  return host === "localhost" || IPV4_RE.test(host) || IPV6_RE.test(host);
}

/**
 * Адрес ведёт на ЭТУ ЖЕ машину — loopback.
 *
 * Отдельный предикат нужен там, где адрес всё-таки печатают: в LAN-режиме
 * «192.168.1.5:8080» под именем машины хотя бы отвечает, по какому адресу до
 * неё дошли, а «127.0.0.1:59999» не отвечает ни на что — это адрес самого себя,
 * и человеку про него сказать нечего (он и так сидит за этим компьютером).
 * Принимаем и целый URL, и один хост: адрес подключения хранится в конфиге
 * строкой «http://127.0.0.1:8080».
 */
export function isLocalAddress(value: string | null | undefined): boolean {
  const raw = (value || "").trim();
  if (!raw) return false;
  const host = hostOf(raw.replace(/^[a-z][a-z\d+.-]*:\/\//i, "").split("/")[0]);
  if (host === "localhost" || host === "::1" || host === "0:0:0:0:0:0:0:1") return true;
  // Loopback — вся сеть 127.0.0.0/8, а не один адрес: агент на Windows отвечает
  // и по 127.0.0.1, и по адресу вида 127.0.1.1 из hosts-файла.
  return IPV4_RE.test(host) && host.startsWith("127.");
}

/**
 * Имя машины так, как его можно показать человеку.
 *
 * В LAN-режиме «имя выбранной машины» берётся из адреса подключения
 * (`getSelectedDeviceName` → `new URL(url).hostname`), поэтому компьютер звали
 * его адресом: «127.0.0.1» стояло в шапке всех рабочих экранов и на плитках
 * главной. Адрес именем не является — «192.168.1.5 на связи» отвечает хуже,
 * чем «Компьютер на связи».
 *
 * Фильтр существовал ровно в одном месте — в заголовке карточки готовности — и
 * до остальных экранов не доехал. Поэтому он живёт здесь, рядом с самим
 * понятием «выбранная машина», и зовут его: `ReadyCard` (заголовок «… на
 * связи»), `DeviceChip` (шапка рабочих экранов) и `DeviceSwitcher` (плитки,
 * тост о переключении и кнопка возврата).
 *
 * `fallback` — чем назвать машину, у которой имени нет вовсе или вместо имени
 * стоит адрес. По умолчанию это «Этот компьютер», но экрану виднее: в облаке
 * выбранная машина может быть чужой, а безымянная запись в списке зовётся «без
 * имени». Пустой `fallback` означает «назвать нечем» — вызывающий сам решит,
 * что рисовать вместо имени.
 */
export function humanDeviceName(name: string | null | undefined, fallback: string = t("devices.chipThisPc")): string {
  const raw = (name || "").trim();
  if (!raw || looksLikeAddress(raw)) return fallback;
  return raw;
}
