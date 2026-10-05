export interface Session {
  name: string;
  agent_type: string;
  agent_name: string;
  agent_icon: string;
  cwd: string;
  is_busy: boolean;
  is_active: boolean;
  // ACP-inspired fields
  mode: "persistent" | "oneshot";
  status: "alive" | "dead" | "not-ready";
  permission_mode?: string;
  created_at?: number;
  last_active_at?: number;
  topic_id?: number;
  ttl_minutes?: number;
}

export interface DiscoveredSession {
  agent_type: string;
  session_id: string;
  cwd: string;
  pid: number;
  started_at: number;
  name: string;
  is_alive: boolean;
  imported: boolean;
}

/** Квота подписки на профиле агента (чип «N% 5ч / N% 7д» в панели).
 *  Процентов нет, если вендор их не отдал — «недоступно», никогда не 0. */
export interface AgentQuota {
  /** available | signed_out | unavailable | unsupported | unknown */
  status: string;
  five_hour_used?: number;
  seven_day_used?: number;
  message?: string;
  message_code?: string;
  captured_at?: string;
  stale?: boolean;
  five_hour_resets_at?: number;
  seven_day_resets_at?: number;
  /** Чей это остаток: при нескольких подписках на одного вендора процент без
   *  имени аккаунта обманывает — человек видит 82%, а работать собирается
   *  вторым аккаунтом, где 11%. */
  account_id?: string;
  account_label?: string;
}

export type PtyScrollMode = "auto" | "terminal" | "agent";

export interface AgentInfo {
  /** Backend-advertised managed surface, also available before installation. */
  native_route?: string;
  id: string;
  name: string;
  icon: string;
  description: string;
  detected: boolean;
  path: string | null;
  supports_resume: boolean;
  /** Команда для интерактивного запуска в терминале ("" у встроенных:
   *  shell/orchestrator/researcher). Отсутствует у старого агента. */
  cli?: string;
  /** Команда установки, выбранная агентом под свою ОС. */
  install?: string;
  /** Байты клавиши, которой у агента открывается ЕГО СОБСТВЕННАЯ история
   *  (Codex: Ctrl+T). Пусто — не знаем и не гадаем. */
  history_key?: string;
  /** Человеческое имя этой клавиши для подписи: «Ctrl+T». */
  history_label?: string;
  /** Чем листается история агента: "page" | "wheel" | "transcript". Пусто —
   *  компьютер не знает, и режим «Авто» решает пробой, как раньше. */
  history_channel?: string;
  /** Команда установки для СЕРВЕРА (POSIX): `install` резолвится по ОС
   *  компьютера-бастиона, и с Windows-ПК туда попадал бы winget. */
  install_posix?: string;
  /** Все известные имена бинаря: на сервере проверять надо каждое. */
  cli_names?: string[];
  /** Интерактивно продолжить последний разговор в текущей папке. */
  resume_cli?: string;
  /** Продолжить КОНКРЕТНУЮ беседу: `{session_id}` подставляет клиент.
   *  Есть только у агентов, которых можно усыпить. */
  resume_id_cli?: string;
  /** Квота подписки; отсутствует у встроенных и неустановленных агентов
   *  и у старого агента на ПК. */
  quota?: AgentQuota | null;
  /** Флаги, с которыми агента осмысленно запускать. Приходят С АГЕНТА (реестр
   *  `internal/agents/registry.go`), а не зашиты в клиент — по тому же правилу,
   *  что и `cli`: новый агент появляется в шторке без правок фронта. У старого
   *  агента поля нет вовсе — тогда раздел флагов просто не рисуется. */
  launch_flags?: AgentLaunchFlag[];
  /** Переменная окружения, которой у этого CLI выбирается аккаунт
   *  (CLAUDE_CONFIG_DIR, CODEX_HOME, GEMINI_CLI_HOME, KIMI_CODE_HOME). Пустая
   *  или отсутствует — переключать нечего, строка аккаунта не рисуется.
   *  Приходит С АГЕНТА по тому же правилу, что `cli` и `launch_flags`. */
  account_env?: string;
  /** Флаг, которым этому CLI задаётся модель (`-m` у opencode). Пусто — мы не
   *  проверяли, чем он принимает модель, и клиент её НЕ подставляет: флаг,
   *  которого у агента нет, роняет разбор аргументов, а человек с телефона
   *  видит только «агент не запустился». Сверяется с живым `--help`. */
  model_flag?: string;
  /** Аргументы, которые дописываются к КАЖДОМУ локальному запуску агента
   *  (новому и «продолжить»): подключают его хуки — «закончил», «ждёт
   *  разрешения», «задал вопрос» (internal/agenthooks). В них пути ЭТОГО
   *  компьютера, поэтому на SSH-сервер они не едут. Нет поля — старый агент. */
  launch_args?: string[];
  /** Канонический начальный маршрут прокрутки этого CLI из реестра агента. */
  scroll_mode_default?: PtyScrollMode;
  /** Как профиль обращается с CSI 3 J: "honor" — исполнять у низа и
   *  откладывать при чтении, "preserve" — не исполнять (ST-04). Свойство
   *  хранения, не навигации. Нет поля — старый агент или реестр не знает. */
  history_retention?: "honor" | "preserve";
}

