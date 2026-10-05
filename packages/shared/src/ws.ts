// Общий клиент потока событий (events-WS) для всех целей сборки: нативный APK,
// веб remotai.ru/app, Telegram /tg/ и окно exe.
//
// Что здесь есть сверх «нового WebSocket с реконнектом раз в 3 секунды»:
//   • Full Jitter-бэкофф с потолком 15 с и полом 250 мс — канон проекта
//     (PtyTermView, памятка realtime-resilience). Плоские 3 с без счётчика
//     попыток давали ~20 коннектов в минуту ВЕЧНО, если сервер отказывает.
//   • Один ping-таймер, привязанный к живому сокету, и stale-guard в КАЖДОМ
//     обработчике: без него старый сокет, дошедший до onopen после смены ПК,
//     гасит чужой ping и заводит второй цикл реконнектов (грабля зомби-циклов).
//   • Вотчдог первого байта: зависший хендшейк без onclose закрываем сами.
//   • Idle-вотчдог, который ВООРУЖАЕТСЯ фактом первого keepalive-ответа сервера
//     (тот же принцип, что `hb` в PTY), а не версией. Сервер, который на
//     прикладной ping не отвечает (сегодня это LAN-агент: он держит keepalive
//     протокольными PING-фреймами, из JS невидимыми), не должен получать
//     разрыв здорового простаивающего сокета каждую минуту.
//   • Стоп ладдера при ДОКАЗАННОЙ потере авторизации: провал WS-хендшейка по
//     401/403 браузер отдаёт неотличимым `1006`, поэтому правду выясняет
//     REST-проба (probeAuth). Коды 1008/4401 обрабатываются «на вырост».
//   • Пауза в фоне — пауза ЛАДДЕРА, а не убийство живого сокета: в нативном
//     APK её вообще включать нельзя (события в фоне = единственный канал
//     уведомлений «агент ждёт ответа»), см. pauseWhenHidden.
//   • Кадр видимости `{"type":"hidden"}` / `{"type":"visible"}`: пауза ладдера
//     сама по себе НЕ снимает присутствие на релее — сокет-то живой. Забытая
//     вкладка remotai.ru/app из-за этого вечно значила «человек в приложении»,
//     и уведомление «агент ждёт ответа» не приходило НИКОГДА. Поэтому цель,
//     которая в фоне ничего человеку не показывает, честно сообщает серверу
//     «меня не видно» — сокет при этом остаётся живым, чтобы возврат на вкладку
//     был мгновенным и без реплея. См. reportVisibility.
//
// Полный ws(s):// URL (хост + auth + since) строит платформа — модуль остаётся
// платформенно-нейтральным.

/** Результат разбора одного кадра: что клиенту с ним делать. */
export interface WSParsed<E = unknown> {
  /** Прикладное событие — раздать подписчикам. */
  event?: E;
  /** id события для реплея через `?since=` (двигает позицию в потоке). */
  id?: number;
  /** Принудительное состояние связи (в облаке — `agent_status` от релея). */
  connected?: boolean;
  /** Keepalive-ответ сервера (pong). Наружу НЕ отдаём: им вооружается idle-вотчдог. */
  keepalive?: boolean;
}

