import { t } from "./i18n";
// Framework-agnostic HTTP client core shared by the Mini App and the APK.
//
// Preserves the original per-app behaviour (30s abort timeout, JSON parse,
// error message from `body.error || statusText`) and adds two improvements the
// plan called for:
//   • typed errors — ApiError carries the HTTP `status` (and optional `code`)
//     so callers can branch on 401/402/410 without string-matching. It extends
//     Error, so existing `catch (e) { e.message }` code keeps working unchanged.
//   • conservative retry — idempotent GETs are retried on network error / 5xx /
//     429 with exponential backoff. Mutating requests are never retried.
//
// Auth and base URL are injected per platform (Mini App = initData header,
// APK = API token / cloud transport), so this module stays platform-agnostic.

import { APP_NAME } from "./brand";

export class ApiError extends Error {
  status: number;
  code?: string;
  /** Отпечаток хост-ключа (SSH host_key_unknown) и подобные машинные поля. */
  fingerprint?: string;
  constructor(message: string, status = 0, code?: string, fingerprint?: string) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.code = code;
    this.fingerprint = fingerprint;
  }
}

/**
 * Каким путём клиент ходит за данными компьютера. Один и тот же сетевой сбой
 * значит разное: на прямом пути (LAN / окно exe / self-hosted) это «компьютер
 * не отвечает», через облако — «нет связи с сервисом». shared не видит
 * getMode() приложения, поэтому маршрут сообщает apk — setNetworkContext().
 * `unknown` = ещё не сообщён: тогда текст выбираем нейтральный, а не врущий.
 */
export type NetworkRoute = "direct" | "cloud" | "unknown";

/** Код сетевого сбоя на прямом пути до компьютера (fetch не получил ответа). */
export const ERR_NETWORK = "network";
/** Тот же сбой, но на пути до облака (вход в аккаунт, пейринг, релей-прокси). */
export const ERR_NETWORK_CLOUD = "network_cloud";

let currentRoute: NetworkRoute = "unknown";

/** apk сообщает актуальный маршрут (при старте и при смене режима/ПК). */
export function setNetworkContext(ctx: { route: NetworkRoute }): void {
  currentRoute = ctx.route;
}

export function getNetworkRoute(): NetworkRoute {
  return currentRoute;
}

/**
 * «Компьютер ТОЧНО на связи» — доказательство от живого канала, а не догадка.
 *
 * Аудит путей 29.08.2026: ответ 502 объявлял выключенным компьютер, который в
 * ту же секунду был на связи, — на экране одновременно горел зелёный индикатор
 * и красное «компьютер не в сети, включите его». Совет вёл к бесполезному
 * действию: включать нечего.
 *
 * Приложение подставляет сюда своё знание о канале (в облаке — кадр
 * `agent_status` от релея при живом сокете). `null` = приложение ничего не
 * сообщило: тогда 502 трактуется как раньше, поведение не меняется.
 */
let pcLiveProbe: (() => boolean) | null = null;

/** apk сообщает, чем проверять живость ПК. `null` — снять пробу. */
export function setPcLiveProbe(probe: (() => boolean) | null): void {
  pcLiveProbe = probe;
}

/** Ответ пробы, безопасный к исключениям: «не знаю» = «не подтверждено». */
export function isPcConfirmedLive(): boolean {
  try { return !!pcLiveProbe?.(); } catch { return false; }
}

/** Браузер знает, что сети нет вовсе (самолётный режим, выключенный Wi-Fi). */
function browserOffline(): boolean {
  return typeof navigator !== "undefined" && navigator.onLine === false;
}

/**
 * Сетевой сбой без ответа сервера. `fetch` в этом случае бросает TypeError
 * («Failed to fetch», «NetworkError when attempting to fetch resource», в
 * Safari — «Load failed»), у которого НЕТ status: до появления этой проверки
 * такая ошибка доезжала до интерфейса сырым английским текстом.
 */
export function isNetworkFailure(e: unknown): boolean {
  const code = (e as { code?: unknown })?.code;
  if (code === ERR_NETWORK || code === ERR_NETWORK_CLOUD) return true;
  const status = (e as { status?: unknown })?.status;
  if (typeof status === "number" && status !== 0) return false;
  if (typeof code === "string" && code) return false; // «timeout», «aborted» — не сеть
  if (e instanceof TypeError) return true;
  const message = String((e as { message?: unknown })?.message ?? "");
  return /failed to fetch|networkerror|network request failed|load failed|connection (refused|reset)/i.test(message);
}