/**
 * Подключение OpenRouter на компьютере (`GET /api/openrouter`).
 *
 * ЗАЧЕМ: один ключ вместо подписки на каждого вендора — и агента становится чем
 * запустить, даже если у человека нет ни Claude, ни ChatGPT.
 *
 * КЛЮЧА ЗДЕСЬ НЕТ И НЕ БУДЕТ: он живёт в окружении процесса агента на ПК, а
 * наружу едет только метка, которую маскирует сам OpenRouter, и цифры расхода.
 *
 * БАЛАНС АККАУНТА ПОКАЗАТЬ НЕЛЬЗЯ — проверено живым ключом: `/api/v1/credits`
 * отвечает 403 обычным ключом вывода. Доступен расход и остаток ЛИМИТА КЛЮЧА,
 * если человек этот лимит задал. Поэтому в интерфейсе «потрачено», а не
 * «баланс»: обещать то, чего неоткуда взять, нельзя.
 */
export interface OpenRouterStatus {
  /** Есть ключ на этом компьютере. Поле есть всегда — по нему клиент отличает
   *  агента, который умеет OpenRouter, от старого. */
  configured: boolean;
  /** Что с ключом прямо сейчас: принят, отвергнут, сервис не отвечает. */
  key_state?: "ok" | "rejected" | "unreachable";
  /** Маскированная метка от OpenRouter («sk-or-v1-611...a5d»). */
  label?: string;
  usage?: number;
  usage_daily?: number;
  usage_weekly?: number;
  usage_monthly?: number;
  /** Потолок трат ключа и остаток; отсутствуют, если потолка нет. */
  limit?: number;
  limit_remaining?: number;
  /** Человек НИ РАЗУ не пополнял счёт — от этого зависит дневная квота
   *  бесплатных моделей, и разница двадцатикратная. */
  is_free_tier?: boolean;
  /** Сколько запросов в сутки к бесплатным моделям разрешено: 50 или 1000. */
  free_daily_quota?: number;
  free_rpm?: number;
  /** Выбранная модель (`openrouter/<slug>`), пусто — агент решает сам. */
  model?: string;
  /** Ключ сохранён, но зашифрован другой учётной записью Windows. */
  foreign?: boolean;
  env_name?: string;
}

/** Модель каталога OpenRouter. */
export interface OpenRouterModel {
  id: string;
  name: string;
  context: number;
  free: boolean;
  /** Умеет вызывать инструменты. Для агента это не украшение, а условие
   *  работы: без этого он не прочитает файл и не запустит команду. */
  tools: boolean;
  /** Цена за МИЛЛИОН токенов в долларах (агент уже привёл её к этому виду). */
  prompt_price: number;
  completion_price: number;
  /** Это не модель, а автоподбор: OpenRouter сам выбирает доступную бесплатную
   *  модель под каждый запрос и учитывает, что агенту нужны инструменты.
   *  Ставится по умолчанию сразу после подключения ключа. */
  router?: boolean;
}

/**
 * Аккаунт нейросети на устройстве.
 *
 * Аккаунт у CLI-агента — это каталог с его OAuth-кредами, поэтому вторая
 * подписка = второй каталог: оба живут одновременно, переключение ничего не
 * разлогинивает. Список хранится на компьютере (`~/.tgcontrol-accounts.json`),
 * а не в пульте — пультов у человека несколько, машина одна.
 */
