/**
 * Состав гида «Как работать с системой» — данные, а не разметка.
 *
 * Правило проекта: то, что можно проверить тестом, живёт вне React. Здесь —
 * структура гида (разделы, примеры, куда ведут кнопки) и поиск по нему; экран
 * только рисует. Тексты идут ключами словаря: гид переводится и правится чаще
 * всего, а искать строки внутри JSX — это то, из-за чего разделы устаревают.
 */
import { SECTIONS } from "../sections";

/** Пример в разделе: команда для терминала или короткий рецепт. */
export interface GuideExample {
  /** Что человек получит — одной строкой. */
  labelKey: string;
  /** Сама команда. Моноширинная, копируется одним тапом. */
  command: string;
}

/** Куда ведёт кнопка раздела: маршрут приложения и подпись. */
export interface GuideAction {
  labelKey: string;
  route: string;
}

export interface GuideSection {
  id: string;
  icon: string;
  /**
   * Группа = раздел приложения, а не своя тема. Гид — карта продукта: до
   * 28.08.2026 у него была четвёртая классификация разделов («Каждый день»,
   * «Надёжность и доступ»), и прочитавший гид искал в приложении названия,
   * которых там нет вовсе (UX-аудит 2026-08-23, HOLISTIC-7).
   */
  group: "terminal" | "files" | "agents" | "remote" | "devices" | "ssh" | "system" | "settings";
  titleKey: string;
  /** Одна строка под заголовком — по ней раздел узнаётся в свёрнутом виде. */
  leadKey: string;
  /** Абзацы простым языком. */
  bodyKeys: string[];
  examples?: GuideExample[];
  actions?: GuideAction[];
}

/**
 * Заголовки групп — имена самих разделов приложения, и берутся они из общей
 * таблицы разделов (apk/src/sections.ts): собственный ключ у гида снова развёл
 * бы имена, ровно ради чего таблица и заведена. У «Терминала», «Файлов» и
 * «Экрана компьютера» строки в таблице нет — у них одна дверь, вкладка панели,
 * — поэтому имя берётся тем же ключом навигации, что рисует вкладку.
 *
 * Порядок — от частого к редкому, а не порядок панели: гид читают сверху вниз.
 */
export const GUIDE_GROUPS: Array<{ id: GuideSection["group"]; titleKey: string }> = [
  { id: "terminal", titleKey: "nav.terminal" },
  { id: "files", titleKey: "nav.files" },
  { id: "agents", titleKey: SECTIONS.usage.nameKey },
  { id: "remote", titleKey: "nav.remote" },
  { id: "devices", titleKey: SECTIONS.devices.nameKey },
  { id: "ssh", titleKey: SECTIONS.ssh.nameKey },
  { id: "system", titleKey: SECTIONS.system.nameKey },
  { id: "settings", titleKey: SECTIONS.settings.nameKey },
];

