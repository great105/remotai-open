// Общие endpoint-обёртки REST API, извлечённые из apk/src/api.ts и
// miniapp/src/api.ts (тела были посимвольно идентичны). Сетевой слой —
// через транспортный реестр (см. api-transport.ts): приложение регистрирует
// setApiTransport({ request, uploadForm, base, authQuery }) в своём api.ts
// и реэкспортирует эти функции из "../api" для обратной совместимости.
//
// НЕ переносить сюда: разошедшиеся функции (getHealthReport), WS-слой,
// cloud-специфику (downloadBlob/ptyExportBlob/streamWSUrl — apk),
// pairing/tunnel (miniapp).

import { transport } from "./api-transport";
import type { ServerAccess } from "./serverAccess";
import type { UploadOptions } from "./api-transport";
import type {
  Session, SessionDetail, FileItem, AppConfig,
  QuickPath, SystemStats, ProcessInfo,
  Bookmark, CostStats, FilePreview, AgentInfo, AgentAccount, DiscoveredSession, DiskInfo,
  PtySessionInfo, PtySleep, PtyState, Project, AutostartStatus, ServiceStatus, VBrowserStatus,
  BrowserPage, BrowserTab,
  RecentFolder, SessionTemplate, Preset, LicenseStatus,
  OpenRouterStatus, OpenRouterModel,
} from "./types";

function api<T>(path: string, init?: RequestInit): Promise<T> {
  return transport().request<T>(path, init);
}

// Reading status never starts the trial. The first server operation does.
export function getServerAccess(): Promise<ServerAccess> {
  return api("/api/server-access");
}

// ── Sessions ────────────────────────────────────────────────────

export async function getSessions(): Promise<{ sessions: Session[]; active: string | null }> {
  return api("/api/sessions");
}

export async function createSession(name: string, agent_type: string, cwd: string) {
  return api("/api/sessions", { method: "POST", body: JSON.stringify({ name, agent_type, cwd }) });
}

export async function getSession(name: string): Promise<SessionDetail> {
  return api(`/api/sessions/${encodeURIComponent(name)}`);
}

export async function deleteSession(name: string) {
  return api(`/api/sessions/${encodeURIComponent(name)}`, { method: "DELETE" });
}

export async function switchSession(name: string) {
  return api(`/api/sessions/${encodeURIComponent(name)}/switch`, { method: "POST" });
}

export async function sendPrompt(name: string, prompt: string) {
  return api(`/api/sessions/${encodeURIComponent(name)}/send`, {
    method: "POST", body: JSON.stringify({ prompt }),
  });
}

export async function stopSession(name: string) {
  return api(`/api/sessions/${encodeURIComponent(name)}/stop`, { method: "POST" });
}

export async function updateSessionConfig(name: string, config: Record<string, string>) {
  return api(`/api/sessions/${encodeURIComponent(name)}/config`, {
    method: "PATCH", body: JSON.stringify(config),
  });
}

// ── Files ───────────────────────────────────────────────────────

/**
 * Список каталога. `hidden` показывает точечные файлы (.env, .ssh, .gitignore) —
 * без него они недостижимы, хотя SFTP-браузер соседним экраном их показывает.
 * `total`/`truncated` — сервер отдаёт не больше 2000 записей: node_modules и
 * System32 иначе уезжают в JSON целиком, а в облаке этот JSON живёт в памяти
 * агента, релея и вкладки одновременно.
 */
export async function listFiles(
  path: string,
  opts?: { hidden?: boolean; sort?: "name" | "date" | "size" },
): Promise<{
  path: string; items: FileItem[]; parent: string;
  total?: number; truncated?: boolean; hidden?: boolean; hidden_skipped?: number;
  /**
   * Эхо применённого сервером порядка. Сортировать нужно ДО обрезки лимитом,
   * иначе «По дате» упорядочивает произвольную алфавитную выборку — поэтому
   * порядок задаёт запрос, а по эху клиент понимает, что локально
   * пересортировывать список уже НЕЛЬЗЯ (агент старой версии эха не пришлёт,
   * и тогда локальная сортировка остаётся единственной).
   */
  sort?: string;
}> {
  const q = new URLSearchParams({ path });
  if (opts?.hidden) q.set("hidden", "1");
  if (opts?.sort) q.set("sort", opts.sort);
  return api(`/api/files?${q}`);
}

/** Сколько файлов и байт внутри папки — чтобы удаление не было прыжком в темноту. */
export async function getDirStat(path: string): Promise<{
  files: number; dirs: number; bytes: number; truncated?: boolean;
}> {
  return api(`/api/files/dir-stat?path=${encodeURIComponent(path)}`);
}

/**
 * Итог переноса/копирования на стороне ПК. Ответ сервера рассказывает больше,
 * чем «ok»: куда именно лёг результат, что не скопировалось и остался ли
 * исходник — без этих полей интерфейс рапортовал «скопирован» неизвестно куда.
 */
export interface FileTransferResult {
  ok: boolean;
  /** Фактический путь результата: в существующую папку сервер кладёт под именем источника. */
  path?: string;
  files?: number;
  dirs?: number;
  bytes?: number;
  /** Симлинки и спецфайлы, которые копирование дерева не переносит. */
  skipped?: number;
  /** true — перенос между дисками выполнен копированием (os.Rename так не умеет). */
  copied?: boolean;
  /** false — исходник остался на месте (не удалось удалить либо внутри спецфайлы). */
  source_removed?: boolean;
  /** Машинный код причины, по которой исходник остался. */
  source_code?: string;
}

/**
 * Копирование файла или дерева на стороне ПК (без прогулки байтов через телефон).
 *
 * `dst` — либо ПАПКА-получатель (сервер сам добавит имя источника), либо полный
 * целевой путь. Передавать папку надёжнее: тогда проверка «уже существует»
 * срабатывает на итоговом пути, а не создаёт матрёшку вида D:\Бэкап\docs\docs.
 */
export async function copyFile(src: string, dst: string, overwrite = false): Promise<FileTransferResult> {
  return api("/api/files/copy", { method: "POST", body: JSON.stringify({ src, dst, overwrite }) });
}

export function downloadUrl(path: string): string {
  return `${transport().base()}/api/files/download?path=${encodeURIComponent(path)}&initData=${encodeURIComponent(transport().authQuery())}`;
}

export async function deleteFile(path: string) {
  return api("/api/files", { method: "DELETE", body: JSON.stringify({ path }) });
}

export async function deleteDir(path: string) {
  return api("/api/files/dir", { method: "DELETE", body: JSON.stringify({ path }) });
}

export async function mkDir(path: string) {
  return api("/api/files/mkdir", { method: "POST", body: JSON.stringify({ path }) });
}

/**
 * Переименование И перемещение (перемещение = та же ручка с новым путём).
 *
 * `overwrite` — заменить существующий файл в цели (без него сервер отвечает
 * `already_exists`, чтобы перемещение не уничтожило чужой файл молча).
 * `allowCopy: false` — «спроси меня»: перенос между дисками одним движением
 * невозможен, и сервер вернёт 409 `cross_device` вместо тихого копирования;
 * повтор с `allowCopy: true` выполняет копию + удаление исходника.
 */
export async function renameFile(
  old_path: string,
  new_path: string,
  opts?: { overwrite?: boolean; allowCopy?: boolean },
): Promise<FileTransferResult> {
  const body: Record<string, unknown> = { old_path, new_path };
  if (opts?.overwrite) body.overwrite = true;
  if (opts?.allowCopy !== undefined) body.allow_copy = opts.allowCopy;
  return api("/api/files/rename", { method: "POST", body: JSON.stringify(body) });
}

export async function sendToTelegram(path: string) {
  return api("/api/files/send-to-telegram", { method: "POST", body: JSON.stringify({ path }) });
}

export async function getQuickPaths(): Promise<{ paths: QuickPath[] }> {
  return api("/api/files/quick-paths");
}

/**
 * Загрузка файлов в папку на ПК.
 *
 * `overwrite` — явное согласие заменить одноимённый файл. Без него сервер не
 * трогает существующий файл и отвечает `already_exists` (в чанкованной загрузке
 * — уже на первом куске, до траты мобильного трафика): решение «заменить или
 * сохранить копию» принимает человек, а не запись поверх.
 * `File[]` принимается наравне с FileList — так вызывающий может повторить
 * загрузку под другим именем (`new File([f], "отчет (2).xlsx")`).
 */