export interface AgentAccount {
  id: string;
  agent_id: string;
  /** Как человек назвал: «рабочий», «личный». У основного пусто. */
  label: string;
  /** Каталог профиля; пусто у основного. */
  dir?: string;
  /** Основной — тот, что уже настроен на машине. Его не трогаем. */
  is_default?: boolean;
  /** Этим аккаунтом запускается агент сейчас. */
  active?: boolean;
  created_at?: number;
  /** Чем дописать команду запуска (пусто у основного) — собирает клиент,
   *  потому что синтаксис зависит от шелла. */
  env_name?: string;
  env_value?: string;
  /** Все переменные запуска списком: каталог профиля и прокси. Их несколько,
   *  и придумывает их компьютер — клиент только подставляет. */
  env?: Array<{ name: string; value: string }>;
  /** Через какой прокси ходит эта подписка. Пусто — напрямую. */
  proxy?: string;
	/** Безопасная метка старой неподдержанной настройки. Это не активный канал
	 * и никогда не содержит userinfo/path/query исходного URL. */
	proxy_legacy?: string;
  /** Legacy-настройка не входит в подтверждённый transport агента и поэтому
   *  запуск аккаунта заблокирован до очистки или замены настройки. */
  proxy_warning?: string;
	/** CLI нельзя запускать: иначе неподдержанный proxy превратится в direct. */
	proxy_blocked?: boolean;
  /** Закреплён за папкой терминала, из которого спросили. Закрепление сильнее
   *  общего выбора: оно сказано про конкретный проект. */
  pinned?: boolean;
}

/** Один переключатель в шторке запуска агента. */
export interface AgentLaunchFlag {
  /** Что дописать к команде, ровно как есть. */
  flag: string;
  /** Как называется на кнопке — словами человека, а не флагом. */
  title: string;
  /** Одна строка «что произойдёт». */
  hint?: string;
  /** Снимает подтверждения: агент начнёт менять файлы и запускать команды без
   *  вопросов. Такие показываются отдельно и предупреждением. */
  danger?: boolean;
  /** Предлагать включённым (у опасных — никогда). */
  default?: boolean;
}

export interface ChatMessage {
  role: "user" | "agent";
  text: string;
  timestamp: number;
  cost_usd?: number | null;
  is_error?: boolean;
  tools?: string[];
}

export interface SessionDetail extends Session {
  messages: ChatMessage[];
  agent_config: Record<string, string>;
}

export interface FileItem {
  name: string;
  is_dir: boolean;
  size: number | null;
  modified: number | null;
  path: string;
}

export interface QuickPath {
  name: string;
  path: string;
}

export interface DiskInfo {
  total: number;
  used: number;
  free: number;
}

export interface AppConfig {
  claude_model: string;
  claude_permission_mode: string;
  codex_model: string;
  codex_reasoning: string;
  codex_approval_mode: string;
  has_codex: boolean;
  default_agent: string;
  default_cwd: string;
  notifications_enabled: boolean;
  max_concurrent_sessions: number;
  allowed_agents: string[] | null;
  default_ttl_minutes: number;
  inbox_dir?: string;
  inbox_dir_default?: string;
  version: string;
  hostname: string;
  platform: string;
  /** false когда Telegram-бот не настроен (cloud-топология) — клиенты прячут «Отправить в Telegram». */
  bot_available?: boolean;
  /** Читать ли экран терминала в поисках вопроса агента (по умолчанию нет). */
  detect_agent_questions?: boolean;
  /** Занимать ли PrtScr на компьютере (снимок файлом в ~/Remotai/files). */
  screenshot_hotkey?: boolean;
  /** ФАКТ, а не желание: удалось ли занять клавишу прямо сейчас. */
  screenshot_hotkey_active?: boolean;
  /** Почему не удалось: hotkey_taken | hotkey_failed | hotkey_unsupported. */
  screenshot_hotkey_error?: string;
  options: {
    claude_models: string[];
    codex_models: string[];
    codex_reasoning: string[];
    claude_permission_modes: string[];
    codex_approval_modes: string[];
    orchestrator_models: string[];
  };
}

