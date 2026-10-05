// Сколько ждать тишины, прежде чем сказать агенту новый размер терминала.
//
// Зачем отдельная политика. Каждый resize — это SIGWINCH и полная перерисовка
// TUI, а у агента, который рисует строками по абсолютным адресам и НЕ стирает
// экран (Kimi: режимы [1004, 2004]), кадры чужой высоты навсегда оседают в
// прокрутке зрителя — разбор 11.08.2026,
// Контекст/Журнал/2026-08-11_kimi-strannyy-vyvod-na-telefone.md.
//
// Общий дебаунс в 350 мс ловит серию промежуточных высот внутри одной анимации
// (поворот, открытие клавиатуры), но НЕ ловит качели панели браузера: адресная
// строка уезжает и возвращается сама, замер боевой сессии — 48x31 → 48x30 →
// 48x28 → 48x30 за десять секунд, то есть шаги разнесены во времени сильно
// шире окна тишины, и каждый уходил агенту отдельным resize.
//
// Признак качели — высота гуляет при НЕИЗМЕННОЙ ширине. Такой правке даём
// отстояться дольше, и это лечит её само собой: пока идёт ожидание, высота
// обычно возвращается к уже отправленной, а одинаковый размер повторно не
// отправляется. Настоящее изменение (поворот, другое окно, смена ширины)
// проходит прежним быстрым путём — там ждать нечего.
//
// ⚠ Порог «качель — это ±1–3 строки» (JITTER_ROWS, 2.55.20) оказался дырой:
// клавиатура и шторки меняют высоту на 10–20 строк ПРИ ТОЙ ЖЕ ШИРИНЕ, и такой
// дрейф уходил агенту быстрым путём. Боевой лог 13.08.2026: 48x31 → 48x12 →
// 48x10 → 48x31 за 22 секунды — три полных перерисовки TUI, и каждая через
// ED2-пуш зарастила историю зеркала дублями. На телефоне ЛЮБОЕ изменение
// высоты при той же ширине — панель, клавиатура или шторка (поворот меняет
// ширину), поэтому отстаивается всё; настоящий вертикальный ресайз окна на
// десктопе просто подождёт те же две секунды.

/** Размер терминала в знакоместах. */
export type TermSize = { cols: number; rows: number };

/** Окно тишины для настоящего изменения размера. */
export const RESIZE_QUIET_MS = 350;

/**
 * Окно тишины для качели по высоте. Две секунды берут с запасом шаг
 * боевых качелей (~2,5 с между ступенями) — за это время панель браузера
 * успевает вернуться на место.
 */
export const RESIZE_SETTLE_MS = 2000;

/**
 * Сколько ждать перед отправкой `next`, если агенту уже отправлен `sent`.
 *
 * `sent` с нулями — размер ещё не отправляли: ждать долго нечего, это первая
 * настоящая величина.
 */
export function resizeDelayMs(sent: TermSize, next: TermSize): number {
  if (sent.cols <= 0 || sent.rows <= 0) return RESIZE_QUIET_MS;
  // Ширина поехала — это поворот или другое окно, а не панель браузера.
  if (sent.cols !== next.cols) return RESIZE_QUIET_MS;
  return sent.rows !== next.rows ? RESIZE_SETTLE_MS : RESIZE_QUIET_MS;
}

/**
 * A debounced resize may have been queued before the software keyboard opened.
 * Once it is open, that measurement is no longer an authoritative PTY size;
 * the close/refit event will enqueue the current logical grid afterwards.
 */
export function shouldFlushResize(sent: TermSize, next: TermSize, keyboardOpen: boolean): boolean {
  if (keyboardOpen) return false;
  if (next.cols < 2 || next.rows < 2) return false;
  return sent.cols !== next.cols || sent.rows !== next.rows;
}

/**
 * ДОСТАВЛЕНА ЛИ НАША ВМЕСТИМОСТЬ ТОМУ СОКЕТУ, ПО КОТОРОМУ ИДЁТ ЗАПРОС КАДРА
 * (ST-08, T-32, I-10).
 *
 * Зачем. Два зрителя одной ширины и разной высоты: высокий получает кадр в
 * общей сетке PTY = min(A, B), считает его «возможно чужим» и просит снова —
 * круг «кадр → resync → запрос» раз в секунду. Сходился он только REST-опросом
 * `/state`, а в скрытой вкладке опроса нет, и круг был вечным (журнал
 * 2026-09-03, п. 4; карта ST-08). Разорвать его можно по порядку сокета:
 * сервер читает `resize` и `screen` одной горутиной по порядку
 * (api_pty.go:1377-1548), а кадр ждёт окончания смены геометрии
 * (session_screen.go:829-839). Значит, кадр на запрос, отправленный ПОСЛЕ
 * доставки нашей вместимости в этот же сокет, снят уже в min(всех, включая
 * нас) — это и есть сетка PTY, и её можно принять (`frameGeometryAction`,
 * вход `capacityDelivered`).
 *
 * Истина только если одновременно:
 *   • вместимость измерена без клавиатуры — строки, измеренные под
 *     клавиатурой, вместимостью не являются (I-09);
 *   • отправлена в ЭТОТ сокет — новый сокет на сервере это новый Viewer без
 *     размера, прежняя отправка ему ничего не сказала;
 *   • равна текущему отчёту — иначе сервер знает старую вместимость;
 *   • дебаунс не взведён — иначе новая вместимость ещё в пути.
 *
 * Сокеты сравниваются по тождеству: правило не знает, что такое WebSocket.
 */