export async function uploadFiles(
  targetDir: string,
  files: FileList | File[],
  onProgress?: (pct: number) => void,
  options?: { signal?: AbortSignal; resumable?: boolean; overwrite?: boolean },
) {
  const form = new FormData();
  for (const f of files) form.append("file", f, f.name);
  const q = new URLSearchParams({ path: targetDir });
  if (options?.overwrite) q.set("overwrite", "1");
  return transport().uploadForm<any>(
    `/api/files/upload?${q}`,
    form,
    onProgress,
    { ...options, resumable: options?.resumable ?? true },
  );
}

export async function previewFile(path: string): Promise<FilePreview> {
  return api(`/api/files/preview?path=${encodeURIComponent(path)}`);
}

export interface FileSearchResult {
  results: FileItem[];
  query: string;
  truncated: boolean;
  timed_out: boolean;
  elapsed_ms: number;
  scanned: number;
  limit: number;
  hidden: boolean;
}

/**
 * Глубокий поиск намеренно идёт POST-запросом. Общий HTTP-клиент повторяет
 * безопасные GET при сетевом сбое, но для WalkDir это означало до трёх
 * одновременных обходов C:\. Signal позволяет остановить обход при нажатии
 * «Отмена» (в LAN контекст запроса также отменится на агенте).
 */
export async function searchFiles(
  query: string,
  path?: string,
  opts?: { hidden?: boolean; limit?: number; signal?: AbortSignal },
): Promise<FileSearchResult> {
  return api("/api/files/search", {
    method: "POST",
    body: JSON.stringify({
      query,
      path: path || "",
      hidden: !!opts?.hidden,
      limit: opts?.limit || 50,
    }),
    signal: opts?.signal,
  });
}

/** Свободное/общее место на ТОМЕ, содержащем path (локальный диск ПК-агента,
 *  gopsutil disk.Usage). Без path сервер берёт домашнюю папку. Ответ в БАЙТАХ.
 *  На ошибке сервер отдаёт 200 c {total:0,used:0,free:0} — вызывающий обязан
 *  проверять total > 0. Для SFTP-браузера аналога НЕТ (эндпоинт локальный). */
export async function getDiskInfo(path?: string): Promise<DiskInfo> {
  return api(`/api/files/disk-info${path ? `?path=${encodeURIComponent(path)}` : ""}`);
}

// ── PTY (Interactive Terminals) ────────────────────────────────

/**
 * Исход терминала: чем закончилась сессия или последняя команда в ней
 * (internal/pty/manager.go, Outcome). Журнал живёт отдельно от сессий и
 * переживает их удаление: сама мёртвая сессия исчезает через 5 минут, статус
 * "error" гаснет тоже через 5 минут, а вернувшийся через час человек обязан
 * узнать, что ночная сборка упала.
 */
export interface PtyOutcome {
  id: string;
  name?: string;
  cwd?: string;
  shell?: string;
  group?: string;
  agent_kind?: string;
  /** "error" — упала команда (терминал мог остаться жив); "dead" — терминал закрылся сам. */
  status: "error" | "dead";
  /** Машинный код: "output_error" | "exited" | "detached" — текст подставляет клиент. */
  reason?: string;
  /** unix ms — когда исход случился. */
  at: number;
  /**
   * Сама строка вывода, на которую сработал детектор ошибок («Error: build
   * failed», «npm ERR! code E404»). Пусто у старого агента и у исходов "dead".
   * Показывать её обязательно: без строки карточка говорила лишь «в выводе
   * мелькнула строка, похожая на ошибку», и проверить это было нечем.
   */
  hint?: string;
}

/**
 * Терминал, не переживший ПЕРЕЗАГРУЗКУ компьютера.
 *
 * Персистентный терминал переживает перезапуск Remotai, но не выключение
 * системы: процесс шелла умирает вместе с ней. Раньше запись о таком терминале
 * просто удалялась, и человек, включив компьютер, видел пустой список — имя,
 * папка, рабочий каталог и всё, что агент успел сделать за ночь, исчезали
 * молча. Теперь терминал ждёт здесь и продолжается одним нажатием
 * (`restorePtySession`): процесс поднимается ЗАНОВО в том же каталоге, а
 * последние строки прошлой работы показываются как архив с чертой.
 */
export interface PtyLostSession {
  id: string;
  name?: string;
  cwd: string;
  shell: string;
  group: string;
  /** unix ms — когда терминал завели впервые. */
  created: number;
  /** unix ms — когда обнаружили, что компьютер перезагрузился. */
  lost_at: number;
  /** Что здесь работало в последний раз («claude»), если известно. */
  agent?: string;
  /** Есть ли сохранённый хвост вывода — обещать «последние строки» иначе нельзя. */
  has_scrollback: boolean;
  /** Папки больше нет: терминал откроется в ближайшей существующей выше. */
  cwd_missing?: boolean;
}

export async function listPtySessions(opts?: { sort?: "last_active" | "created"; limit?: number; aliveOnly?: boolean }): Promise<{ sessions: PtySessionInfo[]; folders?: string[]; outcomes?: PtyOutcome[]; lost?: PtyLostSession[] }> {
  const q = new URLSearchParams();
  if (opts?.sort) q.set("sort", opts.sort);
  if (opts?.limit) q.set("limit", String(opts.limit));
  if (opts?.aliveOnly) q.set("alive", "1");
  return api(`/api/pty${q.toString() ? "?" + q.toString() : ""}`);
}

/**
 * Продолжить работу в терминале, не пережившем перезагрузку. Идентификатор
 * остаётся ТЕМ ЖЕ — ссылки из бота и открытые вкладки ведут к этому терминалу.
 * Ни одна команда автоматически не повторяется: что запускать, решает человек.
 */
export async function restorePtySession(id: string, cols: number, rows: number): Promise<{ id: string }> {
  return api(`/api/pty/${encodeURIComponent(id)}/restore`, {
    method: "POST", body: JSON.stringify({ cols, rows }),
  });
}

/**
 * Вернуть связь с ЖИВЫМ процессом терминала (`host_alive`): канал до него
 * оборвался, а работа внутри продолжается. Не путать с `restorePtySession` —
 * там процесса уже нет и терминал поднимается заново.
 */
export async function reattachPtySession(id: string): Promise<{ id: string }> {
  return api(`/api/pty/${encodeURIComponent(id)}/reattach`, { method: "POST" });
}

export async function createPtySession(cwd: string, shell: string, cols: number, rows: number): Promise<{ id: string }> {
  return api("/api/pty", { method: "POST", body: JSON.stringify({ cwd, shell, cols, rows }) });
}

export async function closePtySession(id: string) {
  return api(`/api/pty/${encodeURIComponent(id)}`, { method: "DELETE" });
}

export async function uploadPtyFile(
  file: File,
  onProgress?: (pct: number) => void,
  options?: UploadOptions,
): Promise<{ path: string }> {
  const form = new FormData();
  form.append("file", file, file.name);
  return transport().uploadForm(
    "/api/pty/upload",
    form,
    onProgress,
    { ...options, resumable: options?.resumable ?? true },
  );
}

export async function closeDeadPtySessions(): Promise<{ closed: number }> {
  return api("/api/pty/dead", { method: "DELETE" });
}

export async function getPtyState(id: string): Promise<PtyState> {
  return api(`/api/pty/${encodeURIComponent(id)}/state`);
}

export async function renamePty(id: string, name: string): Promise<{ name: string }> {
  return api(`/api/pty/${encodeURIComponent(id)}`, {
    method: "PATCH", body: JSON.stringify({ name }),
  });
}

/** Bulk-обновление папки (group) и ручного порядка (sort) терминалов —
 *  одним запросом после drag-and-drop / переименования папки. */
export async function setPtyPlacements(items: { id: string; group: string; sort: number }[]): Promise<{ ok: boolean }> {
  return api("/api/pty/meta", {
    method: "PATCH", body: JSON.stringify({ items }),
  });
}

export async function handoffPty(id: string, command?: string): Promise<{ ok: boolean }> {
  return api(`/api/pty/${encodeURIComponent(id)}/handoff`, {
    method: "POST", body: JSON.stringify({ command: command || "" }),
  });
}

/** Клавиша быстрого ответа агенту. Конкретные байты знает агент (ptyKeyBytes в
 *  internal/web/api_pty.go) — они те же, что шлёт sendRaw на экране терминала,
 *  поэтому ответ с карточки, из уведомления и из бота неотличим от ответа
 *  руками в терминале. */
export type PtyInputKey =
  | "enter" | "y" | "n" | "1" | "2" | "3"
  | "esc" | "tab" | "up" | "down" | "left" | "right"
  | "ctrl-c" | "ctrl-d" | "ctrl-z";