export const GUIDE_SECTIONS: GuideSection[] = [
  {
    // ПЕРВЫМ — и это решение владельца (05.08.2026): «сразу в начале должно
    // быть, что всё можно настроить и получить всю информацию от агента».
    //
    // Смысл раздела не в списке команд, а в снятии главного затруднения:
    // человек не обязан изучать приложение, чтобы им пользоваться. Он может
    // просто спросить у агента, который у него уже работает.
    id: "ask-agent",
    icon: "🤖",
    group: "terminal",
    titleKey: "guide.askAgent.title",
    leadKey: "guide.askAgent.lead",
    bodyKeys: ["guide.askAgent.body1", "guide.askAgent.body2", "guide.askAgent.body3", "guide.askAgent.body4"],
    examples: [
      { labelKey: "guide.askAgent.ex1", command: "remotai doctor" },
      { labelKey: "guide.askAgent.ex2", command: "remotai config list" },
    ],
    actions: [{ labelKey: "guide.action.openTerminals", route: "/pty" }],
  },
  {
    id: "terminals",
    icon: "▶",
    group: "terminal",
    titleKey: "guide.terminals.title",
    leadKey: "guide.terminals.lead",
    bodyKeys: ["guide.terminals.body1", "guide.terminals.body2", "guide.terminals.body3"],
    examples: [
      { labelKey: "guide.terminals.ex1", command: "claude" },
      { labelKey: "guide.terminals.ex2", command: "codex" },
      { labelKey: "guide.terminals.ex3", command: "git status" },
    ],
    actions: [{ labelKey: "guide.action.openTerminals", route: "/pty" }],
  },
  {
    id: "agentSleep",
    icon: "💤",
    group: "terminal",
    titleKey: "guide.agentSleep.title",
    leadKey: "guide.agentSleep.lead",
    bodyKeys: ["guide.agentSleep.body"],
    actions: [{ labelKey: "guide.action.openTerminals", route: "/pty" }],
  },
  {
    id: "answers",
    icon: "✓",
    group: "terminal",
    titleKey: "guide.answers.title",
    leadKey: "guide.answers.lead",
    bodyKeys: ["guide.answers.body1", "guide.answers.body2", "guide.answers.body3"],
    actions: [
      { labelKey: "guide.action.openTerminals", route: "/pty" },
      { labelKey: "guide.action.openHome", route: "/" },
    ],
  },
  {
    id: "files",
    icon: "📁",
    group: "files",
    titleKey: "guide.files.title",
    leadKey: "guide.files.lead",
    bodyKeys: ["guide.files.body1", "guide.files.body2"],
    actions: [{ labelKey: "guide.action.openFiles", route: "/files" }],
  },
  {
    // Ряд под терминалом — самая заметная кнопочная поверхность продукта, и о
    // ней в гиде было две случайные строки. Отсюда и жалобы «не мог удалить и
    // изменить»: человек не знал, что список живёт на компьютере и правится.
    id: "quickCommands",
    icon: "⚡",
    group: "terminal",
    titleKey: "guide.quickCommands.title",
    leadKey: "guide.quickCommands.lead",
    bodyKeys: [
      "guide.quickCommands.body1",
      "guide.quickCommands.body2",
      "guide.quickCommands.body3",
    ],
    actions: [{ labelKey: "guide.action.openTerminals", route: "/pty" }],
  },
  {
    // Заготовки и плитки папок — две вещи, которые экономят больше всего
    // набора с телефона, и обе не были объяснены нигде.
    id: "snippets",
    icon: "📌",
    group: "terminal",
    titleKey: "guide.snippets.title",
    leadKey: "guide.snippets.lead",
    bodyKeys: ["guide.snippets.body1", "guide.snippets.body2", "guide.snippets.body3"],
    actions: [{ labelKey: "guide.action.openTerminals", route: "/pty" }],
  },
  {
    id: "telegram",
    icon: "✈",
    group: "agents",
    titleKey: "guide.telegram.title",
    leadKey: "guide.telegram.lead",
    bodyKeys: ["guide.telegram.body1", "guide.telegram.body2", "guide.telegram.body3"],
    examples: [
      { labelKey: "guide.telegram.ex1", command: 'remotai send "сборка готова"' },
      { labelKey: "guide.telegram.ex2", command: "remotai send --file ./report.pdf" },
      { labelKey: "guide.telegram.ex3", command: "установи скилл из папки Remotai/skills/telegram" },
    ],
  },
  {
    // Продукт вырос быстрее гида: OpenRouter выпущен 09.08.2026, а объяснения
    // ему не было НИГДЕ — ни строки в guide.*. Человек видел в разделе «Агенты»
    // карточку с ключом и не понимал, зачем она и что даёт.
    id: "openrouter",
    icon: "🔑",
    group: "agents",
    titleKey: "guide.openrouter.title",
    leadKey: "guide.openrouter.lead",
    bodyKeys: [
      "guide.openrouter.body1",
      "guide.openrouter.body2",
      "guide.openrouter.body3",
      "guide.openrouter.body4",
    ],
    actions: [{ labelKey: "guide.action.openOpenRouter", route: "/agents?focus=openrouter" }],
  },
  {
    id: "whichAgent",
    icon: "🧩",
    group: "agents",
    titleKey: "guide.whichAgent.title",
    leadKey: "guide.whichAgent.lead",
    bodyKeys: ["guide.whichAgent.body1", "guide.whichAgent.body2", "guide.whichAgent.body3"],
    examples: [
      { labelKey: "guide.whichAgent.ex1", command: "npm i -g opencode-ai" },
      { labelKey: "guide.whichAgent.ex2", command: "npm i -g @anthropic-ai/claude-code" },
    ],
    actions: [{ labelKey: "guide.action.openInstalled", route: "/agents?focus=installed" }],
  },
  {
    id: "accounts",
    icon: "👥",
    group: "agents",
    titleKey: "guide.accounts.title",
    leadKey: "guide.accounts.lead",
    bodyKeys: ["guide.accounts.body1", "guide.accounts.body2", "guide.accounts.body3", "guide.accounts.body4"],
    actions: [{ labelKey: "guide.action.openAccounts", route: "/agents?focus=accounts" }],
  },
  {
    id: "agentSessions",
    icon: "◷",
    group: "agents",
    titleKey: "agentSessions.entryTitle",
    leadKey: "agentSessions.entryNote",
    bodyKeys: ["guide.agentSessions.body"],
    actions: [{ labelKey: "guide.action.openAgentSessions", route: "/agents/sessions" }],
  },
  {
    id: "agentSkills",
    icon: "✦",
    group: "agents",
    titleKey: "skills.title",
    leadKey: "guide.agentSkills.lead",
    bodyKeys: ["guide.agentSkills.body"],
    actions: [{ labelKey: "guide.action.openSkills", route: "/agents?focus=skills" }],
  },
  {
    id: "agentMcp",
    icon: "🔌",
    group: "agents",
    titleKey: "mcp.sectionTitle",
    leadKey: "guide.agentMcp.lead",
    bodyKeys: ["guide.agentMcp.body"],
    actions: [{ labelKey: "guide.action.openMcp", route: "/agents?focus=mcp" }],
  },
  {
    id: "agentCheck",
    icon: "✓",
    group: "agents",
    titleKey: "agentCheck.button",
    leadKey: "guide.agentCheck.lead",
    bodyKeys: ["guide.agentCheck.body"],
    actions: [{ labelKey: "guide.action.openInstalled", route: "/agents?focus=installed" }],
  },
  {
    id: "agentUpdate",
    icon: "⟳",
    group: "agents",
    titleKey: "guide.agentUpdate.title",
    leadKey: "guide.agentUpdate.lead",
    bodyKeys: ["guide.agentUpdate.body"],
    actions: [{ labelKey: "guide.action.openInstalled", route: "/agents?focus=installed" }],
  },
  {
    id: "agentBehaviour",
    icon: "⚙",
    group: "agents",
    titleKey: "agents.behaviourTitle",
    leadKey: "guide.agentBehaviour.lead",
    bodyKeys: ["guide.agentBehaviour.body"],
    actions: [{ labelKey: "guide.action.openBehaviour", route: "/agents?focus=behaviour" }],
  },
  {
    id: "tokenUsage",
    icon: "▥",
    group: "agents",
    titleKey: "tokens.title",
    leadKey: "guide.tokenUsage.lead",
    bodyKeys: ["guide.tokenUsage.body"],
    actions: [{ labelKey: "guide.action.openTokens", route: "/agents?focus=tokens" }],
  },
  {
    id: "agentsWork",
    icon: "🤖",
    group: "agents",
    titleKey: "guide.agentsWork.title",
    leadKey: "guide.agentsWork.lead",
    bodyKeys: ["guide.agentsWork.body1", "guide.agentsWork.body2"],
    examples: [
      { labelKey: "guide.agentsWork.ex1", command: "claude --continue" },
      { labelKey: "guide.agentsWork.ex2", command: "npm i -g @anthropic-ai/claude-code" },
    ],
    actions: [{ labelKey: "guide.action.openUsage", route: "/agents" }],
  },
  {
    id: "screen",
    icon: "🖥",
    group: "remote",
    titleKey: "guide.screen.title",
    leadKey: "guide.screen.lead",
    bodyKeys: ["guide.screen.body1", "guide.screen.body2"],
    actions: [{ labelKey: "guide.action.openScreen", route: "/remote" }],
  },
  {
    // Самый непроходимый сценарий продукта: «сделать картинку чётче и включить
    // звук» доходили до конца примерно один человек из десяти (UX-аудит
    // 2026-08-23, COGNITIVE-9). Причина видна в словаре: среди 163 ключей гида
    // не было НИ ОДНОГО со словом «звук» или «чётк», поэтому и поиск по гиду
    // («Найти: пароль, скилл, экран…») на них отвечал пустотой при живой
    // функции в продукте. Раздел стоит вторым в группе — сразу после того, как
    // человек узнал, что экран вообще можно открыть.
    id: "quality",
    icon: "🔊",
    group: "remote",
    titleKey: "guide.quality.title",
    leadKey: "guide.quality.lead",
    bodyKeys: ["guide.quality.body1", "guide.quality.body2", "guide.quality.body3"],
    actions: [{ labelKey: "guide.action.openScreen", route: "/remote" }],
  },
  {
    id: "browser",
    icon: "🌐",
    group: "remote",
    titleKey: "guide.browser.title",
    leadKey: "guide.browser.lead",
    bodyKeys: ["guide.browser.body1", "guide.browser.body2", "guide.browser.body3"],
    actions: [{ labelKey: "guide.action.openScreen", route: "/remote" }],
  },
  {
    id: "devices",
    icon: "🗂",
    group: "devices",
    titleKey: "guide.devices.title",
    leadKey: "guide.devices.lead",
    bodyKeys: ["guide.devices.body1", "guide.devices.body2"],
    actions: [{ labelKey: "guide.action.openDevices", route: "/infrastructure" }],
  },
  {
    id: "ssh",
    icon: "🔌",
    group: "ssh",
    titleKey: "guide.ssh.title",
    leadKey: "guide.ssh.lead",
    bodyKeys: ["guide.ssh.body1", "guide.ssh.body2"],
    examples: [
      { labelKey: "guide.ssh.ex1", command: "curl -fsSL https://remotai.ru/install.sh | sh" },
      { labelKey: "guide.ssh.ex2", command: "remotai pair" },
      { labelKey: "guide.ssh.ex3", command: "sudo systemctl enable --now remotai" },
    ],
    actions: [{ labelKey: "guide.action.openSsh", route: "/ssh" }],
  },
  {
    id: "sshAdd",
    icon: "＋",
    group: "ssh",
    titleKey: "ssh.addHost",
    leadKey: "guide.sshAdd.lead",
    bodyKeys: ["guide.sshAdd.body"],
    actions: [{ labelKey: "guide.action.openSsh", route: "/ssh" }],
  },
  {
    id: "system",
    icon: "📊",
    group: "system",
    titleKey: "guide.system.title",
    leadKey: "guide.system.lead",
    bodyKeys: ["guide.system.body1"],
    actions: [{ labelKey: "guide.action.openSystem", route: "/system" }],
  },
  {
    id: "computerPower",
    icon: "⏻",
    group: "system",
    titleKey: "guide.computerPower.title",
    leadKey: "guide.computerPower.lead",
    bodyKeys: ["guide.computerPower.body"],
    actions: [{ labelKey: "guide.action.openPower", route: "/system?tab=power" }],
  },
  {
    id: "settingsOverview",
    icon: "⚙",
    group: "settings",
    titleKey: "guide.settingsOverview.title",
    leadKey: "guide.settingsOverview.lead",
    bodyKeys: ["guide.settingsOverview.body"],
    actions: [{ labelKey: "guide.action.openSettings", route: "/settings" }],
  },
  {
    id: "autostart",
    icon: "🛡",
    group: "settings",
    titleKey: "guide.autostart.title",
    leadKey: "guide.autostart.lead",
    bodyKeys: ["guide.autostart.body1", "guide.autostart.body2", "guide.autostart.body3"],
    actions: [{ labelKey: "guide.action.openSettings", route: "/settings?section=computer" }],
  },
  {
    // Деньги и аккаунт объясняем ДО того, как о них спросят: главное обещание
    // канона — дома бесплатно навсегда — человек должен встретить в гиде, а не
    // только на сайте.
    id: "account",
    icon: "⭐",
    group: "settings",
    titleKey: "guide.plan.title",
    leadKey: "guide.plan.lead",
    bodyKeys: ["guide.plan.body1", "guide.plan.body2", "guide.plan.body3"],
    actions: [{ labelKey: "account.title", route: "/account" }],
  },
  {
    id: "logins",
    icon: "🔑",
    group: "settings",
    titleKey: "guide.logins.title",
    leadKey: "guide.logins.lead",
    bodyKeys: ["guide.logins.body1", "guide.logins.body2", "guide.logins.body3"],
    // Вторая дверь — «Панель ПК»: единственное место, где живут привязка
    // телефона и доступ с других устройств аккаунта, и до 09.08.2026 гид о ней
    // не говорил вовсе, хотя пункт в меню есть.
    actions: [
      { labelKey: "guide.action.openSettings", route: "/settings?section=account&focus=logins" },
      { labelKey: "guide.action.openPanel", route: "/panel" },
    ],
  },
  {
    id: "updates",
    icon: "⟳",
    group: "settings",
    titleKey: "guide.updates.title",
    leadKey: "guide.updates.lead",
    bodyKeys: ["guide.updates.body1", "guide.updates.body2"],
    actions: [{ labelKey: "guide.action.openSettings", route: "/settings?section=connection" }],
  },
  {
    id: "peer",
    icon: "🤝",
    group: "settings",
    titleKey: "guide.peer.title",
    leadKey: "guide.peer.lead",
    // Функция существует с v2.54.0, но человек о ней не знал ниоткуда: она
    // упоминалась только в подсказке самой команды. А вопрос «так вообще можно
    // и это безопасно?» возникает первым — поэтому границы доступа стоят
    // РАНЬШЕ возможностей, а не в примечании после них.
    bodyKeys: [
      "guide.peer.body1",
      "guide.peer.body2",
      "guide.peer.body3",
      "guide.peer.body4",
    ],
    examples: [
      { labelKey: "guide.peer.ex1", command: "remotai remote list" },
      { labelKey: "guide.peer.ex2", command: "remotai remote <компьютер> doctor" },
      { labelKey: "guide.peer.ex3", command: "remotai remote <компьютер> vpn on" },
      { labelKey: "guide.peer.ex4", command: "remotai config set peer_access full" },
    ],
    actions: [{ labelKey: "guide.action.openSettings", route: "/settings?section=computer" }],
  },
  {
    // Переезд из «Надёжности и доступа» в «Агентов»: раздел про аккаунты
    // нейросетей и остатки лимитов лежал в группе про безопасность, при том
    // что группа про агентов была рядом (HOLISTIC-7).
    id: "limits",
    icon: "📈",
    group: "agents",
    titleKey: "guide.limits.title",
    leadKey: "guide.limits.lead",
    // Вторая строка — про несколько подписок у одного сервиса: это и есть
    // причина, по которой раздел вообще открывают.
    bodyKeys: ["guide.limits.body1", "guide.limits.body2"],
    actions: [{ labelKey: "guide.action.openLimits", route: "/agents?focus=limits" }],
  },
];

