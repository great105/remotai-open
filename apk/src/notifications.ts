/**
 * Local notifications for AI agent completion events.
 *
 * Backend emits `{type: "notify", event, session, agent, summary, error}` over
 * WebSocket whenever a session finishes (success or error). We surface this as
 * an Android system notification — works in foreground and (when WebView is
 * still alive in recents) in background.
 *
 * Доигрывание пропущенного (#107). Докстринг раньше обещал реплей через
 * `?since=<lastEventId>` — в облаке его нет вовсе: релей события не хранит
 * (ws_client.go только пересылает живой поток), а `?since=` шлёт лишь LAN-ветка.
 * Поэтому телефон, проспавший ночь, узнавал о вопросе агента только когда
 * пользователь сам открывал приложение. Компенсируем добором ТЕКУЩЕГО
 * состояния терминалов на возврате в активное состояние — см.
 * `catchUpPtyEvents` внизу файла.
 */

import { LocalNotifications } from "@capacitor/local-notifications";
import { App as CapApp } from "@capacitor/app";
import { agentDisplayName, isAgentKind, formatDurationMs } from "@tgcontrol/shared";
import { listPtySessions } from "./api";
import { getSelectedDeviceName, isNativeApp } from "./config";
import { getFeatures } from "./features";

const CHANNEL_ID = "agent-events";
let permissionGranted: boolean | null = null;
// Дедуп legacy-событий сессий (showNotifyEvent). ГРАБЛЯ, общая с PTY-событиями:
// id — счётчик в памяти агента, после его перезапуска (в т.ч. штатного
// автообновления) он начинается заново с 1, и уже виденные id глушат новые
// события. Для PTY это исправлено латчом эпизодов (notifiedEpisodes ниже);
// legacy-сессии оставлены как были — они рудимент и на них никто не живёт.
const seenIds = new Set<number>();

/**
 * Настройки уведомлений — свойство ЭТОГО устройства, а не компьютера: раньше
 * тумблер писал `notifications_enabled` в конфиг агента (строка "true"/"false",
 * которую JS считал truthy в обоих случаях — тумблер всегда выглядел
 * включённым), а показ уведомления этот флаг вообще не читал.
 */
export interface NotifyPrefs {
  /** Агент задал вопрос и ждёт ответа. */
  waiting: boolean;
  /** Команда завершена или упала. */
  done: boolean;
}

const PREFS_KEY = "tg.notify.v1";
const DEFAULT_PREFS: NotifyPrefs = { waiting: true, done: true };

export function getNotifyPrefs(): NotifyPrefs {
  try {
    const raw = localStorage.getItem(PREFS_KEY);
    if (!raw) return { ...DEFAULT_PREFS };
    const p = JSON.parse(raw);
    return {
      waiting: p?.waiting !== false,
      done: p?.done !== false,
    };
  } catch {
    return { ...DEFAULT_PREFS };
  }
}

export function setNotifyPrefs(patch: Partial<NotifyPrefs>): NotifyPrefs {
  const next = { ...getNotifyPrefs(), ...patch };
  try {
    localStorage.setItem(PREFS_KEY, JSON.stringify(next));
  } catch { /* приватный режим — настройка не переживёт перезапуск */ }
  return next;
}

/** Текущее состояние разрешения ОС (null — ещё не проверяли). */
export function notificationsAllowed(): boolean | null {
  return permissionGranted;
}

// Реплей истории после возврата из фона не должен звенеть: события старше
// этого порога показываем только в UI, без системного уведомления.
const REPLAY_MAX_AGE_MS = 2 * 60 * 1000;

function isStaleReplay(ts?: number): boolean {
  return typeof ts === "number" && ts > 0 && Date.now() - ts > REPLAY_MAX_AGE_MS;
}

