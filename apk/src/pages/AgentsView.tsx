import { getLocale } from "@tgcontrol/shared";
import { useCallback, useEffect, useMemo, useState, useSyncExternalStore, type ReactNode } from "react";
import { useLocation, useNavigate } from "react-router-dom";
import { APP_NAME, canCheckConnection, mapApiError, setAgentRegistry, t } from "@tgcontrol/shared";
import {
  getAgents, rescanAgents, getConfig, updateConfig, getAgentRequests, answerAgentRequest,
} from "../api";
import type { AgentInfo, AppConfig, AgentSettingRequest } from "../types";
import { AgentQuotaChips } from "../components/AgentQuotaChips";
import { TokenUsagePanel } from "../components/TokenUsagePanel";
import { AgentUpdateLine } from "../components/AgentUpdates";
import { agentFocusFromSearch, agentSectionFocus } from "../agentsNavigation";
import { SkillsPanel } from "../components/SkillsPanel";
import { McpPanel } from "../components/McpPanel";
import {
  useAgentAccounts, AgentAccountsList, accountsOfAgent, activeAccountOfAgent, accountTitle,
} from "../components/AgentAccounts";
import { OpenRouterCard, useOpenRouter } from "../components/OpenRouterCard";
import { AgentCheckSheet } from "../components/AgentCheckSheet";
import {
  getMe,
  listDevices,
  type CloudDevice,
  type CloudMe,
} from "../cloud/api";
import {
  collectInfrastructureAIUsage,
  fetchLocalAIUsage,
  latestDeviceUsage, selectedUsageResource, subscribeUsage, usageRevision,
  type AIProviderUsage,
  type AIUsageSnapshot,
  type AIUsageWindow,
  type DeviceAIUsage,
} from "../aiUsage";
import { preferUsage, usageAgeLabel, usageIsStale, usageTime } from "../aiUsagePolicy";
import { useUsageNow } from "../hooks/useAIUsage";
import { BottomNav } from "../components/BottomNav";
import { DeviceChip } from "../components/DeviceChip";
import { HelpSheet } from "../components/HelpSheet";
import { getMode } from "../config";
import { useGoBack } from "../navBack";
import { usePolling } from "../hooks/usePolling";
import { haptic } from "../telegram";
import { planState } from "../plan";
import { getSubscription, unbindCard, type CloudSubscription } from "../cloud/api";
import { openExternalLink } from "../openExternal";
import { prefillSupportDraft } from "../supportDraft";
import "../agentSessions/agentSessions.css";

/** Как часто перечитывать лимиты, пока экран открыт. */
const REFRESH_MS = 60_000;

interface ProviderGroup {
  key: string;
  provider: AIProviderUsage;
  devices: CloudDevice[];
}

// Марки агентов — нейтральные геометрические знаки: официальные логотипы
// Claude, Codex и остальных чужие, со своими правилами использования.
// У Grok марка НЕ крест: «✕» в этом же клиенте означает «закрыть» в 28 местах,
// и в списке агентов он читался как кнопка отмены, а не как имя (UX-аудит
// 2026-08-23, п. 2.5 — «два языка иконок»). Рядом с маркой всегда стоит
// название агента словом, поэтому знак только опознавательный.
const PROVIDER_ICON: Record<string, string> = {
  codex: "◉",
  claude: "✦",
  gemini: "◆",
  kimi: "☾",
  opencode: "◇",
  aider: "⌘",
  grok: "✵",
};

const PLAN_LABEL: Record<string, string> = {
  free: "Free",
  go: "Go",
  plus: "Plus",
  pro: "Pro",
  prolite: "Pro Lite",
  team: "Team",
  business: "Business",
  self_serve_business_usage_based: "Business",
  enterprise: "Enterprise",
  enterprise_cbp_usage_based: "Enterprise",
  edu: "Education",
  max: "Max",
};

function planLabel(value?: string): string {
  if (!value) return "";
  return PLAN_LABEL[value.toLowerCase()] || value;
}

/** Состояние карточки провайдера с точки зрения человека, а не транспорта. */
type ProviderTone = "available" | "nodata" | "signed_out" | "unsupported" | "unavailable";

/**
 * Честный статус: агент помечает провайдера `available`, даже когда не получил
 * ни одного окна (вход по API-ключу у Codex, «нет активных окон» у Claude).
 * Раньше такая карточка хвасталась зелёным «Данные актуальны» над пустотой,
 * поэтому статус считаем по фактическим числам.
 */
function providerTone(provider: AIProviderUsage): ProviderTone {
  const hasNumbers = provider.windows.length > 0 || !!provider.extra_usage?.enabled;
  if (provider.status === "available") return hasNumbers ? "available" : "nodata";
  if (provider.status === "signed_out") return "signed_out";
  if (provider.status === "unsupported") return "unsupported";
  return "unavailable";
}

/**
 * Подпись статуса карточки.
 *
 * «Данные актуальны» экран писал часами: он грузится один раз и сам не
 * обновляется, поэтому вернувшийся из фона человек видел зелёную плашку над
 * процентами часовой давности и запускал по ним большую задачу. Статус теперь
 * привязан ко времени снимка — «Данные на 14:02».
 */
function toneLabel(tone: ProviderTone, updatedAt: Date | null): string {
  switch (tone) {
    case "available": return updatedAt
      ? t("usage.statusAsOf", { time: hhmm(updatedAt) })
      : t("usage.statusFresh");
    case "nodata": return t("usage.statusNoPercent");
    case "signed_out": return t("usage.statusSignIn");
    case "unsupported": return t("usage.statusNoApi");
    default: return t("usage.statusUnavailable");
  }
}

/**
 * Объяснение пустой карточки — своими словами.
 *
 * «Войдите в провайдера» человек не понимает: провайдер тут Anthropic/OpenAI,
 * а входит он в аккаунт Claude или ChatGPT — и не знает ни куда идти, ни какой
 * командой. Поэтому в текстах стоит имя сервиса (provider.name) и команда,
 * которую надо запустить в терминале (provider.id — это и есть имя CLI:
 * claude, codex, gemini).
 */
function toneNote(tone: ProviderTone, provider: AIProviderUsage): string {
  const name = provider.name;
  switch (tone) {
    case "signed_out": return t("usage.noteSignInAccount", { name, cli: provider.id });
    case "unsupported": return t("usage.noteNoSource");
    case "unavailable": return t("usage.noteUnavailableApp", { name });
    default: return t("usage.noteNoWindowsApp", { name });
  }
}

/** Часы:минуты снимка — в подписи статуса и в заголовке секции один формат. */
function hhmm(date: Date): string {
  return date.toLocaleTimeString(getLocale(), { hour: "2-digit", minute: "2-digit" });
}

function requestValueLabel(value: string): string {
  if (value === "true") return t("agents.requestEnable");
  if (value === "false") return t("agents.requestDisable");
  return value;
}