export interface PtyInputBody {
  /** Быстрый ответ клавишей. Взаимоисключим с data. */
  key?: PtyInputKey;
  /** Свободный текст ответа. Взаимоисключим с key, максимум 4096 байт. */
  data?: string;
  /** Дописать Enter к data — как кнопка ↵ в строке ввода терминала. */
  enter?: boolean;
  /** status_at вопроса, который ВИДЕЛ пользователь. Агент откажет
   *  (409, code "prompt_changed"), если спрашивает уже о другом: карточка или
   *  уведомление могли провисеть в кармане часы. */
  expect_status_at?: number;
}

/**
 * Ответить агенту в терминале, НЕ открывая экран терминала (кнопки на карточке
 * «Требует внимания», действия уведомления, «Остановить» = key "ctrl-c").
 * Успех: { ok: true, bytes } — ввод доставлен в PTY.
 * Ошибки приходят с машинным .code: "pty_not_found" (404), "pty_dead" (410),
 * "prompt_changed" (409), "unknown_key"/"bad_request"/"bad_encoding" (400),
 * "too_large" (413), "write_failed" (500).
 */
export async function ptyInput(id: string, body: PtyInputBody): Promise<{ ok: boolean; bytes: number }> {
  return api(`/api/pty/${encodeURIComponent(id)}/input`, {
    method: "POST", body: JSON.stringify(body),
  });
}

/** Усыпить агента терминала: процесс снимается, беседа ждёт продолжения. */
export async function ptySleep(id: string): Promise<{ ok: boolean; sleep: PtySleep }> {
  return api(`/api/pty/${encodeURIComponent(id)}/sleep`, { method: "POST" });
}

/** Забрать запись сна, чтобы набрать команду продолжения (одному экрану). */
export async function ptyWake(id: string): Promise<{ ok: boolean; sleep: PtySleep }> {
  return api(`/api/pty/${encodeURIComponent(id)}/wake`, { method: "POST" });
}

export function ptyExportUrl(id: string, format: "txt" | "md"): string {
  return `${transport().base()}/api/pty/${encodeURIComponent(id)}/export?format=${format}&initData=${encodeURIComponent(transport().authQuery())}`;
}

// ── SSH (терминал на внешний сервер через агент-бастион) ─────────

/** Параметры SSH-подключения через агент. Пароль нигде не хранится. */
export interface SSHConnectOpts {
  host_id?: string;
  host: string;
  port?: number;
  user: string;
  password?: string;
  identity_file?: string;
  key_passphrase?: string;
  cols?: number;
  rows?: number;
  /** Доверить неизвестный хост-ключ и дописать его в known_hosts агента (TOFU). */
  trust_host?: boolean;
  /** Jump-хост ("user@host:port") — подключение через промежуточный сервер. */
  proxy_jump?: string;
  /** Пароль для jump-хоста (тоже не хранится). */
  proxy_password?: string;
}

/**
 * Подключить SSH-сервер через агент (бастион). Возвращает id обычной
 * PTY-сессии (дальше — тот же /ws/pty/{id}). Ошибки приходят с машинными
 * полями .code ("host_key_unknown" | "auth_failed" | "unreachable" |
 * "bad_request") и .fingerprint (для host_key_unknown) — см. ApiError.
 */
export async function connectSSH(opts: SSHConnectOpts): Promise<{ id: string }> {
  return api("/api/ssh/connect", { method: "POST", body: JSON.stringify(opts) });
}

// ── SSH: сохранённые хосты, история, форвардинг, SFTP ───────────

/** Сохранённый SSH-сервер. source=config — подхвачен из ~/.ssh/config агента
 *  (только чтение), source=saved — добавлен пользователем (PATCH/DELETE). */
export interface SshHost {
  id: string;
  name: string;
  host: string;
  port: number;
  user: string;
  identity_file?: string;
  proxy_jump?: string;
  tags: string[];
  source: "config" | "saved";
  last_at?: string;
  count?: number;
  user_assumed?: boolean;
  unlocked: boolean;
  auth_ready: boolean;
  linked_device_id?: string;
  /** Ключ из хранилища ПК, назначенный серверу (см. SshKey). */
  key_id?: string;
  /** Имя этого ключа — приходит вместе с хостом, чтобы список не делал
   *  второй запрос ради подписи «вход по ключу „Прод“». */
  key_name?: string;
  /** Пароль сохранён на компьютере и переживёт его перезагрузку.
   *  Отличается от unlocked («есть в памяти прямо сейчас»). */
  secret_persisted?: boolean;
}

export interface SshHostInput {
  name: string;
  host: string;
  port?: number;
  user: string;
  identity_file?: string;
  proxy_jump?: string;
  tags?: string[];
  linked_device_id?: string;
  /** Ключ из хранилища; пустая строка снимает назначение. */
  key_id?: string;
}

export async function getSshHosts(): Promise<{ hosts: SshHost[] }> {
  return api("/api/ssh/hosts");
}

export async function createSshHost(input: SshHostInput): Promise<{ host: SshHost }> {
  return api("/api/ssh/hosts", { method: "POST", body: JSON.stringify(input) });
}

export async function updateSshHost(id: string, patch: Partial<SshHostInput>): Promise<{ host: SshHost }> {
  return api(`/api/ssh/hosts/${encodeURIComponent(id)}`, {
    method: "PATCH", body: JSON.stringify(patch),
  });
}

export async function deleteSshHost(id: string) {
  return api(`/api/ssh/hosts/${encodeURIComponent(id)}`, { method: "DELETE" });
}

export interface SshUnlockInput {
  password?: string;
  key_passphrase?: string;
  proxy_password?: string;
  /** Сохранить на компьютере (по умолчанию да). false — держать только до
   *  перезапуска агента: способ подключиться, ничего не оставляя. */
  persist?: boolean;
}

export async function unlockSshHost(id: string, input: SshUnlockInput): Promise<{ ok: boolean; unlocked: boolean; persist?: boolean }> {
  return api(`/api/ssh/hosts/${encodeURIComponent(id)}/unlock`, {
    method: "POST", body: JSON.stringify(input),
  });
}

export async function forgetSshHostSecret(id: string): Promise<{ ok: boolean; unlocked: boolean }> {
  return api(`/api/ssh/hosts/${encodeURIComponent(id)}/unlock`, { method: "DELETE" });
}

export async function forgetSshKnownHost(host: string, port = 22): Promise<{ ok: boolean; removed: number }> {
  return api("/api/ssh/known-hosts", {
    method: "DELETE", body: JSON.stringify({ host, port }),
  });
}

export interface SshHistoryEntry {
  host: string;
  port: number;
  user: string;
  last_at: string;
  count: number;
  proxy_jump?: string;
}

export async function getSshHistory(): Promise<{ entries: SshHistoryEntry[] }> {
  return api("/api/ssh/history");
}

// ── SSH-ключи, живущие внутри Remotai ───────────────────────────
//
// Приватная часть ключа не приходит сюда никогда: она лежит на компьютере
// зашифрованной и читается только самим агентом в момент подключения. Клиент
// оперирует именем, типом и отпечатком.

export interface SshKey {
  id: string;
  name: string;
  /** Тип из самого ключа: "ssh-ed25519", "ssh-rsa", … */
  type: string;
  /** Строка формата authorized_keys — её ставят на сервер. */
  public_key: string;
  fingerprint: string;
  created_at: string;
  /** Ключ защищён собственным паролем (пароль хранится рядом, поэтому
   *  подключение всё равно идёт без вопросов). */
  encrypted?: boolean;
  /** Создан здесь, а не принесён готовым. */
  generated?: boolean;
}

/** foreign=true — ключи есть, но их приватные части зашифрованы другой
 *  учётной записью этого компьютера: подключиться ими отсюда нельзя. */
export async function getSshKeys(): Promise<{ keys: SshKey[]; foreign: boolean }> {
  return api("/api/ssh/keys");
}

export interface SshKeyImportInput {
  name?: string;
  /** Содержимое приватного ключа — единственный способ добавить его с телефона. */
  private_key?: string;
  /** Либо путь к файлу ключа на самом ПК (удобно в окне приложения). */
  path?: string;
  passphrase?: string;
}

export async function importSshKey(input: SshKeyImportInput): Promise<{ key: SshKey }> {
  return api("/api/ssh/keys", { method: "POST", body: JSON.stringify(input) });
}

export async function generateSshKey(input: {
  name?: string;
  type?: "ed25519" | "rsa";
  passphrase?: string;
}): Promise<{ key: SshKey }> {
  return api("/api/ssh/keys/generate", { method: "POST", body: JSON.stringify(input) });
}

export async function renameSshKey(id: string, name: string): Promise<{ key: SshKey }> {
  return api(`/api/ssh/keys/${encodeURIComponent(id)}`, {
    method: "PATCH", body: JSON.stringify({ name }),
  });
}

