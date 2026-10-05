import { useCallback, useEffect, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import {
  formatAgo, formatAgoValue, isAgentKind, isPcOffline, mapApiError, ptyDisplayTitle, useToast,
  type PtyOutcome,
} from "@tgcontrol/shared";
import { listPtySessions, onConnectionChange, ptyInput } from "../api";
import type { PtySessionInfo, PtyInputBody } from "../api";
import { haptic, hapticError, hapticSuccess } from "../telegram";
import { IconClose, IconHourglass, IconRefresh, IconWarning } from "./icons";
import { t } from "../i18n";
import { markHomeStep } from "../homeProgress";
import { outcomeAdviceKey } from "../ptyTerm/rules";
import { humanDeviceName, onSelectedDeviceChange } from "../devices";
import { getAgentUpdateInfo } from "../api";
import { getMode, getSelectedDeviceAgentVersion, getSelectedDeviceName, getTerminalContextKey } from "../config";
import { lastViewedTerminal, terminalToContinue } from "../terminalHistory";
import {
  agentUpdateNotice,
  agentUpdateNoticeDismissed,
  dismissAgentUpdateNotice,
  fetchLatestVersion,
} from "../agentUpdateNotice";

/** Сколько ждём реакции агента, прежде чем честно сказать «не отреагировал». */
const ANSWER_WAIT_MS = 12_000;

/** Свой ПК опрашиваем часто — на нём висит вопрос, ради которого открыли экран. */
const OWN_POLL_MS = 6_000;

/**
 * Сколько исходов показываем на главной. Журнал агента хранит до 30 записей за
 * 12 часов — вывалив их все, мы похоронили бы под ними живой вопрос, ради
 * которого экран и открыт. Показываем самые свежие, остальные ждут в терминалах.
 */
const OUTCOMES_SHOWN = 3;

/** «Понятно» по исходу — локальная память клиента: сервер о прочтении не знает. */
const SEEN_KEY = "home.outcomesSeen.v1";
const SEEN_LIMIT = 60;

/**
 * Сколько кнопки быстрого ответа не принимают нажатий после того, как карточка
 * ПОЯВИЛАСЬ на экране.
 *
 * Сводка приезжает ответом ПК (0,5–2 с на облаке, и это происходит при каждом
 * возврате на главную — блок перемонтируется), вставляется НАД кнопкой
 * «+ Новый терминал» и сдвигает низ экрана на 300+px. Палец, занесённый на
 * кнопку, приземляется на «Да»/«Нет» — то есть отправляет агенту нажатие
 * клавиши, которого человек не выбирал. Место мы держим скелетоном (см. ниже),
 * но точное совпадение высот не гарантировано, поэтому свежая карточка ещё
 * мгновение не принимает ответ.
 */
const ANSWER_ARM_MS = 400;

/** Последняя известная форма сводки — чтобы держать место до ответа ПК. */
const LAYOUT_KEY = "home.act.layout.v1";
const LAYOUT_MAX = 3;

interface HomeLayout {
  /** Карточек «Требует внимания» (высокие, ~150px). */
  cards: number;
  /** Строк «Сейчас работает» + плитка «Продолжить» (низкие, 44px). */
  rows: number;
}

function clampLayout(n: unknown): number {
  const v = Number(n);
  return Number.isFinite(v) ? Math.max(0, Math.min(LAYOUT_MAX, Math.trunc(v))) : 0;
}

function readLayout(): HomeLayout {
  try {
    const raw = JSON.parse(localStorage.getItem(LAYOUT_KEY) || "null");
    return { cards: clampLayout(raw?.cards), rows: clampLayout(raw?.rows) };
  } catch {
    return { cards: 0, rows: 0 };
  }
}

function writeLayout(layout: HomeLayout): void {
  try { localStorage.setItem(LAYOUT_KEY, JSON.stringify(layout)); } catch { /* приватный режим */ }
}

/** Быстрый ответ на карточке: подпись кнопки + тело запроса для ptyInput. */
interface QuickAnswer {
  id: string;
  label: string;
  body: PtyInputBody;
}

/** Пункт нумерованного меню, разобранный из текста вопроса. */
interface ChoiceOption {
  num: number;
  label: string;
}

/**
 * Пункты меню разбираем из самого hint: «Выберите вариант: 1) применить
 * 2) показать diff 3) отмена». Отдельного списка пунктов агент не присылает, но
 * если номер и его подпись стоят в вопросе рядом, кнопка «2. показать diff»
 * обещает ровно то, что человек увидел бы в терминале, — вслепую он ничего не
 * разрешает.
 *
 * Условие строгое: нумерация подряд с единицы. Иначе «версия 2.1) …» или любое
 * число в тексте превратилось бы в кнопку, отправляющую агенту цифру.
 */
const CHOICE_RE = /([1-9])\s*[).\]]\s+(\S[^\n]*?)(?=\s+[1-9]\s*[).\]]\s|$)/g;

function parseChoices(hint: string | undefined): ChoiceOption[] {
  if (!hint) return [];
  const out: ChoiceOption[] = [];
  for (const m of hint.matchAll(CHOICE_RE)) {
    const num = Number(m[1]);
    if (num !== out.length + 1) return [];
    out.push({ num, label: m[2].trim().replace(/[\s.,;:]+$/, "") });
  }
  // Один «пункт» — это не меню, а совпадение в обычной фразе.
  return out.length >= 2 ? out : [];
}

/** Сколько символов подписи помещается на кнопке, не разваливая ряд. */
const CHOICE_LABEL_MAX = 22;

/** Подпись кнопки: номер обязателен (его и понимает агент), текст — если он
 *  короткий; длинную фразу режем, полный текст остаётся в title. */
function choiceLabel(o: ChoiceOption): string {
  if (!o.label) return String(o.num);
  const short = o.label.length > CHOICE_LABEL_MAX
    ? `${o.label.slice(0, CHOICE_LABEL_MAX - 1).trimEnd()}…`
    : o.label;
  return `${o.num}. ${short}`;
}