/** Register the notification channel and optionally request permission. Idempotent. */
export async function initNotifications(options: { request?: boolean } = {}): Promise<boolean> {
  try {
    // Channel must exist before notifications fire on Android 8+.
    await LocalNotifications.createChannel({
      id: CHANNEL_ID,
      name: "Завершение задач AI",
      description: "Уведомления когда AI-агент закончил выполнение",
      importance: 4, // HIGH — heads-up notification
      visibility: 1,
      sound: "default",
      vibration: true,
    }).catch(() => {});

    let perm = await LocalNotifications.checkPermissions();
    if (perm.display !== "granted" && options.request) {
      perm = await LocalNotifications.requestPermissions();
    }
    permissionGranted = perm.display === "granted";
  } catch {
    permissionGranted = false;
  }
  // Возврат приложения из фона = единственный шанс узнать о том, что случилось,
  // пока телефон спал. Вешаем здесь, а не в App.tsx: подписка должна пережить
  // любые размонтирования экранов, а initNotifications уже зовётся один раз при
  // старте (и повторно — когда разрешение выдали из настроек). Идемпотентно.
  hookResumeCatchUp();
  return permissionGranted;
}

export async function requestNotificationPermission(): Promise<boolean> {
  return initNotifications({ request: true });
}

/** Разрешение уже спрашивали хотя бы раз на этом устройстве. */
const ASKED_KEY = "tg.notify.asked.v1";

/**
 * Спросить разрешение ОС один раз — и в момент, когда оно осмысленно.
 *
 * До этого его не просили НИГДЕ, кроме перехода «выкл → вкл» тумблера в
 * настройках: старт приложения только ПРОВЕРЯЛ разрешение (initNotifications
 * без `request`). На Android 13+ оно по умолчанию не выдано, значит
 * permissionGranted = false и показ любого уведомления выходил на первой
 * строке — при двух зелёных тумблерах в настройках человек не получал ни
 * одного уведомления неделями и не мог догадаться, что чинится это выключением
 * и повторным включением тумблера.
 *
 * Зовём из момента первого события агента (App.tsx): системный диалог виден в
 * контексте («сейчас прилетит вот это»), а не пустым вопросом на старте.
 * Латч в localStorage — чтобы отказ не превращался в диалог на каждое событие.
 */
export async function maybeRequestNotificationPermission(): Promise<boolean> {
  if (permissionGranted === true) return true;
  // Веб и Telegram Mini App локальных уведомлений не показывают вовсе —
  // спрашивать там нечего (и Capacitor вернул бы отказ плагина).
  if (!isNativeApp) return false;
  // Человек сам выключил оба вида уведомлений — не тревожим системным диалогом.
  const prefs = getNotifyPrefs();
  if (!prefs.waiting && !prefs.done) return false;
  try {
    // Уже спрашивали, а разрешения так и нет (выше мы вышли бы с true) —
    // второй системный диалог Android всё равно не покажет.
    if (localStorage.getItem(ASKED_KEY)) return false;
    localStorage.setItem(ASKED_KEY, "1");
  } catch {
    // Приватный режим: латч не сохранится. Хуже, чем один диалог, ничего
    // не случится — Android сам перестаёт показывать запрос после двух отказов.
  }
  return requestNotificationPermission();
}

/**
 * Экраны, на которые уведомлению разрешено уводить. Список закрытый: `extra`
 * — это данные уведомления, а не наш код, и подстановка произвольной строки в
 * navigate() была бы открытым редиректом внутри приложения (увести можно и на
 * экран пейринга). Новый экран — новая строка здесь, а не «любой путь».
 */
const ALLOWED_ROUTES = new Set(["/support"]);

/** Register tap handler. Tapping a notification routes back to the session,
 * to a PTY terminal, or to a fixed screen (`extra.route`) depending on the
 * event source. */