/** Удаляет ключ и снимает его со всех серверов (detached_hosts — со скольких). */
export async function deleteSshKey(id: string): Promise<{ ok: boolean; detached_hosts: number }> {
  return api(`/api/ssh/keys/${encodeURIComponent(id)}`, { method: "DELETE" });
}

export interface SshKeyInstallInput {
  host_id?: string;
  host?: string;
  port?: number;
  user?: string;
  /** Пароль сервера — нужен ровно один раз, дальше вход идёт ключом. */
  password?: string;
  trust_host?: boolean;
  proxy_jump?: string;
  proxy_password?: string;
  /** Назначить ключ серверу после установки (по умолчанию да). */
  assign?: boolean;
  /** Заодно запомнить пароль сервера (по умолчанию нет). */
  remember?: boolean;
}

/**
 * Установить публичный ключ на сервер (то же, что ssh-copy-id).
 * already=true — ключ там уже стоял; assigned=true — сервер переведён на него.
 */
export async function installSshKey(id: string, input: SshKeyInstallInput):
  Promise<{ ok: boolean; already: boolean; assigned: boolean }> {
  return api(`/api/ssh/keys/${encodeURIComponent(id)}/install`, {
    method: "POST", body: JSON.stringify(input),
  });
}

// Проброс портов через SSH-сервер (туннели живут на агенте).
export type SshForwardType = "local" | "remote" | "dynamic";

export interface SshForward {
  id: string;
  type: SshForwardType;
  server: string;
  bind_addr: string;
  bind_port: number;
  target_host?: string;
  target_port?: number;
  status: "active" | "error";
  error?: string;
  access: string;
}

export interface SshForwardInput {
  host_id?: string;
  host: string;
  port?: number;
  user: string;
  password?: string;
  identity_file?: string;
  key_passphrase?: string;
  trust_host?: boolean;
  proxy_jump?: string;
  proxy_password?: string;
  type: SshForwardType;
  bind_addr?: string;
  bind_port: number;
  target_host?: string;
  target_port?: number;
  allow_lan?: boolean;
}

export interface SshForwardSpec extends SshForwardInput {
  id: string;
}

export async function getSshForwards(): Promise<{ forwards: SshForward[]; specs?: SshForwardSpec[] }> {
  return api("/api/ssh/forwards");
}

export async function createSshForward(input: SshForwardInput): Promise<{ id: string; spec_id?: string }> {
  return api("/api/ssh/forwards", { method: "POST", body: JSON.stringify(input) });
}

export async function deleteSshForward(id: string) {
  return api(`/api/ssh/forwards/${encodeURIComponent(id)}`, { method: "DELETE" });
}

export async function deleteSshForwardSpec(id: string) {
  return api(`/api/ssh/forward-specs/${encodeURIComponent(id)}`, { method: "DELETE" });
}

// SFTP — файловый менеджер по SSH. Пароль только в параметрах запроса,
// нигде не сохраняется.
export interface SftpConn {
  host_id?: string;
  host: string;
  port?: number;
  user: string;
  password?: string;
  identity_file?: string;
  key_passphrase?: string;
  proxy_jump?: string;
  proxy_password?: string;
  trust_host?: boolean;
}

export interface SftpEntry {
  name: string;
  size: number;
  is_dir: boolean;
  mod_time: string;
  permissions: string;
  hidden?: boolean;
}

function sftpQuery(c: SftpConn, extra?: Record<string, string>): string {
  const q = new URLSearchParams({
    host: c.host,
    port: String(c.port || 22),
    user: c.user,
  });
  if (c.password) q.set("password", c.password);
  if (c.host_id) q.set("host_id", c.host_id);
  if (c.identity_file) q.set("identity_file", c.identity_file);
  if (c.key_passphrase) q.set("key_passphrase", c.key_passphrase);
  if (c.proxy_jump) q.set("proxy_jump", c.proxy_jump);
  if (c.proxy_password) q.set("proxy_password", c.proxy_password);
  if (c.trust_host) q.set("trust_host", "1");
  for (const [k, v] of Object.entries(extra || {})) q.set(k, v);
  return q.toString();
}

export interface SftpListOpts {
  /** Порядок записей считает СЕРВЕР: он сортирует весь каталог и только потом
   *  режет лимит в 2000 записей, поэтому «по размеру» на клиенте упорядочивало
   *  бы произвольную алфавитную выборку. Ответ эхом отдаёт применённый режим. */
  sort?: "name" | "date" | "size";
  /** «Помнить пароль до перезапуска ПК»: агент кладёт секрет в память ТОЛЬКО
   *  после удачного листинга и отвечает secret_remembered — по нему экран
   *  говорит правду вместо надежды (отдельный POST /unlock молча терялся). */
  remember?: boolean;
}

export async function sftpList(c: SftpConn, path: string, hidden = false, opts?: SftpListOpts): Promise<{
  entries: SftpEntry[]; cwd: string; total: number; truncated: boolean;
  sort?: string;
  /** Есть только когда признак применим (сохранённый хост) и remember послан. */
  secret_remembered?: boolean;
}> {
  const extra: Record<string, string> = { path, hidden: hidden ? "1" : "0" };
  if (opts?.sort) extra.sort = opts.sort;
  if (opts?.remember) extra.remember = "1";
  return api(`/api/ssh/sftp/list?${sftpQuery(c, extra)}`);
}

export interface SftpPreview {
  kind: "text" | "binary";
  content?: string;
  content_type: string;
  size: number;
  truncated: boolean;
}

export async function sftpPreview(c: SftpConn, path: string): Promise<SftpPreview> {
  return api(`/api/ssh/sftp/preview?${sftpQuery(c, { path })}`);
}

function sftpBodyConn(c: SftpConn) {
  return {
    host_id: c.host_id,
    host: c.host,
    port: c.port || 22,
    user: c.user,
    password: c.password,
    identity_file: c.identity_file,
    key_passphrase: c.key_passphrase,
    proxy_jump: c.proxy_jump,
    proxy_password: c.proxy_password,
    trust_host: c.trust_host,
  };
}

// ── Переносы ПК ↔ SSH-сервер ───────────────────────────────────
// Байты идут ВНУТРИ ПК: телефон только запускает перенос и смотрит прогресс.
// Ручки отвечают 202 + id сразу — облачный запрос через релей живёт максимум
// 60 с, а копирование дампа на гигабайты столько не укладывается.

export interface SshTransfer {
  id: string;
  /** push — ПК→сервер, pull — сервер→ПК. */
  dir: "push" | "pull";
  name: string;
  target: string;
  state: "running" | "done" | "error" | "canceled";
  done: number;
  total: number;
  remote_path: string;
  local_path: string;
  code?: string;
  error?: string;
}

/** Отправить файл С ПК на сервер. local — файл на ПК, remoteDir — папка на сервере. */
export async function sftpPush(c: SftpConn, local: string, remoteDir: string): Promise<{ id: string; transfer: SshTransfer }> {
  return api("/api/ssh/sftp/push", {
    method: "POST",
    body: JSON.stringify({ ...sftpBodyConn(c), local, path: remoteDir }),
  });
}

/** Забрать файл С СЕРВЕРА на ПК. remote — файл на сервере, localDir — папка на ПК. */
export async function sftpPull(c: SftpConn, remote: string, localDir: string): Promise<{ id: string; transfer: SshTransfer }> {
  return api("/api/ssh/sftp/pull", {
    method: "POST",
    body: JSON.stringify({ ...sftpBodyConn(c), path: remote, local: localDir }),
  });
}

/** Активные и недавно завершённые переносы. Переносы живут в памяти агента:
 *  исчезновение running-задания означает «прервано», а не «готово». */
export async function sftpTransfers(): Promise<{ transfers: SshTransfer[] }> {
  return api("/api/ssh/sftp/transfers");
}

export async function sftpCancelTransfer(id: string): Promise<{ ok: boolean }> {
  return api("/api/ssh/sftp/transfers/cancel", { method: "POST", body: JSON.stringify({ id }) });
}

/** Загрузка файлов на SSH-сервер. path — целевая папка на сервере.
 *  Большие файлы в cloud-режиме режет сам транспорт (uploadForm). */
export async function sftpUpload(c: SftpConn, dir: string, files: FileList, onProgress?: (pct: number) => void) {
  const form = new FormData();
  for (const f of files) form.append("file", f, f.name);
  return transport().uploadForm<any>(`/api/ssh/sftp/upload?${sftpQuery(c, { path: dir })}`, form, onProgress);
}

export async function sftpMkdir(c: SftpConn, path: string) {
  return api("/api/ssh/sftp/mkdir", {
    method: "POST", body: JSON.stringify({ ...sftpBodyConn(c), path }),
  });
}