/** Маршрут, на котором случился сбой: помеченный в ошибке важнее глобального. */
function routeOfFailure(e: unknown): NetworkRoute {
  const code = (e as { code?: unknown })?.code;
  if (code === ERR_NETWORK_CLOUD) return "cloud";
  if (code === ERR_NETWORK) return "direct";
  return currentRoute;
}

/**
 * Обернуть сетевой сбой fetch в типизированную ошибку. Не-сетевые ошибки
 * (ApiError, AbortError, битый JSON) возвращаются как есть.
 */
export function toNetworkError(e: unknown, route: "direct" | "cloud"): unknown {
  if (e instanceof ApiError) return e;
  if (!isNetworkFailure(e)) return e;
  const raw = String((e as { message?: unknown })?.message ?? "");
  // Сырое «Failed to fetch» — только в отладочный лог: в интерфейсе его
  // заменяет человеческий текст из mapApiError.
  if (raw) console.debug("[api] сетевой сбой:", route, raw);
  return new ApiError(
    route === "cloud" ? t("ui.api.m0133aa7d59") : t("conn.pcNotResponding"),
    0,
    route === "cloud" ? ERR_NETWORK_CLOUD : ERR_NETWORK,
  );
}

/** `fetch`, у которого сетевой сбой уже типизирован (см. toNetworkError). */
export async function fetchOrNetworkError(
  url: string,
  init?: RequestInit,
  route: "direct" | "cloud" = "cloud",
): Promise<Response> {
  try {
    return await fetch(url, init);
  } catch (e) {
    throw toNetworkError(e, route);
  }
}

/**
 * «Компьютер не в сети» — отдельный класс ошибки, а не просто 5xx.
 *
 * Релей отдаёт машинный код (`pc_offline` — агента нет в хабе, `pc_timeout` —
 * был, но не ответил); старые релеи присылают только англоязычный текст
 * «agent offline» / «agent disconnected», его тоже распознаём. На прямом пути
 * (LAN/self-hosted) выключенный ПК даёт голый сетевой сбой без ответа — это
 * тоже он, но только если сеть у самого клиента есть и маршрут прямой:
 * в облаке тот же сбой означает «нет связи с сервисом», а не «ПК выключен».
 */
export function isPcOffline(e: unknown): boolean {
  const code = (e as { code?: unknown })?.code;
  const status = (e as { status?: unknown })?.status;
  const message = String((e as { message?: unknown })?.message ?? "");
  if (code === "pc_offline" || code === "pc_timeout") return true;
  if (/agent (offline|unreachable|disconnected|did not respond)/i.test(message)) return true;
  // 502/504 — это «шлюз не смог», а не «компьютер выключен»: один сорвавшийся
  // проброс на релее выглядит так же. Если канал в эту же секунду доказывает,
  // что ПК на связи, — верим доказательству, а не коду ответа.
  if (status === 502 || status === 504) return !isPcConfirmedLive();
  if (isNetworkFailure(e) && routeOfFailure(e) === "direct" && !browserOffline()) return true;
  return false;
}

/**
 * Map an error (typically an ApiError) to a localized, actionable RU message
 * by HTTP status, instead of surfacing raw "Unauthorized"/"Bad Request" text.
 */