/** Сколько цифр принимает агент (таблица ptyKeyBytes в api_pty.go). */
const CHOICE_KEYS_MAX = 3;

/**
 * Набор кнопок по типу вопроса — тот же разбор, что на экране терминала
 * (PtyTermView: y/n с Enter, цифры меню без него, Esc без него). Сами байты
 * выбирает агент по `key` (таблица ptyKeyBytes в internal/web/api_pty.go), и она
 * умышленно повторяет sendRaw экрана терминала — ответ с карточки неотличим от
 * ответа руками.
 *
 * Вопрос с меню (kind="choice") — самый частый у Codex и Claude Code, и до сих
 * пор он упирался в тупик: обрезанный текст, одна кнопка «Отмена» и совет
 * открыть терминал. Теперь пункты, если они названы в самом вопросе,
 * превращаются в кнопки «1»/«2»/«3». Не разобрались (у Claude hint бывает
 * «Claude: разрешить инструмент» без пунктов вовсе) — ведём себя как раньше:
 * никаких цифр вслепую, только отмена и дорога в терминал.
 *
 * Пустой список = тип вопроса не распознан (или агент старый и hint_kind не
 * присылает): угадывать нечего, остаётся «Ответить →» на экран терминала.
 */
function quickAnswers(s: PtySessionInfo): QuickAnswer[] {
  const cancel: QuickAnswer = {
    id: "esc",
    label: t("home.act.cancelQuestion"),
    body: { key: "esc" },
  };
  switch (s.hint_kind) {
    case "yes_no":
      return [
        { id: "y", label: t("home.act.yes"), body: { key: "y" } },
        { id: "n", label: t("home.act.no"), body: { key: "n" } },
        cancel,
      ];
    case "enter":
      return [{ id: "enter", label: t("pty.enterKey"), body: { key: "enter" } }, cancel];
    case "choice": {
      const options = parseChoices(s.hint).slice(0, CHOICE_KEYS_MAX);
      return [
        ...options.map((o) => ({
          id: String(o.num),
          label: choiceLabel(o),
          body: { key: String(o.num) } as PtyInputBody,
        })),
        cancel,
      ];
    }
    // Свободный текст с карточки не ответить — но снять вопрос можно, и это
    // лучше, чем тупик «только открыть терминал».
    case "text":
      return [cancel];
    default:
      return [];
  }
}

/**
 * Почему на карточке нет кнопок самого ответа. Молчаливое отсутствие кнопок
 * читается как «сломалось»; строка объясняет, куда идти отвечать.
 */
function answerNoteKey(s: PtySessionInfo): string {
  if (s.hint_kind === "text") return "home.act.textInTerminal";
  if (s.hint_kind !== "choice") return "";
  const options = parseChoices(s.hint);
  if (options.length === 0) return "home.act.choiceInTerminal";
  // Пунктов больше, чем цифр принимает агент: кнопки честно закрывают первые
  // три, про остальные говорим прямо.
  return options.length > CHOICE_KEYS_MAX ? "home.act.choiceMoreInTerminal" : "";
}

/**
 * Ключ эпизода: пока агент стоит на одном вопросе, он не меняется. Сменился —
 * значит агент отреагировал (пошёл дальше или спросил другое), и подпись под
 * кнопками пора убирать.
 */
function episodeKey(s: PtySessionInfo): string {
  return `${s.status || ""}:${s.status_at || 0}:${s.hint || ""}`;
}

/** Состояние отправленного с карточки ответа. */
interface ReplyState {
  /** Что именно нажали — чтобы подпись говорила «Отправлено «Да»», а не «ок». */
  label: string;
  /**
   * sending — запрос в полёте; sent — агент подтвердил, что ввод дошёл до PTY;
   * error — ТОЧНО не доставлено (повтор безопасен, кнопки возвращаем);
   * unknown — доставка неизвестна (таймаут/обрыв): ввод мог примениться,
   * поэтому кнопки остаются заблокированными, см. answerDelivery.
   */
  status: "sending" | "sent" | "error" | "unknown";
  /** Эпизод на момент отправки; разошёлся с текущим — ответ сработал. */
  episode: string;
  at: number;
  error?: string;
}

/**
 * Честная причина отказа — только для случая «ответ ТОЧНО не доставлен» (см.
 * answerDelivery). Общий mapApiError ветвится по HTTP-статусу и для ptyInput
 * врёт: 409 у него — «компьютер привязан к другому аккаунту», 410 — «код
 * подключения устарел». Поэтому машинные коды эндпоинта разбираем сами, а
 * остальное (выключенный ПК) отдаём общему маппингу.
 */
function answerError(e: unknown): string {
  switch ((e as { code?: string })?.code) {
    case "prompt_changed": return t("home.act.answerChanged");
    case "pty_dead": return t("home.act.answerDead");
    case "pty_not_found": return t("home.act.answerGone");
    default: return mapApiError(e);
  }
}

/**
 * Доставлен ли ответ — вопрос отдельный от «была ли ошибка».
 *
 * «Нет» и «не знаю» ведут себя по-разному, и путать их нельзя: повторное
 * нажатие шлёт агенту ВТОРОЙ «y», а `expect_status_at` спасает только когда
 * агент уже сдвинулся с вопроса. Если первый ввод дошёл, но агент ещё стоит на
 * том же промпте, повтор применится вторым нажатием клавиши — ровно то, чего
 * фича обещает не делать.
 *
 *  • not_delivered — ответ ТОЧНО не применён: любой 4xx (агент/релей получил
 *    запрос и отказал — вопрос сменился, терминал закрыт, ключ не понят) и
 *    502 pc_offline (релей не нашёл ПК в хабе, команда не отправлялась);
 *  • unknown — всё остальное: таймаут запроса, обрыв сети, 5xx, pc_timeout и
 *    504 pc_offline (агент отвалился, ПОКА команда летела). Ввод мог дойти.
 */