export async function sftpDelete(c: SftpConn, path: string) {
  return api("/api/ssh/sftp/delete", {
    method: "POST", body: JSON.stringify({ ...sftpBodyConn(c), path }),
  });
}

export async function sftpRename(c: SftpConn, from: string, to: string) {
  return api("/api/ssh/sftp/rename", {
    method: "POST", body: JSON.stringify({ ...sftpBodyConn(c), from, to }),
  });
}

// ── Projects (.git folders, auto-scanned) ──────────────────────

export async function getProjects(): Promise<{ projects: Project[]; cached: boolean }> {
  return api("/api/projects");
}

// ── System ──────────────────────────────────────────────────────

export async function getSystemStats(): Promise<SystemStats> {
  return api("/api/system/stats");
}

/**
 * Список процессов. `cpu_warmup` — дельты нет ни у одной строки (у отдельных
 * строк тот же смысл несёт их собственный флаг): клиент рисует «—», а не
 * «0.0%», иначе «нет данных» читается как «процессор свободен».
 * `cpu_window_ms` — промежуток, за который посчитана загрузка.
 */
export async function getProcesses(sort?: string): Promise<{
  processes: ProcessInfo[];
  cpu_warmup?: boolean;
  cpu_window_ms?: number;
}> {
  const q = sort ? `?sort=${sort}` : "";
  return api(`/api/system/processes${q}`);
}

/**
 * Лимиты AI-подписок компьютера, к которому клиент подключён напрямую (LAN и
 * окно exe): в облаке тот же ответ приходит через релей-прокси по device_id.
 * Тип снимка живёт в apk/src/aiUsage.ts, поэтому обёртка параметризована.
 */
export async function getAIUsage<T>(refresh = false): Promise<T> {
  return api(refresh ? "/api/ai-usage?refresh=1" : "/api/ai-usage");
}

/** Расход токенов агентами этого компьютера за последние `days` дней (1, 7, 30). */
export async function getTokenUsage<T>(days: number): Promise<T> {
  return api(`/api/token-usage?days=${days}`);
}

/**
 * Завершить процесс. `confirm` агент требует для своих (сам Remotai, pty-хосты,
 * окно панели) и системных процессов: без него он отвечает машинным кодом
 * `confirm_required` и ничего не убивает. Ответ отдаёт `self:true`, когда агент
 * завершил сам себя — обновлять список процессов после этого уже некому.
 */
export async function killProcess(pid: number, confirm = false): Promise<{ ok: boolean; self?: boolean } | null> {
  return api("/api/system/kill", { method: "POST", body: JSON.stringify({ pid, confirm }) });
}

export interface ScreenshotResult {
  data: string;
  mime: string;
  display?: number;
  width?: number;
  height?: number;
  original?: boolean;
  displays?: Array<{ id: number; w: number; h: number }>;
}

export async function setPtyFolders(folders: string[]): Promise<{ ok: boolean }> {
  return api("/api/pty/meta", {
    method: "PATCH", body: JSON.stringify({ folders }),
  });
}

/**
 * Снимок экрана ФАЙЛОМ на сам компьютер — чтобы отдать его агенту.
 *
 * `takeScreenshot` возвращает кадр в ответе, и он уезжает в галерею телефона.
 * Здесь наоборот: PNG остаётся НА ПК, в `~/Remotai/files`, и в ответе приходит
 * путь. Читать картинку будет Claude Code или Codex, а они видят только
 * файловую систему своей машины — кадр в телефоне для них не существует.
 */
export async function saveScreenshotToPC(options: { display?: number } = {}): Promise<{
  path: string;
  name: string;
  dir: string;
  display: number;
  width: number;
  height: number;
  cached: boolean;
}> {
  const q = new URLSearchParams();
  if (options.display != null) q.set("display", String(options.display));
  return api(`/api/system/screenshot/save${q.size ? `?${q}` : ""}`, { method: "POST" });
}

export async function takeScreenshot(options: {
  width?: number;
  display?: number;
  original?: boolean;
} = {}): Promise<ScreenshotResult> {
  const q = new URLSearchParams();
  if (options.width) q.set("w", String(options.width));
  if (options.display != null) q.set("display", String(options.display));
  if (options.original) q.set("original", "1");
  return api(`/api/system/screenshot${q.size ? `?${q}` : ""}`);
}

export async function powerAction(action: string) {
  return api("/api/system/power", { method: "POST", body: JSON.stringify({ action }) });
}

export async function getAutostartStatus(): Promise<AutostartStatus> {
  return api("/api/system/autostart");
}

export async function setAutostart(enable: boolean): Promise<AutostartStatus> {
  return api("/api/system/autostart", { method: "POST", body: JSON.stringify({ enable }) });
}

export async function getServiceStatus(): Promise<ServiceStatus> {
  return api("/api/system/service");
}

/**
 * Здоровье компьютера: почему он пропадал и что у него со связью.
 *
 * Отчёт о перерыве компьютер собирает при старте (журнал Windows + собственное
 * сердцебиение), состояние сети — сторож зависшего VPN. Обе ручки читает и
 * приложение, и `remotai doctor`, и агент с другого устройства аккаунта.
 */
export interface BootReport {
  kind: string;          // first_run | reboot | power_loss | bugcheck | agent_crash | agent_restart
  title: string;         // одна фраза для человека
  detail?: string;
  gap_seconds: number;   // сколько компьютера не было
  off_seconds?: number;  // сколько из них он был выключен
  rebooted: boolean;
  unexpected: boolean;   // повод показать это заметно
  boot_at: number;
  last_seen?: number;
  at: number;
  repairs?: string[];    // что агент починил сам, вернувшись
  prev_version?: string;
}

export interface NetworkStatus {
  internet: boolean;
  cloud: boolean;
  checked_at: number;
  watchdog: boolean;
  offline_from?: number;
  vpn: null | {
    name: string;
    pid?: number;
    exe?: string;
    task?: string;
    running: boolean;
    start_hint?: string;
  };
  last_action?: null | { at: number; what: string; result: string; text: string };
}

export async function getBootReport(): Promise<{ available: boolean; report?: BootReport }> {
  return api("/api/system/boot-report");
}

// check=true заставляет компьютер проверить сеть прямо сейчас (несколько
// секунд). Обычный опрос отдаёт последний снимок: открытие экрана не должно
// стоить пачки сетевых проб.
export async function getNetworkStatus(check = false): Promise<NetworkStatus> {
  return api("/api/system/network" + (check ? "?check=1" : ""));
}

export async function controlVPN(action: "start" | "stop"): Promise<{
  ok: boolean; action: string; vpn?: string; how?: string; start_hint?: string; status: NetworkStatus;
}> {
  return api("/api/system/vpn", { method: "POST", body: JSON.stringify({ action }) });
}

// ── Virtual browser (Xvfb на headless Linux-агенте) ─────────────

export async function getVBrowserStatus(): Promise<VBrowserStatus> {
  return api("/api/vbrowser/status");
}

export async function startVBrowser(opts: { browser?: string; width?: number; height?: number; lang?: string } = {}): Promise<VBrowserStatus> {
  return api("/api/vbrowser/start", { method: "POST", body: JSON.stringify(opts) });
}

export async function stopVBrowser(): Promise<{ ok: boolean }> {
  return api("/api/vbrowser/stop", { method: "POST", body: "{}" });
}

/** «Оставить включённым»: сдвигает автоостановку браузера по простою. */
export async function keepaliveVBrowser(): Promise<{ ok: boolean }> {
  return api("/api/vbrowser/keepalive", { method: "POST", body: "{}" });
}

/** Управление вкладкой виртуального браузера напрямую: адрес, история,
 *  перезагрузка. Кадр, который видит человек, — это САМА СТРАНИЦА, без
 *  адресной строки браузера, поэтому открыть новый сайт иначе можно было
 *  только вслепую (Ctrl+L и печать в невидимое поле). */
export async function navigateVBrowser(
  target: { url?: string; action?: "back" | "forward" | "reload" },
): Promise<{ ok: boolean; url?: string }> {
  return api("/api/vbrowser/navigate", { method: "POST", body: JSON.stringify(target) });
}

/** Что открыто в браузере на машине: адрес, заголовок, идёт ли загрузка. */
export async function getBrowserPage(): Promise<BrowserPage> {
  return api("/api/vbrowser/page");
}

/** Файл, скачанный виртуальным браузером (path — для downloadUrl). */
export interface VBrowserDownload {
  name: string;
  path: string;
  size: number;
  mtime: number;
}

/** Файлы, скачанные виртуальным браузером (новейшие сверху). */
export async function getVBrowserDownloads(): Promise<{ downloads: VBrowserDownload[] }> {
  return api("/api/vbrowser/downloads");
}