export interface SystemStats {
  cpu: { percent: number; cores: number };
  memory: { total: number; used: number; available: number; percent: number };
  disk: {
    total: number; used: number; free: number; percent: number;
    mount?: string; device?: string; fstype?: string;
  };
  disks?: Array<{
    total: number; used: number; free: number; percent: number;
    mount: string; device?: string; fstype?: string;
  }>;
  uptime: number;
  network: { bytes_sent: number; bytes_recv: number };
}

export interface ProcessInfo {
  pid: number;
  name: string;
  cpu: number;
  memory: number;
  status: string;
}

export interface Bookmark {
  name: string;
  path: string;
}

export interface CostStats {
  total_cost: number;
  today_cost: number;
  week_cost: number;
  total_messages: number;
}

export interface FilePreview {
  type: "text" | "image" | "binary";
  content?: string;
  language?: string;
  data?: string;
  mime?: string;
  size?: number;
}

// PTY (interactive terminals)
export interface PtySessionInfo {
  id: string;
  cwd: string;
  shell: string;
  created: number;
  last_active?: number;
  alive: boolean;
  name?: string;
  /** Папка (группа) в списке терминалов; "" / отсутствует = без папки. */
  group?: string;
  /** Ручной порядок в списке; 0 / отсутствует = сортировка по -created. */
  sort?: number;
  fg_process?: string;
  /** Аккаунт нейросети, под которым запущен агент ЭТОГО терминала: у соседнего
   *  он может быть другим (окружение выдаётся процессу при запуске). Пусто =
   *  основной аккаунт. */
  account_id?: string;
  account_label?: string;  agent_kind?: string;
  /**
   * "working" | "idle" | "waiting" | "ready" | "error" | "dead" — бейдж в списке.
   * "waiting" = агент задал РАСПОЗНАННЫЙ вопрос (hint непустой);
   * "ready"   = агент жив и молчит, но вопрос не распознан («освободился»).
   * Раньше оба случая были "waiting" с пустым hint, и закончивший агент навсегда
   * висел в «Требует внимания».
   */
  status?: string;
  /** unix ms: начало состояния (завершилось / ошибка / начали ждать). */
  status_at?: number;
  /** unix ms when the process exited; scrollback is retained for five minutes. */
  died_at?: number;
  /**
   * Терминал числится завершённым, но его ПРОЦЕСС ЖИВ — оборвалась только связь
   * с ним. Разные новости: «работа закончилась» и «я потерял с ней связь».
   * Перезапуск здесь неверен (был бы второй терминал в той же папке поверх
   * работающего первого) — нужно вернуть связь: `reattachPtySession`.
   */
  host_alive?: boolean;
  /** подсказка для "waiting" ("Подтвердите: y/n"). */
  hint?: string;
  /**
   * Тип вопроса: "yes_no" | "choice" | "enter" | "text" | "" (не распознан).
   * Ключ приходит всегда от нового агента (без omitempty на бэкенде), поэтому
   * `hint_kind === undefined` = старый агент → показываем обычный набор кнопок.
   */
  hint_kind?: string;
  /** "" — локальный шелл ПК, "ssh" — сессия к удалённому серверу. */
  kind?: string;
  ssh_host?: string;
  ssh_host_id?: string;
  ssh_user?: string;
  ssh_port?: number;
  ssh_proxy_jump?: string;
  remote?: boolean;
  sleep?: PtySleep;
}

/** Агент терминала усыплён человеком: процесс снят, беседа ждёт продолжения
 *  по номеру (internal/pty/agent_sleep.go). */
export interface PtySleep {
  agent: string;
  session_id: string;
  /** Флаги режима, с которыми агент был запущен (--dangerously-skip-permissions). */
  flags?: string[];
  at: number;
  /** Экран уже забрал запись и будит агента. */
  waking_at?: number;
}