function groupUsage(results: DeviceAIUsage[]): ProviderGroup[] {
  const groups = new Map<string, ProviderGroup>();
  for (const result of results) {
    for (const provider of result.snapshot?.providers || []) {
      // Без account нельзя утверждать, что две машины используют одну подписку.
      const identity = provider.account?.trim().toLowerCase() || result.device.id;
      // account_id — на одной машине подписок бывает несколько, и у только что
      // заведённой почты ещё нет: без него два аккаунта одного вендора слились
      // бы в одну карточку и показали чужой процент.
      const key = `${provider.id}|${identity}|${provider.plan || ""}|${provider.account_id || ""}`;
      const existing = groups.get(key);
      if (existing) {
        existing.devices.push(result.device);
        // На случай небольшого рассинхрона выбираем более свежий/полный набор
        // окон, не складывая одинаковый процент подписки по машинам.
        existing.provider = preferUsage(existing.provider, provider);
      } else {
        groups.set(key, { key, provider, devices: [result.device] });
      }
    }
  }
  return [...groups.values()].sort(byProviderInterest);
}

/**
 * Сверху — карточки с настоящими процентами, затем пустые, затем требующие
 * действия: раньше available-без-данных стоял выше «Нужен вход».
 */
function byProviderInterest(a: ProviderGroup, b: ProviderGroup): number {
  const rank = (provider: AIProviderUsage) => {
    const tone = providerTone(provider);
    return tone === "available" ? 0 : tone === "nodata" ? 1 : tone === "signed_out" ? 2 : 3;
  };
  return rank(a.provider) - rank(b.provider) ||
    a.provider.name.localeCompare(b.provider.name, "ru");
}

/**
 * Тот же порядок карточек для прямого подключения (окно exe, LAN): опрошен один
 * компьютер, поэтому чипы машин пусты — сводить одну подписку по машинам не
 * требуется, а группировать по account нечего.
 */
function groupLocalUsage(snapshot: AIUsageSnapshot | null): ProviderGroup[] {
  if (!snapshot) return [];
  return (snapshot.providers || [])
    .map((provider) => ({
      key: `${provider.id}|${provider.account || "local"}|${provider.plan || ""}|${provider.account_id || ""}`,
      provider,
      devices: [] as CloudDevice[],
    }))
    .sort(byProviderInterest);
}

function resetLabel(timestamp?: number): string {
  if (!timestamp) return t("usage.resetUnknown");
  const date = new Date(timestamp * 1000);
  if (!Number.isFinite(date.getTime())) return t("usage.resetUnknown");
  const delta = date.getTime() - Date.now();
  if (delta <= 0) return t("usage.waitReset");
  if (delta > 0 && delta < 48 * 60 * 60 * 1000) {
    const hours = Math.floor(delta / 3_600_000);
    const minutes = Math.max(0, Math.floor((delta % 3_600_000) / 60_000));
    if (hours > 0) return t("usage.resetInHours", { hours, minutes });
    return t("usage.resetInMinutes", { minutes });
  }
  return t("usage.resetAt", {
    date: new Intl.DateTimeFormat(getLocale(), {
      day: "2-digit", month: "short", hour: "2-digit", minute: "2-digit",
    }).format(date),
  });
}

function durationLabel(minutes?: number): string {
  if (!minutes) return "";
  if (minutes % 10080 === 0) return t("usage.durationWeeks", { n: minutes / 10080 });
  if (minutes % 1440 === 0) return t("usage.durationDays", { n: minutes / 1440 });
  if (minutes % 60 === 0) return t("usage.durationHours", { n: minutes / 60 });
  return t("usage.durationMinutes", { n: minutes });
}

function percentClass(percent: number): string {
  if (percent >= 90) return "danger";
  if (percent >= 70) return "warning";
  return "ok";
}

function money(value: number, currency = "USD", decimalPlaces = 2): string {
  const divisor = 10 ** Math.max(0, decimalPlaces);
  try {
    return new Intl.NumberFormat(getLocale(), {
      style: "currency",
      currency,
      maximumFractionDigits: decimalPlaces,
    }).format(value / divisor);
  } catch {
    return `${(value / divisor).toFixed(decimalPlaces)} ${currency}`;
  }
}

/**
 * Окно лимита. ОДНА ШКАЛА НА ЭКРАН — остаток.
 *
 * Один и тот же пятичасовой лимит стоял здесь крупным «91%», в аккаунтах —
 * «осталось 9% за 5 ч», а чипом у агента — снова «91% 5ч». Три числа про одно
 * и то же в двух шкалах человек складывает в голове неправильно, а решение он
 * принимает по остатку: «хватит ли добить задачу». Поэтому показываем остаток
 * — и числом, и полосой, и словами легенды.
 *
 * СЧИТАЕМ ПО-ПРЕЖНЕМУ РАСХОД: `percentClass` красит по нему (danger при 90%
 * расхода). Отдать в него остаток значит покрасить почти выбранное окно
 * зелёным — тихая инверсия, которую тестами не поймать.
 */
function UsageWindow({ window }: { window: AIUsageWindow }) {
  const percent = Math.max(0, Math.min(100, Number(window.used_percent) || 0));
  const left = Math.max(0, Math.round(100 - percent));
  return (
    <div className="usage-window">
      <div className="usage-window-line">
        <span><b>{window.label}</b><small>{[durationLabel(window.duration_minutes), resetLabel(window.resets_at)].filter(Boolean).join(" · ")}</small></span>
        <strong className={percentClass(percent)}>{left}%</strong>
      </div>
      <div className="usage-progress" aria-label={t("usage.leftAria", { percent: left })}>
        <i className={percentClass(percent)} style={{ width: `${100 - percent}%` }} />
      </div>
      <div className="usage-progress-legend">
        <span>{t("usage.legendLeftLabel")}</span>
        <span>{t("usage.legendSpent", { percent: Math.round(percent) })}</span>
      </div>
    </div>
  );
}

/** Имя записи о раскрытых секциях. Храним ИМЕНА, а не номера: состав секций
 *  зависит от режима и наполнения, и по номеру запомненный выбор съехал бы на
 *  соседний раздел. */
// Новый вид показывает краткое содержание в закрытой строке. Старые настройки
// раскрытия скрывали большие блоки без пояснения, поэтому начинаем с чистого вида.
const SECTIONS_KEY = "remotai.agents.sections.v2";

/**
 * Память раскрытых секций.
 *
 * localStorage в Telegram-мини-аппе и в приватном окне БРОСАЕТ на самом
 * обращении, а не возвращает пустоту: без try/catch падение здесь белит весь
 * экран. Поэтому обёрнуты оба обращения, а отказ означает «раскладка по
 * умолчанию», а не поломку.
 */
function useOpenSections(defaults: string[]) {
  const [open, setOpen] = useState<string[]>(() => {
    try {
      const saved = localStorage.getItem(SECTIONS_KEY);
      const parsed = saved ? JSON.parse(saved) : null;
      return Array.isArray(parsed) ? parsed.filter((v) => typeof v === "string") : defaults;
    } catch {
      return defaults;
    }
  });
  const persist = (next: string[]) => {
    try { localStorage.setItem(SECTIONS_KEY, JSON.stringify(next)); } catch { /* квота или приватное окно */ }
    return next;
  };
  return {
    isOpen: (id: string) => open.includes(id),
    toggle: (id: string) => setOpen((prev) => persist(
      prev.includes(id) ? prev.filter((v) => v !== id) : [...prev, id],
    )),
    /** Раскрыть принудительно: сюда пришли ЗА этой секцией (переход из шторки
     *  запуска «Нет своей подписки?»), и человек не должен искать её сам. */
    ensureOpen: (id: string) => setOpen((prev) => (prev.includes(id) ? prev : persist([...prev, id]))),
  };
}