export function onNotificationTap(
  onSession: (sessionName: string) => void,
  onPty?: (ptyId: string) => void,
  onRoute?: (path: string) => void,
) {
  LocalNotifications.addListener("localNotificationActionPerformed", (action) => {
    const extra = (action.notification as any)?.extra;
    if (extra?.ptyId && onPty) {
      onPty(String(extra.ptyId));
      return;
    }
    // Уведомления, ведущие на конкретный экран (ответ поддержки → /support).
    // Незнакомый путь молча игнорируем: тап просто открывает приложение — это
    // не хуже, чем было до появления маршрутов.
    if (typeof extra?.route === "string" && ALLOWED_ROUTES.has(extra.route) && onRoute) {
      onRoute(extra.route);
      return;
    }
    if (typeof extra?.session === "string" && extra.session) {
      onSession(extra.session);
    }
  }).catch(() => {});
}

interface NotifyEvent {
  id?: number;
  type: "notify";
  event: "session.completed" | "session.failed";
  session: string;
  agent?: string;
  summary?: string;
  error?: boolean;
}

interface PtyEvent {
  id?: number;
  type: "pty_event";
  event: "waiting_input" | "finished" | "error" | string;
  pty_id: string;
  name?: string;
  agent?: string;
  fg_process?: string;
  /** Текст вопроса агента («Подтвердите: y/n») — с v2.28. */
  hint?: string;
  hint_kind?: string;
  /** Когда событие произошло (unix ms) — с v2.28. */
  ts?: number;
  /**
   * Начало ЭПИЗОДА (unix ms): «этот самый вопрос агента». В отличие от ts (он
   * всегда «сейчас») штамп стабилен, пока агент стоит на одном вопросе, —
   * по нему латч ниже отличает новый вопрос от уже отзвонённого. Старый агент
   * поля не присылает; тогда откатываемся на ts.
   */
  status_at?: number;
  /**
   * Длительность завершившегося эпизода работы агента (мс) — только у
   * finished от агентского PTY. Старые агенты и finished обычных команд поля
   * не присылают — текст уведомления тогда прежний, без длительности.
   */
  duration_ms?: number;
}

/** Откуда пришло событие: живой WS или добор состояния после сна. */
interface ShowOptions {
  source?: "ws" | "catchup";
}

/** Show a system notification for a notify-event. Deduplicates by event id. */
export async function showNotifyEvent(ev: NotifyEvent): Promise<void> {
  if (!permissionGranted) return;
  // Уведомление обязано вести туда, где о нём можно что-то узнать. Экран
  // сессии закрыт фича-флагом ai (по умолчанию выключен, включается секретным
  // пятикратным тапом по версии), поэтому «Сессия "build" упала с ошибкой»
  // приводила на главную — без объяснений и без единой двери к этой сессии;
  // ошибку человек видел, только вернувшись в бота. Пока раздел скрыт, молчим:
  // терминалы (/pty) о своей работе звонят сами, отдельным типом события.
  if (!getFeatures().ai) return;
  if (typeof ev.id === "number") {
    if (seenIds.has(ev.id)) return;
    seenIds.add(ev.id);
    // Cap memory: drop oldest ~half when set grows too large.
    if (seenIds.size > 500) {
      const toDelete = Array.from(seenIds).slice(0, 250);
      toDelete.forEach((id) => seenIds.delete(id));
    }
  }

  const success = ev.event === "session.completed";
  const title = success
    ? `${ev.agent || "Агент"} закончил`
    : `Ошибка в сессии`;
  const body = ev.summary?.trim()
    ? ev.summary.trim()
    : success
    ? `Сессия "${ev.session}" завершена`
    : `Сессия "${ev.session}" упала с ошибкой`;

  // Android notification id must fit in int32; derive a stable id from session
  // name + event id so identical events overwrite (don't pile up).
  const nid = stableInt32(`${ev.session}:${ev.id ?? Date.now()}`);

  try {
    await LocalNotifications.schedule({
      notifications: [{
        id: nid,
        title,
        body,
        channelId: CHANNEL_ID,
        smallIcon: "ic_stat_icon_config_sample",
        ongoing: false,
        autoCancel: true,
        extra: { session: ev.session, event: ev.event },
      }],
    });
  } catch {
    // Silent — permission may have been revoked.
  }
}

