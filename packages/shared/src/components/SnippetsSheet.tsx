import { useEffect, useState } from "react";
import { useEscape } from "../hooks/useEscape";
import { AGENT_INSTALL_ORDER } from "../agentInstall";
import { SheetShell } from "./DialogHost";

/**
 * Сниппеты — частые команды одним тапом. Открывается через кнопку "⚡"
 * в pty-keys-bar. Тап вставляет команду в input (не выполняет сразу),
 * кнопка ↵ — выполняет. Группа «Установка» — готовые команды установки
 * агентов/Remotai/вирт. браузера. Свои кнопки («Мои команды») живут в
 * localStorage — добавление/удаление прямо в шите.
 */
export interface Snippet {
  label: string;
  cmd: string;
  group: "custom" | "remotai" | "install" | "git" | "node" | "go" | "agent" | "shell" | "system";
  platform?: "windows" | "posix";
}

export const REMOTAI_SERVER_INSTALL_COMMAND = "curl -fsSL https://remotai.ru/install.sh | sh";
/** Запуск службы на сервере: без него привязанная машина не выходит на связь. */
export const REMOTAI_SERVICE_START_COMMAND = "sudo systemctl enable --now remotai";
/** Привязка сервера к аккаунту: печатает код и ждёт подтверждения. */
export const REMOTAI_PAIR_COMMAND = "remotai pair";
/**
 * Перезапуск службы. Нужен, когда привязку сделали ПОСЛЕ её старта: конфиг
 * читается один раз при запуске, и до перезапуска сервер остаётся «не в сети»
 * при живой службе. С v2.42.13 `remotai pair` делает это сам, но для машин,
 * привязанных раньше, кнопка остаётся единственным лечением.
 */
export const REMOTAI_SERVICE_RESTART_COMMAND = "sudo systemctl restart remotai";
/**
 * Обновление агента на сервере целиком: свежий бинарь под эту платформу плюс
 * перезапуск службы. Раньше единственным путём была переустановка через
 * install.sh, после которой служба всё равно работала со старой версией в
 * памяти — и человек решал, что обновление не сработало.
 */
export const REMOTAI_UPDATE_COMMAND = "sudo remotai update";
/** Жив ли агент на сервере — первое, что смотрят, если машина «не в сети». */
export const REMOTAI_SERVICE_STATUS_COMMAND = "systemctl status remotai --no-pager";
/** Последние строки журнала службы: сюда смотрят, если она не поднимается. */
export const REMOTAI_SERVICE_LOGS_COMMAND = "journalctl -u remotai -n 50 --no-pager";

/**
 * Файл подкачки на 2 ГБ. Лечит целый класс «всё вылетело» на дешёвых VPS:
 * 1 ГБ памяти без swap не переживает установку npm-агента, и ядро убивает
 * процесс на середине. Команда ничего не делает, если подкачка уже есть,
 * и добавляет запись в fstab — чтобы после перезагрузки не пропала.
 */
export const SWAP_SETUP_COMMAND = "swapon --show | grep -q /swapfile || "
  + "(sudo fallocate -l 2G /swapfile && sudo chmod 600 /swapfile && sudo mkswap /swapfile "
  + "&& sudo swapon /swapfile && echo '/swapfile none swap sw 0 0' | sudo tee -a /etc/fstab); free -h";

// xdotool в этой же строке не для полноты: без него Xvfb и браузер поднимутся,
// картинка уйдёт на телефон — и ни одно нажатие не дойдёт. Ровно так и вышло у
// владельца: экран есть, кликнуть нельзя.
export const VIRTUAL_BROWSER_INSTALL_COMMAND ="sudo apt update && sudo apt install -y xvfb xdotool fonts-liberation fonts-dejavu-core fonts-noto-color-emoji && (sudo apt install -y libopenh264-8 || sudo apt install -y libopenh264-7 || true) && wget -q https://dl.google.com/linux/direct/google-chrome-stable_current_amd64.deb -O /tmp/chrome.deb && sudo apt install -y /tmp/chrome.deb";

/**
 * Установка Node.js — предусловие для ВСЕХ npm-агентов (claude, codex, kimi,
 * gemini…). Раньше сниппет был только posix-версии, и на чистой Windows кнопка
 * «Установить» у агента давала «npm не является внутренней или внешней
 * командой» без выхода. Команды экспортируются, потому что тот же шаг
 * показывает шторка запуска агента (AgentLaunchSheet).
 */
export const NODE_INSTALL_WINDOWS_COMMAND = "winget install -e --id OpenJS.NodeJS.LTS";
export const NODE_INSTALL_POSIX_COMMAND = "curl -fsSL https://deb.nodesource.com/setup_lts.x | sudo -E bash - && sudo apt install -y nodejs";

