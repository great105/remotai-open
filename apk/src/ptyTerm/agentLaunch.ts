import { t } from "@tgcontrol/shared";
/**
 * Запуск агента с флагами: сборка команды и память выбора.
 *
 * Живая просьба владельца (04.08.2026): «в команды запуска агентов надо добавить
 * флаги дангерус пермиссон скипс и другие полезные; сделать удобно, чтобы можно
 * было запустить с разными флагами». Само по себе «допишите флаг руками» ответом
 * не является: набирать `--dangerously-skip-permissions` с телефона — это
 * двадцать девять символов без единой опечатки, и делать это надо КАЖДЫЙ раз.
 *
 * Поэтому здесь два правила:
 *   • набор флагов у каждого агента СВОЙ и приходит с агента (реестр на Go), а
 *     не зашит в клиент — новый агент появляется в шторке без правок фронта;
 *   • выбор ЗАПОМИНАЕТСЯ по агенту: со второго раза запуск — одно нажатие.
 *
 * Правила вынесены сюда, а не оставлены в компоненте, чтобы проверяться тестом
 * без React и DOM (общий приём проекта).
 */

import type { AgentAccount, AgentInfo } from "../types";

const PREFS_KEY = "pty.agentLaunch.v1";
const SAFE_PROXY_CONTRACT_VERSION = 1;
const ENV_NAME_RE = /^[A-Za-z_][A-Za-z0-9_]*$/;

/** Proxy env, который contract v1 разрешает передать CLI. */
const PROXY_URL_ENV_NAMES = [
  "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY",
  "http_proxy", "https_proxy", "all_proxy",
] as const;
const PROXY_AUX_ENV_NAMES = ["NO_PROXY", "no_proxy"] as const;
const PROXY_ENV_NAMES = [...PROXY_URL_ENV_NAMES, ...PROXY_AUX_ENV_NAMES] as const;
const PROXY_ENV_EXACT = new Set<string>(PROXY_ENV_NAMES);
const PROXY_URL_ENV = new Set(["HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY"]);
const PROXY_AUX_ENV = new Set(["NO_PROXY"]);
const LOOPBACK_NO_PROXY = "localhost,127.0.0.1,::1";

/**
 * Всё proxy-окружение, которое когда-либо могло остаться в PowerShell.
 * Contract v1 не разрешает SOCKS/NODE-переменные, но перед каждым
 * запуском их всё равно нужно снять, а затем точно восстановить.
 */
const WINDOWS_ACCOUNT_ENV_SCOPE = [
  "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY",
  "SOCKS_PROXY", "SOCKS5_PROXY", "NODE_USE_ENV_PROXY",
];
const POSIX_ACCOUNT_ENV_SCOPE = [
  ...PROXY_ENV_NAMES,
  "SOCKS_PROXY", "SOCKS5_PROXY", "NODE_USE_ENV_PROXY",
  "socks_proxy", "socks5_proxy", "node_use_env_proxy",
] as const;

const BLOCKED_PROXY_UPDATE = t("ui.agentlaunch.m64f917c9e5");
const BLOCKED_PROXY_FORMAT = t("ui.agentlaunch.m8bf431d60e");
const BLOCKED_PROXY_LEGACY = t("ui.agentlaunch.m2e43b27146");
const BLOCKED_ACCOUNT_ENV = t("ui.agentlaunch.mc4b67f418b");
const BLOCKED_ACCOUNTS_LOADING = t("ui.agentlaunch.m5faf82eb73");
/** Экспортируется: шторка запуска показывает по этой причине объяснение и ход
 *  наверху, а не оставляет её сноской в погасшей строке (J03, 05.09.2026). */
export const BLOCKED_ACCOUNTS_MISSING = t("ui.agentlaunch.m38e8391e01");

/** Что человек выбрал для конкретного агента. */
export interface LaunchPrefs {
  /** Флаги из списка агента (хранятся строками — список у агента может измениться). */
  flags: string[];
  /** Дописанное руками: `--model sonnet`, свой ключ и т.п. */
  extra: string;
}

export const EMPTY_PREFS: LaunchPrefs = { flags: [], extra: "" };

/** Описание флага, каким его присылает агент (см. LaunchFlag в registry.go). */
export interface FlagSpec {
  flag: string;
  title?: string;
  hint?: string;
  danger?: boolean;
  /** Переменные окружения, без которых флаг не работает (только POSIX). */
  env?: string[];
  /** Флаги, которые обязаны идти вместе с этим. */
  with?: string[];
  /** Где применим: "" везде, "posix", "windows". */
  only?: string;
}

/**
 * Аккаунт, под которым запускать агента.
 *
 * У всех четырёх CLI аккаунт — это КАТАЛОГ, и задаётся он переменной окружения
 * (`CLAUDE_CONFIG_DIR`, `CODEX_HOME`, `GEMINI_CLI_HOME`, `KIMI_CODE_HOME` —
 * какая у кого, знает реестр на Go, клиент их не хардкодит). Поэтому
 * «переключить аккаунт» здесь = дописать одну переменную к команде запуска:
 * ничего не разлогинивается, оба аккаунта живут одновременно.
 *
 * У основного аккаунта переменной нет вовсе — команда остаётся прежней.
 */