/** Состояние PTY-сессии (GET /api/pty/{id}/state). */
export interface PtyState {
  cwd: string;
  alive: boolean;
  name?: string;
  /**
   * ЛОГИЧЕСКАЯ геометрия терминала — размер, применённый к PTY. Это НЕ то же
   * самое, что видимая область экрана: при поднятой клавиатуре человек видит
   * 9–11 строк, а терминал остаётся тридцатистрочным (размер PTY клавиатура не
   * меняет — иначе TUI перерисовывает всю переписку дважды за круг «открыл,
   * написал, закрыл»). Клиенту, открытому уже с клавиатурой, эту высоту взять
   * больше неоткуда.
   */
  cols?: number;
  rows?: number;
  fg_process?: string;
  /** Аккаунт нейросети, под которым запущен агент ЭТОГО терминала: у соседнего
   *  он может быть другим (окружение выдаётся процессу при запуске). Пусто =
   *  основной аккаунт. */
  account_id?: string;
  account_label?: string;  fg_pid?: number;
  /**
   * Время старта переднего процесса (unix мс) — ПОКОЛЕНИЕ процесса. PID без
   * поколения не является ключом (план стабилизации 13.09, ST-02): номер может
   * достаться новому процессу. Нет поля или 0 — агент старый или время
   * недоступно; клиент тогда держится одного PID, как раньше.
   */
  fg_started?: number;
  agent_kind?: string;
  /**
   * Как профиль агента обращается с CSI 3 J (стирание истории прокрутки):
   * "honor" — исполнять у низа и откладывать при чтении, "preserve" — не
   * исполнять. Свойство ХРАНЕНИЯ, не навигации (ST-04): режим «Авто»/«Вывод»/
   * «Агент» и замер плотности на него не влияют. Нет поля — решает клиент по
   * умолчанию.
   */
  history_retention?: "honor" | "preserve";
  /** Начальный режим «Куда листать» для текущего foreground-агента. Ручной
   * выбор действует до смены foreground-процесса. Нет поля = «Авто». */
  scroll_mode_default?: PtyScrollMode;
  /** Шелл сессии; "ssh" — сессия открыта к удалённому серверу. */
  shell?: string;
  /**
   * Статус того же расчёта, что в списке терминалов. До v2.28 экран терминала
   * этих полей не получал вовсе — поэтому не мог показать ни вопрос агента, ни
   * кнопки ответа: приходишь по «Ответить» с главной, а тут зелёное «Готов».
   */
  status?: string;
  status_at?: number;
  hint?: string;
  hint_kind?: string;
  /** "" — локальный шелл ПК, "ssh" — сессия к удалённому серверу. */
  kind?: string;
  ssh_host?: string;
  ssh_host_id?: string;
  ssh_user?: string;
  ssh_port?: number;
  ssh_proxy_jump?: string;
  remote?: boolean;
  sleep?: PtySleep;
}

// Projects (.git folders, auto-scanned)
export interface Project {
  name: string;
  path: string;
  parent: string;
  modified: number;
}

// System: autostart / service
export interface AutostartStatus {
  enabled: boolean;
  method?: string;
  recommended?: string;
  legacy?: boolean;
  ok?: boolean;
  /**
   * Автозапуском управляет система (system-юнит systemd, поставленный
   * install.sh / `remotai install`). UI не должен предлагать «Исправить»:
   * user-юнит рядом поднимет ВТОРОЙ агент на том же порту.
   */
  managed_externally?: boolean;
}

export interface ServiceStatus {
  service_installed: boolean;
  service_running: boolean;
  running_as_service?: boolean;
  remote_desktop_supported?: boolean;
  has_display?: boolean;
  remote_desktop_warning?: string;
  vbrowser?: VBrowserStatus;
  device_id: string;
  hostname: string;
  os: string;
  arch?: string;
}

/** Virtual browser (Xvfb + browser on a headless Linux agent). */
export interface VBrowserStatus {
  available: boolean;
  xvfb_path?: string;
  browsers?: string[];
  running: boolean;
  display?: string;
  browser?: string;
  since?: number;
  hint?: string;
  /** Есть ли на машине чем нажимать (Linux: xdotool). Экран без него —
   *  картинка, по которой нельзя кликнуть. */
  input_ready?: boolean;
  input_hint?: string;
  /** Агент поставит инструмент ввода сам, по кнопке с экрана. */
  can_install_input?: boolean;
  /** Установка идёт прямо сейчас (она фоновая — переживает уход с экрана). */
  input_installing?: boolean;
  /** Чем кончилась прошлая попытка установки. */
  input_error?: string;
  /** Страница в браузере на машине — то, что показывает адресная строка. */
  page?: BrowserPage;
  /** Порт протокола отладки браузера (только 127.0.0.1 на самой машине).
   *  Ненулевой — значит кадры и ввод идут через сам браузер, а не через экран:
   *  на этом основана адресная строка в интерфейсе. */
  debug_port?: number;
}