function answerDelivery(e: unknown): "not_delivered" | "unknown" {
  const code = (e as { code?: string })?.code;
  const status = (e as { status?: number })?.status;
  // Агент не нашёлся на релее ещё до отправки команды (proxy.go: 502) — в
  // отличие от 504, где связь оборвалась уже в процессе.
  if (code === "pc_offline" && status === 502) return "not_delivered";
  if (typeof status === "number" && status >= 400 && status < 500) return "not_delivered";
  return "unknown";
}

/** Ответ ещё «про эту карточку»: либо вопрос тот же, либо это свежая ОШИБКА.
 *  Ошибку показываем и после смены эпизода: типичный отказ — «агент уже
 *  спрашивает о другом» (409 prompt_changed), и именно тогда эпизод и
 *  меняется. Промолчать здесь значило бы съесть единственное сообщение о том,
 *  что нажатие не сработало.
 *  Статус "unknown" сюда НЕ попадает намеренно: смена эпизода как раз и
 *  снимает неизвестность (агент сдвинулся — значит ввод дошёл), и держать
 *  предупреждение поверх УЖЕ ДРУГОГО вопроса, блокируя по нему кнопки, было бы
 *  враньём. */
function replyIsCurrent(r: ReplyState, episode: string): boolean {
  return r.episode === episode
    || (r.status === "error" && Date.now() - r.at < ANSWER_WAIT_MS);
}

/** Выбросить ответы, которые уже ни о чём не говорят (агент отреагировал,
 *  ошибка отвисела своё, сессии больше нет) — иначе словарь растёт всю жизнь
 *  экрана. */
function pruneReplies(
  prev: Record<string, ReplyState>,
  list: PtySessionInfo[],
): Record<string, ReplyState> {
  const episodes = new Map(list.map((s) => [s.id, episodeKey(s)]));
  const next: Record<string, ReplyState> = {};
  let changed = false;
  for (const [id, r] of Object.entries(prev)) {
    const episode = episodes.get(id);
    if (episode !== undefined && replyIsCurrent(r, episode)) next[id] = r;
    else changed = true;
  }
  // Возвращаем ту же ссылку, если ничего не выкинули: лишний setState = лишний
  // рендер на каждом такте поллинга.
  return changed ? next : prev;
}

/** Один исход = один эпизод: повторное падение того же терминала приходит с
 *  новым `at`, поэтому скрытое «Понятно» его не глушит. */
function outcomeKey(o: PtyOutcome): string {
  return `${o.id}:${o.status}:${o.at}`;
}

function readSeenOutcomes(): string[] {
  try {
    const raw = JSON.parse(localStorage.getItem(SEEN_KEY) || "[]");
    return Array.isArray(raw) ? raw.filter((k): k is string => typeof k === "string") : [];
  } catch {
    return [];
  }
}

/**
 * Текст исхода — из машинного кода причины (сервер текстов не присылает).
 *
 * У ошибки печатаем САМУ строку вывода: обобщение «мелькнула строка, похожая на
 * ошибку» нельзя ни проверить, ни отличить упавшую сборку от красной строки,
 * которую агент прочитал и исправил. Обобщение остаётся фолбэком для старого
 * агента, который строку не присылает.
 */
function outcomeReason(o: PtyOutcome): string {
  if (o.status === "error") return o.hint || t("home.act.outcomeError");
  return t(o.reason === "detached" ? "home.act.outcomeDetached" : "home.act.outcomeExited");
}

/**
 * «Работает» — терминал занят прямо сейчас: агент печатает (working) либо
 * печатал и застрял на месте (stalled — для человека это та же занятость, а не
 * свобода). Предикат общий для заголовка секции и её строк: пока он существовал
 * дважды, заголовок мог утверждать одно, а строка под ним — другое.
 */
function isRunning(s: PtySessionInfo): boolean {
  return s.status === "working" || s.status === "stalled";
}

/**
 * Плашка про версию АГЕНТА на главной — тонкая, в потоке, с крестиком.
 *
 * До этого состояние обновления агента было видно только в «Моих компьютерах»
 * и в настройках, куда за ним не идут: человек месяцами жил на старом агенте,
 * не зная, что обновление уже вышло (auto) или что самообновление отстало
 * безвозвратно (stuck — тут нужен поход к компьютеру).
 *
 * Источник правды зависит от режима: в облаке версию выбранной машины знает
 * конфиг (её положил selectDevice из списка устройств), а актуальную — манифест
 * релея; в локальном режиме (окно exe на самом ПК, LAN с телефона) обе версии
 * сообщает сам агент через /api/system/version. Кнопки «Перезапустить» здесь
 * нет намеренно — она уже есть в настройках и панели ПК, а плашка отвечает
 * только на «стоит ли об этом знать».
 *
 * «Скрыть» запоминается на конкретную версию (agentUpdateNotice.ts), поэтому
 * при каждом возврате на главную плашка не мигает: dismissed читается
 * синхронно из localStorage ещё до ответа сети.
 */