export function capacityDelivered(s: {
  /** Что и когда последний раз ушло серверу (`null` — ещё ничего). */
  sent: TermSize | null;
  /** Текущий отчёт о вместимости (`reportSizeRef`). */
  report: TermSize;
  /** Строки отчёта измерены без клавиатуры. */
  measuredWithoutKeyboard: boolean;
  /** Взведён таймер отложенной отправки размера. */
  resizePending: boolean;
  /** Сокет, в который ушёл `sent`. */
  sentOnSocket: unknown;
  /** Сокет, в который сейчас уйдёт запрос кадра. */
  socket: unknown;
}): boolean {
  const { sent, report } = s;
  if (!s.measuredWithoutKeyboard || s.resizePending) return false;
  if (!sent || !validSize(report)) return false;
  if (s.socket == null || s.sentOnSocket !== s.socket) return false;
  return sent.cols === report.cols && sent.rows === report.rows;
}

/**
 * ЧТО СООБЩИТЬ СЕРВЕРУ КАК ВМЕСТИМОСТЬ — ИЛИ НИЧЕГО (I-10).
 *
 * Серверный минимум по зрителям считается по их вместимостям. Если вместо
 * вместимости отправить принятую сетку терминала (`term.cols/rows`), общий
 * PTY больше не вырастет: зритель сам подтверждает чужой минимум. Раньше
 * `sizeForServer` при пустом отчёте отдавал именно сетку терминала, а ветка
 * клавиатуры в `fitLocal` писала в отчёт логическую высоту (дыры (а) и (б) в
 * карте ST-08). Поэтому неизвестная вместимость — это `null`: лучше не
 * сообщить ничего (сервер держит прежний размер, у агентских сессий его и
 * так держит `targetSizeLocked`), чем сообщить неправду.
 *
 * `termCols/termRows` правило получает НАМЕРЕННО и никогда не возвращает:
 * так вызывающий код не может «на всякий случай» подставить сетку, а тест
 * проверяет, что её здесь нет.
 *
 * Вместимость, измеренная без клавиатуры, годится и при поднятой клавиатуре:
 * так новый сокет, открытый под клавиатурой, всё равно получает честный
 * размер (карта ST-08, шаг 2: onopen при клавиатуре).
 */
export function capacityForServer(s: {
  report: TermSize;
  measuredWithoutKeyboard: boolean;
  termCols?: number;
  termRows?: number;
}): TermSize | null {
  if (!s.measuredWithoutKeyboard || !validSize(s.report)) return null;
  return { cols: s.report.cols, rows: s.report.rows };
}

/**
 * ЧТО «УЖЕ ОТПРАВЛЕНО» ЭТОМУ СОКЕТУ (ST-08, I-10).
 *
 * Новый сокет на сервере — новый Viewer без размера: он выпадает из минимума
 * по зрителям, пока не получит свой resize. Прежнее «уже отправлено» ему
 * ничего не говорит, и сравнивать с ним нельзя: переподключение при поднятой
 * клавиатуре молчало (force-путь), а после её закрытия flushResize видел
 * sent == size и тоже молчал — сервер так и не узнавал нашу вместимость
 * (карта ST-08, gap «после переподключения сервер знает вместимость»).
 * Отправленное в другой сокет — это «ещё ничего» ({0,0}): resizeDelayMs даёт
 * быстрый путь, shouldFlushResize — отправку.
 */
export function sentBaseline(sent: TermSize, sentOnSocket: unknown, socket: unknown): TermSize {
  if (socket == null || sentOnSocket !== socket) return { cols: 0, rows: 0 };
  return sent;
}

/** Запрос кадра, как он ушёл: сокет, ревизия сетки, номер (если был), была ли
 * к этому моменту наша вместимость доставлена этому сокету и получен ли уже
 * ответ на него (answered — ставит адаптер при первом кадре-ответе). */
export type ScreenRequestNote = {
  socket: unknown; geomRev: number; req: number | null; delivered: boolean; answered?: boolean;
  /** Вместимость, сообщённая к моменту запроса (отчёт): кадр-сетка не больше неё. */
  capacity?: TermSize | null;
};