/** Команда установки Node.js под платформу терминала. */
export function nodeInstallCommand(platform?: string): string {
  return platform === "windows" ? NODE_INSTALL_WINDOWS_COMMAND : NODE_INSTALL_POSIX_COMMAND;
}

const ALL: Snippet[] = [
  // ── Remotai на этом сервере: весь путь по шагам ──
  //
  // Просьба владельца: «в горячие команды надо добавить всю привязку, запуск
  // служб — чтобы любой, кто даже не разбирается, мог это легко сделать».
  // Порядок кнопок = порядок действий, подписи пронумерованы: человеку не надо
  // знать ни install.sh, ни systemctl — он нажимает 1, 2, 3.
  //
  // Почему это отдельная группа, а не «Установка»: там соседствуют Node.js и
  // браузер, среди которых шаги сценария терялись, а порядок был не виден.
  { label: "1 · Установить Remotai", cmd: REMOTAI_SERVER_INSTALL_COMMAND, group: "remotai", platform: "posix" },
  { label: "2 · Привязать к аккаунту", cmd: REMOTAI_PAIR_COMMAND, group: "remotai", platform: "posix" },
  { label: "3 · Запустить службу", cmd: REMOTAI_SERVICE_START_COMMAND, group: "remotai", platform: "posix" },
  { label: "Проверить: работает ли", cmd: REMOTAI_SERVICE_STATUS_COMMAND, group: "remotai", platform: "posix" },
  { label: "Не в сети? Перезапустить службу", cmd: REMOTAI_SERVICE_RESTART_COMMAND, group: "remotai", platform: "posix" },
  { label: "Если не поднялась: журнал", cmd: REMOTAI_SERVICE_LOGS_COMMAND, group: "remotai", platform: "posix" },
  { label: "Обновить Remotai", cmd: REMOTAI_UPDATE_COMMAND, group: "remotai", platform: "posix" },
  { label: "Отправить в Telegram 💬", cmd: 'remotai send ""', group: "remotai" },
  { label: "Файл в Telegram 📎", cmd: "remotai send --file ", group: "remotai" },
  { label: "Сколько памяти свободно", cmd: "free -h", group: "remotai", platform: "posix" },
  // Дешёвые VPS идут с 1 ГБ и БЕЗ подкачки, а установка любого npm-агента там
  // съедает больше: у владельца `npm i @anthropic-ai/claude-code` убил
  // oom-killer прямо во время работы виртуального браузера, и с телефона это
  // выглядело как «всё вылетело» — команда пропала, машина перестала отвечать.
  // Подкачка эту грань убирает; команда идемпотентна — повторное нажатие
  // ничего не ломает.
  { label: "Мало памяти? Включить подкачку 2 ГБ", cmd: SWAP_SETUP_COMMAND, group: "remotai", platform: "posix" },
  { label: "Node.js LTS (Ubuntu/Debian)", cmd: NODE_INSTALL_POSIX_COMMAND, group: "install", platform: "posix" },
  { label: "Node.js LTS (Windows, winget)", cmd: NODE_INSTALL_WINDOWS_COMMAND, group: "install", platform: "windows" },
  // Без winget (старая Windows 10 / выключенный App Installer) остаётся только
  // офлайн-установщик: страница загрузки открывается в браузере ПК.
  { label: "Node.js: открыть страницу загрузки", cmd: "start https://nodejs.org/en/download", group: "install", platform: "windows" },
  { label: "Remotai на сервер", cmd: REMOTAI_SERVER_INSTALL_COMMAND, group: "install", platform: "posix" },
  { label: "Виртуальный браузер (Xvfb+Chrome)", cmd: VIRTUAL_BROWSER_INSTALL_COMMAND, group: "install", platform: "posix" },
  // Запуск агентов живёт в отдельной кнопке «Агент» рядом с ⚡: там видно, что
  // установлено, а что нужно поставить (см. AgentLaunchSheet). Дубли-сниппеты
  // «Запустить Claude/Codex/…» убраны, чтобы не было двух разных путей.
  // ── Git ──
  { label: "git status", cmd: "git status", group: "git" },
  { label: "git log короткий", cmd: "git log --oneline -20", group: "git" },
  { label: "git diff", cmd: "git diff", group: "git" },
  { label: "git pull", cmd: "git pull", group: "git" },
  { label: "git push", cmd: "git push", group: "git" },
  // ── Node / Go ──
  { label: "npm run build", cmd: "npm run build", group: "node" },
  { label: "npm test", cmd: "npm test", group: "node" },
  { label: "npm install", cmd: "npm install", group: "node" },
  { label: "go build", cmd: "go build ./...", group: "go" },
  { label: "go test ./...", cmd: "go test ./...", group: "go" },
  { label: "go vet ./...", cmd: "go vet ./...", group: "go" },
  // ── Shell / Система ──
  { label: "ls детально", cmd: "ls -la", group: "shell", platform: "posix" },
  { label: "Где я", cmd: "pwd", group: "shell", platform: "posix" },
  { label: "Процессы", cmd: "ps aux", group: "system", platform: "posix" },
  { label: "Диски", cmd: "df -h", group: "system", platform: "posix" },
  { label: "Журнал systemd", cmd: "journalctl -n 100 --no-pager", group: "system", platform: "posix" },
  { label: "Что тут?", cmd: "dir", group: "shell", platform: "windows" },
  { label: "Запущенные процессы", cmd: "tasklist | findstr /i node", group: "system", platform: "windows" },
];