/** Вкладки браузера на машине. */
export async function getBrowserTabs(): Promise<{ tabs: BrowserTab[] }> {
  return api("/api/vbrowser/tabs");
}

/** Открыть новую вкладку (пустой url — пустая вкладка) и перейти на неё. */
export async function newBrowserTab(url = ""): Promise<{ ok: boolean; id: string }> {
  return api("/api/vbrowser/tabs", { method: "POST", body: JSON.stringify({ url }) });
}

/** Смотреть другую вкладку: она же становится активной в самом браузере. */
export async function activateBrowserTab(id: string): Promise<{ ok: boolean }> {
  return api(`/api/vbrowser/tabs/${encodeURIComponent(id)}/activate`, { method: "POST", body: "{}" });
}

export async function closeBrowserTab(id: string): Promise<{ ok: boolean }> {
  return api(`/api/vbrowser/tabs/${encodeURIComponent(id)}`, { method: "DELETE" });
}

/** Кем притворяется браузер. Размеры телефонов КАТАЛОЖНЫЕ (их задаёт агент):
 *  сайт, увидевший «Pixel 7» с экраном 600×1222, знает, что таких телефонов не
 *  бывает. Размер зрителя учитывается только у вида «Компьютер». */
export async function emulateBrowserDevice(opts: {
  device: string;
  width?: number; height?: number; scale?: number;
}): Promise<{ ok: boolean; device: string }> {
  return api("/api/vbrowser/emulate", { method: "POST", body: JSON.stringify(opts) });
}

/** Вид сайта в меню: список профилей знает агент, а не приложение. */
export interface BrowserDevice {
  id: string;
  title: string;
  mobile: boolean;
  width: number;
  height: number;
}

export async function getBrowserDevices(): Promise<{ devices: BrowserDevice[]; current: string }> {
  return api("/api/vbrowser/devices");
}

/** Что под пальцем: ссылка, картинка, поле ввода — из этого собирается меню
 *  долгого нажатия. Координаты — доли кадра, как весь остальной ввод. */
export interface BrowserHit {
  link?: string;
  link_text?: string;
  image?: string;
  video?: string;
  text?: string;
  editable?: boolean;
  tag?: string;
  selection?: string;
}

export async function hitBrowserPoint(
  x: number, y: number, select = false,
): Promise<BrowserHit> {
  return api("/api/vbrowser/hit", { method: "POST", body: JSON.stringify({ x, y, select }) });
}

/** Что сейчас выделено на странице (для «Копировать»). */
export async function getBrowserSelection(): Promise<{ text: string }> {
  return api("/api/vbrowser/selection");
}

/** Поиск по странице: fresh — новый запрос, иначе переход к следующему. */
export async function findOnBrowserPage(
  query: string, opts: { forward?: boolean; fresh?: boolean } = {},
): Promise<{ matches: number; query: string }> {
  return api("/api/vbrowser/find", {
    method: "POST",
    body: JSON.stringify({ query, forward: opts.forward ?? true, fresh: !!opts.fresh }),
  });
}

/** Какая доля экрана осталась видимой после того, как выехала клавиатура
 *  телефона (1 — клавиатуры нет). Без этого страница считает экран целым и
 *  оставляет поле ввода ПОД клавиатурой — человек печатает вслепую. */
export async function setBrowserViewport(visible: number): Promise<{ ok: boolean }> {
  return api("/api/vbrowser/viewport", { method: "POST", body: JSON.stringify({ visible }) });
}

// ── Стартовая страница: закладки и «часто открываю» ─────────────

export interface BrowserBookmark { url: string; title: string; host: string; added: number }
export interface BrowserTopSite { host: string; url: string; title?: string; count: number; last: number }

export async function getBrowserPlaces(): Promise<{ bookmarks: BrowserBookmark[]; top: BrowserTopSite[] }> {
  return api("/api/vbrowser/places");
}

/** Пустой url означает «текущую страницу» — как звёздочка в браузере. */
export async function addBrowserBookmark(url = "", title = ""): Promise<{ ok: boolean; bookmark: BrowserBookmark }> {
  return api("/api/vbrowser/places", { method: "POST", body: JSON.stringify({ url, title }) });
}

export async function removeBrowserBookmark(url: string): Promise<{ ok: boolean }> {
  return api(`/api/vbrowser/places?url=${encodeURIComponent(url)}`, { method: "DELETE" });
}

export async function forgetBrowserSite(host: string): Promise<{ ok: boolean }> {
  return api(`/api/vbrowser/places?host=${encodeURIComponent(host)}`, { method: "DELETE" });
}

/** Страница, на которой человек был. Отдельно от «часто открываю»: то про
 *  сайты, а история отвечает на вопрос «где я видел ту страницу вчера». */
export interface BrowserHistoryEntry {
  url: string;
  title: string;
  host: string;
  at: number;
}

export async function getBrowserHistory(
  query = "", limit = 100,
): Promise<{ history: BrowserHistoryEntry[]; query: string }> {
  const q = new URLSearchParams();
  if (query) q.set("q", query);
  if (limit) q.set("limit", String(limit));
  return api(`/api/vbrowser/history${q.size ? `?${q}` : ""}`);
}

/** Забыть одну страницу; all=true — очистить историю целиком (без отмены). */
export async function forgetBrowserHistory(
  url = "", all = false,
): Promise<{ ok: boolean; removed: number }> {
  const q = new URLSearchParams();
  if (url) q.set("url", url);
  if (all) q.set("all", "1");
  return api(`/api/vbrowser/history?${q}`, { method: "DELETE" });
}

// ── Пароли и анкета: чтобы регистрироваться с телефона ──────────

/** Сохранённый вход. Пароля здесь НЕТ и не будет: он не покидает машину, а в
 *  поле страницы попадает внутри агента по идентификатору записи. */
export interface BrowserLogin {
  id: string;
  host: string;
  login: string;
  note?: string;
  created: number;
  used_at?: number;
}

export interface BrowserProfileData {
  first_name?: string;
  last_name?: string;
  email?: string;
  phone?: string;
  birthday?: string;
  country?: string;
  city?: string;
  address?: string;
  zip?: string;
}

export async function getBrowserLogins(host = ""): Promise<{
  host: string; logins: BrowserLogin[]; all: number;
  profile: BrowserProfileData; foreign: boolean;
}> {
  return api(`/api/vbrowser/logins${host ? `?host=${encodeURIComponent(host)}` : ""}`);
}

/** Сохранить вход. Пустые поля означают «возьми то, что сейчас в полях
 *  страницы»: человек уже их набрал. */
export async function saveBrowserLogin(
  data: { host?: string; login?: string; password?: string; note?: string } = {},
): Promise<{ ok: boolean; login: BrowserLogin }> {
  return api("/api/vbrowser/logins", { method: "POST", body: JSON.stringify(data) });
}

export async function deleteBrowserLogin(id: string): Promise<{ ok: boolean }> {
  return api(`/api/vbrowser/logins/${encodeURIComponent(id)}`, { method: "DELETE" });
}

export async function saveBrowserProfile(profile: BrowserProfileData): Promise<{ ok: boolean; profile: BrowserProfileData }> {
  return api("/api/vbrowser/profile", { method: "PUT", body: JSON.stringify(profile) });
}

/** Что за форма на странице: какие поля просит и есть ли чем их заполнить. */
export interface BrowserFormField {
  index: number;
  kind: string;
  type: string;
  label?: string;
  value?: string;
  empty: boolean;
}

export async function getBrowserForm(): Promise<{
  host: string; fields: BrowserFormField[]; kinds: Record<string, number>;
  logins: BrowserLogin[]; profile: BrowserProfileData; has_form: boolean; signup: boolean;
}> {
  return api("/api/vbrowser/form");
}

/** Заполнить форму: сохранённым входом, анкетой или новым паролем.
 *  Сгенерированный пароль возвращается ОДИН раз — человек должен увидеть, что
 *  попало в поле, прежде чем решит сохранять. */
export async function fillBrowserForm(opts: {
  login_id?: string;
  profile?: boolean;
  new_password?: number;
  values?: Record<string, string>;
  submit?: boolean;
}): Promise<{ ok: boolean; filled: number; password?: string; skipped?: string[] }> {
  return api("/api/vbrowser/fill", { method: "POST", body: JSON.stringify(opts) });
}

/** Масштаб страницы — то, что человек делает щипком. */
export async function setBrowserScale(scale: number): Promise<{ ok: boolean }> {
  return api("/api/vbrowser/scale", { method: "POST", body: JSON.stringify({ scale }) });
}