/** Пол сетки PTY на сервере (manager.go, 20×10): меньше него PTY не бывает,
 * даже если вместимость зрителя меньше (телефон в ландшафте: 103×5 → 103×10). */
export const PTY_FLOOR: TermSize = { cols: 20, rows: 10 };

/**
 * МОЖЕТ ЛИ КАДР БЫТЬ СЕТКОЙ PTY ПОСЛЕ НАШЕЙ ВМЕСТИМОСТИ (ST-08).
 *
 * Сервер держит PTY по минимуму зрителей, куда входим и мы, — сетка не шире и
 * не выше нашей вместимости (с поправкой на пол 20×10). Кадр больше — снят до
 * нашего resize или чужой: доказательством сетки он не служит, и решает
 * прежнее правило (отказ по ширине, resync по высоте). Замер qa:terminal
 * 14.09: снапшот-шов с кадром на колонку шире вместимости принимался как
 * сетка PTY вместо отказа `snapshot-width`.
 */
export function frameFitsCapacity(snapCols: unknown, snapRows: unknown, capacity: TermSize | null | undefined): boolean {
  if (!capacity || !validSize(capacity)) return false;
  if (typeof snapCols !== "number" || typeof snapRows !== "number") return false;
  return snapCols <= Math.max(capacity.cols, PTY_FLOOR.cols) && snapRows <= Math.max(capacity.rows, PTY_FLOOR.rows);
}

/**
 * ЯВЛЯЕТСЯ ЛИ КАДР ОТВЕТОМ ИМЕННО НА ЭТОТ ЗАПРОС (ST-08, T-32).
 *
 * Доказательством «кадр снят в сетке PTY после нашей вместимости» служит только
 * ответ на НАШ запрос. Кадр без просьбы (повтор старого агента) или кадр чужой
 * операции ничего не доказывает. Замер qa:terminal 14.09 (T-24: пачка из 20
 * кадров старого агента в чужой высоте с ревизией последнего запроса): при
 * сопоставлении по одной ревизии каждый кадр пачки принимался как сетка PTY,
 * клиент качался 19↔23 строки и дозапрашивал кадр после каждой смены — 4
 * просьбы вместо одной.
 *
 * Сопоставление:
 *   • сокет тот же — кадр прежнего соединения ничего не доказывает;
 *   • есть координатор восстановления (ticket определён) — только кадр, который
 *     он приписал запросу в пути (ticket = номер запроса); кадр без просьбы
 *     (ticket null) — нет;
 *   • координатора нет (откат recoveryV1) — один ответ на запрос: после первого
 *     совпавшего кадра запись израсходована (answered). Совпадение — по req,
 *     если он есть, иначе по ревизии сетки.
 */
export function frameAnswersRequest(
  last: ScreenRequestNote | null,
  frame: { socket: unknown; geomRev: unknown; req: unknown; ticket?: number | null },
): boolean {
  if (!last || frame.socket == null || last.socket !== frame.socket) return false;
  if (frame.ticket !== undefined) return frame.ticket !== null && last.req !== null && frame.ticket === last.req;
  if (last.answered) return false;
  if (typeof frame.req === "number") return last.req === frame.req;
  return typeof frame.geomRev === "number" && frame.geomRev === last.geomRev;
}

/**
 * КАДР — СЕТКА PTY ПОСЛЕ НАШЕЙ ВМЕСТИМОСТИ: вход `capacityDelivered` у
 * frameGeometryAction. ЕДИНСТВЕННОЕ место этого правила (ST-08); три условия
 * вместе:
 *   1. ответ именно на наш запрос (frameAnswersRequest);
 *   2. к моменту запроса вместимость была доставлена этому сокету (delivered).
 *      Ревизия сетки меняется вместе с отчётом о вместимости (noteReportSize),
 *      поэтому внутри одной ревизии «не доставлено → доставлено» бывает только
 *      отправкой того же отчёта (риск A4);
 *   3. кадр не больше сообщённой вместимости с поправкой на пол PTY
 *      (frameFitsCapacity). Кадр шире или выше — снят до нашего resize или
 *      чужой.
 * Любое сомнение — false: прежний путь resync.
 *
 * Чистая: запись не расходуется. Вызывать ДО пометки `answered`, иначе у
 * отката без координатора (сопоставление по первому совпавшему кадру) ответ
 * уже израсходован.
 */
export function frameAnswersDeliveredRequest(
  last: ScreenRequestNote | null,
  frame: { socket: unknown; geomRev: unknown; req: unknown; ticket?: number | null },
  snap: { cols: unknown; rows: unknown },
): boolean {
  return !!last && last.delivered && frameAnswersRequest(last, frame)
    && frameFitsCapacity(snap.cols, snap.rows, last.capacity);
}

function validSize(v: TermSize): boolean {
  return Number.isFinite(v.cols) && Number.isFinite(v.rows) && v.cols >= 2 && v.rows >= 2;
}