const GROUP_LABEL: Record<Snippet["group"], string> = {
  custom: "⭐ Мои команды",
  remotai: "🖥 Remotai на этом сервере (по шагам)",
  install: "📦 Установка",
  agent: "AI агенты",
  git: "Git",
  node: "Node / npm",
  go: "Go",
  shell: "Shell",
  system: "Система",
};

// Порядок групп в шите (свои и установка — сверху).
const GROUP_ORDER: Snippet["group"][] = ["custom", "remotai", "install", "git", "node", "go", "shell", "system"];

const CUSTOM_KEY = "tg.snippets.custom.v1";

function loadCustom(): Snippet[] {
  try {
    const arr = JSON.parse(localStorage.getItem(CUSTOM_KEY) || "[]");
    if (!Array.isArray(arr)) return [];
    return arr
      .filter((s) => s && typeof s.label === "string" && typeof s.cmd === "string" && s.label && s.cmd)
      .map((s) => ({ label: s.label, cmd: s.cmd, group: "custom" as const }));
  } catch {
    return [];
  }
}

function saveCustom(list: Snippet[]): void {
  try {
    localStorage.setItem(CUSTOM_KEY, JSON.stringify(list.map(({ label, cmd }) => ({ label, cmd }))));
  } catch { /* приватный режим — свои кнопки просто не сохранятся */ }
}

