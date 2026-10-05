/**
 * Непрочитанные ответы поддержки — ЕДИНЫЙ источник для бейджа в настройках,
 * точки в нижней навигации и локального уведомления APK.
 *
 * Зачем модуль, а не useState в настройках: раньше счётчик тянулся ровно один
 * раз — в useEffect экрана настроек, и жил в его локальном состоянии. За
 * пределами настроек цифры не было нигде, поэтому узнать об ответе поддержки
 * можно было только случайно туда зайдя (аудит #77/#111).
 *
 * Анти-шторм (грабля 2.28.1 «6549 запросов за 6 секунд»):
 *  - сеть трогает только refreshSupportUnread, и он сам гасит частые вызовы
 *    (`MIN_FETCH_INTERVAL_MS`) и параллельные (`inFlight`) — сколько бы экранов
 *    ни подписалось, запрос уходит один;
 *  - подписчики получают уже готовое число, без собственных запросов;
 *  - интервал заводит ТОЛЬКО usePolling в useSupportUnread (гейт по видимости).
 */
import { getSupportUnread } from "./cloud/support";
import { getCloudJWT } from "./config";
import * as notifications from "./notifications";

/** Сколько непрочитанных пользователь уже видел (переживает перезапуск). */
const SEEN_KEY = "tg.support.seen.v1";

/** Ближе этого к прошлому запросу в сеть не ходим (навигация = ремаунт хука). */
const MIN_FETCH_INTERVAL_MS = 10_000;

let count = 0;
let inFlight: Promise<void> | null = null;
let lastFetch = 0;
const listeners = new Set<(n: number) => void>();

function readSeen(): number {
  try {
    const n = parseInt(localStorage.getItem(SEEN_KEY) || "0", 10);
    return Number.isFinite(n) && n > 0 ? n : 0;
  } catch {
    return 0;
  }
}

/** Виденное значение. In-memory — источник правды в рамках запуска: при
 *  недоступном localStorage (приватный режим) иначе каждый такт поллинга
 *  считал бы ответ «новым» и звенел бы уведомлением раз в минуту. */
let seen = readSeen();

function setSeen(n: number): void {
  seen = n;
  try {
    localStorage.setItem(SEEN_KEY, String(n));
  } catch {
    /* приватный режим / quota — переживёт только текущий запуск */
  }
}

function emit(n: number): void {
  if (n === count) return;
  count = n;
  listeners.forEach((cb) => {
    try {
      cb(n);
    } catch {
      /* подписчик уже размонтирован — остальных это не касается */
    }
  });
}

/**
 * Есть облачная авторизация: подписанный initData Telegram либо user-JWT.
 *
 * Это НЕ то же самое, что «маршрут до компьютера — облачный». Гейт по режиму
 * (`getMode() === "cloud"`) уносил чат поддержки и бейдж непрочитанного, стоило
 * человеку переключиться на локальную сеть: транспорт чата от режима не зависит
 * (cloud/support.ts всегда ходит на релей), а saveConfig({mode}) мержит поверх
 * кэша и JWT не стирает — переписка была на месте, просто скрыта (N160/V14).
 */
export function cloudAccountAvailable(): boolean {
  const initData = typeof window !== "undefined"
    ? window.Telegram?.WebApp?.initData
    : undefined;
  return !!initData || !!getCloudJWT();
}

/** Чат поддержки живёт на релее: нужен облачный аккаунт, а не облачный маршрут. */
export function supportChatAvailable(): boolean {
  return cloudAccountAvailable();
}

/** Текущее известное число непрочитанных (без запроса в сеть). */
export function getUnread(): number {
  return count;
}

/** Подписка на изменения; сразу отдаёт текущее значение. Возвращает отписку. */
export function onUnread(cb: (n: number) => void): () => void {
  listeners.add(cb);
  cb(count);
  return () => {
    listeners.delete(cb);
  };
}

/** Локальное уведомление о новом ответе.
 *  notifications.ts — файл соседнего исполнителя (showSupportReply приезжает
 *  туда в этой же волне), поэтому обращаемся мягко: пока функции нет — или это
 *  веб/Telegram, где Capacitor-уведомлений не существует, — просто ничего не
 *  звенит, а бейдж и точка работают. */
function ring(n: number): void {
  const mod = notifications as unknown as {
    showSupportReply?: (count: number) => Promise<void>;
  };
  if (typeof mod.showSupportReply === "function") {
    void mod.showSupportReply(n).catch(() => { /* разрешение отозвано */ });
  }
}

async function fetchUnread(): Promise<void> {
  let n: number;
  try {
    n = (await getSupportUnread()).count | 0;
  } catch {
    return; // релей недоступен — бейдж не трогаем (не гасим и не зажигаем)
  }
  if (n < 0) n = 0;
  // Звеним только на ПРИРОСТ относительно виденного: холодный старт с уже
  // висящим непрочитанным не должен будить пользователя повторно.
  if (n > seen) ring(n);
  // Записываем и вниз тоже — иначе после прочтения счётчик «залипнет» на
  // старом максимуме и следующий ответ окажется «не новым».
  if (n !== seen) setSeen(n);
  emit(n);
}

/** Обновить счётчик. Стабильная ссылка — её и передаём в usePolling
 *  (колбэк из тела компонента там запрещён, см. докстринг usePolling). */
export function refreshSupportUnread(): Promise<void> {
  if (!supportChatAvailable()) return Promise.resolve();
  if (inFlight) return inFlight;
  if (Date.now() - lastFetch < MIN_FETCH_INTERVAL_MS) return Promise.resolve();
  lastFetch = Date.now();
  inFlight = fetchUnread().finally(() => {
    inFlight = null;
  });
  return inFlight;
}

/** Чат открыт — релей уже пометил всё прочитанным (MarkSupportReadUser в
 *  handleSupportList), гасим цифру локально, не дожидаясь следующего такта. */
export function clearSupportUnread(): void {
  // Идемпотентно: чат перечитывается каждые 10 с, лишних записей в
  // localStorage и оповещений подписчиков быть не должно.
  if (seen === 0 && count === 0) return;
  setSeen(0);
  emit(0);
}