function stableInt32(s: string): number {
  let h = 0;
  for (let i = 0; i < s.length; i++) {
    h = (h * 31 + s.charCodeAt(i)) | 0;
  }
  return Math.abs(h) % 0x7fffffff;
}

/**
 * Латч эпизодов — дедуп PTY-уведомлений вместо прежнего дедупа по числовому
 * `ev.id`. Ключ «терминал + событие», значение — штамп эпизода (status_at, у
 * старого агента ts) и момент, когда мы по нему звенели.
 *
 * Почему не id: это счётчик в памяти агента (internal/web/events.go), после
 * каждого перезапуска — включая штатное автообновление — он начинается заново
 * с 1. Уже виденные 1..N лежали в seenIds живущего в памяти WebView
 * приложения, и первые события после апдейта молча проглатывались.
 */
const notifiedEpisodes = new Map<string, { stamp: number; at: number }>();

/** Show a system notification for a PTY heuristic event. Deduplicates by episode. */
export async function showPtyEvent(ev: PtyEvent, opts: ShowOptions = {}): Promise<void> {
  if (!permissionGranted) return;
  // Настройка устройства: что именно показывать.
  const prefs = getNotifyPrefs();
  if (ev.event === "waiting_input" && !prefs.waiting) return;
  if ((ev.event === "finished" || ev.event === "error") && !prefs.done) return;
  // Реплей после возврата из фона: старые события в UI видны, но не звенят.
  // Добор состояния (catchUpPtyEvents) сюда НЕ попадает: он присылает ts =
  // «сейчас», потому что описывает не историю, а то, что происходит прямо
  // сейчас — иначе ночной вопрос агента был бы погашен как старый.
  if (isStaleReplay(ev.ts)) return;

  const episodeKey = `${ev.pty_id}:${ev.event}`;
  const stamp = ev.status_at ?? ev.ts ?? 0;
  const seen = notifiedEpisodes.get(episodeKey);
  if (seen) {
    // Тот же эпизод — по нему уже звенели.
    if (stamp > 0 && seen.stamp === stamp) return;
    // Добор после сна у СТАРОГО агента: status_at он не присылает, штамп там
    // «сейчас» и с WS-штампом не совпадёт никогда. Поэтому для добора смотрим
    // не на штамп, а на давность звонка: только что отзвонённый терминал
    // второй раз не тревожим.
    if (opts.source === "catchup" && Date.now() - seen.at < REPLAY_MAX_AGE_MS) return;
  }
  if (stamp > 0) {
    notifiedEpisodes.set(episodeKey, { stamp, at: Date.now() });
    // Терминалы приходят и уходят — за долгую жизнь приложения ключей
    // набегает больше, чем живых сессий. Подрезаем самые старые.
    if (notifiedEpisodes.size > 200) {
      Array.from(notifiedEpisodes.keys())
        .slice(0, 100)
        .forEach((k) => notifiedEpisodes.delete(k));
    }
  }

  const ptyName = ev.name?.trim() || "терминал";
  const deviceName = getSelectedDeviceName();
  const agentLabel = ev.agent && isAgentKind(ev.agent)
    ? agentDisplayName(ev.agent)
    : "Терминал";

  let title: string;
  let body: string;
  switch (ev.event) {
    case "waiting_input":
      title = `${agentLabel} ждёт ответа`;
      // Текст вопроса прямо в уведомлении — раньше приходилось открывать
      // терминал, чтобы понять, о чём спрашивают.
      body = ev.hint?.trim()
        ? `«${ptyName}»: ${ev.hint.trim()}`
        : `В терминале «${ptyName}» ожидается ответ`;
      break;
    case "finished": {
      title = "Команда завершена";
      // Агентский finished несёт duration_ms — называем, сколько он работал.
      // Поля нет (старый агент / обычная команда) — текст прежний.
      const dur = typeof ev.duration_ms === "number" && ev.duration_ms > 0
        ? ` · ${formatDurationMs(ev.duration_ms)}`
        : "";
      body = `${agentLabel} закончил в «${ptyName}»${dur}`;
      break;
    }
    case "error":
      title = "Ошибка в терминале";
      body = `${agentLabel} в «${ptyName}»`;
      break;
    default:
      // Новый агент прислал незнакомый тип события: молчим, вместо системного
      // уведомления с «undefined» в заголовке.
      return;
  }
  if (deviceName) body = `${deviceName} · ${body}`;

  // nid БЕЗ id события: одинаковые события перекрывают друг друга вместо того,
  // чтобы копиться пачкой (за ночь простоя их набегали сотни).
  const nid = stableInt32(`pty:${ev.pty_id}:${ev.event}`);
  try {
    await LocalNotifications.schedule({
      notifications: [{
        id: nid,
        title,
        body,
        channelId: CHANNEL_ID,
        smallIcon: "ic_stat_icon_config_sample",
        ongoing: false,
        autoCancel: true,
        extra: { ptyId: ev.pty_id, event: ev.event },
      }],
    });
  } catch {
    // permission revoked
  }
}