// Своего чек-листа у гида больше нет. Он вёл СВОЙ список из пяти шагов, который
// человек отмечал пальцем, а «Первые шаги» на главной считали свой — по факту.
// Про одного и того же человека выходило два взаимоисключающих ответа: «2 из 3»
// на главной против «0 из 5» здесь, причём «Запустите агента» в первом было
// отмечено, а во втором нет (UX-аудит 2026-08-23, NIELSEN-11). Остался один,
// автоматический: GuideView рисует тот же компонент FirstSteps, что и главная.
//
// Три шага, которым в автоматическом списке соответствия нет и не будет
// («Спросите агента про Remotai» — формулировка владельца от 05.08.2026,
// «Отвечайте агенту цифрой», «Получайте отчёты в Telegram»), не потерялись:
// их содержание живёт разделами ask-agent, answers и telegram выше.

/** Частая беда и что с ней делать — по симптому, как его называет человек. */
export interface GuideTrouble {
  id: string;
  symptomKey: string;
  answerKey: string;
}

export const GUIDE_TROUBLES: GuideTrouble[] = [
  { id: "terminal-lost", symptomKey: "guide.trouble.lost.q", answerKey: "guide.trouble.lost.a" },
  { id: "pc-offline", symptomKey: "guide.trouble.offline.q", answerKey: "guide.trouble.offline.a" },
  { id: "agent-silent", symptomKey: "guide.trouble.silent.q", answerKey: "guide.trouble.silent.a" },
  { id: "screen-taps", symptomKey: "guide.trouble.taps.q", answerKey: "guide.trouble.taps.a" },
];