/**
 * Секция-аккордеон.
 *
 * Большие группы отвечают на вопросы «как начать», «доступ и расходы» и
 * «подключение». Внутри группы каждая строка остаётся самостоятельным h3 и
 * кнопкой с aria-expanded. Закрытая строка объясняет, что откроется.
 *
 * Тело рендерится УСЛОВНО, а не прячется атрибутом `hidden`: у
 * `.agents-section-body` есть `display: flex`, который перебивает UA-правило
 * `[hidden] { display: none }` — секция «свернулась» бы и осталась видимой.
 *
 * `action` показывается в раскрытом теле: вложенная кнопка не перехватывает
 * нажатие по заголовку и не смешивается с его доступным именем.
 */
function AgentsSection({ className, title, note, summary, action, open, onToggle, children }: {
  className: string;
  title: string;
  note?: string;
  summary?: string;
  action?: ReactNode;
  open: boolean;
  onToggle: () => void;
  children: ReactNode;
}) {
  return (
    <section className={`agents-section ${className}`}>
      <h3 className="usage-section-title">
        <button
          className="agents-section-toggle"
          aria-expanded={open}
          onClick={() => { haptic(); onToggle(); }}
        >
          <span className="agents-section-copy">
            <span className="agents-section-name">{title}</span>
            {note && <span className="agents-section-note">{note}</span>}
          </span>
          {summary && <span className="agents-section-summary">{summary}</span>}
          <svg className={`agents-section-chev${open ? " is-open" : ""}`} viewBox="0 0 24 24" width="20" height="20" aria-hidden="true" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><path d="m6 9 6 6 6-6" /></svg>
        </button>
      </h3>
      {open && <div className="agents-section-body">{action && <div className="agents-section-tools">{action}</div>}{children}</div>}
    </section>
  );
}

/**
 * Раздел «🤖 Агенты» — всё, что касается AI-агентов на машине, в одном месте.
 *
 * Раньше это жило в трёх: «Подписки и лимиты» (остатки), «Настройки →
 * Расширенный» (что установлено, агент по умолчанию, поведение) и шторка в
 * терминале (запуск, флаги, аккаунты). Человек, у которого две подписки Claude
 * и две ChatGPT, ходил за одним разговором по трём экранам.
 *
 * Сначала новая или прошлая беседа, затем доступ и расход, затем установка
 * агентов и их возможности. Срочный запрос разрешения всегда выше действий.
 */