/**
 * Ответ поддержки — тот же канал уведомлений, что и события агента.
 *
 * Раньше об ответе можно было узнать, только случайно зайдя в настройки: там
 * счётчик непрочитанных тянулся ровно один раз при монтировании экрана
 * (аудит #77/#111). Решение «звонить или нет» принимает supportUnread.ts —
 * он звенит только на ПРИРОСТ относительно уже виденного, поэтому холодный
 * старт с висящим непрочитанным не будит пользователя повторно.
 */
export async function showSupportReply(count: number): Promise<void> {
  if (!permissionGranted) return;
  try {
    await LocalNotifications.schedule({
      notifications: [{
        // id постоянный: второй ответ ЗАМЕНЯЕТ уведомление, а не кладётся
        // рядом (та же логика, что у pty-событий).
        id: stableInt32("support"),
        title: "Поддержка Remotai ответила",
        // Без склонений по числу: «2 новых сообщения» и «5 новых сообщений»
        // требуют правил, а двоеточие корректно при любом count.
        body: count > 1
          ? `Новых сообщений: ${count}`
          : "Откройте чат, чтобы прочитать",
        channelId: CHANNEL_ID,
        smallIcon: "ic_stat_icon_config_sample",
        ongoing: false,
        autoCancel: true,
        extra: { route: "/support" },
      }],
    });
  } catch {
    // permission revoked
  }
}

// ── Добор пропущенного после сна (#107) ───────────────────────────────────
//
// Телефон спал / приложение лежало в фоне — WS-события агента не пришли, а
// доигрывания в облаке нет (см. докстринг файла). На возврате в активное
// состояние спрашиваем ТЕКУЩЕЕ состояние терминалов и поднимаем уведомление по
// тем, кто прямо сейчас ждёт ответа или упал.
//
// Почему это не шторм запросов и не лавина уведомлений:
//  * НИКАКОГО нового интервала здесь не заводится — только реакция на возврат
//    приложения; поллингом главной по-прежнему заведует usePolling/HomeActivity;
//  * запрос ровно один (GET /api/pty) и не чаще раза в CATCH_UP_MIN_INTERVAL_MS:
//    Android WebView шлёт и appStateChange, и visibilitychange — они прилетают
//    подряд, и без паузы получилось бы два снимка дерева процессов на КАЖДУЮ
//    сессию (та же цена, из-за которой в волне A вводили usePolling);
//  * параллельный вызов гасится флагом catchUpInFlight;
//  * повтор уведомления гасит латч эпизодов из showPtyEvent — «тот же самый»
//    вопрос не звенит второй раз ни с WS, ни с добора;
//  * вне нативного APK (веб, Telegram Mini App) функция выходит на первой
//    строке: локальных уведомлений там нет, значит и запрос не нужен.