// Люди ищут действие своими словами. Эти короткие запросы дополняют видимый
// текст гида; каждый ведёт к уже существующему экрану, а не запускает действие.
const SEARCH_TERMS: Record<string, string[]> = {
  terminals: ["открыть терминал", "командная строка", "запустить агента"],
  agentSleep: ["остановить агента", "поставить агента на паузу", "усыпить агента", "сон агента"],
  files: ["загрузить файл", "отправить файл", "скачать файл"],
  whichAgent: ["установить агента", "поставить Claude Code", "установить Codex"],
  agentSessions: ["история чатов", "история бесед", "возобновить беседу", "продолжить диалог"],
  agentSkills: ["установить скилл", "добавить навык агенту"],
  agentMcp: ["подключить MCP", "добавить MCP", "инструменты MCP"],
  agentCheck: ["проверить доступ к модели", "проверить ключ агента"],
  agentUpdate: ["обновить агента", "новая версия агента"],
  agentBehaviour: ["агент по умолчанию", "кнопки ответа агента", "настроить запуск агента"],
  tokenUsage: ["сколько токенов потратил", "потраченные токены", "статистика токенов"],
  screen: ["удаленный рабочий стол", "посмотреть экран", "управление экраном"],
  quality: ["четкость картинки", "качество экрана", "включить звук"],
  devices: ["подключить второй компьютер", "подключить новый ПК", "подключить ноутбук", "подключить новый ноутбук", "добавить компьютер", "доступ к компьютеру"],
  ssh: ["подключиться по SSH", "SSH туннель", "файлы SSH сервера"],
  sshAdd: ["добавить SSH-сервер", "подключить сервер по SSH"],
  system: ["нагрузка процессора", "память компьютера", "запущенные процессы"],
  computerPower: ["сон ПК", "разбудить ПК", "усыпить компьютер", "выключить ПК", "перезагрузить ПК", "сон компьютера"],
  autostart: ["настроить автозапуск", "запускать при включении компьютера"],
  logins: ["подключить телефон", "привязать телефон", "способы входа"],
};