export function mapApiError(e: unknown): string {
  // Duck-type on `status` so this covers ApiError, the cloud CloudError, and any
  // other error carrying an HTTP status — not just the shared ApiError class.
  const status = (e as { status?: unknown })?.status;
  const code = (e as { code?: unknown })?.code;
  const message = (e as { message?: unknown })?.message;
  // Файловые операции возвращают машинный code. Он важнее HTTP-статуса:
  // например, 403 no_permission — это не протухшая сессия, а 409 file_in_use
  // — не конфликт пейринга. До появления этой таблицы оба случая показывали
  // пользователю совершенно неверное действие.
  const codeMessages: Record<string, string> = {
    // Отказ денежного гейта релея. Строки `subscription_required` в клиенте не
    // было ни одной, и человек на 402 читал «Достигнут лимит компьютеров» —
    // совет отвязать машину при живом и нормальном аккаунте (аудит путей
    // 29.08.2026). Формулировка канона: НЕ «доступ заблокирован».
    subscription_required: t("ui.apicore.m01d700d09d"),
    server_subscription_required: t("ui.apicore.maba35f1f99"),
    server_account_required: t("ui.apicore.m1208f9faa3"),
    server_access_unavailable: t("ui.apicore.ma793b27336"),
    no_permission: t("ui.apicore.m9124d44363"),
    file_in_use: t("ui.apicore.m71ba92cf61"),
    disk_full: t("ui.apicore.m2864ba2e80"),
    not_empty: t("ui.apicore.m6593b80270"),
    dir_not_empty: t("ui.apicore.m6593b80270"),
    read_only: t("ui.apicore.m83e5eda292"),
    too_large: t("ui.apicore.me8f72d307e"),
    outside_roots: t("ui.apicore.m940c64d194"),
    bad_path: t("ui.apicore.m2e4da78b20"),
    not_found: t("ui.apicore.m780f1d68ff"),
    already_exists: t("ui.apicore.m49d56df154"),
    is_dir: t("ui.apicore.ma9b34a8f6c"),
    not_a_dir: t("ui.apicore.m04d4ee4950"),
    bad_range: t("ui.apicore.m7a618e7e9e"),
    file_changed: t("ui.apicore.m8c5fe71387"),
    dst_inside_src: t("ui.apicore.mba7fe8e063"),
    io_error: t("ui.apicore.meecf88add0"),
    query_too_short: t("ui.apicore.m0e636ac599"),
    upload_offset: t("ui.apicore.m42530eba70"),
    bad_upload_id: t("ui.apicore.m85a426a542"),
    telegram_not_linked: t("ui.apicore.m07e2b9a748"),
    telegram_unavailable: t("ui.apicore.m209116bd36"),
    telegram_send_failed: t("ui.apicore.mffab414197"),
    file_transfer_failed: t("ui.apicore.m5f4159e03a"),
    no_terminal_emulator: t("ui.apicore.m8015b64ed3"),
    device_limit: t("ui.apicore.md3078fee64"),
    pty_limit: t("ui.apicore.me1212fbdcd"),
    invalid_cwd: t("ui.apicore.m5a8a5db560"),
    license_required: t("ui.apicore.m526e6779d9"),
    team_required: t("ui.apicore.m7667556949"),
    pro_required: t("ui.apicore.mb36c9754eb"),
    agent_not_allowed: t("ui.apicore.mee22944f88"),
    session_expired: t("ui.apicore.m80c2318d7d"),
    // Совет «проверьте символы» был почти всегда ложным следом: регистр, дефис,
    // пробелы и похожие знаки (5/S, 2/Z, 8/B, 6/G, U/V) релей сворачивает сам,
    // а живой разбор 23.08 показал другое — за сутки на релее НИ ОДНОГО
    // /v1/pair/request и пустая таблица кодов при попытке ввода. То есть код
    // был не «набран с опечаткой», а взят с давно погасшего экрана: он живёт
    // час, а через час после этого исчезает и из базы. Ведём к действию,
    // которое правда помогает, — взять свежий код на том компьютере.
    pair_code_not_found:
      t("ui.apicore.m9e93b2d652"),
    viewer_read_only: t("ui.apicore.m6e40dafc5c"),
    // Конфликт именно пейринга. Раньше этот текст висел на ВСЁМ статусе 409, и
    // «этот вход уже занят» или «в компании остались устройства» советовали
    // отвязать рабочий ПК (находка N131). Совет про «Отключить облако» верен
    // только здесь, по явному коду.
    device_taken: t("ui.apicore.m28f2454b41"),
    // Остальные конфликты 409 релея: у каждого своя причина и своё действие,
    // и ни одно из них не связано с отвязкой компьютера.
    identity_taken: t("ui.apicore.m40fd6aab57"),
    workspace_not_empty: t("ui.apicore.m2dc6f363c1"),
    personal_workspace: t("ui.apicore.m41a86a9f10"),
    current_session: t("ui.apicore.m8563e3641f"),
    cross_device: t("ui.apicore.me40143dc60"),
    power_not_pending: t("ui.apicore.mc9f77cb6f7"),
    power_unsupported: t("ui.apicore.m1ee350436d"),
    screenshot_failed: t("ui.apicore.m10476e4473"),
    managed_externally: t("ui.apicore.m1b2e6176ea"),
    secret_not_remembered: t("ui.apicore.maf1fc6fff0"),
    // Терминал жив, но связь с ним сейчас не поднялась. Без этих двух строк
    // ответ агента проваливался в безкодовый 404 и человек читал «Не найдено.»
    // о терминале, в котором прямо сейчас идёт работа (боевой случай
    // 17.08.2026). Ждать вручную не нужно: связь возвращает фоновый relink.
    host_busy: t("ui.apicore.mbe75b9ed49"),
    host_gone: t("ui.apicore.m6119cc9a86"),
  };
  if (typeof code === "string" && codeMessages[code]) return codeMessages[code];
  // Сетевой сбой (fetch без ответа) разбираем ДО статусов: у TypeError нет
  // status, поэтому раньше он проваливался в финальный фолбэк и человек читал
  // английское «Failed to fetch» — в том числе на самом первом экране входа.
  if (isNetworkFailure(e)) {
    if (browserOffline()) return t("ui.apicore.m0852e77dd2");
    const route = routeOfFailure(e);
    if (route === "cloud") {
      return t("ui.apicore.mf3a1498cd6", { p0: (APP_NAME) });
    }
    // Маршрут неизвестен (приложение не сообщило его через setNetworkContext) —
    // называем обе возможные причины, но ни одну не выдаём за факт.
    if (route === "unknown") return t("ui.apicore.m875883151f");
    // Прямой путь: ниже сработает isPcOffline с текстом «Компьютер не в сети».
  }
  // Компьютер выключен/спит/обновляется — самый частый отказ дня. Проверяем ДО
  // общей ветки 5xx, иначе получалось «Ошибка сервера — попробуйте позже», хотя
  // сервер в порядке, а ждать нужно не «позже», а включения компьютера.
  if (isPcOffline(e)) return t("ui.apicore.me8b6475fcc");
  if (typeof status === "number") {
    switch (status) {
      // Безкодовый 401/403 приходит не только от протухшей сессии: так же
      // отвечают старый агент без нужной ручки, роль «только просмотр» и
      // отозванный доступ к компьютеру. Поэтому 401 и 403 разведены, и ни один
      // не выдаёт «сессия истекла» за факт (находка V13) — причину называет
      // машинный код выше (session_expired, viewer_read_only и другие).
      case 401: return t("ui.apicore.m4f656ee049");
      case 403: return t("ui.apicore.m56dd0087fd");
      case 402: return t("ui.apicore.md3078fee64");
      case 404: return t("ui.apicore.m4ed62665dd");
      // Безкодовый 409 приходит из совершенно разных мест (привязка второго
      // способа входа, удаление непустой компании, закрытие текущей сессии), и
      // конкретный совет здесь неизбежно врёт. Про пейринг говорит только код
      // device_taken выше.
      case 409: return t("ui.apicore.m673cda4807");
      case 410: return t("ui.apicore.m3d46fa177a");
      case 429: return t("ui.apicore.m46eed9c208");
      case 0: return code === "timeout" ? t("ui.apicore.me43ae1cc1e") : t("ui.apicore.m7825abf2a5");
      // 503 — не «что-то сломалось», а «сейчас не обслуживаем»: перезапуск или
      // перегрузка на нашей стороне. Прежний общий текст «Ошибка сервера —
      // попробуйте позже» не называл ни причину, ни срок, и человек не знал,
      // ждать ему или что-то чинить (аудит путей 29.08.2026).
      case 503: return t("ui.apicore.mbc79b79bb1");
      // 502/504 — запрос не дошёл до компьютера или ответ не успел вернуться.
      // Сюда попадают только те случаи, где `isPcOffline` уже сказала «машина
      // жива» (иначе экран показал бы честное «не в сети»), поэтому звать
      // включать компьютер нельзя — он включён. Прежний общий текст «Ошибка
      // сервера — попробуйте позже» не называл ни причины, ни шага: замер
      // сценария J10 05.09.2026 поймал его в «Файлах» при 502.
      case 502:
      case 504: return t("ui.apicore.m608411cf74");
    }
    if (status >= 500) return t("ui.apicore.m4211f52862");
  }
  // Фолбэк. Сообщение отдаём человеку ТОЛЬКО если оно написано по-русски (то
  // есть кем-то из наших и для людей). Машинные и англоязычные тексты агента,
  // релея и браузера уходят в отладочный лог: интерфейс продукта — русский.
  const text = typeof message === "string" ? message.trim() : "";
  if (/[А-Яа-яЁё]/.test(text)) return text;
  if (text) console.debug("[api] непереведённая ошибка:", text);
  return t("ui.apicore.ma2cb4bf0a7");
}