export interface LaunchAccount {
  /** Агент, для которого сервер собрал этот аккаунт. */
  agentID?: string;
  envName: string;
  envValue: string;
  /**
   * Остальные переменные аккаунта — прокси и то, что понадобится дальше.
   *
   * Список приходит С АГЕНТА (`env` в ответе `/api/accounts`), клиент их не
   * придумывает: transport конкретного агента допускается компьютером только
   * после проверки. Знать схему клиенту незачем — он подставляет ровно тот
   * список, который прислал компьютер.
   */
  env?: Array<{ name: string; value: string }>;
  /** Версия allowlist-контракта, полученная вместе со списком. */
  proxyContractVersion?: number;
  /** Прокси настроен, даже если sanitizer уже удалил его raw-значения. */
  proxyConfigured?: boolean;
  /** Fail-closed причина; команда при ней не собирается. */
  blockedReason?: string;
}

interface AccountTransportInput {
  agentID?: string;
  expectedAgentID?: string;
  envName?: string;
  expectedEnvName?: string;
  envValue?: string;
  env?: Array<{ name: string; value: string }>;
  proxyURL?: string;
  proxyLegacy?: string;
  proxyBlocked?: boolean;
  proxyConfigured?: boolean;
  proxyContractVersion?: number;
  blockedReason?: string;
}

interface AccountTransportResult {
  env: Array<{ name: string; value: string }>;
  profileEnv: Array<{ name: string; value: string }>;
  safeProxyURL?: string;
  proxyConfigured: boolean;
  blockedReason: string;
}