/**
 * Поиск по гиду. Ищем по тексту раздела (заголовок, строка-лид,
 * абзацы, подписи примеров и сами команды), а не по одному заголовку: человек
 * ищет словом из проблемы («пароль», «скилл», «батарея»), а не названием
 * раздела. Пустой запрос возвращает всё — гид остаётся читаемым целиком.
 */
export function searchGuide(
  sections: GuideSection[],
  query: string,
  translate: (key: string) => string,
): GuideSection[] {
  const words = queryWords(query);
  if (words.length === 0) return query.trim() ? [] : sections;
  const haystacks = sections.map((section) => textWords([
    translate(section.titleKey),
    translate(section.leadKey),
    ...section.bodyKeys.map(translate),
    ...(section.examples ?? []).flatMap((example) => [translate(example.labelKey), example.command]),
    ...(SEARCH_TERMS[section.id] ?? []),
  ].join(" ")));

  // Слова запроса могут стоять в разных частях текста: «сон компьютера» должен
  // находить «Сон и перезагрузка компьютера». Окончание каждого слова режем
  // не дальше двух букв: «пароль» и «пароли» близки, а «пар» уже мусор.
  return sections.filter((_, index) => containsAll(haystacks[index], words));
}

/** Короткий список переходов: точная задача и название выше упоминания в абзаце. */
export function searchGuideShortcuts(
  sections: GuideSection[],
  query: string,
  translate: (key: string) => string,
): GuideSection[] {
  const words = queryWords(query);
  if (words.length === 0) return [];
  return searchGuide(sections, query, translate)
    .map((section, index) => {
      const title = translate(section.titleKey);
      const lead = translate(section.leadKey);
      const alias = (SEARCH_TERMS[section.id] ?? []).some((phrase) => containsAll(textWords(phrase), words));
      const rank = alias ? 0 : containsAll(textWords(title), words) ? 1
        : containsAll(textWords(`${title} ${lead}`), words) ? 2 : 3;
      return { section, index, rank };
    })
    .sort((a, b) => a.rank - b.rank || a.index - b.index)
    .map(({ section }) => section);
}

