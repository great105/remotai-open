// Скиллы агентов: типы, вызовы API и правила раздела «Скиллы» на экране
// «Агенты». Правила — чистые функции без React (проверяются skills.test.ts).
//
// Сервер: internal/web/api_skills.go + internal/skillsmgr. Удаление там не
// удаляет, а переносит в резервную копию — отсюда «Вернуть».

import { transport } from "./api-transport";

export interface Skill {
  name: string;
  title?: string;
  description?: string;
  path: string;
  /** Папка скилла — ссылка на другое место (удаление уносит только ссылку). */
  link?: boolean;
  link_target?: string;
  /** Встроенный или из плагина: не удаляется и не заменяется. */
  readonly?: boolean;
  /** "" | "system" | имя плагина. */
  source?: string;
}

export interface SkillLocation {
  id: string;
  agent: string;
  agent_name: string;
  label?: string;
  kind: "main" | "account" | "plugins";
  dir: string;
  exists: boolean;
  readonly?: boolean;
  /** Аккаунты, чья папка skills связана с этой. */
  shared_with?: string[];
  skills: Skill[];
}

export interface SkillBackup {
  id: string;
  location: string;
  agent: string;
  label?: string;
  name: string;
  reason: "delete" | "replace";
  from: string;
  at: number;
  link?: boolean;
  description?: string;
}

export interface SkillsState {
  locations: SkillLocation[];
  backups: SkillBackup[];
}

export interface FoundSkill {
  name: string;
  title?: string;
  description?: string;
  files: number;
  bytes: number;
  /** Почему поставить нельзя. */
  skip?: string;
}

export type SkillOpStatus = "installed" | "replaced" | "exists" | "same" | "error";

export interface SkillOpResult {
  location: string;
  name: string;
  status: SkillOpStatus;
  error?: string;
  backup?: string;
}

export interface SkillInstallReport {
  found: FoundSkill[];
  results?: SkillOpResult[];
}

export interface SkillInstallOptions {
  targets: string[];
  replace?: boolean;
  dryRun?: boolean;
  only?: string[];
}

// ── API ──────────────────────────────────────────────────────────────────────

function api<T>(path: string, init?: RequestInit): Promise<T> {
  return transport().request<T>(path, init);
}

export async function getSkills(): Promise<SkillsState> {
  const res = await api<Partial<SkillsState>>("/api/skills");
  return { locations: res.locations ?? [], backups: res.backups ?? [] };
}

export function installSkillFromGitHub(url: string, o: SkillInstallOptions): Promise<SkillInstallReport> {
  return api("/api/skills/install", {
    method: "POST",
    body: JSON.stringify({ url, targets: o.targets, replace: !!o.replace, dry_run: !!o.dryRun, only: o.only ?? [] }),
  });
}

/** ZIP идёт multipart-частью `file`, параметры — в query: так устроены все
 *  загрузки клиента, и облачный путь передаёт query как есть. */
export function installSkillFromZip(file: File, o: SkillInstallOptions): Promise<SkillInstallReport> {
  const form = new FormData();
  form.append("file", file, file.name);
  const q = new URLSearchParams();
  if (o.targets.length) q.set("targets", o.targets.join(","));
  if (o.replace) q.set("replace", "1");
  if (o.dryRun) q.set("dry_run", "1");
  if (o.only?.length) q.set("only", o.only.join(","));
  return transport().uploadForm<SkillInstallReport>(`/api/skills/install?${q}`, form);
}

export function copySkill(name: string, from: string, to: string[], replace = false): Promise<{ results: SkillOpResult[] }> {
  return api("/api/skills/copy", { method: "POST", body: JSON.stringify({ name, from, to, replace }) });
}

export function deleteSkill(location: string, name: string): Promise<{ ok: boolean; backup: SkillBackup }> {
  const q = new URLSearchParams({ location, name });
  return api(`/api/skills?${q}`, { method: "DELETE" });
}

export function restoreSkill(b: Pick<SkillBackup, "id" | "location" | "name">, replace = false): Promise<{ ok: boolean; result: SkillOpResult }> {
  return api("/api/skills/restore", {
    method: "POST",
    body: JSON.stringify({ id: b.id, location: b.location, name: b.name, replace }),
  });
}

// ── Правила ──────────────────────────────────────────────────────────────────

/** Самый большой ZIP, который клиент отправит. 12 МБ, а не серверные 20:
 *  через облако больше одного куска загрузки этот маршрут не принимает. */
export const SKILL_ZIP_MAX_BYTES = 12 * 1024 * 1024;

const GH_PART = /^[A-Za-z0-9_.-]{1,100}$/;

/** Похоже ли на ссылку GitHub, которую примет компьютер: репозиторий или
 *  папка в нём (`/tree/<ветка>/…`, `/blob/<ветка>/…`). Строгая проверка — на
 *  сервере; здесь только чтобы кнопка не отправляла очевидную ошибку. */