const CATCH_UP_MIN_INTERVAL_MS = 15_000;
// Потолок на один добор. Реальных агентов у человека 1–3, но десяток
// терминалов, одновременно ждущих ответа, не должен превратиться в стену
// уведомлений: остальные видны на главной в «Требует внимания».
const CATCH_UP_MAX_NOTIFICATIONS = 5;
let lastCatchUpAt = 0;
let catchUpInFlight = false;

/** Разовый добор состояния терминалов. Вызывается на возврате приложения. */
export async function catchUpPtyEvents(): Promise<void> {
  if (permissionGranted !== true) return;
  const now = Date.now();
  if (catchUpInFlight || now - lastCatchUpAt < CATCH_UP_MIN_INTERVAL_MS) return;
  lastCatchUpAt = now;
  catchUpInFlight = true;
  try {
    const { sessions } = await listPtySessions({ aliveOnly: true });
    // Тот же предикат, что в «Требует внимания» (HomeActivity): waiting только
    // с непустым hint — иначе будили бы из-за молчащего агента. Фильтра по
    // свежести здесь быть НЕ должно: ночной вопрос агента висит часами, ради
    // него всё и делается. Статус "error" агент гасит сам через 5 минут
    // (internal/pty/manager.go), так что о давней ошибке не позвоним.
    const pending = (sessions || [])
      .filter((s) => s.alive && ((s.status === "waiting" && !!s.hint) || s.status === "error"))
      .sort((a, b) => (b.status_at || 0) - (a.status_at || 0))
      .slice(0, CATCH_UP_MAX_NOTIFICATIONS);
    for (const s of pending) {
      const waiting = s.status === "waiting";
      await showPtyEvent({
        type: "pty_event",
        event: waiting ? "waiting_input" : "error",
        pty_id: s.id,
        name: s.name,
        agent: s.agent_kind,
        fg_process: s.fg_process,
        hint: s.hint,
        hint_kind: s.hint_kind,
        // ts = «сейчас» НАМЕРЕННО: это живое состояние, а не реплей истории.
        // Со временем эпизода isStaleReplay погасил бы ночной вопрос. За «не
        // звонить дважды» отвечает status_at (латч эпизода), а не свежесть.
        ts: now,
        status_at: s.status_at,
      }, { source: "catchup" });
    }
  } catch {
    // ПК не в сети / компьютер не выбран / нет связи — добор не состоялся,
    // состояние экраны и так перечитают своим поллингом.
  } finally {
    catchUpInFlight = false;
  }
}

/**
 * Подписка на возврат приложения. Живёт весь запуск (как и обработчик тапа по
 * уведомлению), поэтому регистрируется один раз и не снимается: снимать её
 * некому — модуль переживает любые размонтирования экранов.
 */
let resumeHooked = false;
function hookResumeCatchUp(): void {
  if (resumeHooked) return;
  resumeHooked = true;
  // Натив: приложение подняли из фона. Пробуждением events-WS на это же
  // событие занимается App.tsx (wakeWS) — здесь только добор состояния,
  // дублировать работу с сокетом не нужно.
  CapApp.addListener("appStateChange", ({ isActive }) => {
    if (isActive) void catchUpPtyEvents();
  }).catch(() => { /* не Capacitor-среда */ });
  // WebView шлёт ещё и visibilitychange (в вебе/Telegram он единственный).
  // Двойное срабатывание безопасно: catchUpPtyEvents троттлится изнутри.
  document.addEventListener("visibilitychange", () => {
    if (!document.hidden) void catchUpPtyEvents();
  });
}