function queryWords(query: string): string[] {
  const ignore = new Set(["к", "по", "на", "в", "во", "с", "со", "из", "у", "от", "для", "и", "или",
    "как", "где", "что", "это", "мне", "можно", "хочу", "найти", "открыть", "показать",
    "настроить", "сделать", "поменять", "пожалуйста"]);
  return textWords(query).filter((word) => !ignore.has(word));
}

function textWords(value: string): string[] {
  return value.toLowerCase().replace(/ё/g, "е").match(/[\p{L}\p{N}]+/gu) ?? [];
}

function containsAll(haystack: string[], words: string[]): boolean {
  return words.every((word) => stems(word).some((stem) => haystack.some((candidate) => candidate.startsWith(stem))));
}

/** Совпадение для названий разделов в поиске шторки «Ещё». */
export function matchesGuideQuery(query: string, text: string): boolean {
  const words = queryWords(query);
  return words.length > 0 && containsAll(textWords(text), words);
}

/** Запрос и его укороченные основы: «пароль» → «пароль», «парол», «паро». */
function stems(needle: string): string[] {
  const out = [needle];
  if (needle.length >= 5) out.push(needle.slice(0, -1));
  if (needle.length >= 6) out.push(needle.slice(0, -2));
  return out;
}

/** Сколько разделов в группе после фильтра — по нему прячется пустой заголовок. */
export function groupCount(sections: GuideSection[], group: GuideSection["group"]): number {
  return sections.filter((section) => section.group === group).length;
}