export interface HttpClientConfig {
  /**
   * Base URL prefix ("" for same-origin). A function is resolved per request,
   * so the APK can derive it from mutable config (and surface a security error
   * by throwing).
   */
  baseUrl: string | (() => string);
  /** Per-request headers — auth lives here (initData / API token). */
  getHeaders: () => HeadersInit;
  /** Request timeout in ms (default 30000 — matches the original client). */
  timeoutMs?: number;
  /** Max retries for idempotent GETs (default 2). */
  maxGetRetries?: number;
  /** Message for the abort/timeout ApiError (default "Request timed out"). */
  timeoutMessage?: string;
  /**
   * Маршрут этого клиента для текстов сетевых ошибок (default "direct"):
   * baseUrl здесь всегда указывает на конкретный сервер (агент в LAN,
   * self-hosted, тот же origin), а облачный путь в apk идёт своим транспортом.
   */
  route?: "direct" | "cloud";
}

export interface HttpClient {
  request<T>(path: string, init?: RequestInit): Promise<T>;
}

function sleep(ms: number): Promise<void> {
  return new Promise((r) => setTimeout(r, ms));
}

export function createHttpClient(config: HttpClientConfig): HttpClient {
  const timeoutMs = config.timeoutMs ?? 30000;
  const maxGetRetries = config.maxGetRetries ?? 2;
  const timeoutMessage = config.timeoutMessage ?? "Request timed out";
  const route = config.route ?? "direct";

  async function once<T>(path: string, init?: RequestInit): Promise<T> {
    const controller = new AbortController();
    const externalSignal = init?.signal;
    const onExternalAbort = () => controller.abort();
    if (externalSignal?.aborted) controller.abort();
    else externalSignal?.addEventListener("abort", onExternalAbort, { once: true });
    const timer = setTimeout(() => controller.abort(), timeoutMs);
    try {
      const base = typeof config.baseUrl === "function" ? config.baseUrl() : config.baseUrl;
      const res = await fetch(`${base}${path}`, {
        ...init,
        headers: { ...config.getHeaders(), ...init?.headers },
        signal: controller.signal,
      });
      if (!res.ok) {
        const body = await res.json().catch(() => ({ error: res.statusText }));
        throw new ApiError(body.error || res.statusText, res.status, body.code, body.fingerprint);
      }
      return res.json() as Promise<T>;
    } catch (e: any) {
      if (e?.name === "AbortError") {
        if (externalSignal?.aborted) throw new ApiError(t("ui.api.m542bd26f07"), 0, "aborted");
        throw new ApiError(timeoutMessage, 0, "timeout");
      }
      // Сеть не дала ответа (ПК выключен, Wi-Fi пропал, VPN рвёт TLS): fetch
      // бросает TypeError без status, и без обёртки он доезжал до интерфейса
      // сырым «Failed to fetch».
      throw toNetworkError(e, route);
    } finally {
      clearTimeout(timer);
      externalSignal?.removeEventListener("abort", onExternalAbort);
    }
  }

  async function request<T>(path: string, init?: RequestInit): Promise<T> {
    const method = (init?.method || "GET").toUpperCase();
    const idempotent = method === "GET";
    let attempt = 0;
    for (;;) {
      try {
        return await once<T>(path, init);
      } catch (e: any) {
        const status = e instanceof ApiError ? e.status : 0;
        // Ответа не было вовсе (сеть/таймаут): три подхода по 30 с превращали
        // выключенный ПК в минуту с лишним пустого ожидания. Один повтор
        // страхует моргнувший Wi-Fi, дальше — честная ошибка (UX-аудит V3).
        // А если браузер знает, что сети нет, повторять нечего вообще.
        const limit = status === 0 ? Math.min(maxGetRetries, 1) : maxGetRetries;
        const retriable =
          idempotent &&
          attempt < limit &&
          !browserOffline() &&
          (status === 0 || status >= 500 || status === 429);
        if (!retriable) throw e;
        await sleep(300 * Math.pow(2, attempt)); // 300ms, 600ms
        attempt++;
      }
    }
  }

  return { request };
}