export interface WSClientOptions<E = unknown> {
  /** Полный ws(s):// URL для (пере)подключения. `""` = слушать нечего (нет ПК
   *  или авторизации) — ладдер не заводим. Бросок трактуется как `""`. */
  getUrl: (since: number) => string;
  /** Идентичность соединения (в облаке — deviceId, на LAN — адрес сервера).
   *  Расхождение с открытым сокетом = сокет слушает ЧУЖУЮ машину → пересоздать. */
  getKey?: () => string;
  /** Ключ localStorage для последнего виденного id события. */
  lastEventIdKey?: string;
  /** Период прикладного ping (мс). */
  pingIntervalMs?: number;
  /** Молчание сервера = pingIntervalMs × idleFactor → сокет считаем мёртвым. */
  idleFactor?: number;
  /** Сколько ждать onopen, прежде чем закрыть зависший хендшейк самим (мс). */
  openTimeoutMs?: number;
  /** Разбор кадра. По умолчанию: `{type:"pong"}` → keepalive, иначе плоское событие. */
  parse?: (raw: unknown) => WSParsed<E>;
  /** Пауза ладдера, пока страница скрыта. ⚠️ НЕ включать там, где поток событий
   *  в фоне что-то делает (нативный APK: показывает уведомления). */
  pauseWhenHidden?: boolean;
  /** Сообщать серверу видимость кадрами `hidden`/`visible`.
   *
   *  По умолчанию совпадает с pauseWhenHidden, и это ровно тот же критерий:
   *  цель, которая в фоне НИЧЕГО человеку не показывает (веб /app/, Telegram
   *  /tg/, окно exe), не должна и считаться «человек в приложении» — иначе
   *  релейный notifier молчит, хотя вопрос агента никто не видит. Нативный APK
   *  в фоне поднимает системное уведомление сам, поэтому он присутствие НЕ
   *  снимает: иначе на один вопрос пришли бы два сигнала (пуш + Telegram). */
  reportVisibility?: boolean;
  /** Проба «авторизация ДЕЙСТВИТЕЛЬНО потеряна?» (обычно REST-вызов профиля).
   *  Сеть легла / 5xx → должна вернуть false: это не потеря авторизации. */
  probeAuth?: () => Promise<boolean>;
  /** Сколько закрытий подряд, не дошедших до onopen, до пробы. */
  authProbeAfter?: number;
  /** Авторизация потеряна: ладдер остановлен, приложение решает, куда вести. */
  onAuthLost?: (reason: string) => void;
  /** Диагностический лог (apk передаёт tlog → виден в logcat/chrome://inspect). */
  log?: (event: string, data?: unknown) => void;
}

export interface WSClient<E = unknown> {
  /** Подключиться, если ещё не подключены. No-op после disconnect()/потери
   *  авторизации — их снимают resume()/reconnect(). */
  connect(): void;
  /** «Доступ только что появился» (холодный старт, пейринг, вход): снять стоп и
   *  подключиться. Живой сокет с той же идентичностью переиспользуется. */
  resume(): void;
  /** Форс: снять стоп, обнулить бэкофф, пересоздать сокет (реплей через since). */
  reconnect(): void;
  /** Возврат из фона / сеть вернулась: реконнект ТОЛЬКО если сокет мёртв или не
   *  доказал, что жив. Живой сокет не трогаем — иначе жжём мост на релее. */
  wake(): void;
  /** Ручное отключение: ладдер стоит до resume()/reconnect(). */
  disconnect(): void;
  /** Явный сигнал видимости от платформы — там, где `visibilitychange` не
   *  срабатывает или значит не то: Capacitor `appStateChange` («свернул
   *  приложение»), Telegram `activated`/`deactivated`. Веб-цели свой
   *  `visibilitychange` ловят сами, звать это оттуда не нужно.
   *  Без reportVisibility — no-op (см. опцию). */
  setVisible(visible: boolean): void;
  onEvent(cb: (event: E) => void): () => void;
  onConnectionChange(cb: (connected: boolean) => void): () => void;
  isConnected(): boolean;
}

/** Разбор по умолчанию: плоский кадр события, `{type:"pong"}` — keepalive. */
function defaultParse<E>(raw: unknown): WSParsed<E> {
  const o = raw as { id?: unknown; type?: unknown } | null;
  if (o && o.type === "pong") return { keepalive: true };
  return { event: raw as E, id: typeof o?.id === "number" ? o.id : undefined };
}

/** Причина остановки ладдера: "" — работаем. */
type StopReason = "" | "manual" | "auth";