export function AgentsView() {
  const navigate = useNavigate();
  // Аккаунты — всегда у ТОЙ машины, с которой клиент говорит сейчас: каталоги
  // лежат на ней. В облаке лимиты собираются со всех компьютеров, а аккаунты
  // остаются про выбранный — об этом прямо сказано в подписи секции.
  const accountsState = useAgentAccounts(true);
  // OpenRouter: ключ и выбранная модель того компьютера, который сейчас выбран.
  const openrouter = useOpenRouter(true);
  // На первом входе показываем карту задач; содержимое открывают по нужной
  // строке. Прямая ссылка из гида по-прежнему раскрывает целевой раздел.
  const sections = useOpenSections([]);
  // Пришли сюда ЗА конкретной секцией (шторка запуска, «Нет своей подписки?»):
  // раскрываем её и подводим к ней экран. Без этого человек попадал на общий
  // экран, где нужная секция свёрнута, — то есть переход обрывался на шаг
  // раньше цели.
  const location = useLocation();
  const [agents, setAgents] = useState<AgentInfo[]>([]);
  const [agentsLoading, setAgentsLoading] = useState(true);
  const [agentsError, setAgentsError] = useState(false);
  const focus = agentSectionFocus((location.state as { focus?: string } | null)?.focus)
    || agentFocusFromSearch(location.search);
  const focusAgentID = (location.state as { agentID?: string } | null)?.agentID || "";
  useEffect(() => {
    if (!focus) return;
    sections.ensureOpen(focus);
    // Кадр на раскрытие секции, потом прокрутка: до перерисовки узла ещё нет.
    const timer = window.setTimeout(() => {
      const card = focusAgentID
        ? Array.from(document.querySelectorAll<HTMLElement>(".agents-account-card")).find((el) => el.dataset.agentId === focusAgentID)
        : null;
      (card || document.querySelector(focus === "accounts" ? ".agents-accounts-block" : `.agents-${focus}`))
        ?.scrollIntoView({ block: "start", behavior: "auto" });
    }, 80);
    return () => window.clearTimeout(timer);
    // sections пересоздаётся каждым рендером — в зависимостях только цель.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [focus, focusAgentID, agents]);
  // Справка экрана: чужих слов здесь больше, чем на любом другом (окно,
  // аккаунт, подписка, ключ), а объяснений в шапке не было вовсе.
  const [helpOpen, setHelpOpen] = useState(false);
  const [checkAgentID, setCheckAgentID] = useState("");
  const [scanning, setScanning] = useState(false);
  // Просьбы агента изменить опасную настройку. Решение принимается здесь, где
  // видно, ЧТО именно меняется и зачем.
  const [requests, setRequests] = useState<AgentSettingRequest[]>([]);
  const [answering, setAnswering] = useState("");
  // Настройки поведения агентов живут в конфиге АГЕНТА, а не в памяти пульта:
  // распознавание вопросов работает на компьютере и касается всех его
  // терминалов, а агент по умолчанию — того, кого поднимут по кнопке.
  const [config, setConfig] = useState<AppConfig | null>(null);
  // Вне облака (окно exe и LAN) облачного инвентаря нет вовсе: лимиты отдаёт
  // агент того компьютера, к которому подключён клиент, — ровно та машина, на
  // которой человек и запускает Claude Code с Codex.
  const cloudMode = getMode() === "cloud";
  const [me, setMe] = useState<CloudMe | null>(null);
  const [devices, setDevices] = useState<CloudDevice[]>([]);
  const [results, setResults] = useState<DeviceAIUsage[]>([]);
  const revision = useSyncExternalStore(subscribeUsage, usageRevision);
  const now = useUsageNow();
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  // Отказ «этот агент ещё не умеет лимиты» лечится обновлением на самом ПК, и
  // подсказка про отложенный перезапуск нужна ровно в этом случае.
  const [localNeedsUpdate, setLocalNeedsUpdate] = useState(false);

  // Список агентов машины: нужен и для секции аккаунтов (у кого их вообще
  // можно заводить — это знает реестр на Go, поле account_env), и для секции
  // «что установлено». Отказ молчаливый: остальные секции работают без него.
  const loadAgents = useCallback(() => {
    setAgentsLoading(true);
    getAgents()
      .then((d) => {
        const list = d.agents || [];
        setAgentRegistry(list);
        setAgents(list);
        setAgentsError(false);
        setAgentsLoading(false);
        // Список агентов Remotai считает ОДИН РАЗ при старте. Человек ставит
        // агента командой в терминале, возвращается сюда — и видит прежнюю
        // кнопку «Установить», как будто установка не удалась. Так и было:
        // «установил, а он всё равно пишет установить».
        //
        // Поэтому если кто-то числится ненайденным, пересчитываем сами, молча.
        // Пересчёт стоит одного скана PATH и делается только когда есть что
        // искать: у того, у кого всё найдено, лишней работы не будет.
        if (list.some((a) => a.install && !a.detected)) {
          rescanAgents()
            .then((r) => {
              const fresh = r.agents || [];
              if (fresh.length) { setAgentRegistry(fresh); setAgents(fresh); }
            })
            .catch(() => { /* остаётся то, что уже показано — это честнее пустого */ });
        }
      })
      .catch(() => { setAgentsError(true); setAgentsLoading(false); });
  }, []);

  /** Просьбы агента: пустой ответ и отказ одинаково означают «показывать нечего». */
  const loadRequests = useCallback(() => {
    getAgentRequests()
      .then((d) => setRequests((d.requests || []).filter((r) => r.status === "pending")))
      .catch(() => setRequests([]));
  }, []);

  useEffect(() => {
    loadAgents();
    loadRequests();
    getConfig().then(setConfig).catch(() => setConfig(null));
  }, [loadAgents, loadRequests]);

  /**
   * Ответ человека. Подтверждение сразу и применяет настройку — разводить
   * «согласился» и «сработало» по разным шагам значило бы оставить человека с
   * ощущением, что он разрешил, а ничего не произошло.
   */
  const answer = async (id: string, approve: boolean) => {
    haptic();
    setAnswering(id);
    try {
      const { request } = await answerAgentRequest(id, approve);
      if (request?.error) setError(request.error);
      // Настройки могли измениться этим же действием — перечитываем.
      getConfig().then(setConfig).catch(() => {});
    } catch (e) {
      setError(mapApiError(e));
    } finally {
      setAnswering("");
      loadRequests();
    }
  };

  /** Пересканировать: только что поставленный агент иначе числится «не найден»
   *  до перезапуска Remotai (список считается один раз при старте). */
  const rescan = () => {
    haptic();
    setScanning(true);
    rescanAgents()
      .then((d) => { setAgentRegistry(d.agents || []); setAgents(d.agents || []); setAgentsError(false); })
      .catch(() => { setAgentsError(true); /* прежний список остаётся виден */ })
      .finally(() => setScanning(false));
  };

  /** Настройка уходит на агента сразу: она про его компьютер, а не про пульт. */
  const updateSetting = async (key: string, value: string | boolean) => {
    haptic();
    const before = config;
    setConfig(config ? { ...config, [key]: value } as AppConfig : config);
    try {
      await updateConfig({ [key]: value });
    } catch {
      // Откатываем показанное: молча оставить галочку включённой — соврать.
      setConfig(before);
    }
  };

  const load = useCallback(async (force = false) => {
    setLoading(true);
    setError("");
    try {
      if (!cloudMode) {
        const localUsage = await fetchLocalAIUsage(force);
        setLocalNeedsUpdate(!!localUsage.needsUpdate);
        if (localUsage.error) setError(localUsage.error);
        return;
      }
      const [profile, inventory] = await Promise.all([getMe(), listDevices()]);
      const allDevices = inventory.devices || [];
      setMe(profile);
      setDevices(allDevices);
      setResults(await collectInfrastructureAIUsage(allDevices, force));
    } catch (reason) {
      setError(mapApiError(reason));
    } finally {
      setLoading(false);
    }
  }, [cloudMode]);

  // Экран грузился один раз и молчал часами: оставленный открытым (или поднятый
  // из фона) он показывал проценты часовой давности как свежие. usePolling
  // обновляет их, пока экран виден, и сразу при возврате из фона — а в фоне не
  // будит ни телефон, ни агентов.
  usePolling(load, REFRESH_MS);

  /**
   * Куда ведёт «←».
   *
   * Экранная стрелка в облаке всегда уносила в «Инфраструктуру» — экран, где
   * человек мог вообще не быть (лимиты открывают с главной и из «Системы»), а
   * системная «Назад» с того же места вела на главную. Две кнопки «Назад» на
   * одном экране, ведущие в разные места, человек предсказать не может, поэтому
   * обе теперь зовут одно правило из navBack.ts: шаг назад по истории, а по
   * прямой ссылке — «Инфраструктура» в облаке и «Система» при прямом
   * подключении. Длину `window.history` тут больше не спрашиваем: в APK и
   * Telegram маршруты живут в MemoryRouter, где она про WebView, а не про нас.
   */
  const stepBack = useGoBack();
  const goBack = () => {
    haptic();
    stepBack();
  };

  const currentResults = useMemo(() => results.map(result => result.needsUpdate ? result
    : ({ device: result.device, ...latestDeviceUsage(result.device) })), [results, revision]);
  const groups = useMemo(
    () => (cloudMode ? groupUsage(currentResults)
      : groupLocalUsage(selectedUsageResource().get().snapshot || null)),
    [cloudMode, revision, currentResults],
  );
  const failed = currentResults.filter((result) => !!result.error);
  const offline = devices.filter((device) => !device.online);
  // Подписка и привязанная карта. Тянем отдельным запросом: /v1/me про карту
  // ничего не знает, а экран отвязки — обязательное условие ЮKassa для
  // включения автоплатежей (менеджер, 01.09.2026).
  const [sub, setSub] = useState<CloudSubscription | null>(null);
  const [unbinding, setUnbinding] = useState(false);
  // Отдельная отметка вместо тоста: этого экрана тосты не знают, а
  // человеку нужен ответ «карта отвязана» прямо там, где он нажал.
  const [unbindDone, setUnbindDone] = useState(false);
  useEffect(() => {
    let gone = false;
    getSubscription().then((d) => { if (!gone) setSub(d); }).catch(() => { /* касса может быть выключена */ });
    return () => { gone = true; };
  }, []);

  const handleUnbind = async () => {
    if (unbinding) return;
    if (!window.confirm(t("billing.unbindConfirm"))) return;
    setUnbinding(true);
    try {
      const r = await unbindCard();
      setSub((prev) => (prev ? { ...prev, card: null, auto_renew: false, paid_until: r.paid_until ?? prev.paid_until } : prev));
      setError("");
      setUnbindDone(true);
    } catch (e: any) {
      setError(mapApiError(e));
    } finally {
      setUnbinding(false);
    }
  };

  const plan = me ? planState(me) : null;
  const planHeadline = plan
    ? t(
        plan.kind === "founder" ? "agents.planFounder"
          : plan.kind === "beta" ? "agents.planBeta"
          : plan.kind === "trial" ? "agents.planTrial"
          : plan.kind === "pro" ? "agents.planPro"
          : "agents.planFree",
      )
    : "";
  /**
   * Заявка на Про, пока кассы нет. Обращение уходит обычным каналом
   * поддержки — заготовку кладём в черновик и переводим человека туда, где он
   * её видит и отправляет сам. Начатое им обращение не затираем.
   */
  const requestPro = () => {
    prefillSupportDraft(t("agents.planRequestDraft"));
    navigate("/support");
  };
  const planNote = plan
    ? plan.kind === "founder" ? t("agents.planFounderNote")
      : plan.kind === "beta" ? t("agents.planBetaNote")
      : plan.kind === "trial" ? t("agents.planTrialNote", { days: String(plan.trialDaysLeft) })
      : plan.kind === "pro" ? t("agents.planProNote")
      : t("agents.planFreeNote")
    : "";
  // Аккаунты бывают не у всех: у встроенных агентов подписки нет вовсе, а у
  // части CLI каталог переменной не выбирается — гадать нельзя, поэтому список
  // строим по тому, что агент сам объявил (`account_env`).
  const accountAgents = agents.filter((a) => !!a.account_env && a.detected);
  // В списке «что установлено» показываем только CLI-агентов: у встроенных
  // (shell, оркестратор) нет ни команды запуска, ни подписки — ставить нечего.
  const cliAgents = agents.filter((a) => !!a.cli);
  const detectedAgents = agents.filter((a) => a.detected);
  const detectedCliCount = cliAgents.filter((a) => a.detected).length;
  // «Проверить подключение»: какой агент открыт в шторке (null — закрыта).
  const checkAgent = agents.find((a) => a.id === checkAgentID) || null;
  const openCheck = (id: string) => { haptic(); setCheckAgentID(id); };

  return (
    <div className="usage-page">
      <header className="usage-header">
        <button className="infra-back" aria-label={t("usage.back")} onClick={goBack}>←</button>
        <div>
          {cloudMode
            ? <div className="infra-eyebrow">{t("agents.eyebrow")}</div>
            : <DeviceChip />}
          <h1>{t("agents.title")}</h1>
          <p>{t(cloudMode ? "agents.subtitleCloud" : "agents.subtitleLocal")}</p>
        </div>
        {/* Обе кнопки — в ОДНОЙ обёртке: `.usage-header` это грид из трёх
            колонок (44px / текст / auto), и четвёртый прямой потомок уехал бы
            на вторую строку. Класс `.usage-refresh` кнопке «?» давать нельзя:
            на узком экране у него `font-size: 0` и `::after { content: "↻" }` —
            получилась бы вторая кнопка «Обновить». */}
        <div className="infra-header-actions">
          <button className="usage-refresh" aria-label={t("usage.refresh")} disabled={loading} onClick={() => { haptic(); void load(true); }}>
            {loading ? t("usage.refreshing") : `↻ ${t("usage.refresh")}`}
          </button>
          <button className="infra-back" aria-label={t("help.title")} onClick={() => { haptic(); setHelpOpen(true); }}>?</button>
        </div>
      </header>

      <main className="usage-content">
        {/* ── Просьбы агента ────────────────────────────────────────────────
            Стоит ПЕРВЫМ и ни за каким аккордеоном: агент ждёт ответа прямо
            сейчас, а настройки, о которых он просит, могут отрезать телефон от
            компьютера — решать такое человек должен видя, что именно меняется
            и зачем. Комментарий обещал это и раньше, но в облаке над просьбой
            стояла карточка тарифа: теперь порядок совпал с обещанием. */}
        {requests.length > 0 && (
          <section className="agents-requests">
            <h2 className="usage-section-title"><span>{t("agents.requestTitle")}</span></h2>
            {requests.map((r) => (
              <article key={r.id} className="agents-request">
                <div className="agents-request-body">
                  <b>{r.title} — {requestValueLabel(r.value)}</b>
                  {/* Причина — главное здесь: по имени ключа решить нельзя. */}
                  <small>{r.reason || t("agents.requestNoReason")}</small>
                </div>
                <div className="agents-request-actions">
                  <button
                    className="btn btn-sm btn-secondary"
                    disabled={answering === r.id}
                    onClick={() => void answer(r.id, false)}
                  >
                    {t("agents.requestReject")}
                  </button>
                  <button
                    className="btn btn-sm btn-primary"
                    disabled={answering === r.id}
                    onClick={() => void answer(r.id, true)}
                  >
                    {t("agents.requestApprove")}
                  </button>
                </div>
              </article>
            ))}
          </section>
        )}

        <section className="agents-work" aria-label={t("agents.workTitle")}>
          {agents.filter(agent => agent.native_route).map(agent => (
            <button type="button" className="agents-start-entry" key={agent.id} onClick={() => { haptic(); navigate(agent.native_route!); }}>
              <span><b>{agent.name}</b><small>{t("ui.agentsview.m67949b41cc")}</small></span>
              <svg viewBox="0 0 24 24" width="20" height="20" aria-hidden="true" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><path d="m9 6 6 6-6 6" /></svg>
            </button>
          ))}
          <button type="button" className="agents-start-entry" onClick={() => { haptic(); navigate("/pty?new=1&agent=1"); }}>
            <span>
              <b>{t("agents.startTitle")}</b>
              <small>{t("agents.startNote")}</small>
            </span>
            <svg viewBox="0 0 24 24" width="20" height="20" aria-hidden="true" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><path d="m9 6 6 6-6 6" /></svg>
          </button>
          <button type="button" className="as-entry" onClick={() => { haptic(); navigate("/agents/sessions?from=/agents"); }}>
            <span>
              <b>{t("agentSessions.entryTitle")}</b>
              <small>{t("agentSessions.entryNote")}</small>
            </span>
            <svg viewBox="0 0 24 24" width="20" height="20" aria-hidden="true" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><path d="m9 6 6 6-6 6" /></svg>
          </button>
        </section>

        {/* ── Тариф Remotai ────────────────────────────────────────────────
            Тариф — свойство облачного аккаунта, поэтому только в cloudMode и
            когда профиль загрузился. Пока идёт бета — карточка честно говорит
            «бесплатно в бете»; founder видит «навсегда»; пробнику капает
            отсчёт. Кнопка оформления показывается только когда касса включена
            (plan.showUpgrade), то есть после запуска оплаты. */}
        {cloudMode && plan && (
          <section className="agents-plan">
            {/* Тариф не сворачивается: это одна карточка, а не список, и
                прятать за шторкой срок пробного периода нечестно. */}
            <h2 className="usage-section-title"><span>{t("agents.planTitle")}</span></h2>
            <article className={`plan-card plan-${plan.kind}`}>
              <div className="plan-head">
                <b className="plan-name">{planHeadline}</b>
                {plan.kind === "founder" && <span className="plan-badge" aria-hidden>∞</span>}
                {plan.kind === "trial" && <span className="plan-badge">{plan.trialDaysLeft}</span>}
              </div>
              <p className="plan-note">{planNote}</p>
              {plan.devices.max > 0 && (
                <div className="plan-devices">
                  <span>{t("agents.planDevices", { used: String(plan.devices.used), max: String(plan.devices.max) })}</span>
                  <div className="plan-bar"><div style={{ width: `${plan.devices.percent}%` }} /></div>
                </div>
              )}
              {plan.showUpgrade && (
                <button
                  className="btn btn-primary btn-sm plan-upgrade"
                  // Раньше уводило на сайт, где оплатить всё равно было нечем.
                    // Теперь — на экран подписки, где и деньги, и карта.
                    onClick={() => { haptic(); navigate("/plan"); }}
                >
                  {t("agents.planUpgrade")}
                </button>
              )}
              {/* Кассы ещё нет — и раньше здесь не было НИЧЕГО: ни цены, ни
                  хода. Заявка уходит тем же каналом, что и любое обращение
                  (раздел «Поддержка» — переписка через релей), с уже
                  набранным текстом: человеку остаётся нажать «Отправить». */}
              {plan.showRequest && (
                <>
                  <button
                    className="btn btn-secondary btn-sm plan-upgrade"
                    onClick={() => { haptic(); requestPro(); }}
                  >
                    {t("agents.planRequest")}
                  </button>
                  <p className="plan-note">{t("agents.planRequestNote")}</p>
                </>
              )}
              {/* ── Способ оплаты и отвязка карты ──────────────────────
                  Этот блок — обязательное условие ЮKassa: автоплатежи включают
                  только увидев, что человек может отвязать карту САМ и в любой
                  момент (менеджер, 01.09.2026). Плюс наше обязательство перед
                  ними: при отвязке удалить токен повторов у себя, что и делает
                  POST /v1/billing/unbind.

                  Показываем и КОГДА карты нет: «автоматических списаний нет» —
                  это ответ на вопрос «а меня не спишут внезапно», который
                  человек задаёт ровно здесь. */}
              {sub && (
                <div className="plan-card-block">
                  <div className="plan-card-title">{t("billing.cardTitle")}</div>
                  {sub.card ? (
                    <>
                      <div className="plan-card-line">
                        {t("billing.cardBound", { type: sub.card.type || t("ui.accountview.m5736d0caff"), last4: sub.card.last4 })}
                      </div>
                      <p className="plan-note">{t("billing.autoRenew")}</p>
                      {sub.paid_until && (
                        <p className="plan-note">
                          {t("billing.paidUntil", { date: new Date(sub.paid_until).toLocaleDateString(getLocale()) })}
                        </p>
                      )}
                      <button
                        className="btn btn-secondary btn-sm plan-upgrade"
                        onClick={() => { haptic(); void handleUnbind(); }}
                        disabled={unbinding}
                      >
                        {t("billing.unbind")}
                      </button>
                    </>
                  ) : (
                    <p className="plan-note">
                      {unbindDone ? t("billing.unbindDone") : t("billing.noCard")}
                    </p>
                  )}
                </div>
              )}
            </article>
          </section>
        )}

        <section className="agents-group" aria-labelledby="agents-access-title">
          <h2 id="agents-access-title" className="agents-group-title">{t("agents.accessGroup")}</h2>
          <div className="agents-section-list">
        {/* ── Аккаунты ──────────────────────────────────────────────────────
            Первым идёт ответ на вопрос «каким аккаунтом я работаю и сколько
            там осталось»: ради него человек и держит вторую подписку.
            Показываем только агентов, которые аккаунты умеют, — переменную
            называет реестр на Go (`account_env`), клиент её не хардкодит. */}
        {accountAgents.length > 0 && (
          <AgentsSection
            className="agents-accounts-block"
            title={t("agents.accountsTitle")}
            note={t("agents.accountsSummary")}
            open={sections.isOpen("accounts")}
            onToggle={() => sections.toggle("accounts")}
          >
            <p className="agents-section-context">{t(cloudMode ? "agents.accountsHereCloud" : "agents.accountsHere")}</p>
            {accountAgents.map((a) => {
              const active = activeAccountOfAgent(accountsState, a.id);
              return (
                <article key={a.id} data-agent-id={a.id} className={`agents-account-card${focusAgentID === a.id ? " is-target" : ""}`}>
                  <div className="agents-account-card-head">
                    <span className="agent-launch-icon">{a.icon}</span>
                    <div>
                      <b>{a.name}</b>
                      <small>{active ? accountTitle(active) : t("agentLaunch.accountDefault")}</small>
                    </div>
                  </div>
                  {/* «Войти» здесь — не запуск на месте: терминала на этом
                      экране нет. Уводим в терминал с открытой шторкой агента,
                      где «Запустить» уже уйдёт под выбранным аккаунтом.
                      Аккаунт едет в адресе (`?account=`): без него шторка
                      запускала агента под АКТИВНЫМ аккаунтом, а не под тем, у
                      которого нажали «Войти» (аудит ИА 02.09.2026, D4). */}
                  <AgentAccountsList
                    agentID={a.id}
                    state={accountsState}
                    hint={false}
                    signInLabel={t("agents.signInTerminal")}
                    onSignIn={(acc) => { haptic(); navigate(`/pty?new=1&agent=1&account=${encodeURIComponent(acc.id)}`); }}
                  />
                  {canCheckConnection(a) && (
                    <button className="btn btn-secondary ac-open" onClick={() => openCheck(a.id)}>
                      {t("agentCheck.button")}
                    </button>
                  )}
                </article>
              );
            })}
            <div className="agent-account-note">{t("agents.accountsNote")}</div>
          </AgentsSection>
        )}

        {/* ── OpenRouter ────────────────────────────────────────────────────
            Стоит СРАЗУ ПОСЛЕ аккаунтов и до списка установленного, потому что
            отвечает на тот же вопрос — «чем агенту думать», — но для человека
            БЕЗ подписок: один ключ вместо счёта у каждого вендора. Свёрнут по
            умолчанию: предложение целиком читается по самой шапке («Без своей
            подписки · ключ хранится на этом компьютере»), а форма ввода ключа
            нужна один раз в жизни и всё остальное время просто занимала
            экран. */}
        <AgentsSection
          className="agents-openrouter"
          title={t("agents.openrouterTitle")}
          note={t("agents.openrouterSummary")}
          summary={openrouter.status?.configured ? t("agents.openrouterConnected") : undefined}
          open={sections.isOpen("openrouter")}
          onToggle={() => sections.toggle("openrouter")}
        >
          <OpenRouterCard state={openrouter} />
        </AgentsSection>

        {/* ── AI-подписки ───────────────────────────────────────────────────
            У этого блока не было ни своей секции, ни заголовка-элемента:
            подпись висела прямым потомком <main>, а карточки лимитов и
            «Покрытие сводки» были ей соседями, а не содержимым. Время снимка
            отдаём через `action`, а не через `note`: внутри кнопки оно вошло бы
            в доступное имя заголовка, и то менялось бы каждые три минуты
            («AI-подписки обновлено в 14:02» → «…14:05»). */}
        <AgentsSection
          className="agents-limits"
          title={t("usage.sectionTitle")}
          note={t("agents.limitsSummary")}
          action={<small>{t("usage.autoRefresh")}</small>}
          open={sections.isOpen("limits")}
          onToggle={() => sections.toggle("limits")}
        >
        {loading && groups.length === 0 ? (
          <div className="usage-loading"><i />{t(cloudMode ? "usage.loadingCloud" : "usage.loadingLocal")}</div>
        ) : error && groups.length === 0 ? (
          <div className="infra-empty">
            <span>⌁</span>
            <h2>{t("usage.loadFailed")}</h2>
            <p>{error}</p>
            {localNeedsUpdate && <p className="usage-coverage-hint">{t("usage.needsUpdateHint")}</p>}
            <button className="btn btn-primary" onClick={() => void load(true)}>{t("usage.retry")}</button>
          </div>
        ) : groups.length === 0 ? (
          <div className="usage-empty">
            <span>◎</span>
            <h2>{t("usage.emptyTitle")}</h2>
            <p>{t(cloudMode ? "usage.emptyCloud" : "usage.emptyLocal")}</p>
          </div>
        ) : (
          <div className="usage-grid">
            {groups.map(({ key, provider, devices: providerDevices }) => {
              const tone = providerTone(provider);
              const hasNumbers = provider.windows.length > 0 || !!provider.extra_usage?.enabled;
              return (
              <article key={key} className={`usage-provider ${tone}${hasNumbers && usageIsStale(provider, now) ? " usage-stale" : ""}`}>
                <div className="usage-provider-head">
                  <span className={`usage-provider-icon ${provider.id}`}>{PROVIDER_ICON[provider.id] || "◎"}</span>
                  <div>
                    {/* Имя аккаунта — рядом с именем сервиса, а не в подписи:
                        при двух подписках на одного вендора карточки
                        отличаются ТОЛЬКО этим, и разбирать их по мелкой почте
                        человек не должен. «Сейчас» — тот, которым запустится
                        агент. */}
                    <b>
                      {provider.name}
                      {provider.account_label && (
                        <span className="usage-account-name"> · {provider.account_label}</span>
                      )}
                      {provider.account_active && (
                        <span className="usage-account-current" title={t("usage.accountCurrentHint")}> ● {t("usage.accountCurrent")}</span>
                      )}
                    </b>
                    <small>{[provider.account, planLabel(provider.plan)].filter(Boolean).join(" · ") || t("usage.localAccount")}</small>
                  </div>
                  <strong className={tone}>{hasNumbers ? usageAgeLabel(provider, now) : toneLabel(tone, null)}</strong>
                </div>

                {/* Чипы машин — только облачный путь: при прямом подключении
                    опрошен один компьютер, и он назван в шапке. */}
                {providerDevices.length > 0 && (
                  <div className="usage-device-chips">
                    {providerDevices.map((device) => (
                      <button key={device.id} onClick={() => navigate(`/infrastructure?focus=${encodeURIComponent(device.id)}`)}>
                        {device.device_type === "server" ? "▰" : "▣"} {device.name}
                      </button>
                    ))}
                  </div>
                )}

                {provider.windows.map((window) => <UsageWindow key={window.id} window={window} />)}
                {provider.next_retry_at && usageTime(provider.next_retry_at) > now && (
                  <p className="usage-note">{t("usage.retryAt", { time: hhmm(new Date(provider.next_retry_at)) })}</p>
                )}

                {provider.extra_usage?.enabled && (
                  <div className="usage-extra">
                    <div>
                      <span>{t("usage.extraTitle")}</span>
                      <b>{t("usage.extraAmount", {
                        used: money(provider.extra_usage.used, provider.extra_usage.currency, provider.extra_usage.decimal_places),
                        limit: money(provider.extra_usage.limit, provider.extra_usage.currency, provider.extra_usage.decimal_places),
                      })}</b>
                    </div>
                    <strong className={percentClass(provider.extra_usage.used_percent)}>{Math.round(provider.extra_usage.used_percent)}%</strong>
                  </div>
                )}

                {/* Карточка без чисел обязана объяснить причину основным текстом,
                    а не 10px-сноской под пустотой. */}
                {hasNumbers
                  ? provider.message && <p className="usage-message">{provider.message}</p>
                  /* ⚠ У «нужен вход» СВОЙ текст, и он важнее сообщения агента.
                     Агент всегда заполняет message («Войдите в Claude Code на
                     этом устройстве» — internal/aiusage/usage.go), и с телефона
                     это читается как «иди к компьютеру», против самого обещания
                     продукта. А строка, которая говорит, ЧТО СДЕЛАТЬ отсюда —
                     «откройте на нём терминал и запустите „claude“» — была
                     мёртвым кодом: до неё очередь не доходила никогда
                     (аудит онбординга 30.08.2026). */
                  : <p className="usage-note">{tone === "signed_out"
                      ? toneNote(tone, provider)
                      : (provider.message || toneNote(tone, provider))}</p>}
                {provider.source && <div className="usage-source">{t("usage.source", { source: provider.source })}</div>}
              </article>
              );
            })}
          </div>
        )}

        {(failed.length > 0 || offline.length > 0) && (
          <section className="usage-coverage">
            <b>{t("usage.coverageTitle")}</b>
            <p>
              {t("usage.coverageAnswered", {
                answered: currentResults.filter(result => result.device.online && result.snapshot && !result.error).length,
                online: devices.filter((device) => device.online).length,
              })}
              {offline.length > 0 ? ` ${t("usage.coverageOffline", { count: offline.length })}` : ""}
            </p>
            {failed.length > 0 && (
              <div className="usage-coverage-fails">
                {failed.map((item) => (
                  <div key={item.device.id} className="usage-coverage-fail">
                    <span><b>{item.device.name}</b>{item.error}</span>
                    <button onClick={() => { haptic(); navigate(`/infrastructure?focus=${encodeURIComponent(item.device.id)}`); }}>
                      {t("usage.openDevice")}
                    </button>
                  </div>
                ))}
                {failed.some((item) => item.needsUpdate) && (
                  <p className="usage-coverage-hint">{t("usage.needsUpdateHint")}</p>
                )}
              </div>
            )}
          </section>
        )}
        </AgentsSection>

        {/* ── Расход токенов ────────────────────────────────────────────────
            Лимиты выше говорят «сколько осталось», этот блок — «куда ушло»:
            по дням, аккаунтам, моделям, папкам и сессиям. Считает сам
            компьютер по файлам сессий (internal/tokenusage). Тело монтируется
            только в раскрытом блоке — свёрнутый не опрашивает компьютер. */}
        <AgentsSection
          className="agents-tokens"
          title={t("tokens.title")}
          note={t("agents.tokensSummary")}
          open={sections.isOpen("tokens")}
          onToggle={() => sections.toggle("tokens")}
        >
          <TokenUsagePanel />
        </AgentsSection>
          </div>
        </section>

        <section className="agents-group" aria-labelledby="agents-setup-title">
          <h2 id="agents-setup-title" className="agents-group-title">{t("agents.setupGroup")}</h2>
          <div className="agents-section-list">
        {/* Сначала проверяем, есть ли агент на компьютере; затем настраиваем
            скиллы и подключения для него. */}
        <AgentsSection
          className="agents-installed"
          title={t("agents.installedTitle")}
          note={t("agents.installedSummary")}
          summary={agentsLoading ? t("agents.loadingList") : agentsError ? t("agents.listUnavailable") : t("agents.installedCount", { found: String(detectedCliCount), all: String(cliAgents.length) })}
          action={(
            <button className="usage-link agents-rescan" disabled={scanning} onClick={rescan}>
              {scanning ? t("agents.rescanning") : `↻ ${t("agents.rescan")}`}
            </button>
          )}
          open={sections.isOpen("installed")}
          onToggle={() => sections.toggle("installed")}
        >
          {cliAgents.length > 0 ? <div className="agents-list">
            {cliAgents.map((a) => (
              <div key={a.id} className="agent-row">
                <span className="agent-row-icon">{a.icon}</span>
                <div className="agent-row-info">
                  <div className="agent-row-name">{a.name}</div>
                  <div className="agent-row-desc">{ownedText(a.description)}</div>
                  {a.path && a.path !== "built-in" && <div className="agent-row-path">{a.path}</div>}
                  {canCheckConnection(a) && (
                    <button className="btn btn-secondary btn-sm ac-open ac-open-row" onClick={() => openCheck(a.id)}>
                      {t("agentCheck.button")}
                    </button>
                  )}
                </div>
                <AgentQuotaChips quota={a.quota} agentName={a.name} agentID={a.id} />
                {a.native_route ? (
                  <button className="btn btn-secondary btn-sm" onClick={() => { haptic(); navigate(a.native_route!); }}>{t("remote.browserOpen")}</button>
                ) : a.detected ? (
                  <span className="agent-row-status" style={{ color: "var(--color-success)" }}>✓</span>
                ) : a.install ? (
                  <button className="btn btn-secondary btn-sm" onClick={() => { haptic(); navigate(`/pty?new=1&agent=${encodeURIComponent(a.id)}&install=1`); }}>
                    {t("agents.install")}
                  </button>
                ) : (
                  <span className="agent-row-status" style={{ color: "var(--tg-hint)" }}>✗</span>
                )}
                {a.detected && <AgentUpdateLine agentId={a.id} />}
              </div>
            ))}
          </div> : <p className="agents-section-empty">{agentsLoading ? t("agents.loadingList") : agentsError ? t("agents.listUnavailableHint") : t("agents.noCli")}</p>}
        </AgentsSection>

        {/* ── Скиллы ────────────────────────────────────────────────────────
            Что стоит у каждого агента, установка из GitHub/ZIP, копия другому
            агенту, удаление с возвратом (SkillsPanel.tsx). Тело — только в
            раскрытом блоке: свёрнутый не ходит на компьютер. */}
        <AgentsSection
          className="agents-skills"
          title={t("skills.title")}
          note={t("agents.skillsSummary")}
          open={sections.isOpen("skills")}
          onToggle={() => sections.toggle("skills")}
        >
          <SkillsPanel />
        </AgentsSection>

        {/* ── MCP-серверы ── подключить агенту новые умения без правки JSON
            (McpPanel.tsx, internal/mcpmgr). Свёрнут: настраивают один раз. */}
        <AgentsSection
          className="agents-mcp"
          title={t("mcp.sectionTitle")}
          note={t("agents.mcpSummary")}
          open={sections.isOpen("mcp")}
          onToggle={() => sections.toggle("mcp")}
        >
          <McpPanel />
        </AgentsSection>

        {/* ── Как запускать ─────────────────────────────────────────────────
            Настройки, которые меняют поведение агентов, — здесь же, а не в
            общих настройках: одно место на один разговор. */}
        {config && (
          <AgentsSection
            className="agents-behaviour"
            title={t("agents.behaviourTitle")}
            note={t("agents.behaviourSummary")}
            summary={detectedAgents.find((agent) => agent.id === config.default_agent)?.name}
            open={sections.isOpen("behaviour")}
            onToggle={() => sections.toggle("behaviour")}
          >
            {detectedAgents.length > 0 && (
              <div className="setting-group">
                <div className="setting-label">{t("settings.defaultAgent")}</div>
                <div className="setting-options">
                  {detectedAgents.map((a) => (
                    <button
                      key={a.id}
                      className={`setting-chip ${config.default_agent === a.id ? "active" : ""}`}
                      onClick={() => void updateSetting("default_agent", a.id)}
                    >
                      {a.icon} {a.name}
                    </button>
                  ))}
                </div>
              </div>
            )}
            {/* Кнопки ответа на вопрос агента. Выключено по умолчанию: чтобы
                показать их, компьютер читает ЭКРАН терминала, а текст на экране
                легко спутать — агент печатал ответ ПРО вопросы, и терминал
                объявлял «ждёт подтверждения» при законченной работе. */}
            <div className="settings-info-card">
              {/* Нажимается ВСЯ строка, а не только тумблер: сам тумблер —
                  цель в 26 px, промахнуться по нему пальцем легко (замер
                  shot-agents-page ловит цели ниже 40 px). Тумблер остался
                  прежним на вид — это тот же орган, что во всех настройках. */}
              <button
                className="agents-toggle-row"
                onClick={() => void updateSetting("detect_agent_questions", !config.detect_agent_questions)}
                aria-pressed={!!config.detect_agent_questions}
              >
                <span className="settings-info-label">{t("settings.detectQuestions")}</span>
                <span className={`advanced-toggle${config.detect_agent_questions ? " on" : ""}`} aria-hidden>
                  <span className="advanced-toggle-thumb" />
                </span>
              </button>
              <div className="agents-setting-hint">{t("settings.detectQuestionsHint")}</div>
            </div>
          </AgentsSection>
        )}
          </div>
        </section>

        {/* Двух пояснительных карточек в подвале больше нет: «почему у
            некоторых сервисов нет процентов» и «без передачи ключей» переехали
            в справку под «?» теми же ключами. Внизу свитка их читал тот, кто
            долистал, а условие `groups.length > 0` прятало объяснение
            приватности ровно тогда, когда данных нет и вопросов больше всего. */}
      </main>
      {/* Справка экрана. Последние две карточки — те самые объяснения, что
          стояли карточками в подвале: ключи прежние, ничего не потеряно. */}
      <HelpSheet
        open={helpOpen}
        onClose={() => setHelpOpen(false)}
        // Аудит ИА 02.09.2026, P1-5: из подсказок экрана — в тему гида.
        guide="agents"
        title={t("agents.helpTitle")}
        items={[
          { icon: "⏱", title: t("agents.help.windowTitle"), text: t("agents.help.windowText") },
          { icon: "👤", title: t("agents.help.accountTitle"), text: t("agents.help.accountText") },
          { icon: "🔀", title: t("agents.help.openrouterTitle"), text: t("agents.help.openrouterText") },
          { icon: "％", title: t("usage.footnoteTitle"), text: t("usage.footnoteText") },
          { icon: "◈", title: t("usage.privacyTitle"), text: t("usage.privacyText") },
        ]}
      />
      {/* Раздел, обещанный навигацией, отбирал саму навигацию: уйти отсюда в
          «Файлы» одним нажатием было нельзя, а на широком экране «Агентов» не
          было в сайдбаре вовсе (UX-аудит 2026-08-23, п. 2.1). */}
      {checkAgent && (
        <AgentCheckSheet
          agentID={checkAgent.id}
          agentName={checkAgent.name}
          accounts={accountsOfAgent(accountsState, checkAgent.id).map((acc) => ({ id: acc.id, title: accountTitle(acc) }))}
          initialAccount={activeAccountOfAgent(accountsState, checkAgent.id)?.id || ""}
          onClose={() => setCheckAgentID("")}
        />
      )}
      <BottomNav active="usage" />
    </div>
  );
}
import { ownedText } from "@tgcontrol/shared";