/** Завершить перехваченный браузером <input type=file>. Сами байты уже
 * загружены на агент через uploadPtyFile; имя передаём отдельно, чтобы сайт
 * видел исходное `report.pdf`, а не транспортное `pty-upload-...`. */
export async function chooseVBrowserFiles(
  chooserId: number,
  files: Array<{ path: string; name: string }>,
): Promise<{ ok: boolean; files: number }> {
  return api("/api/vbrowser/file-chooser", {
    method: "POST",
    body: JSON.stringify({ chooser_id: chooserId, files }),
  });
}

export async function cancelVBrowserFileChooser(
  chooserId: number,
): Promise<{ ok: boolean; cancelled: boolean }> {
  return api("/api/vbrowser/file-chooser", {
    method: "POST",
    body: JSON.stringify({ chooser_id: chooserId, cancel: true }),
  });
}

/** Ответить на невидимый в видеопотоке alert/confirm/prompt страницы. */
export async function answerVBrowserDialog(
  dialogId: number,
  accept: boolean,
  promptText = "",
): Promise<{ ok: boolean }> {
  return api("/api/vbrowser/dialog", {
    method: "POST",
    body: JSON.stringify({ dialog_id: dialogId, accept, prompt_text: promptText }),
  });
}

/** Доустановить на Linux-машине инструмент ввода (xdotool): без него экран
 *  показывается, но не принимает нажатия. Ставит сам агент — человеку с
 *  телефона не нужно идти в SSH.
 *
 *  Запрос возвращается СРАЗУ, установка идёт на машине фоном: apt там занимает
 *  минуты (и ждёт чужой lock), а облачный запрос живёт 30 секунд. Ход дела —
 *  в getVBrowserStatus(): input_installing → input_ready. */
export async function installVBrowserInput(): Promise<{ ok: boolean; status?: VBrowserStatus }> {
  return api("/api/vbrowser/install-input", { method: "POST", body: "{}" });
}

// ── Bookmarks ───────────────────────────────────────────────────

export async function getBookmarks(): Promise<{ bookmarks: Bookmark[] }> {
  return api("/api/bookmarks");
}

export async function addBookmark(name: string, path: string) {
  return api("/api/bookmarks", { method: "POST", body: JSON.stringify({ name, path }) });
}

export async function removeBookmark(path: string) {
  return api("/api/bookmarks", { method: "DELETE", body: JSON.stringify({ path }) });
}

// ── Recent Folders ─────────────────────────────────────────────────

export async function getRecentFolders(): Promise<{ folders: RecentFolder[] }> {
  return api("/api/recent-folders");
}

// ── Quick Actions ───────────────────────────────────────────────

export async function cloneSession(name: string): Promise<{ name: string }> {
  return api(`/api/sessions/${encodeURIComponent(name)}/clone`, { method: "POST" });
}

export async function clearSession(name: string) {
  return api(`/api/sessions/${encodeURIComponent(name)}/clear`, { method: "POST" });
}

// ── Pinned Prompts ──────────────────────────────────────────────

export async function getPinnedPrompts(name: string): Promise<{ prompts: string[] }> {
  return api(`/api/sessions/${encodeURIComponent(name)}/prompts`);
}

export async function addPinnedPrompt(name: string, prompt: string) {
  return api(`/api/sessions/${encodeURIComponent(name)}/prompts`, {
    method: "POST", body: JSON.stringify({ prompt }),
  });
}

export async function removePinnedPrompt(name: string, prompt: string) {
  return api(`/api/sessions/${encodeURIComponent(name)}/prompts`, {
    method: "DELETE", body: JSON.stringify({ prompt }),
  });
}

// ── Templates ───────────────────────────────────────────────────

export async function getTemplates(): Promise<{ templates: SessionTemplate[] }> {
  return api("/api/templates");
}

export async function createTemplate(tmpl: SessionTemplate) {
  return api("/api/templates", { method: "POST", body: JSON.stringify(tmpl) });
}

export async function deleteTemplate(name: string) {
  return api("/api/templates", { method: "DELETE", body: JSON.stringify({ name }) });
}

export async function applyTemplate(template_name: string, session_name?: string): Promise<{ name: string }> {
  return api("/api/templates/apply", {
    method: "POST", body: JSON.stringify({ template_name, session_name }),
  });
}

// ── Multi-Send ──────────────────────────────────────────────────

export async function multiSend(sessions: string[], prompt: string): Promise<{ sent: number }> {
  return api("/api/multi-send", {
    method: "POST", body: JSON.stringify({ sessions, prompt }),
  });
}

// ── Claude History Sync ─────────────────────────────────────────

export async function getClaudeHistory(name: string): Promise<{ messages: Array<{ role: string; text: string; timestamp: number; source: string }> }> {
  return api(`/api/sessions/${encodeURIComponent(name)}/claude-history`);
}

// ── Discover ────────────────────────────────────────────────────

export async function discoverSessions(): Promise<{ sessions: DiscoveredSession[] }> {
  return api("/api/discover");
}

export async function importDiscoveredSession(session: DiscoveredSession): Promise<{ name: string; imported: boolean }> {
  return api("/api/discover/import", {
    method: "POST",
    body: JSON.stringify({
      session_id: session.session_id,
      agent_type: session.agent_type,
      cwd: session.cwd,
      name: `${session.agent_type}-${session.name}`,
    }),
  });
}

// ── Research ─────────────────────────────────────────────────────

export async function getResearch(name: string): Promise<import("./types").ResearchState> {
  return api(`/api/sessions/${encodeURIComponent(name)}/research`);
}

export async function startResearch(name: string, task: string) {
  return api(`/api/sessions/${encodeURIComponent(name)}/research`, {
    method: "POST", body: JSON.stringify({ task }),
  });
}

export async function stopResearch(name: string) {
  return api(`/api/sessions/${encodeURIComponent(name)}/research/stop`, { method: "POST" });
}

export async function saveResearchConfig(name: string, config: import("./types").ResearchConfig) {
  return api(`/api/sessions/${encodeURIComponent(name)}/research/config`, {
    method: "POST", body: JSON.stringify(config),
  });
}

// ── Stats ───────────────────────────────────────────────────────

export async function getCostStats(): Promise<CostStats> {
  return api("/api/stats");
}

// ── Agents ──────────────────────────────────────────────────────

export async function getAgents(): Promise<{ agents: AgentInfo[] }> {
	return api("/api/agents?account_proxy_contract=1");
}

export async function rescanAgents(): Promise<{ agents: AgentInfo[] }> {
	return api("/api/agents/rescan?account_proxy_contract=1", { method: "POST" });
}

// ── Config ──────────────────────────────────────────────────────

export async function getConfig(): Promise<AppConfig> {
  return api("/api/config");
}

// Значение — не только строка: булевы переключатели (detect_agent_questions,
// notifications_enabled) и числа (max_concurrent_sessions) идут тем же PATCH.
export async function updateConfig(patch: Record<string, string | boolean | number | string[]>) {
  return api("/api/config", { method: "PATCH", body: JSON.stringify(patch) });
}

// ── Presets / quick-launch tiles ────────────────────────────────

export async function getPresets(): Promise<{ presets: Preset[] }> {
  return api("/api/presets");
}

export async function launchPreset(id: string): Promise<{ preset: Preset; ok: boolean }> {
  return api(`/api/presets/launch?id=${encodeURIComponent(id)}`, { method: "POST" });
}

// ── Свои команды человека ───────────────────────────────────────
//
// Один список НА КОМПЬЮТЕР, а не по списку на пульт. Раньше их было два и оба в
// localStorage пульта, поэтому команда, заведённая с телефона, в окне exe
// отсутствовала (см. internal/web/api_commands.go).

export interface UserCommand {
  id: string;
  cmd: string;
  /** Подпись кнопки; пусто — показываем саму команду. */
  label?: string;
  /** Стоит в ряду под терминалом. Меняет и МЕСТО кнопки, и путь отправки. */
  pinned?: boolean;
  sort?: number;
}

export async function getUserCommands(): Promise<{ commands: UserCommand[] }> {
  return api("/api/commands");
}

// ── Каталог настроек компьютера ─────────────────────────────────
// Одна правда для человека и для AI-агента: что можно настроить, что это
// значит и что опасно (см. internal/web/api_settings_catalog.go). До 09.08.2026
// клиент к нему не обращался вовсе — и настройки, у которых не было своего
// экрана, человек не видел никак, хотя агент про них знал.

export interface SettingSpecDTO {
  key: string;
  title: string;
  hint: string;
  type: string;
  options?: string[];
  risk: string;
  value?: unknown;
}

export async function getSettingsCatalog(): Promise<{ settings: SettingSpecDTO[]; risk_note?: string }> {
  return api("/api/settings/catalog");
}