/** Состояние вкладки браузера на машине. */
export interface BrowserPage {
  url: string;
  title: string;
  can_back: boolean;
  can_forward: boolean;
  loading: boolean;
  target_id?: string;
  device?: string;
  page_scale?: number;
}

/** Вкладка браузера на машине. */
export interface BrowserTab {
  id: string;
  title: string;
  url: string;
  active: boolean;
  /** Значок сайта (data-URL). Агент кэширует его по домену; пусто — значка нет. */
  icon?: string;
  /** Картинка страницы, снятая при уходе с вкладки: по ней вкладка узнаётся
   *  взглядом, без чтения заголовка. */
  preview?: string;
}

export interface RecentFolder {
  name: string;
  path: string;
  time: number;
}

export interface SessionTemplate {
  name: string;
  agent_type: string;
  cwd: string;
  permission_mode?: string;
  mode?: string;
}

// Presets / quick-launch tiles
export interface Preset {
  id: string;
  title: string;
  icon?: string;
  kind: "session" | "pty" | "url";
  agent?: string;
  cwd?: string;
  shell?: string;
  url?: string;
  pinned: boolean;
  last_used?: number;
  used_count?: number;
}

// Healthcheck
export interface HealthCheck {
  ok: boolean;
  note?: string;
  extra?: any;
}

export interface HealthReport {
  ok: boolean;
  checks: Record<string, HealthCheck>;
  ran_at: string;
  took_ms: number;
}

// Research types
export interface ResearchExperiment {
  id: number;
  description: string;
  metric: number;
  baseline: number;
  delta: string;
  kept: boolean;
  reverted: boolean;
  invariants_passed: boolean;
  files_changed?: string[];
  diff?: string;
  timestamp: number;
  error?: string;
}

export interface ResearchConfig {
  eval_command: string;
  metric_pattern: string;
  metric_file?: string;
  metric_name: string;
  lower_is_better: boolean;
  max_experiments: number;
  max_wall_clock_minutes: number;
  max_cost_usd: number;
  invariants_command: string;
  protected_paths: string[];
}

export interface ResearchState {
  running: boolean;
  config: ResearchConfig;
  experiments: ResearchExperiment[];
  baseline: number;
  final: number;
  improvement: string;
  branch?: string;
  final_diff?: string;
  started_at: number;
  finished_at?: number;
  summary?: string;
  is_error?: boolean;
}

// Orchestrator step event
export interface OrchestratorStep {
  iteration: number;
  type: "thinking" | "tool_call" | "tool_result" | "api_call" | "error";
  tool?: string;
  input?: string;
  output?: string;
  error?: string;
  duration_ms?: number;
  tokens_in?: number;
  tokens_out?: number;
}

// ── License & Billing ───────────────────────────────────────────

export interface LicenseStatus {
  tier: "free" | "pro" | "team";
  effective_tier?: "free" | "pro" | "team";
  beta?: boolean;
  active: boolean;
  email: string;
  device_id: string;
  device_slots: number;
  active_devices: string[];
  valid_until?: string;
  cancelled_at?: string;
  trial_end?: string;
  limits: {
    max_devices: number;
    max_concurrent_agents: number;
    max_pty_terminals: number;
    remote_desktop_max_fps: number;
    remote_desktop_max_res: number;
    file_manager_write: boolean;
    orchestrator_enabled: boolean;
    researcher_enabled: boolean;
    auto_update: boolean;
    all_agents: boolean;
  };
}

export interface PricingTier {
  id: string;
  name: string;
  price_monthly: number;
  price_annual: number;
  trial_days?: number;
  limits: LicenseStatus["limits"];
}

// WebSocket events
export type WSEvent =
  | { type: "sessions_updated" }
  | { type: "status"; session: string; is_busy: boolean }
  | { type: "progress"; session: string; text: string }
  | { type: "message"; session: string; message: ChatMessage }
  | { type: "orchestrator_step"; session: string; step: OrchestratorStep }
  | { type: "research"; session: string; action: string; text?: string; experiment?: ResearchExperiment; summary?: string; improvement?: string; experiments?: ResearchExperiment[] }
  | { type: "pong" };