function canonicalProxyAuthority(raw: string): string | null {
  try {
    const parsed = new URL(raw);
    const trimmed = raw.trim();
    const afterScheme = trimmed.slice(trimmed.indexOf("://") + 3);
    const authority = afterScheme.split(/[/?#]/, 1)[0] || "";
    const suffix = afterScheme.slice(authority.length);
    const safe = (parsed.protocol === "http:" || parsed.protocol === "https:")
      && !!parsed.hostname
      && !authority.includes("@")
      && !parsed.username
      && !parsed.password
      // Proxy endpoint — только authority. Path/query/fragment часто
      // используют для токенов; им нельзя попадать ни в DOM, ни в shell.
      && (parsed.pathname === "" || parsed.pathname === "/")
      && !parsed.search
      && !parsed.hash
      && (suffix === "" || suffix === "/");
    return safe ? `${parsed.protocol}//${parsed.host}` : null;
  } catch {
    return null;
  }
}

function isSafeProxyURL(raw: string): boolean {
  return canonicalProxyAuthority(raw) !== null;
}

function proxyEnvKind(name: string): "url" | "aux" | "forbidden" | "" {
  const upper = name.toUpperCase();
  if (PROXY_URL_ENV.has(upper)) return "url";
  if (PROXY_AUX_ENV.has(upper)) return "aux";
  if (upper === "NODE_USE_ENV_PROXY" || upper === "SOCKS_PROXY" || upper === "SOCKS5_PROXY") return "forbidden";
  return "";
}

function normalizeLoopbackNoProxy(raw: string): string | null {
  const parts = raw.split(",").map((part) => part.trim());
  const allowed = new Set(LOOPBACK_NO_PROXY.split(","));
  if (parts.length !== allowed.size || new Set(parts).size !== allowed.size) return null;
  return parts.every((part) => allowed.has(part)) ? LOOPBACK_NO_PROXY : null;
}

/**
 * Единая fail-closed проверка для API-sanitizer и самого composeLaunch.
 * На ошибке возвращаем только пару каталога: ни одна часть proxy-группы
 * не доживает до команды или sanitized state.
 */
function analyzeAccountTransport(input: AccountTransportInput): AccountTransportResult {
  const expectedEnvName = (input.expectedEnvName || input.envName || "").trim();
  const profileEnv: Array<{ name: string; value: string }> = [];
  const proxyEnv: Array<{ name: string; value: string }> = [];
  const allEnv: Array<{ name: string; value: string }> = [];
  const seenExact = new Map<string, string>();
  const seenFolded = new Map<string, string>();
  let invalidEnv = false;
  let invalidProxyEnv = false;

  const addPair = (pair?: { name: string; value: string }) => {
    const name = (pair?.name || "").trim();
    if (!name || !ENV_NAME_RE.test(name)) {
      if (name || pair?.value) invalidEnv = true;
      return;
    }
    const rawValue = typeof pair?.value === "string" ? pair.value : "";
    const key = name.toUpperCase();
    const isProfile = Boolean(expectedEnvName && key === expectedEnvName.toUpperCase());
    const kind = isProfile ? "" : proxyEnvKind(name);
    if (!isProfile && !kind) {
      invalidEnv = true;
      return;
    }
    if (kind && !PROXY_ENV_EXACT.has(name)) invalidProxyEnv = true;
    let value = rawValue;
    if (kind === "aux") {
      const normalized = normalizeLoopbackNoProxy(rawValue);
      if (normalized === null) invalidProxyEnv = true;
      else value = normalized;
    }

    const exactPrevious = seenExact.get(name);
    if (exactPrevious !== undefined) {
      if (exactPrevious !== value) {
        if (isProfile) invalidEnv = true;
        else invalidProxyEnv = true;
      }
      return;
    }
    const foldedPrevious = seenFolded.get(key);
    if (foldedPrevious !== undefined && foldedPrevious !== value) {
      if (isProfile) invalidEnv = true;
      else invalidProxyEnv = true;
      return;
    }
    seenExact.set(name, value);
    if (foldedPrevious === undefined) seenFolded.set(key, value);

    if (isProfile) {
      // POSIX не должен получать два разных регистра profile env.
      if (profileEnv.length) return;
      const safe = { name: expectedEnvName, value };
      profileEnv.push(safe);
      allEnv.push(safe);
      return;
    }
    // В POSIX оба регистра значимы: inherited `https_proxy` не должен
    // перебить выбранный аккаунтом `HTTPS_PROXY`.
    const safe = { name, value };
    proxyEnv.push(safe);
    allEnv.push(safe);
  };

  for (const pair of input.env || []) addPair(pair);
  if (input.envName && input.envValue) addPair({ name: input.envName, value: input.envValue });

  const proxyConfigured = Boolean(
    input.proxyConfigured
    || input.proxyURL
    || input.proxyLegacy
    || input.proxyBlocked
    || proxyEnv.length,
  );
  let blockedReason = (input.blockedReason || "").trim();
  if (!blockedReason && input.expectedAgentID && input.agentID && input.expectedAgentID !== input.agentID) {
    blockedReason = BLOCKED_ACCOUNT_ENV;
  }
  if (!blockedReason && expectedEnvName && !ENV_NAME_RE.test(expectedEnvName)) {
    blockedReason = BLOCKED_ACCOUNT_ENV;
  }
  if (!blockedReason && input.envName && expectedEnvName
    && input.envName.toUpperCase() !== expectedEnvName.toUpperCase()) {
    blockedReason = BLOCKED_ACCOUNT_ENV;
  }
  if (!blockedReason && invalidEnv) blockedReason = BLOCKED_ACCOUNT_ENV;
  if (!blockedReason && input.proxyBlocked) blockedReason = BLOCKED_PROXY_LEGACY;
  if (!blockedReason && input.proxyLegacy) blockedReason = BLOCKED_PROXY_LEGACY;

  if (!blockedReason && proxyConfigured && input.proxyContractVersion !== SAFE_PROXY_CONTRACT_VERSION) {
    blockedReason = BLOCKED_PROXY_UPDATE;
  }
  if (!blockedReason && invalidProxyEnv) blockedReason = BLOCKED_PROXY_FORMAT;
  if (!blockedReason && input.proxyURL && !isSafeProxyURL(input.proxyURL)) {
    blockedReason = BLOCKED_PROXY_FORMAT;
  }
  if (!blockedReason && proxyConfigured) {
    for (const pair of proxyEnv) {
      const kind = proxyEnvKind(pair.name);
      if (kind === "forbidden" || (kind === "url" && !isSafeProxyURL(pair.value))) {
        blockedReason = BLOCKED_PROXY_FORMAT;
        break;
      }
    }
    // Contract v1 — не «хотя бы одна похожая переменная», а полный атомарный
    // набор. Иначе inherited lowercase/NO_PROXY может оказаться сильнее и CLI
    // незаметно уйдёт через другой канал или напрямую.
    if (!blockedReason && PROXY_ENV_NAMES.some((name) => !seenExact.has(name))) {
      blockedReason = BLOCKED_PROXY_FORMAT;
    }
    const urlAuthorities = PROXY_URL_ENV_NAMES.map((name) => (
      canonicalProxyAuthority(seenExact.get(name) || "")
    ));
    const commonAuthority = urlAuthorities[0];
    if (!blockedReason && (!commonAuthority || urlAuthorities.some((value) => value !== commonAuthority))) {
      blockedReason = BLOCKED_PROXY_FORMAT;
    }
    const declaredAuthority = input.proxyURL ? canonicalProxyAuthority(input.proxyURL) : null;
    if (!blockedReason && input.proxyURL && declaredAuthority !== commonAuthority) {
      blockedReason = BLOCKED_PROXY_FORMAT;
    }
    if (!blockedReason && PROXY_AUX_ENV_NAMES.some((name) => (
      normalizeLoopbackNoProxy(seenExact.get(name) || "") !== LOOPBACK_NO_PROXY
    ))) {
      blockedReason = BLOCKED_PROXY_FORMAT;
    }
  }

  return {
    env: blockedReason ? profileEnv : allEnv,
    profileEnv,
    safeProxyURL: !blockedReason && input.proxyURL ? input.proxyURL : undefined,
    proxyConfigured,
    blockedReason,
  };
}

/**
 * Причина, по которой нельзя запускать этот аккаунт.
 * Строка никогда не цитирует raw proxy, поэтому её можно показывать в UI.
 */
export function launchAccountBlockedReason(account: AgentAccount, contractVersion?: number): string {
  return analyzeAccountTransport({
    agentID: account.agent_id,
    expectedAgentID: account.agent_id,
    envName: account.env_name,
    expectedEnvName: account.env_name,
    envValue: account.env_value,
    env: account.env,
    proxyURL: account.proxy,
    proxyLegacy: account.proxy_legacy,
    // Промежуточные серверы присылали warning раньше отдельного blocked-флага.
    // Warning означает непроверенный transport и тоже обязан закрыть запуск.
    proxyBlocked: Boolean(account.proxy_blocked || account.proxy_warning),
    proxyConfigured: Boolean(account.proxy || account.proxy_legacy || account.proxy_blocked || account.proxy_warning),
    proxyContractVersion: contractVersion,
  }).blockedReason;
}

/**
 * Отсекает proxy-группу до того, как React положит ответ API в state.
 * Даже подменённый старый agent не сможет оставить userinfo в `proxy`/`env`.
 */
export function normalizeAccountProxyForClient(account: AgentAccount, contractVersion?: number): AgentAccount {
  const analyzed = analyzeAccountTransport({
    agentID: account.agent_id,
    expectedAgentID: account.agent_id,
    envName: account.env_name,
    expectedEnvName: account.env_name,
    envValue: account.env_value,
    env: account.env,
    proxyURL: account.proxy,
    proxyLegacy: account.proxy_legacy,
    proxyBlocked: Boolean(account.proxy_blocked || account.proxy_warning),
    proxyConfigured: Boolean(account.proxy || account.proxy_legacy || account.proxy_blocked || account.proxy_warning),
    proxyContractVersion: contractVersion,
  });
  const normalized: AgentAccount = { ...account };
  if (analyzed.env.length) normalized.env = analyzed.env.map((pair) => ({ ...pair }));
  else delete normalized.env;
  if (analyzed.safeProxyURL) normalized.proxy = analyzed.safeProxyURL;
  else delete normalized.proxy;
  // Legacy-метка нужна UI, но старый agent мог прислать туда raw URL.
  if (normalized.proxy_legacy && /[:@/?#\\]/.test(normalized.proxy_legacy)) {
    normalized.proxy_legacy = t("ui.agentlaunch.md7ebeda500");
  }
  if (analyzed.blockedReason) {
    normalized.proxy_blocked = true;
    normalized.proxy_warning = analyzed.blockedReason;
  }
  return normalized;
}

export interface AgentAccountsLaunchState {
  accounts: AgentAccount[];
  proxyContractVersion: number;
  /** true только для ответа, полученного для текущего cwd. */
  accountsReady: boolean;
}

/** Аккаунт, записанный у конкретной PTY; пустой id означает основной. */
export function recordedResumeAccount(
  accounts: AgentAccount[],
  agentID: string,
  recordedAccountID?: string,
): AgentAccount | null {
  const wanted = recordedAccountID || "default";
  return accounts.find((account) => account.agent_id === agentID && account.id === wanted) || null;
}

/**
 * Выбранный аккаунт для любой локальной двери запуска.
 *
 * Важно, что отсутствие/ошибка `/api/accounts` — не «основной напрямую».
 * Пока сервер не подтвердил список для текущей папки, account-capable CLI
 * запускать нельзя: иначе временный 500 или гонка cwd обходят proxy.
 */
export function launchAccountForAgent(
  agent: Pick<AgentInfo, "id" | "account_env">,
  state: AgentAccountsLaunchState,
  selected?: AgentAccount | null,
): LaunchAccount | undefined {
  const envName = (agent.account_env || "").trim();
  if (!envName) return undefined;
  if (!state.accountsReady) {
    return {
      agentID: agent.id,
      envName,
      envValue: "",
      proxyContractVersion: state.proxyContractVersion,
      blockedReason: BLOCKED_ACCOUNTS_LOADING,
    };
  }
  // `null` здесь означает «нужный записанный аккаунт удалён» и НЕ должен
  // откатываться к текущему active. `undefined` — обычный новый запуск.
  const source = selected !== undefined
    ? selected
    : state.accounts.find((account) => account.agent_id === agent.id && account.active);
  if (!source) {
    return {
      agentID: agent.id,
      envName,
      envValue: "",
      proxyContractVersion: state.proxyContractVersion,
      blockedReason: BLOCKED_ACCOUNTS_MISSING,
    };
  }
  const active = normalizeAccountProxyForClient(source, state.proxyContractVersion);
  const blockedReason = launchAccountBlockedReason(active, state.proxyContractVersion);
  return {
    agentID: active.agent_id || agent.id,
    envName: active.env_name || envName,
    envValue: active.env_value || "",
    env: active.env,
    proxyContractVersion: state.proxyContractVersion,
    proxyConfigured: Boolean(
      active.proxy
      || active.proxy_legacy
      || active.proxy_blocked
      || active.proxy_warning
      || active.env?.some((pair) => /proxy/i.test(pair.name || "")),
    ),
    blockedReason,
  };
}

/**
 * Модель, которой запускать агента (OpenRouter).
 *
 * ЗАЧЕМ ОТДЕЛЬНО ОТ ФЛАГОВ: флаг — это переключатель с готовой строкой, а
 * модель — значение, и меняется оно чаще всего остального. Человек подключил
 * один ключ OpenRouter и выбирает между четырьмя сотнями моделей, в том числе
 * бесплатными.
 *
 * ОБА ПОЛЯ ПРИХОДЯТ С АГЕНТА, клиент не хардкодит ни то, ни другое: `flag` —
 * из реестра на Go (`model_flag`, сверен с живым `--help`), `value` — из
 * настройки на компьютере (`GET /api/openrouter`). Агент, у которого флаг не
 * проверен, модель не получит вовсе — это лучше, чем упавший разбор аргументов.
 *
 * КЛЮЧА ЗДЕСЬ НЕТ И БЫТЬ НЕ МОЖЕТ: команда собирается на телефоне и уходит
 * через облако, поэтому ключ живёт в окружении процесса агента на ПК
 * (internal/openrouter/store.go), а сюда приезжает только имя модели.
 */
export interface LaunchModel {
  flag: string;
  value: string;
}

export interface LaunchContext {
  /** Какой CLI запускаем: не даёт подсунуть аккаунт другого агента. */
  agentID?: string;
  /** Имя profile env из Go-реестра; клиент его не хардкодит. */
  accountEnvName?: string;
  /** Версия proxy allowlist из того же API-ответа. */
  proxyContractVersion?: number;
  /** UI-проверка уже заблокировала аккаунт; composeLaunch повторяет отказ. */
  blockedReason?: string;
  /** POSIX-шелл: Linux/mac-агент или терминал SSH-сервера. */
  posix?: boolean;
  /** Что агент объявил про свои флаги. */
  specs?: FlagSpec[];
  /** Выбранный аккаунт (пусто = основной, как настроено на машине). */
  account?: LaunchAccount;
  /** Модель и флаг, которым агент её принимает. */
  model?: LaunchModel;
  /**
   * Хуки агента (`launch_args` из /api/agents): `--settings <файл>` у Claude,
   * `-c notify=[…]` у Codex. Приходят С АГЕНТА — путь знает только компьютер.
   * Только для запуска на этом компьютере: composeRemoteLaunch их снимает.
   */
  launchArgs?: string[];
}

/** Аргумент без кавычек, если он из «безопасных» символов (флаги вроде `-c`). */
const BARE_ARG_RE = /^[A-Za-z0-9_\-.,:/=@+]+$/;

/**
 * Значение переменной в одинарных кавычках.
 *
 * Кавычки здесь не перестраховка: каталог аккаунта — это путь профиля
 * пользователя, а в нём сплошь и рядом пробелы («C:\Users\Иван Петров\…»).
 * Без кавычек шелл разорвал бы его на два аргумента, и агент стартовал бы с
 * пустым каталогом, то есть с требованием войти заново.
 *
 * Экранирование разное: в POSIX одинарную кавычку внутри закрывают и
 * переоткрывают ('\''), в PowerShell — удваивают ('').
 */
/**
 * Задал ли человек модель сам, в своей добавке.
 *
 * Сверяем с флагом, который назвал агент, и дополнительно с `--model`: длинного
 * имени реестр не передаёт, а пишут руками чаще всего именно его. Это
 * подстраховка, а не знание — цена ошибки здесь маленькая (модель просто не
 * подставится, и останется та, что человек написал сам), а цена дубля больше.
 */
function mentionsFlag(extra: string, flag: string): boolean {
  if (!extra) return false;
  return extra.split(" ").some((token) => token === flag || token === "--model" || token.startsWith("--model="));
}

function quoteValue(value: string, posix: boolean): string {
  const v = value || "";
  return posix
    ? `'${v.split("'").join(`'\\''`)}'`
    : `'${v.split("'").join("''")}'`;
}

function resolveLaunchAccount(ctx: LaunchContext): AccountTransportResult {
  const account = ctx.account;
  const contractVersion = ctx.proxyContractVersion ?? account?.proxyContractVersion;
  return analyzeAccountTransport({
    agentID: account?.agentID,
    expectedAgentID: ctx.agentID,
    envName: account?.envName,
    expectedEnvName: ctx.accountEnvName || account?.envName,
    envValue: account?.envValue,
    env: account?.env,
    proxyConfigured: account?.proxyConfigured,
    proxyContractVersion: contractVersion,
    blockedReason: ctx.blockedReason || account?.blockedReason,
  });
}

/**
 * PowerShell живёт дольше одного CLI. Обычное `$env:X=...; cli`
 * оставляло X в шелле, и следующий «основной» аккаунт незаметно
 * наследовал чужой каталог и proxy. Здесь каждый запуск — временная
 * область: snapshot, clear, safe apply, CLI, exact restore в finally.
 */
function composePowerShellScopedLaunch(
  cmd: string,
  profileEnvName: string,
  accountEnv: Array<{ name: string; value: string }>,
): string {
  const scopeNames: string[] = [];
  const addScopeName = (name: string) => {
    const clean = (name || "").trim();
    if (clean && !scopeNames.some((current) => current.toUpperCase() === clean.toUpperCase())) {
      scopeNames.push(clean);
    }
  };
  addScopeName(profileEnvName);
  for (const name of WINDOWS_ACCOUNT_ENV_SCOPE) addScopeName(name);
  const names = scopeNames.map((name) => quoteValue(name, false)).join(",");
  const capture = `$__remotaiEnvState=@{}; foreach ($__remotaiEnvName in @(${names})) { `
    + `$__remotaiEnvState[$__remotaiEnvName]=@{ Exists=(Test-Path -LiteralPath ('Env:'+$__remotaiEnvName)); `
    + `Value=[Environment]::GetEnvironmentVariable($__remotaiEnvName,'Process') }; `
    + `Remove-Item -LiteralPath ('Env:'+$__remotaiEnvName) -ErrorAction SilentlyContinue }`;
  const apply = accountEnv
    .map((pair) => `$env:${pair.name}=${quoteValue(pair.value, false)}`)
    .join("; ");
  const restore = `foreach ($__remotaiEnvName in @(${names})) { `
    + `$__remotaiSaved=$__remotaiEnvState[$__remotaiEnvName]; `
    + `if ($__remotaiSaved.Exists) { `
    + `[Environment]::SetEnvironmentVariable($__remotaiEnvName,[string]$__remotaiSaved.Value,'Process') `
    + `} else { Remove-Item -LiteralPath ('Env:'+$__remotaiEnvName) -ErrorAction SilentlyContinue } }`;
  // Scriptblock даёт helper-переменным свою PowerShell-область: мы не
  // затираем одноимённые переменные человека и ничего не оставляем после CLI.
  return `& { ${capture}; try { ${apply ? `${apply}; ` : ""}${cmd} } finally { ${restore} } }`;
}

/**
 * Собрать команду запуска.
 *
 * Порядок намеренный: сначала окружение, потом сам агент, потом флаги в порядке
 * списка, потом дописанное руками — свою добавку человек видит в конце, там же,
 * где набирал. Повторы схлопываются: повтор `--continue` часть агентов считает
 * ошибкой разбора.
 *
 * НА СЕРВЕРЕ КОМАНДА ПИШЕТСЯ ИНАЧЕ, и это не догадка, а замер на живом Linux
 * под root (04.08.2026):
 *   • `claude --dangerously-skip-permissions` отвечает «cannot be used with
 *     root/sudo privileges for security reasons» и НЕ запускается; с
 *     `IS_SANDBOX=1` — запускается. Сервер почти всегда root;
 *   • `gemini --yolo` на новой папке пишет «Approval mode overridden to
 *     "default" because the current folder is not trusted» — то есть МОЛЧА
 *     возвращает подтверждения; лечится спутником `--skip-trust`.
 * Поэтому окружение дописывается через `env VAR=1` (а не `VAR=1 cmd`): так
 * работает и перед `exec`, которым запускается агент в SSH-терминале. И только
 * в POSIX-шелле: в PowerShell такого синтаксиса нет вовсе, там префикс сломал
 * бы команду.
 */
export function composeLaunch(
  cli: string,
  prefs: LaunchPrefs = EMPTY_PREFS,
  ctx: LaunchContext = {},
): string {
  const base = (cli || "").trim();
  if (!base) return "";
  const posix = Boolean(ctx.posix);
  const accountTransport = resolveLaunchAccount(ctx);
  // Defense in depth: UI должен отключить кнопку, но даже прямой
  // вызов composeLaunch не получит исполняемую CLI-строку.
  if (accountTransport.blockedReason) return "";
  const specs = new Map((ctx.specs || []).map((s) => [s.flag, s]));

  const chosen: string[] = [];
  const seen = new Set<string>();
  const add = (raw: string) => {
    const flag = (raw || "").trim();
    if (!flag || seen.has(flag)) return;
    const spec = specs.get(flag);
    // Флаг не для этой площадки — молча пропускаем: показывать его тут и не
    // должны были, а уронить запуск он может.
    if (spec?.only === "posix" && !posix) return;
    if (spec?.only === "windows" && posix) return;
    seen.add(flag);
    chosen.push(flag);
    for (const companion of spec?.with || []) add(companion);
  };
  for (const f of prefs.flags || []) add(f);

  const env: string[] = [];
  if (posix) {
    for (const f of chosen) {
      for (const e of specs.get(f)?.env || []) {
        if (e && !env.includes(e)) env.push(e);
      }
    }
  }

  const extra = (prefs.extra || "").trim().replace(/\s+/g, " ");

  // Модель идёт первой после имени агента — так её видно в готовой команде, а
  // не приходится искать среди флагов.
  //
  // ПРАВИЛО: то, что человек вписал РУКАМИ, сильнее настройки. Если в своей
  // добавке уже есть флаг модели, второй мы не подставляем: у части CLI повтор
  // флага — ошибка разбора, а у остальных выигрывает не тот, кого человек имел
  // в виду.
  const parts: string[] = [];
  const model = ctx.model;
  if (model?.flag && model.value && !mentionsFlag(extra, model.flag)) {
    parts.push(model.flag, model.value);
  }
  parts.push(...chosen);
  // Хуки — до ручной добавки: свою добавку человек видит в конце, где набирал.
  for (const raw of ctx.launchArgs || []) {
    const arg = (raw || "").trim();
    if (arg) parts.push(BARE_ARG_RE.test(arg) ? arg : quoteValue(arg, posix));
  }
  if (extra) parts.push(extra);
  const cmd = parts.length ? `${base} ${parts.join(" ")}` : base;

  // Аккаунт добавляется на ОБЕИХ площадках, в отличие от env самих флагов:
  // тем нужен POSIX (IS_SANDBOX=1 лечит запуск под root на сервере), а аккаунт
  // одинаково нужен и на компьютере с Windows — там у человека и стоят его
  // подписки. Синтаксис при этом разный: в PowerShell префикса `env VAR=…` не
  // существует, там переменная ставится отдельным выражением через `;`.
  // Переменных у аккаунта может быть несколько: каталог профиля и proxy env.
  // В PowerShell каждая ставится
  // отдельным выражением через `;`, в POSIX все идут одним `env`.
  const accountEnv = accountTransport.env;
  if (!posix) {
    // Даже «основной / напрямую» проходит scope: ему нужно снять
    // каталог/proxy, оставшиеся от предыдущего запуска в том же shell.
    const profileEnvName = (ctx.accountEnvName || ctx.account?.envName || "").trim();
    if (!profileEnvName && !ctx.account && !accountTransport.proxyConfigured) return cmd;
    return composePowerShellScopedLaunch(cmd, profileEnvName, accountEnv);
  }
  const posixEnv = [...env];
  for (const p of accountEnv) {
    posixEnv.push(`${p.name}=${quoteValue(p.value, true)}`);
  }
  const profileEnvName = (ctx.accountEnvName || ctx.account?.envName || "").trim();
  const accountScoped = Boolean(profileEnvName || accountTransport.proxyConfigured);
  if (!accountScoped) return posixEnv.length ? `env ${posixEnv.join(" ")} ${cmd}` : cmd;
  // POSIX env имён регистрозависим: перед каждым account-capable launch
  // снимаем ВЕСЬ известный scope и лишь затем применяем проверенный полный
  // набор. Поэтому системный `https_proxy` или `NO_PROXY=*` не переживает
  // переключение аккаунта и не может победить выбранный канал.
  const scope = [profileEnvName, ...POSIX_ACCOUNT_ENV_SCOPE]
    .filter((name, index, names) => Boolean(name) && names.indexOf(name) === index)
    .map((name) => `-u ${name}`);
  return `env ${[...scope, ...posixEnv].join(" ")} ${cmd}`;
}

/**
 * Команда запуска для ТЕРМИНАЛА СЕРВЕРА (SSH).
 *
 * Отличается от локальной двумя вещами, и обе стоили бы человеку сессии.
 *
 * 1. НЕТ `exec`. Раньше строка была
 *      `command -v claude … && exec claude … || echo "не установлен"`,
 *    и `exec` ЗАМЕЩАЛ образ процесса. А замещался единственный интерактивный
 *    шелл SSH-сессии (`session.RequestPty` + `session.Shell()` в
 *    internal/pty/ssh_conn.go) — больше в канале ничего нет. Поэтому любой
 *    выход из агента (`/exit`, Ctrl-D, падение) возвращаться было НЕКУДА:
 *    sshd закрывал канал, и человек получал «SSH-сессия завершена» с
 *    единственной кнопкой «Подключиться снова». На своём ПК того же не
 *    происходило — там агент запускается дочерним процессом шелла, и после
 *    выхода человек снова на приглашении. Теперь одинаково.
 *
 * 2. НЕТ конструкции `A && B || C`. Она левоассоциативна и одноприоритетна:
 *    как только исчезает `exec`, ЛЮБОЙ ненулевой код выхода агента печатает
 *    «агент не установлен» — на сервере, где он установлен. Полное ветвление
 *    `if … then … else … fi` разводит «не найден» и «вышел с ошибкой».
 */
export function composeRemoteLaunch(
  cli: string,
  prefs: LaunchPrefs = EMPTY_PREFS,
  ctx: LaunchContext = {},
  notFound = "",
  cliNames?: string[],
): string {
  const base = (cli || "").trim();
  if (!base) return "";
  // Проверяем ВСЕ известные имена бинаря, а не одно.
  //
  // `cli` — это имя, под которым агент нашёлся на КОМПЬЮТЕРЕ; если на
  // компьютере его нет, приходит просто первое из списка. У copilot имён два
  // (`github-copilot-cli` и `copilot`), поэтому человек читал «агент не
  // установлен на сервере», хотя бинарь там стоял под вторым именем.
  const names: string[] = [];
  for (const n of [base, ...(cliNames || [])]) {
    const name = (n || "").trim();
    if (name && !names.includes(name)) names.push(name);
  }
  // Метка нужна детектору агента: у SSH-сессии он читает ТЕКСТ экрана, а в
  // сообщении об отказе стоит имя агента — без метки «агента здесь нет»
  // превращалось в «агент работает» (см. AgentMissingMarker в events.go).
  const msg = (notFound || t("ui.agentlaunch.m80870f3eb8", { p0: (base) })).replace(/"/g, "'");
  // Аккаунт на сервер НЕ едет, даже если он выбран на компьютере: каталог
  // профиля лежит на этом компьютере, а команда уходит в шелл сервера — там
  // такого пути нет, и агент встретил бы человека требованием войти заново.
  // Своя подписка на сервере — это его собственный аккаунт, заводить его надо
  // там (та же граница, что у устройств: SSH-сервер не устройство аккаунта).
  //
  // МОДЕЛЬ OpenRouter не едет по ТОЙ ЖЕ причине: ключ живёт в окружении агента
  // НА ЭТОМ компьютере и в шелл сервера не попадает, а `-m openrouter/…` без
  // ключа — это отказ провайдера вместо запуска. Нужен OpenRouter на сервере —
  // там свой Remotai и свой ключ.
  const serverCtx = {
    ...ctx,
    agentID: undefined,
    accountEnvName: undefined,
    proxyContractVersion: undefined,
    blockedReason: undefined,
    account: undefined,
    model: undefined,
    // Хуки — пути ЭТОГО компьютера (файл настроек, remotai.exe): на сервере
    // их нет, и `claude --settings <нет файла>` там просто не запустится.
    launchArgs: undefined,
    posix: true,
  };
  const branches = names
    .map((n, i) => `${i === 0 ? "if" : "elif"} command -v ${n} >/dev/null 2>&1; then ${composeLaunch(n, prefs, serverCtx)}`)
    .join("; ");
  return `${branches}; else echo "REMOTAI_AGENT_MISSING ${msg}"; fi`;
}

/** Есть ли среди выбранного хоть один флаг, снимающий подтверждения. */
export function hasDanger(prefs: LaunchPrefs, danger: string[]): boolean {
  const set = new Set(danger);
  return (prefs.flags || []).some((f) => set.has(f));
}

function readAll(): Record<string, LaunchPrefs> {
  try {
    const raw = localStorage.getItem(PREFS_KEY);
    if (!raw) return {};
    const parsed = JSON.parse(raw);
    return parsed && typeof parsed === "object" ? parsed as Record<string, LaunchPrefs> : {};
  } catch {
    return {}; // приватный режим или мусор в хранилище — молча начинаем с чистого
  }
}

/** Выбор для агента; пустой — если человек ещё ничего не выбирал. */
export function loadPrefs(agentId: string): LaunchPrefs {
  if (!agentId) return { ...EMPTY_PREFS };
  const p = readAll()[agentId];
  return {
    flags: Array.isArray(p?.flags) ? p.flags.filter((f) => typeof f === "string") : [],
    extra: typeof p?.extra === "string" ? p.extra : "",
  };
}

/** Запомнить выбор. Пустой выбор запись УДАЛЯЕТ: хранилище не копит мусор. */
export function savePrefs(agentId: string, prefs: LaunchPrefs): void {
  if (!agentId) return;
  try {
    const all = readAll();
    if ((prefs.flags || []).length === 0 && !(prefs.extra || "").trim()) delete all[agentId];
    else all[agentId] = { flags: prefs.flags || [], extra: (prefs.extra || "").trim() };
    localStorage.setItem(PREFS_KEY, JSON.stringify(all));
  } catch { /* приватный режим — выбор проживёт до закрытия шторки */ }
}

/** Переключить один флаг. */
export function toggleFlag(prefs: LaunchPrefs, flag: string): LaunchPrefs {
  const on = (prefs.flags || []).includes(flag);
  return {
    ...prefs,
    flags: on ? prefs.flags.filter((f) => f !== flag) : [...(prefs.flags || []), flag],
  };
}