export function looksLikeGitHubUrl(raw: string): boolean {
  let s = raw.trim();
  if (!s || /\s/.test(s)) return false;
  if (!/^[a-z]+:\/\//i.test(s)) s = `https://${s}`;
  let u: URL;
  try { u = new URL(s); } catch { return false; }
  if (!/^https?:$/.test(u.protocol)) return false;
  if (!/^(www\.)?github\.com$/i.test(u.hostname)) return false;
  const parts = u.pathname.split("/").filter(Boolean);
  if (parts.length < 2 || !GH_PART.test(parts[0]) || !GH_PART.test(parts[1])) return false;
  if (parts.length === 2) return true;
  return (parts[2] === "tree" || parts[2] === "blob") && parts.length >= 4 && !parts.includes("..");
}

/** Имя места: «Claude Code» или «Claude Code · рабочий». Плагины — отдельно. */
export function skillLocationTitle(loc: Pick<SkillLocation, "agent_name" | "label" | "kind">, pluginsWord: string): string {
  if (loc.kind === "plugins") return `${loc.agent_name} · ${pluginsWord}`;
  return loc.label ? `${loc.agent_name} · ${loc.label}` : loc.agent_name;
}

/** Места, куда можно ставить и копировать. */
export function writableLocations(locs: SkillLocation[]): SkillLocation[] {
  return locs.filter((l) => l.kind !== "plugins" && !l.readonly);
}

/** Число «своих» скиллов места — без встроенных и плагинов. */
export function ownSkillCount(loc: SkillLocation): number {
  return loc.skills.filter((s) => !s.readonly).length;
}

export interface CopyTarget {
  location: SkillLocation;
  /** Скилл с таким именем там уже стоит. */
  has: boolean;
}

/** Куда можно скопировать скилл: все записываемые места, кроме исходного. */
export function copyTargets(locs: SkillLocation[], fromId: string, name: string): CopyTarget[] {
  return writableLocations(locs)
    .filter((l) => l.id !== fromId)
    .map((l) => ({ location: l, has: l.skills.some((s) => s.name === name) }));
}

/** Какие из выбранных к установке уже стоят в выбранных местах. */
export function installConflicts(locs: SkillLocation[], targets: string[], names: string[]): string[] {
  const out = new Set<string>();
  for (const l of locs) {
    if (!targets.includes(l.id)) continue;
    for (const n of names) if (l.skills.some((s) => s.name === n)) out.add(n);
  }
  return [...out];
}

/** Места, отмеченные для установки по умолчанию: основной профиль первого
 *  агента (Claude Code, если он есть). Ставить во все сразу без спроса — нет:
 *  у каждого агента свой набор, и лишний скилл засоряет его выбор. */
export function defaultInstallTargets(locs: SkillLocation[]): string[] {
  const first = writableLocations(locs).find((l) => l.kind === "main");
  return first ? [first.id] : [];
}

export interface SkillOpSummary {
  installed: number;
  replaced: number;
  exists: number;
  same: number;
  errors: string[];
}

export function summarizeResults(results: SkillOpResult[] | undefined): SkillOpSummary {
  const s: SkillOpSummary = { installed: 0, replaced: 0, exists: 0, same: 0, errors: [] };
  for (const r of results ?? []) {
    if (r.status === "installed") s.installed++;
    else if (r.status === "replaced") s.replaced++;
    else if (r.status === "exists") s.exists++;
    else if (r.status === "same") s.same++;
    else s.errors.push(r.error || r.name);
  }
  return s;
}

/** Поиск по имени, названию и описанию; регистр не важен. */
export function filterSkills(skills: Skill[], query: string): Skill[] {
  const q = query.trim().toLowerCase();
  if (!q) return skills;
  return skills.filter((s) =>
    s.name.toLowerCase().includes(q)
    || (s.title ?? "").toLowerCase().includes(q)
    || (s.description ?? "").toLowerCase().includes(q)
    || (s.source ?? "").toLowerCase().includes(q));
}

/** Свежие резервные копии: только удалённое (заменённое вернуть можно, но
 *  это другой разговор) и не больше limit штук. */
export function recentDeleted(backups: SkillBackup[], limit = 10): SkillBackup[] {
  return backups.filter((b) => b.reason === "delete").slice(0, limit);
}

/** Ключ i18n для кода ошибки сервера (или null — показать текст сервера). */
export function skillErrorKey(code: string | undefined): string | null {
  switch (code) {
    case "bad_url": return "skills.err.badUrl";
    case "not_found": return "skills.err.notFound";
    case "download_failed": return "skills.err.download";
    case "too_large": return "skills.err.tooLarge";
    case "bad_archive": return "skills.err.badArchive";
    case "unsafe_archive": return "skills.err.unsafe";
    case "no_skills": return "skills.err.noSkills";
    case "exists": return "skills.err.exists";
    case "readonly": return "skills.err.readonly";
    default: return null;
  }
}