/**
 * Изменить настройку. Значение уходит СТРОКОЙ: ровно то же шлёт `remotai config
 * set`, и разбирает его сервер — второй разборщик на клиенте разошёлся бы с ним.
 * Опасные настройки ручка не меняет: отвечает 403 `needs_approval`.
 */
export async function applySetting(key: string, value: string): Promise<{ key: string; value: unknown; applied: boolean }> {
  return api("/api/settings/apply", { method: "POST", body: JSON.stringify({ key, value }) });
}

export async function saveUserCommand(cmd: Partial<UserCommand>): Promise<{ commands: UserCommand[]; command: UserCommand; existed?: boolean }> {
  return api("/api/commands", { method: "POST", body: JSON.stringify(cmd) });
}

export async function deleteUserCommand(id: string): Promise<{ commands: UserCommand[] }> {
  return api(`/api/commands?id=${encodeURIComponent(id)}`, { method: "DELETE" });
}

/** Разовый перенос своих команд с пульта. Повторный вызов безопасен. */
export async function importUserCommands(commands: Partial<UserCommand>[]): Promise<{ commands: UserCommand[]; added: number }> {
  return api("/api/commands/import", { method: "POST", body: JSON.stringify({ commands }) });
}

// ── Аккаунты нейросетей ─────────────────────────────────────────
// Аккаунт CLI-агента — это каталог с его кредами, поэтому вторая подписка =
// второй каталог (см. internal/web/api_accounts.go). Список живёт на машине с
// агентами: пультов несколько, машина одна.

/**
 * Аккаунты всех агентов, у каждого первым идёт основной.
 *
 * `cwd` — папка терминала, из которого спрашивают: с ней сервер учитывает
 * закрепление аккаунта за папкой, и клиенту не нужно знать эти правила вовсе
 * («активный» уже посчитан для ЭТОГО места).
 */
export async function getAgentAccounts(agentID?: string, cwd?: string): Promise<{
  accounts: AgentAccount[];
  /** Версия server-side allowlist-а proxy transport. Нет поля = старый агент. */
  proxy_contract_version?: number;
}> {
  const q = new URLSearchParams();
  if (agentID) q.set("agent_id", agentID);
  if (cwd) q.set("cwd", cwd);
  const query = q.toString();
  return api(query ? `/api/accounts?${query}` : "/api/accounts");
}

// ── OpenRouter ──────────────────────────────────────────────────────────────
// Один ключ вместо подписки на каждого вендора: 400 моделей, из них полтора
// десятка бесплатных. Ключ живёт на КОМПЬЮТЕРЕ (в окружении процесса агента) и
// наружу не отдаётся ни одним из этих вызовов — приезжает только маскированная
// метка от самого OpenRouter и цифры расхода.

/** Состояние подключения на выбранном компьютере. */
export async function getOpenRouter(): Promise<OpenRouterStatus> {
  return api("/api/openrouter");
}

/**
 * Сохранить ключ. Агент ПРОВЕРЯЕТ его до сохранения: зелёная галочка на
 * непроверенном ключе означала бы отказ через полчаса, посреди работы агента.
 */
export async function setOpenRouterKey(key: string): Promise<{
  ok: boolean; label?: string; is_free_tier?: boolean; free_daily_quota?: number;
  applies_to_new_terminals?: boolean;
}> {
  return api("/api/openrouter/key", { method: "POST", body: JSON.stringify({ key }) });
}

/** Забыть ключ — и на диске, и в окружении агента. */
export async function forgetOpenRouterKey(): Promise<{ ok: boolean }> {
  return api("/api/openrouter/key", { method: "DELETE" });
}

/** Каталог моделей; `free` — оставить только бесплатные. */
export async function getOpenRouterModels(free = false): Promise<{
  models: OpenRouterModel[]; total: number; free_total: number; selected: string;
}> {
  return api(free ? "/api/openrouter/models?free=1" : "/api/openrouter/models");
}

/** Какой моделью запускать агента. Пусто — агент решает сам. */
export async function setOpenRouterModel(model: string): Promise<{ ok: boolean; model: string }> {
  return api("/api/openrouter/model", { method: "POST", body: JSON.stringify({ model }) });
}

// ── Просьбы агента изменить настройку ───────────────────────────────────────
// Опасные настройки (порт, автозапуск, облако) AI-агент не меняет сам: он
// заводит просьбу и ждёт человека. Решение принимается ЗДЕСЬ, на экране, где
// видно, что именно меняется, — а не кнопкой в чате, который ходит через то
// самое облако, которое просят выключить.

export interface AgentSettingRequest {
  id: string;
  key: string;
  /** Название настройки словами человека. */
  title: string;
  /** Что агент просит поставить. */
  value: string;
  /** Зачем — словами агента. Человек решает по причине, а не по ключу. */
  reason?: string;
  created_at: number;
  /** pending | approved | rejected | expired */
  status: string;
  /** Разрешили, но применить не вышло — честно показываем. */
  error?: string;
}

export async function getAgentRequests(): Promise<{ requests: AgentSettingRequest[] }> {
  return api("/api/settings/requests");
}

/** Ответ человека. Подтверждение сразу и ПРИМЕНЯЕТ настройку. */
export async function answerAgentRequest(id: string, approve: boolean): Promise<{ request: AgentSettingRequest }> {
  return api("/api/settings/requests/answer", { method: "POST", body: JSON.stringify({ id, approve }) });
}

/** Закрепить аккаунт за папкой (пустой id снимает закрепление). */
export async function pinAgentAccount(agentID: string, id: string, path: string): Promise<{ pinned: boolean; path: string; account: AgentAccount }> {
  return api("/api/accounts/pin", { method: "POST", body: JSON.stringify({ agent_id: agentID, id, path }) });
}

/**
 * Запомнить, каким аккаунтом запущен агент ЭТОГО терминала.
 *
 * Зовётся в момент отправки команды: только клиент знает, что именно уходит в
 * шелл. Дальше терминал показывает это в шапке — у соседнего терминала аккаунт
 * может быть другим.
 */
export async function setPtyAccount(ptyID: string, accountID: string, label: string): Promise<{ ok: boolean }> {
  return api(`/api/pty/${encodeURIComponent(ptyID)}/account`, {
    method: "POST",
    body: JSON.stringify({ account_id: accountID, label }),
  });
}

/** Что у нового аккаунта стало общим с основным (скиллы, настройки). */
export interface AgentAccountShared {
  name: string;
  title: string;
  /** Папка связана: правишь в одном месте — работает во всех аккаунтах. */
  linked?: boolean;
  /** Файл скопирован: связь порвал бы первый же «временный файл + rename». */
  copied?: boolean;
  error?: string;
}

/**
 * Завести аккаунт (нужен agent_id + label) или переименовать (id + label).
 *
 * В ответе — честный список того, что стало общим: у второй подписки свой вход
 * и своя переписка, но скиллы и настройки те же. Обещать «всё перенесено»
 * нельзя, а молчать — значит оставить человека с агентом без скиллов.
 */
export async function saveAgentAccount(account: {
  id?: string;
  agent_id?: string;
  label?: string;
  /** Прокси аккаунта. Пустая строка — снять; НЕ передавать — не трогать. */
  proxy?: string;
}): Promise<{
  account: AgentAccount;
  shared?: AgentAccountShared[];
  mcp_servers?: number;
  /** Прокси сохранён, но сейчас не отвечает — человек должен об этом знать. */
  warning?: string;
  /** Живая проверка самого proxy endpoint прошла; transport агента проверен отдельно. */
  proxy_exit?: string;
}> {
  return api("/api/accounts", { method: "POST", body: JSON.stringify(account) });
}

/** Каким аккаунтом запускать агента дальше. На уже открытые терминалы не влияет. */
export async function activateAgentAccount(agentID: string, id: string): Promise<{ account: AgentAccount }> {
  return api("/api/accounts/activate", { method: "POST", body: JSON.stringify({ agent_id: agentID, id }) });
}

/** Забыть аккаунт вместе с его каталогом (там лежит живой токен). */
export async function deleteAgentAccount(id: string): Promise<{ ok: boolean }> {
  return api(`/api/accounts?id=${encodeURIComponent(id)}`, { method: "DELETE" });
}

// ── License & Billing ───────────────────────────────────────────

export async function getLicenseStatus(): Promise<LicenseStatus> {
  return api("/api/license");
}

export async function createCheckout(tier: string, annual: boolean, email?: string): Promise<{ url: string }> {
  return api("/api/license/checkout", {
    method: "POST",
    body: JSON.stringify({ tier, annual, email }),
  });
}

export async function getPortalURL(): Promise<{ url: string }> {
  return api("/api/license/portal", { method: "POST" });
}