export function SnippetsSheet({
  open, onClose, onInsert, onRun, platform = "", agentInstalls = [],
  userCommands, onAddCommand, onRemoveCommand, onTogglePin,
}: {
  open: boolean;
  onClose: () => void;
  onInsert: (cmd: string) => void;
  onRun: (cmd: string) => void;
  platform?: string;
  agentInstalls?: Array<{ id: string; name: string; install?: string }>;
  /** Общий список своих команд (живёт на компьютере). Если не передан —
   *  работает прежнее локальное хранилище: шторка остаётся самостоятельной. */
  userCommands?: Array<{ id: string; cmd: string; label?: string; pinned?: boolean }>;
  onAddCommand?: (label: string, cmd: string) => void;
  onRemoveCommand?: (id: string) => void;
  /** «📌 Закрепить в ряду» — то же место хранения, другое место показа. */
  onTogglePin?: (id: string, pinned: boolean) => void;
}) {
  // Список приходит сверху — значит хранилище общее с рядом под терминалом.
  const shared = Array.isArray(userCommands);
  const [search, setSearch] = useState("");
  const [custom, setCustom] = useState<Snippet[]>([]);
  const [addOpen, setAddOpen] = useState(false);
  const [newLabel, setNewLabel] = useState("");
  const [newCmd, setNewCmd] = useState("");
  useEscape(open, onClose);

  useEffect(() => {
    if (open) {
      if (!shared) setCustom(loadCustom());
      setAddOpen(false);
      setNewLabel("");
      setNewCmd("");
    }
  }, [open]);

  if (!open) return null;

  const addCustom = () => {
    const label = newLabel.trim();
    const cmd = newCmd.trim();
    if (!label || !cmd) return;
    if (shared) {
      onAddCommand?.(label, cmd);
    } else {
      const next = [...custom, { label, cmd, group: "custom" as const }];
      setCustom(next);
      saveCustom(next);
    }
    setNewLabel("");
    setNewCmd("");
    setAddOpen(false);
  };

  const removeCustom = (cmd: string) => {
    if (shared) {
      const found = (userCommands || []).find((c) => c.cmd === cmd);
      if (found) onRemoveCommand?.(found.id);
      return;
    }
    const next = custom.filter((s) => s.cmd !== cmd);
    setCustom(next);
    saveCustom(next);
  };

  const platformKind = platform === "windows" ? "windows" : platform ? "posix" : "";
  const installOrder = new Map<string, number>(AGENT_INSTALL_ORDER.map((id, i) => [id, i]));
  const registryInstalls: Snippet[] = agentInstalls
    .filter((a) => !!a.install)
    .sort((a, b) => (installOrder.get(a.id) ?? 999) - (installOrder.get(b.id) ?? 999))
    .map((a) => ({ label: `Установить ${a.name}`, cmd: a.install!, group: "install" }));
  const sharedCustom: Snippet[] = (userCommands || [])
    .filter((c) => !!c.cmd)
    .map((c) => ({ label: (c.label || "").trim() || c.cmd, cmd: c.cmd, group: "custom" as const }));
  const pool = [
    ...(shared ? sharedCustom : custom),
    ...registryInstalls,
    ...ALL.filter((snippet) => !snippet.platform || !platformKind || snippet.platform === platformKind),
  ];
  const filtered = search
    ? pool.filter((s) => s.label.toLowerCase().includes(search.toLowerCase()) || s.cmd.toLowerCase().includes(search.toLowerCase()))
    : pool;
  const grouped: Record<string, Snippet[]> = {};
  for (const s of filtered) { grouped[s.group] ||= []; grouped[s.group].push(s); }
  const groups = GROUP_ORDER.filter((g) => grouped[g]?.length);

  return (
    <SheetShell
      open={open}
      onClose={onClose}
      overlayClassName="snippets-backdrop"
      className="snippets-sheet"
      labelledBy="snippets-title"
    >
        <div className="snippets-head">
          <h3 id="snippets-title">⚡ Команды</h3>
          <button onClick={onClose} className="snippets-close" aria-label="Закрыть">×</button>
        </div>
        <input
          className="snippets-search"
          placeholder="Поиск…"
          value={search}
          onChange={(e) => setSearch(e.target.value)}
          autoFocus
        />

        {addOpen ? (
          <div className="snippets-add">
            <input
              className="snippets-add-input"
              placeholder="Название кнопки"
              value={newLabel}
              onChange={(e) => setNewLabel(e.target.value)}
            />
            <input
              className="snippets-add-input"
              placeholder="Команда (напр. docker compose up -d)"
              value={newCmd}
              onChange={(e) => setNewCmd(e.target.value)}
              autoCapitalize="off"
              autoCorrect="off"
              onKeyDown={(e) => { if (e.key === "Enter") { e.preventDefault(); addCustom(); } }}
            />
            <div className="snippets-add-actions">
              <button className="btn btn-secondary" onClick={() => setAddOpen(false)}>Отмена</button>
              <button className="btn btn-primary" onClick={addCustom} disabled={!newLabel.trim() || !newCmd.trim()}>
                Сохранить
              </button>
            </div>
          </div>
        ) : (
          <button className="snippets-add-btn" onClick={() => setAddOpen(true)}>
            ＋ Своя команда
          </button>
        )}

        <div className="snippets-list">
          {groups.map((g) => (
            <div key={g}>
              <div className="snippets-group">{GROUP_LABEL[g] || g}</div>
              {grouped[g].map((s) => (
                <div key={`${g}:${s.cmd}`} className="snippets-row">
                  <button className="snippets-item" onClick={() => { onInsert(s.cmd); onClose(); }}>
                    <span className="snippets-label">{s.label}</span>
                    <code className="snippets-cmd">{s.cmd}</code>
                  </button>
                  {g === "custom" && shared && (() => {
                    // «Закрепить в ряду» — это не только место кнопки, но и путь
                    // отправки: из ряда команда уходит сразу, из шторки —
                    // вставляется в поле ввода, и отправляет её человек.
                    const own = (userCommands || []).find((c) => c.cmd === s.cmd);
                    if (!own) return null;
                    return (
                      <button
                        className={`snippets-pin${own.pinned ? " on" : ""}`}
                        onClick={() => onTogglePin?.(own.id, !own.pinned)}
                        title={own.pinned ? "Убрать из ряда под терминалом" : "Закрепить в ряду под терминалом"}
                        aria-label={own.pinned ? "Убрать из ряда" : "Закрепить в ряду"}
                        aria-pressed={Boolean(own.pinned)}
                      >📌</button>
                    );
                  })()}
                  {g === "custom" && (
                    <button
                      className="snippets-del"
                      onClick={() => removeCustom(s.cmd)}
                      title="Удалить"
                      aria-label={`Удалить ${s.label}`}
                    >×</button>
                  )}
                  <button className="snippets-run" onClick={() => { onRun(s.cmd); onClose(); }} title="Выполнить">↵</button>
                </div>
              ))}
            </div>
          ))}
          {filtered.length === 0 && <div className="snippets-empty">Ничего не найдено</div>}
        </div>
    </SheetShell>
  );
}