function AgentUpdateNoticeBanner() {
  // kind: auto/stuck — облако (сравнение с манифестом релея);
  // ready/available — локальный режим (флаги самого агента).
  const [notice, setNotice] = useState<
    { kind: "auto" | "stuck" | "ready" | "available"; version: string; latest: string } | null
  >(null);

  useEffect(() => {
    let cancelled = false;
    const load = async () => {
      if (getMode() === "cloud") {
        const version = getSelectedDeviceAgentVersion();
        const latest = await fetchLatestVersion();
        if (cancelled) return;
        const n = agentUpdateNotice(version, latest);
        setNotice(
          n && !agentUpdateNoticeDismissed(n.latest)
            ? { kind: n.state, version: version.trim(), latest: n.latest }
            : null,
        );
        return;
      }
      // Локальный режим: версии и флаги знает сам агент, релея в цепочке нет.
      try {
        const info = await getAgentUpdateInfo();
        if (cancelled) return;
        const latest = (info.latest || "").trim();
        if (!latest || agentUpdateNoticeDismissed(latest)) {
          setNotice(null);
        } else if (info.pending_restart) {
          setNotice({ kind: "ready", version: "", latest });
        } else if (info.available) {
          setNotice({ kind: "available", version: "", latest });
        } else {
          setNotice(null);
        }
      } catch {
        // ПК не ответил — плашки нет: отсутствие новости лучше выдуманной.
        if (!cancelled) setNotice(null);
      }
    };
    void load();
    // Сменили машину — версия в конфиге уже другая, плашку пересчитываем.
    const off = onSelectedDeviceChange(() => { void load(); });
    return () => { cancelled = true; off(); };
  }, []);

  if (!notice) return null;

  const device = humanDeviceName(getSelectedDeviceName());
  const text = notice.kind === "auto"
    ? t("home.agentUpdateAuto", { device, latest: `v${notice.latest}` })
    : notice.kind === "stuck"
    ? t("home.agentUpdateStuck", { device, version: `v${notice.version}`, latest: `v${notice.latest}` })
    : notice.kind === "ready"
    ? t("home.agentUpdateReady", { latest: `v${notice.latest}` })
    : t("home.agentUpdateAvailable", { latest: `v${notice.latest}` });

  return (
    <div className={`agent-update-notice${notice.kind === "stuck" ? " warn" : ""}`} role="status">
      <span className="agent-update-notice-text">{text}</span>
      <button
        type="button"
        className="agent-update-notice-close"
        aria-label={t("common.dismiss")}
        onClick={() => {
          haptic();
          dismissAgentUpdateNotice(notice.latest);
          setNotice(null);
        }}
      >
        <IconClose size={14} />
      </button>
    </div>
  );
}

/**
 * Живая сводка ПК на главной: что ждёт ответа, что работает, куда вернуться.
 *
 * Зачем: пользователь открывает приложение с телефона в основном потому, что
 * агент на ПК спросил «y/n» или должен был закончить. Раньше это узнавалось
 * только заходом в /pty, а главная молчала. Данные берутся из того же
 * `GET /api/pty`, что и список терминалов (status/hint/agent_kind), поэтому
 * бейджи и формулировки совпадают с /pty, а не живут своей жизнью.
 *
 * Оттуда же приходит журнал исходов (`outcomes`): статус "error" гаснет через
 * 5 минут, а мёртвая сессия через те же 5 минут исчезает совсем — то есть без
 * журнала человек, вернувшийся через час, видел мирную плитку «Продолжить с
 * того места» вместо «сборка упала».
 *
 * На карточку «Требует внимания» вынесены кнопки быстрого ответа
 * (POST /api/pty/{id}/input): раньше ответить агенту можно было ТОЛЬКО открыв
 * экран терминала — REST-ввода в PTY не существовало вовсе.
 *
 * Со старым агентом (без поля `status`) блоки просто не рендерятся —
 * деградация до «Продолжить с того места».
 */