export function createWSClient<E = unknown>(opts: WSClientOptions<E>): WSClient<E> {
  const LAST_EVENT_ID_KEY = opts.lastEventIdKey ?? "tg.lastEventId";
  const pingMs = opts.pingIntervalMs ?? 25000;
  const idleFactor = opts.idleFactor ?? 2.5;
  const openTimeoutMs = opts.openTimeoutMs ?? 15000;
  const probeAfter = opts.authProbeAfter ?? 3;
  const reportsVisibility = opts.reportVisibility ?? !!opts.pauseWhenHidden;

  const IDLE_TICK_MS = 5000;      // тик проверки молчания (как в PtyTermView)
  const BACKOFF_CAP_MS = 15000;   // потолок Full Jitter
  const BACKOFF_FLOOR_MS = 250;   // пол: random() ≈ 0 на мгновенно падающем
                                  // хендшейке иначе давал бы горячий цикл
  const PROBE_COOLDOWN_MS = 60000; // не чаще одной REST-пробы в минуту
  const WAKE_COOLDOWN_MS = 1000;   // APK шлёт и visibilitychange, и appStateChange
  const WAKE_PROOF_MS = 5000;      // «докажи, что жив» после ping на резюме
  const RECONNECT_DEDUP_MS = 1000; // два будильника резюма подряд = один сокет

  let _sock: WebSocket | null = null;
  let _sockAt = 0;
  let _key = "";
  let _attempts = 0;
  let _reconnectTimer: ReturnType<typeof setTimeout> | null = null;
  let _pingTimer: ReturnType<typeof setInterval> | null = null;
  let _idleTimer: ReturnType<typeof setInterval> | null = null;
  let _openTimer: ReturnType<typeof setTimeout> | null = null;
  let _lastMsgAt = 0;
  /** 0 = idle-вотчдог ВЫКЛЮЧЕН (сервер ещё не доказал, что умеет отвечать). */
  let _idleLimit = 0;
  let _openedThisSocket = false;
  let _failsBeforeOpen = 0;
  let _stopped: StopReason = "";
  let _probing = false;
  let _lastProbeAt = 0;
  let _lastWakeAt = 0;
  let _lifecycleAttached = false;
  /** Чего мы хотим сказать серверу про видимость (последний сигнал побеждает:
   *  visibilitychange или setVisible). Инициализируется состоянием документа —
   *  вкладку могли открыть сразу в фоне, и тогда visibilitychange не придёт. */
  let _hiddenWanted = docHidden();
  /** Что серверу про ТЕКУЩИЙ сокет уже сказано. Новый сокет = сервер считает
   *  нас присутствующими, поэтому сбрасывается в false вместе с сокетом. */
  let _hiddenReported = false;
  let _connected = false;
  let _listeners: Array<(event: E) => void> = [];
  let _connListeners: Array<(connected: boolean) => void> = [];

  function clearTimer(t: ReturnType<typeof setTimeout> | null) {
    if (t) clearTimeout(t);
  }

  function stopPing() {
    if (_pingTimer) { clearInterval(_pingTimer); _pingTimer = null; }
  }

  function stopIdleWatch() {
    if (_idleTimer) { clearInterval(_idleTimer); _idleTimer = null; }
  }

  function stopOpenTimer() {
    if (_openTimer) { clearTimeout(_openTimer); _openTimer = null; }
  }

  function _setConnected(v: boolean) {
    if (_connected === v) return;
    _connected = v;
    _connListeners.forEach((cb) => cb(v));
  }

  function getLastEventId(): number {
    try {
      const raw = localStorage.getItem(LAST_EVENT_ID_KEY);
      const n = raw ? parseInt(raw, 10) : 0;
      return Number.isFinite(n) && n > 0 ? n : 0;
    } catch { return 0; }
  }

  function setLastEventId(id: number) {
    try { localStorage.setItem(LAST_EVENT_ID_KEY, String(id)); } catch { /* ignore */ }
  }

  /** Продвижение позиции в потоке. Отдельный случай — счётчик сервера
   *  перезапустился (ПК перезагрузили, события снова с 1): сохранённое большое
   *  значение навсегда отсекало бы `?since=`-реплей, поэтому откатываемся. */
  function noteEventId(id: number) {
    if (!Number.isFinite(id) || id <= 0) return;
    const cur = getLastEventId();
    if (id > cur) { setLastEventId(id); return; }
    if (cur - id > 1000) {
      opts.log?.("ws:event-id-reset", { cur, id });
      setLastEventId(id);
    }
  }

  /** Скрыта ли страница прямо сейчас (вне браузера — считаем видимой). */
  function docHidden(): boolean {
    return typeof document !== "undefined" && document.visibilityState === "hidden";
  }

  /** Скрыта ли страница (и разрешена ли пауза для этой цели сборки). */
  function paused(): boolean {
    return !!opts.pauseWhenHidden && docHidden();
  }

  /** Досказать серверу видимость, если она разошлась с уже сказанным.
   *
   *  Кадр шлём только в ОТКРЫТЫЙ сокет и только на смену состояния: закрытый
   *  сокет присутствие снимает сам (сервер видит разрыв), а лишний кадр каждую
   *  секунду был бы просто трафиком. Ошибку send глушим — сокет всё равно
   *  закроется, и сервер снимет присутствие по разрыву. */
  function syncVisibility() {
    if (!reportsVisibility) return;
    const s = _sock;
    if (!s || s.readyState !== WebSocket.OPEN) return;
    if (_hiddenReported === _hiddenWanted) return;
    try {
      s.send(JSON.stringify({ type: _hiddenWanted ? "hidden" : "visible" }));
      _hiddenReported = _hiddenWanted;
      opts.log?.("ws:visibility", { hidden: _hiddenWanted });
    } catch { /* закроется сам */ }
  }

  /** Новый сигнал видимости (страница/приложение ушли в фон или вернулись). */
  function noteVisibility(hidden: boolean) {
    _hiddenWanted = hidden;
    syncVisibility();
  }

  /** Отцепить сокет и закрыть. Обработчики снимаем ДО close, иначе его onclose
   *  воскресит цикл реконнектов (правка 2.28.0 + грабля зомби-циклов). */
  function detach(sock: WebSocket | null) {
    if (!sock) return;
    if (_sock === sock) {
      _sock = null;
      stopPing();
      stopIdleWatch();
      stopOpenTimer();
    }
    sock.onopen = null;
    sock.onmessage = null;
    sock.onclose = null;
    sock.onerror = null;
    try { sock.close(); } catch { /* уже закрыт */ }
  }

  /** Будильники уровня страницы. Клиент — синглтон модуля, снимать слушатели
   *  не нужно и некому (иначе пришлось бы считать ссылки). */
  function attachLifecycle() {
    if (_lifecycleAttached) return;
    _lifecycleAttached = true;
    if (typeof document !== "undefined" && document.addEventListener) {
      document.addEventListener("visibilitychange", () => {
        const hidden = document.visibilityState === "hidden";
        // Сначала кадр видимости, потом ладдер: на возврате присутствие обязано
        // встать даже когда сокет жив и будить нечего.
        noteVisibility(hidden);
        if (!hidden) wake();
      });
    }
    if (typeof window !== "undefined" && window.addEventListener) {
      window.addEventListener("online", () => wake());
    }
  }

  function startPing(sock: WebSocket) {
    stopPing();
    if (_sock !== sock) return;
    const tick = () => {
      if (_sock !== sock || sock.readyState !== WebSocket.OPEN) { stopPing(); return; }
      try { sock.send(JSON.stringify({ type: "ping" })); } catch { /* закроется сам */ }
    };
    tick(); // ранний ping → ранний pong → idle-вотчдог вооружается сразу
    _pingTimer = setInterval(tick, pingMs);
  }

  function startIdleWatch(sock: WebSocket) {
    stopIdleWatch();
    if (_sock !== sock) return;
    _idleTimer = setInterval(() => {
      if (_sock !== sock) { stopIdleWatch(); return; }
      if (_idleLimit <= 0) return; // сервер ни разу не ответил — не вооружены
      const silent = Date.now() - _lastMsgAt;
      if (silent > _idleLimit) {
        opts.log?.("ws:idle-kill", { silentMs: silent });
        try { sock.close(); } catch { /* ignore */ } // onclose → обычный бэкофф
      }
    }, IDLE_TICK_MS);
  }

  function authLost(reason: string) {
    _stopped = "auth";
    clearTimer(_reconnectTimer); _reconnectTimer = null;
    detach(_sock);
    _setConnected(false);
    opts.log?.("ws:auth-lost", { reason });
    opts.onAuthLost?.(reason);
  }

  /** Провал хендшейка мог быть и «сеть легла», и «токен протух» — браузер в
   *  обоих случаях даёт 1006. После нескольких подряд спрашиваем сервер по REST. */
  function maybeProbeAuth() {
    if (!opts.probeAuth) return;
    if (_probing || _failsBeforeOpen < probeAfter) return;
    if (Date.now() - _lastProbeAt < PROBE_COOLDOWN_MS) return;
    _probing = true;
    _lastProbeAt = Date.now();
    opts.probeAuth()
      .then((lost) => { _probing = false; if (lost) authLost("probe"); })
      .catch(() => { _probing = false; }); // проба сама не прошла — не приговор
  }

  function schedule() {
    if (_stopped) return;
    // В фоне ладдер не крутим (там, где это разрешено): открытый сокет живёт
    // дальше, а мёртвый поднимем на visibilitychange → wake().
    if (paused()) { opts.log?.("ws:ladder-paused"); return; }
    clearTimer(_reconnectTimer);
    // Full Jitter: delay = random(0, min(cap, base·2^attempts)) — без него все
    // клиенты, отвалившиеся в один момент, ломятся обратно синхронно.
    const ceil = Math.min(BACKOFF_CAP_MS, 1000 * Math.pow(2, _attempts));
    const delay = Math.max(BACKOFF_FLOOR_MS, Math.random() * ceil);
    _attempts++;
    _reconnectTimer = setTimeout(connect, delay);
  }

  /** Текущая идентичность соединения (бросок getKey = «неизвестна»). */
  function currentKey(): string {
    try { return opts.getKey ? opts.getKey() : ""; } catch { return ""; }
  }

  function connect() {
    if (_stopped) return;
    attachLifecycle();

    const key = currentKey();
    // Идентичность сменилась (выбрали другой ПК / другой режим) — прежний сокет
    // слушает чужую машину: закрываем молча, без авто-реконнекта.
    if (_sock && key !== _key) detach(_sock);
    if (_sock && (_sock.readyState === WebSocket.OPEN || _sock.readyState === WebSocket.CONNECTING)) return;

    let url = "";
    try { url = opts.getUrl(getLastEventId()); } catch { url = ""; }
    if (!url) {
      // Слушать нечего (нет выбранного ПК или авторизации). Раньше здесь могло
      // остаться «всё хорошо» — экраны считали компьютер на связи и молча
      // получали ошибки на каждый запрос.
      _key = "";
      _setConnected(false);
      return;
    }
    _key = key;
    if (paused()) { opts.log?.("ws:connect-deferred"); return; }

    const sock = new WebSocket(url);
    _sock = sock;
    _sockAt = Date.now();
    _openedThisSocket = false;
    _hiddenReported = false; // новый сокет: сервер считает нас присутствующими

    // Вотчдог хендшейка: сокет может «висеть» в CONNECTING без onclose.
    stopOpenTimer();
    _openTimer = setTimeout(() => {
      _openTimer = null;
      if (_sock === sock && sock.readyState !== WebSocket.OPEN) {
        opts.log?.("ws:open-timeout");
        try { sock.close(); } catch { /* ignore */ }
      }
    }, openTimeoutMs);

    sock.onopen = () => {
      if (sock !== _sock) return; // устаревший сокет — не воскрешать
      stopOpenTimer();
      clearTimer(_reconnectTimer); _reconnectTimer = null;
      _openedThisSocket = true;
      _failsBeforeOpen = 0;
      _attempts = 0;
      _setConnected(true);
      // lastMsg сбрасываем сейчас, чтобы не сработать на паузе МЕЖДУ
      // соединениями; лимит — 0, пока сервер не доказал, что отвечает.
      _lastMsgAt = Date.now();
      _idleLimit = 0;
      startPing(sock);
      startIdleWatch(sock);
      // Страница могла уйти в фон, пока шёл хендшейк, — досказываем.
      syncVisibility();
    };

    sock.onmessage = (ev) => {
      if (sock !== _sock) return;
      _lastMsgAt = Date.now();
      let raw: unknown;
      try { raw = JSON.parse(ev.data as string); } catch { return; }
      const p = opts.parse ? opts.parse(raw) : defaultParse<E>(raw);
      if (p.keepalive) {
        // Сервер ответил на наш ping — только теперь молчание что-то значит.
        _idleLimit = pingMs * idleFactor;
        return;
      }
      if (typeof p.connected === "boolean") _setConnected(p.connected);
      if (typeof p.id === "number") noteEventId(p.id);
      if (p.event !== undefined) {
        const e = p.event as E;
        _listeners.forEach((cb) => cb(e));
      }
    };

    sock.onclose = (ev) => {
      if (sock !== _sock) return; // закрылся устаревший сокет
      _sock = null;
      stopOpenTimer();
      stopPing();
      stopIdleWatch();
      _setConnected(false);
      if (_stopped) return;
      // «На вырост»: сегодня и агент, и релей отдают 401/403 ДО upgrade, поэтому
      // браузер видит 1006. Когда сервер начнёт закрывать честно — узнаем правду
      // за одно закрытие, без REST-пробы.
      if (ev.code === 1008 || ev.code === 4401 || ev.code === 4403) {
        authLost(`close-${ev.code}`);
        return;
      }
      if (!_openedThisSocket) {
        _failsBeforeOpen++;
        maybeProbeAuth();
      }
      schedule();
    };

    sock.onerror = () => { try { sock.close(); } catch { /* ignore */ } };
  }

  function hardReconnect() {
    // Дедуп: сокет к ТОЙ ЖЕ цели создан только что и ещё в хендшейке — рвать его
    // незачем. На возврате в приложение будильников два (visibilitychange и
    // Capacitor appStateChange), без дедупа получалось два сокета подряд.
    // Смена ПК сюда не попадает: там ключ другой и реконнект обязателен.
    if (_sock && _sock.readyState === WebSocket.CONNECTING
        && currentKey() === _key && Date.now() - _sockAt < RECONNECT_DEDUP_MS) {
      opts.log?.("ws:reconnect-deduped");
      return;
    }
    clearTimer(_reconnectTimer); _reconnectTimer = null;
    // Состояние связи НЕ сбрасываем: детач гасит onclose старого сокета, а
    // мигать баннером «Переподключение…» на каждом возврате в приложение нельзя.
    detach(_sock);
    connect();
  }

  function resume() {
    _stopped = "";
    _attempts = 0;
    _failsBeforeOpen = 0;
    connect();
  }

  function reconnect() {
    _stopped = "";
    _attempts = 0;
    _failsBeforeOpen = 0;
    hardReconnect();
  }

  function wake() {
    if (_stopped) return; // из manual/auth выводят только resume()/reconnect()
    // Страница всё ещё скрыта (сработал `online`, а не возврат на вкладку) —
    // будить нечего. Важно выйти ДО кулдауна: иначе такой будильник «съел» бы
    // право на реальный wake, если вкладку откроют через полсекунды.
    if (paused()) return;
    const now = Date.now();
    if (now - _lastWakeAt < WAKE_COOLDOWN_MS) return;
    _lastWakeAt = now;

    const s = _sock;
    if (!s || s.readyState === WebSocket.CLOSED || s.readyState === WebSocket.CLOSING) {
      _attempts = 0;
      connect();
      return;
    }
    // CONNECTING не трогаем: незавершённый хендшейк сторожит _openTimer.
    if (s.readyState !== WebSocket.OPEN) return;
    if (_idleLimit <= 0) {
      // Сервер не отвечает на ping — доказать жизнь сокета нечем. Ведём себя
      // как раньше: полный реконнект (на LAN он же даёт реплей через ?since=).
      hardReconnect();
      return;
    }
    try { s.send(JSON.stringify({ type: "ping" })); } catch { hardReconnect(); return; }
    const at = _lastMsgAt;
    setTimeout(() => {
      if (_sock === s && _lastMsgAt === at) {
        opts.log?.("ws:wake-no-proof");
        try { s.close(); } catch { /* ignore */ } // onclose → обычный бэкофф
      }
    }, WAKE_PROOF_MS);
  }

  function disconnect() {
    _stopped = "manual";
    clearTimer(_reconnectTimer); _reconnectTimer = null;
    detach(_sock);
    _key = "";
    _setConnected(false);
  }

  return {
    connect,
    resume,
    reconnect,
    wake,
    disconnect,
    setVisible: (visible: boolean) => noteVisibility(!visible),
    isConnected: () => _connected,
    onEvent(cb) {
      _listeners.push(cb);
      return () => { _listeners = _listeners.filter((l) => l !== cb); };
    },
    onConnectionChange(cb) {
      _connListeners.push(cb);
      cb(_connected); // fire immediately with current state
      return () => { _connListeners = _connListeners.filter((l) => l !== cb); };
    },
  };
}