/** Идентификатор группы гида = раздел приложения; им подсказки по экрану говорят, куда вести. */
export type GuideGroupId = GuideSection["group"];

/** Что открыть в гиде по теме из адреса: группа и раздел, который раскрыть. */
export interface GuideFocus {
  group: GuideGroupId;
  sectionId: string;
}

/**
 * Тема из адреса `?topic=` → что раскрыть. Принимает id ГРУППЫ («files» — так
 * ведут подсказки по экрану, HelpSheet) и id РАЗДЕЛА («snippets» — так может
 * вести ссылка из текста): для группы раскрывается её первый раздел, для
 * раздела — он сам. Сначала ищем группу: подсказки по экрану типизированы
 * группой, и раздел с таким же id (files, ssh, system…) их не перехватит.
 * Незнакомое значение → null: гид открывается как обычно, а не пустым
 * (аудит ИА 02.09.2026, P1-5, P1-31; волна 3, п. 5 «гид по контексту»).
 */
export function resolveGuideTopic(
  sections: GuideSection[],
  groups: ReadonlyArray<{ id: GuideGroupId }>,
  topic: string | null | undefined,
): GuideFocus | null {
  // Old 2.74.0 links to the former catch-all card still open a useful topic.
  const wanted = (topic ?? "").trim() === "agentTools" ? "agentSessions" : (topic ?? "").trim();
  if (!wanted) return null;
  const group = groups.find((candidate) => candidate.id === wanted);
  if (group) {
    const first = sections.find((section) => section.group === group.id);
    return first ? { group: group.id, sectionId: first.id } : null;
  }
  const section = sections.find((candidate) => candidate.id === wanted);
  return section ? { group: section.group, sectionId: section.id } : null;
}