export function HomeActivity() {
  const navigate = useNavigate();
  const { toastSuccess } = useToast();
  const [sessions, setSessions] = useState<PtySessionInfo[]>([]);
  // Журнал исходов своего ПК (см. комментарий выше).
  const [outcomes, setOutcomes] = useState<PtyOutcome[]>([]);
  // Исходы, по которым человек нажал «Понятно» — по ключу эпизода.
  const [seenOutcomes, setSeenOutcomes] = useState<string[]>(readSeenOutcomes);
  // Первый ответ ПК получен: до него блок держит место скелетоном, иначе
  // асинхронные карточки вставлялись НАД кнопкой «+ Новый терминал» и уводили
  // её из-под пальца (холодный старт, 0,5–2 с на облачный round-trip).
  const [loaded, setLoaded] = useState(false);
  // Тик раз в секунду: «вывод 3с назад» должен идти сам, без ответа сервера.
  // Он же двигает подпись «Агент пока не отреагировал» — отдельный таймер под
  // неё не нужен.
  const [, setTick] = useState(0);
  // Результат быстрых ответов по id терминала.
  const [replies, setReplies] = useState<Record<string, ReplyState>>({});
  // Сводка устарела: ПК не в сети («offline») или последний запрос не удался по
  // другой причине («error»). Карточки не выбрасываем — приглушаем и говорим,
  // что это снимок последней связи; кнопки быстрых ответов при офлайне
  // блокируем: отвечать сейчас некуда.
  const [stale, setStale] = useState<"offline" | "error" | null>(null);
  // Кнопки быстрых ответов «взведены»: карточка простояла на экране хотя бы
  // ANSWER_ARM_MS и её точно не подставили под уже занесённый палец.
  const [armed, setArmed] = useState(false);
  // Форма сводки на прошлом визите — держим под неё место, пока ПК не ответил.
  // Ref, а не state: значение нужно ровно на первом рендере и меняться по ходу
  // жизни экрана не должно.
  const reserve = useRef<HomeLayout>(readLayout()).current;

  /** Свой ПК: терминалы + журнал исходов одним запросом. */
  const loadOwn = useCallback(async () => {
    try {
      const r = await listPtySessions();
      const list = r.sessions || [];
      setSessions(list);
      setOutcomes(r.outcomes || []);
      setStale(null);
      setReplies((prev) => pruneReplies(prev, list));
      // Шаги онбординга — по факту, а не по кликам: терминал существует,
      // AI-агент виден в foreground хотя бы одного терминала.
      if (list.length > 0) markHomeStep("terminal");
      if (list.some((s) => isAgentKind(s.agent_kind))) markHomeStep("agent");
    } catch (e: unknown) {
      // Офлайн-ПК: прежний список не сбрасываем (иначе сводка врала бы «ничего
      // не происходит»), но помечаем его устаревшим — кнопки «Да»/«Нет» на
      // карточках мёртвой машины оставались активными бессрочно.
      setStale(isPcOffline(e) ? "offline" : "error");
    } finally {
      setLoaded(true);
    }
  }, []);

  /**
   * Отправить быстрый ответ агенту, не открывая терминал.
   *
   * `expect_status_at` — защита от «ответил не на тот вопрос»: карточка могла
   * провисеть в кармане часы, за это время агент спросил другое. Агент в таком
   * случае отвечает 409 prompt_changed и НЕ применяет ввод.
   */
  const sendAnswer = useCallback(async (s: PtySessionInfo, a: QuickAnswer) => {
    const episode = episodeKey(s);
    haptic();
    setReplies((prev) => ({
      ...prev,
      [s.id]: { label: a.label, status: "sending", episode, at: Date.now() },
    }));
    try {
      await ptyInput(s.id, { ...a.body, expect_status_at: s.status_at || undefined });
      hapticSuccess();
      setReplies((prev) => (
        // Пока летел запрос, поллинг мог уже увидеть смену эпизода и выкинуть
        // запись — воскрешать её не надо, агент явно отреагировал.
        prev[s.id]?.episode === episode
          ? { ...prev, [s.id]: { label: a.label, status: "sent", episode, at: Date.now() } }
          : prev
      ));
      // Разовое перечитывание состояния: карточка обязана показать, что вышло,
      // а не ждать до 6 секунд очередного такта. Нового интервала не заводим.
      void loadOwn();
    } catch (e: unknown) {
      hapticError();
      // Провал провалу рознь: «точно не доставлено» разблокирует кнопки (повтор
      // безопасен), «не знаю» — оставляет их заблокированными и честно говорит,
      // что ответ мог дойти. Раньше таймаут выдавался за отказ, и второе
      // нажатие могло отправить агенту второй ответ.
      const unknown = answerDelivery(e) === "unknown";
      setReplies((prev) => ({
        ...prev,
        [s.id]: {
          label: a.label,
          status: unknown ? "unknown" : "error",
          episode,
          at: Date.now(),
          error: unknown ? t("home.act.answerUnknown") : answerError(e),
        },
      }));
      // Причина отказа могла быть «агент спрашивает уже другое» — обновляем
      // карточку, чтобы человек увидел новый вопрос, а не старый. При
      // неизвестной доставке это тем более нужно: сдвинувшийся эпизод сам
      // снимет предупреждение и вернёт кнопки.
      void loadOwn();
    }
  }, [loadOwn]);

  /**
   * Живой канал: релей сообщает кадром agent_status, что ПК ушёл или вернулся,
   * — до 6 секунд раньше очередного такта поллинга. Реагируем только на
   * названную причину «pc_offline»: в вебе, Telegram и окне exe сокет
   * намеренно рвётся в фоне, и `!connected` про ПК ничего не говорит.
   */
  useEffect(() => onConnectionChange((state) => {
    if (state.reason === "pc_offline") { setStale("offline"); setLoaded(true); return; }
    if (state.connected) void loadOwn();
  }), [loadOwn]);

  /**
   * Сменили управляемую машину — сводка прежней перестаёт что-либо значить
   * НЕМЕДЛЕННО. Раньше её карточки висели ещё 0,5–2 с (реконнект канала) уже
   * под новым именем и с активными кнопками «Да»/«Нет»: нажатие уходило на
   * новую машину с чужим id терминала и возвращалось словами «Терминал не
   * найден», по которым догадаться о причине невозможно. Уходим в скелетон —
   * ровно так же, как это уже делается для возможностей и канала.
   */
  useEffect(() => onSelectedDeviceChange(() => {
    setSessions([]);
    setOutcomes([]);
    setReplies({});
    setStale(null);
    setLoaded(false);
  }), []);

  useEffect(() => {
    let ownPoll = 0;
    let tick = 0;
    // В фоне (телефон в кармане, другая вкладка) не опрашиваем: на облачном
    // канале это лишний трафик и разряд, а при возврате мы сразу обновляемся.
    const start = () => {
      if (ownPoll) return;
      void loadOwn();
      ownPoll = window.setInterval(() => void loadOwn(), OWN_POLL_MS);
      tick = window.setInterval(() => setTick((n) => n + 1), 1000);
    };
    const stop = () => {
      window.clearInterval(ownPoll); ownPoll = 0;
      window.clearInterval(tick); tick = 0;
    };
    const onVis = () => (document.hidden ? stop() : start());
    if (!document.hidden) start();
    document.addEventListener("visibilitychange", onVis);
    return () => { stop(); document.removeEventListener("visibilitychange", onVis); };
  }, [loadOwn]);

  // Прочитанные исходы храним на устройстве: серверу о них знать нечего, а
  // «Требует внимания», которое нельзя убрать, превращается в вечный укор.
  useEffect(() => {
    try { localStorage.setItem(SEEN_KEY, JSON.stringify(seenOutcomes)); } catch { /* приватный режим */ }
  }, [seenOutcomes]);

  const markOutcomeSeen = useCallback((o: PtyOutcome) => {
    const key = outcomeKey(o);
    setSeenOutcomes((prev) => (prev.includes(key) ? prev : [...prev, key].slice(-SEEN_LIMIT)));
  }, []);

  // ⚠ `?from=/` — это дом для «Назад». Без него терминал, открытый С ГЛАВНОЙ,
  // возвращал в список терминалов: человек нажимал «Назад» и оказывался в
  // разделе, где не был (обход карты 04.09.2026 — единственная строка в
  // «системная „назад“ ведёт не туда»). Правило экрана: дом называет тот, кто
  // открыл (PtyTermView, `fromParam`).
  const open = (id: string) => { haptic(); navigate(`/pty/${id}?from=%2F`); };

  /** Исход: открыть терминал (мёртвый честно скажет о себе и предложит
   *  перезапуск в той же папке) и считать исход прочитанным. */
  const openOutcome = (o: PtyOutcome) => { markOutcomeSeen(o); open(o.id); };

  const alive = sessions.filter((s) => s.alive);
  // В «Требует внимания» пускаем только РЕАЛЬНЫЙ вопрос (waiting приходит с
  // непустым hint) и ошибку. Старый агент присылал waiting и на «агент просто
  // молчит» — такие сессии навсегда оставались здесь с ложным «ждёт ответа»,
  // из-за чего настоящий вопрос в списке терялся. Молчащие агенты (status
  // "ready" у нового агента; waiting без hint у старого) показываем в
  // «Сейчас работает» строкой «свободен».
  const attention = alive.filter(
    (s) => (s.status === "waiting" && !!s.hint) || s.status === "error",
  );
  const attnIds = new Set(attention.map((s) => s.id));
  const seenSet = new Set(seenOutcomes);
  // Исход показываем, пока он новость: не отмечен «Понятно» и не повторяет
  // живую карточку того же терминала (там та же ошибка, только с кнопками).
  const shownOutcomes = outcomes
    .filter((o) => !seenSet.has(outcomeKey(o)) && !attnIds.has(o.id))
    .slice(0, OUTCOMES_SHOWN);
  const working = alive.filter(
    (s) => !attnIds.has(s.id) &&
      (s.status === "working" || s.status === "stalled" || s.status === "ready" || s.status === "waiting"),
  );
  // Одной СТРОКОЙ, а не списком. Список работающих терминалов с главной убран
  // намеренно (разгрузка 2.60.0), но и молчать о них нельзя: главная отвечала
  // только на «что от меня хотят», а на «что там сейчас» — нет, хотя это
  // второй вопрос, с которым сюда заходят (аудит путей 29.08.2026).
  const runningNow = working.filter((s) => s.status === "working" || s.status === "stalled").length;
  const freeNow = working.filter((s) => s.status === "ready").length;
  const showRunSummary = runningNow > 0 || freeNow > 0;
  const outcomeIds = new Set(shownOutcomes.map((o) => o.id));
  const resume = terminalToContinue(
    alive.filter((s) => !attnIds.has(s.id) && !outcomeIds.has(s.id)),
    lastViewedTerminal(localStorage, getTerminalContextKey()),
  );

  // Занятые терминалы показаны общей строкой; личный возврат не зависит от
  // фоновой активности и не дублирует карточку вопроса или результата.
  const nothingToShow = attention.length === 0 && shownOutcomes.length === 0 && !resume && !showRunSummary;
  const cardCount = Math.min(LAYOUT_MAX, attention.length + shownOutcomes.length);
  const rowCount = (resume ? 1 : 0) + (showRunSummary ? 1 : 0);

  // Запоминаем форму сводки: в следующий раз (и при каждом возврате на главную,
  // блок перемонтируется) она станет высотой скелетона — кнопка «+ Новый
  // терминал» не уедет из-под пальца, когда придёт ответ ПК. Устаревшую сводку
  // офлайн-машины не запоминаем: она не про то, что будет на экране дальше.
  useEffect(() => {
    if (!loaded || stale) return;
    writeLayout({ cards: cardCount, rows: rowCount });
  }, [loaded, stale, cardCount, rowCount]);

  /**
   * Карточка с кнопками ответа только что появилась — даём пальцу долететь.
   * Ключ эффекта — состав «Требует внимания»: пока он тот же, кнопки взведены;
   * приехала новая карточка — снова короткая пауза.
   */
  const attnKey = attention.map((s) => s.id).join(",");
  useEffect(() => {
    if (!attnKey) return;
    setArmed(false);
    const timer = window.setTimeout(() => setArmed(true), ANSWER_ARM_MS);
    return () => window.clearTimeout(timer);
  }, [attnKey]);

  // Пока ПК не ответил ни разу, блок держит место скелетоном по ПРОШЛОЙ форме
  // сводки (карточки высокие, строки низкие) — иначе приехавшие карточки
  // сдвигали низ экрана на 300+px вместе с кнопкой из-под занесённого пальца.
  // Показывать нечего и резервировать нечего — не рисуем ничего: пустая тёмная
  // плашка под карточкой офлайн-ПК была ровно этим скелетоном.
  if (nothingToShow) {
    if (loaded || stale || (reserve.cards === 0 && reserve.rows === 0)) {
      // Сводки нет — но плашка про версию агента к сводке не привязана,
      // она живёт на главной сама по себе.
      return <AgentUpdateNoticeBanner />;
    }
    return (
      <>
        <AgentUpdateNoticeBanner />
        <div aria-hidden>
          {Array.from({ length: reserve.cards }, (_, i) => (
            <div key={`c${i}`} className="home-attn-skeleton" />
          ))}
          {Array.from({ length: reserve.rows }, (_, i) => (
            <div key={`r${i}`} className="home-run-skeleton" />
          ))}
        </div>
      </>
    );
  }
  // Офлайн-ПК: и карточки, и быстрые ответы относятся к состоянию на момент
  // последней связи. Кнопки ответов при этом мертвы — команда всё равно не
  // дойдёт, а её «отправка» была бы обманом.
  const offlineStale = stale === "offline";
  // Пометка про устаревшие данные относится к ЭТОМУ компьютеру: сводка других
  // машин приходит с релея отдельно и остаётся свежей.
  const ownContent = attention.length > 0 || shownOutcomes.length > 0 || !!resume || showRunSummary;

  return (
    <>
      {/* Плашка про версию агента — над карточками сводки: это новость о самом
          компьютере, а не о том, «что от меня хотят» прямо сейчас. */}
      <AgentUpdateNoticeBanner />
      {/* Порядок блоков = порядок важности для человека: СВОЙ вопрос отвечается
          одной кнопкой отсюда, поэтому он первый; чужие машины — ниже, тап по
          ним меняет весь контекст приложения (см. openOther). */}
      {/* «Компьютер не в сети» отсюда убрано: эту новость на офлайн-главной уже
          говорят баннер связи и карточка готовности. Здесь остаётся только то,
          чего больше нигде нет, — что сводка ниже показана на момент последней
          связи. */}
      {stale && ownContent && (
        <div className="home-activity-stale" role="status">
          {t(offlineStale ? "home.act.staleShort" : "home.act.staleError")}
        </div>
      )}
      {(attention.length > 0 || shownOutcomes.length > 0) && (
        <>
          <div className="home-guide-title">{t("home.act.attention")}</div>
          {attention.map((s) => {
            const waiting = s.status === "waiting";
            const answers = waiting ? quickAnswers(s) : [];
            const noteKey = waiting ? answerNoteKey(s) : "";
            // Результат показываем, пока он про ТОТ ЖЕ вопрос (и ещё немного —
            // если это ошибка, см. replyIsCurrent).
            const r = replies[s.id];
            const reply = r && replyIsCurrent(r, episodeKey(s)) ? r : undefined;
            // Ввод ушёл в PTY (агент подтвердил доставку), но агент за
            // ANSWER_WAIT_MS так и не сдвинулся с вопроса. Врать «отправлено,
            // всё хорошо» дальше нельзя — говорим как есть и снова даём нажать.
            const stalled = reply !== undefined && reply.status === "sent"
              && Date.now() - reply.at > ANSWER_WAIT_MS;
            // Пока ждём доставки и реакции — кнопки заблокированы, чтобы
            // случайный второй тап не отправил агенту ещё один «y». Статус
            // "unknown" (таймаут/обрыв) блокирует их до смены эпизода: там мы
            // не знаем, дошёл ли первый ответ, и повтор мог бы стать вторым
            // нажатием клавиши в терминале. Выход из этого состояния —
            // «Ответить →» на экран терминала, где видно правду.
            const answerBusy = reply !== undefined
              && (reply.status === "sending" || reply.status === "unknown"
                || (reply.status === "sent" && !stalled));
            return (
              // Карточка — div, а не button: внутри живут кнопки быстрых
              // ответов, а вложенная кнопка в кнопке невалидна (и ломает тап).
              // Тот же приём, что у .pty-card в списке терминалов.
              <div
                key={s.id}
                className={`home-attn-card ${waiting ? "waiting" : "error"}${stale ? " stale" : ""}`}
                onClick={() => open(s.id)}
              >
                <div className="home-attn-top">
                  <span className="home-attn-title">{title(s)}</span>
                  <span className={`pty-status ${waiting ? "waiting" : "error"}`}>
                    {/* size задаём числом: .pty-status-icon держит 11px через
                        font-size, а у SVG величина от шрифта не зависит — без
                        этого пилюля выросла бы до 20px и разорвала строку. */}
                    <span className="pty-status-icon">
                      {waiting ? <IconHourglass size={11} /> : <IconWarning size={11} />}
                    </span>
                    {waiting ? t("pty.statusWaiting") : t("home.act.errorShort")}
                  </span>
                </div>
                {/* У ошибки в hint лежит сама сработавшая строка вывода (её
                    присылает агент с v2.35.0) — показываем её, а не общую
                    фразу: по ней сразу видно, авария это или красная строка,
                    с которой агент справился. */}
                <div className={waiting ? "home-attn-hint" : "home-attn-hint error"}>
                  {/* Строка вывода бывает длинной (до 160 символов) — своим
                      span'ом, иначе она разносит карточку по высоте: у текста
                      прямо во флекс-контейнере многоточие не работает. */}
                  <span className={`home-attn-hint-text${waiting || !s.hint ? "" : " output"}`}>
                    {(waiting ? s.hint : s.hint || t("home.act.errorHint"))}
                  </span>
                </div>
                <div className="home-attn-foot">
                  <span className="home-attn-since">
                    {ago(s.status_at || s.last_active || s.created)}
                  </span>
                  {/* Кнопка, а не span: карточка перестала быть button, и без
                      этого экран потерял бы доступ с клавиатуры (окно exe). */}
                  <button
                    type="button"
                    className="home-attn-cta"
                    onClick={(e) => { e.stopPropagation(); open(s.id); }}
                  >
                    {waiting ? t("home.act.answer") : t("home.act.open")} {"→"}
                  </button>
                </div>
                {answers.length > 0 && (
                  // stopPropagation на ряду: тап по кнопке ответа не должен
                  // заодно уводить на экран терминала.
                  <div
                    className="home-attn-actions"
                    role="group"
                    aria-label={t("home.act.answerA11y")}
                    onClick={(e) => e.stopPropagation()}
                  >
                    {answers.map((a) => (
                      <button
                        key={a.id}
                        type="button"
                        className="btn btn-secondary btn-sm home-attn-answer"
                        // !armed — карточка появилась миллисекунды назад и могла
                        // подставиться под уже занесённый палец (см. ANSWER_ARM_MS).
                        disabled={answerBusy || offlineStale || !armed}
                        // Подпись пункта на кнопке обрезана до 22 символов —
                        // полный текст остаётся доступен наведением и голосом.
                        title={a.label}
                        onClick={() => { void sendAnswer(s, a); }}
                      >
                        {a.label}
                      </button>
                    ))}
                    {/* Причина, по которой кнопки мертвы, стоит там же, где они:
                        «Компьютер не в сети» одним словарным ключом с баннером. */}
                    {offlineStale && (
                      <span className="home-attn-result error" role="status">
                        {t("conn.pcOffline")}
                      </span>
                    )}
                    {!offlineStale && reply && (
                      <span
                        className={`home-attn-result${
                          reply.status === "error" || reply.status === "unknown" ? " error" : ""
                        }`}
                        role="status"
                      >
                        {reply.status === "sending"
                          ? t("home.act.answerSending")
                          : reply.status === "error" || reply.status === "unknown"
                          ? reply.error
                          : stalled
                          ? t("home.act.answerNoReaction")
                          : t("home.act.answerSent", { label: reply.label })}
                      </span>
                    )}
                    {/* Почему нет кнопок самого ответа (выбор из меню, свободный
                        текст) — пока не занято сообщением о результате. */}
                    {!offlineStale && !reply && noteKey && (
                      <span className="home-attn-result">{t(noteKey)}</span>
                    )}
                  </div>
                )}
              </div>
            );
          })}
          {/* Исходы: терминал уже закрылся или в выводе мелькнула ошибка. Живого
              вопроса тут нет — только «что случилось, пока вас не было». */}
          {shownOutcomes.map((o) => {
            const dead = o.status === "dead";
            return (
              <div
                key={outcomeKey(o)}
                // «Упало» сохраняет красную подложку живой ошибки, «завершился
                // сам» — нейтральную: это новость, а не тревога.
                className={`home-attn-card ${dead ? "outcome" : "error"}${stale ? " stale" : ""}`}
                onClick={() => openOutcome(o)}
              >
                <div className="home-attn-top">
                  <span className="home-attn-title">{outcomeTitle(o)}</span>
                  <span className={`pty-status ${dead ? "dead" : "error"}`}>
                    {!dead && <span className="pty-status-icon"><IconWarning size={11} /></span>}
                    {dead ? t("home.act.outcomeDeadShort") : t("home.act.errorShort")}
                  </span>
                </div>
                <div className="home-attn-hint outcome">
                  <div className="home-attn-hint-body">
                    <span className={`home-attn-hint-text${o.status === "error" && o.hint ? " output" : ""}`}>
                      {outcomeReason(o)}
                    </span>
                    {/* Строка вывода объясняет не всё: одинокое «Killed» — это
                        смерть от нехватки памяти, и без расшифровки человек
                        видит лишь, что команда исчезла (см. outcomeAdviceKey). */}
                    {outcomeAdviceKey(o.hint) && (
                      <span className="home-attn-hint-advice">{t(outcomeAdviceKey(o.hint))}</span>
                    )}
                  </div>
                </div>
                <div className="home-attn-foot">
                  <span className="home-attn-since">{ago(o.at)}</span>
                  <button
                    type="button"
                    className="home-attn-cta"
                    onClick={(e) => { e.stopPropagation(); openOutcome(o); }}
                  >
                    {t("home.act.open")} {"→"}
                  </button>
                  <button
                    type="button"
                    className="home-attn-dismiss"
                    onClick={(e) => { e.stopPropagation(); haptic(); markOutcomeSeen(o); }}
                  >
                    {t("home.act.outcomeDismiss")}
                  </button>
                </div>
              </div>
            );
          })}
        </>
      )}

      {/* Секции «Требует внимания на других компьютерах» здесь больше нет.
          Про соседние машины говорят плитки переключателя ВЫШЕ по экрану
          (DeviceSwitcher), а дальше работает более короткий путь: тап по плитке
          → главная той машины → её же карточка с кнопками «Да»/«Нет». Строка
          «на других компьютерах» кнопок ответа не имела вовсе и вела в терминал
          через диалог подтверждения — то есть была длиннее, а второй заголовок
          «Требует внимания» на одном экране путал. */}

      {/* Список работающих терминалов с главной УБРАН (по просьбе владельца:
          «с главной убери агентов, это неудобно»). Он повторял вкладку
          «Терминал», где та же работа видна полностью — с папками, поиском и
          действиями, — а на главной занимал экран между карточкой компьютера и
          кнопкой «+ Новый терминал».

          Что осталось: «Требует внимания» (агент ЗАДАЛ вопрос или упал — на это
          отвечают прямо отсюда) и «Продолжить с того места» одной плиткой.
          Главная отвечает на «что от меня нужно», а не перечисляет всё. */}

      {/* «Что там сейчас» одной строкой: сколько агентов работает и сколько
          уже закончили. Ход один — в «Терминалы», где это видно подробно. */}
      {showRunSummary && (
        <button className={`resume-tile${stale ? " stale" : ""}`} onClick={() => { haptic(); navigate("/pty"); }}>
          <div className="resume-tile-icon"><IconHourglass size={24} /></div>
          <div className="resume-tile-body">
            <div className="resume-tile-title">
              {runningNow > 0
                ? t("home.act.runningNow", { n: String(runningNow) })
                : t("home.act.freeNow", { n: String(freeNow) })}
            </div>
            <div className="resume-tile-meta">
              {runningNow > 0 && freeNow > 0
                ? t("home.act.runningAndFree", { n: String(freeNow) })
                : t("home.act.runningOpen")}
            </div>
          </div>
          <span className="home-guide-chevron">{"›"}</span>
        </button>
      )}
      {resume && (
        <button className={`resume-tile${stale ? " stale" : ""}`} onClick={() => open(resume.id)}>
          {/* «Вернуться к работе» — тот же круговой знак, что у «Продолжить»
              на карточке терминала: одно действие, одна картинка. 24px —
              размер, который .resume-tile-icon задавал шрифтом. */}
          <div className="resume-tile-icon"><IconRefresh size={24} /></div>
          <div className="resume-tile-body">
            <div className="resume-tile-title">{t("home.act.reopen")}</div>
            <div className="resume-tile-meta">
              {title(resume)}
              {" · "}
              {/* Тот же якорь возраста, что у бейджа «готово · N назад» в /pty
                  (status_at → last_active → created): раньше главная считала от
                  last_active и показывала «22ч» там, где список писал «20ч». */}
              {ago(resume.status_at || resume.last_active || resume.created)}
            </div>
          </div>
          <span className="home-guide-chevron">{"›"}</span>
        </button>
      )}
    </>
  );
}

/**
 * Имя терминала для сводки: своё имя → «Claude · папка» → «bash · папка».
 * На главной важнее «кто ждёт ответа», чем в каком шелле он запущен, поэтому
 * агент вытесняет имя шелла (в /pty логика обратная: там строка про терминал).
 */
function title(s: PtySessionInfo): string {
  return ptyDisplayTitle(s, { preferAgent: true });
}

/** Заголовок исхода — тем же правилом, что у живых карточек: сессии уже нет,
 *  но имя, папка и агент в журнале сохранены (internal/pty Outcome). */
function outcomeTitle(o: PtyOutcome): string {
  return ptyDisplayTitle(
    { name: o.name, shell: o.shell || "", cwd: o.cwd || "", agent_kind: o.agent_kind },
    { preferAgent: true },
  );
}

// Возраст считает общий хелпер (@tgcontrol/shared/timeAgo) — тот же, что в
// списке терминалов: свои копии формата давали «22ч» на главной и «20h» в /pty
// для одного и того же терминала.
const agoValue = (ms: number): string => formatAgoValue(ms);
const ago = (ms: number): string => formatAgo(ms);
