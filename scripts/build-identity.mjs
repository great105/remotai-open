/**
 * Build identity (ST-00, T-40): какой именно код и какие зависимости стоят за
 * сборкой или результатом проверки.
 *
 * План требует, чтобы разработчик и тестировщик проверяли ОДНУ И ТУ ЖЕ сборку,
 * а каждый результат нёс build identity. Версии в четырёх местах совпадают у
 * нескольких сборок (стенд, повторная публикация, локальная), поэтому здесь
 * полный commit, признак грязного дерева, хеши lockfile и версии участников:
 * node, go, xterm и его аддонов, npm-обёртки, CHANGELOG и Android; версия
 * агента в терминах -ldflags (agent) и значения по умолчанию terminalFeatures()
 * клиента (terminalFeatures — разбором литерала, без исполнения TS).
 * Если собран apk/dist — ещё и хеши index.html и JS-чанков: так видно, что
 * проверялся тот самый бандл, а не соседний.
 *
 * Только чтение: ничего не собирает и ничего не меняет в дереве.
 *
 * Запуск: node scripts/build-identity.mjs [--out <файл>]
 */
import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import { existsSync, readdirSync, readFileSync, statSync, writeFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");

/** Вывод команды или null: отсутствие go или git не валит отчёт, а видно в нём. */
function run(cmd, args) {
  try {
    return execFileSync(cmd, args, { cwd: root, encoding: "utf8", stdio: ["ignore", "pipe", "ignore"] }).trim();
  } catch {
    return null;
  }
}

function sha256(file) {
  return createHash("sha256").update(readFileSync(file)).digest("hex");
}

function fileHash(rel) {
  const file = path.join(root, rel);
  return existsSync(file) ? sha256(file) : null;
}

function readText(rel) {
  const file = path.join(root, rel);
  return existsSync(file) ? readFileSync(file, "utf8") : null;
}

/** Версия пакета по первому существующему месту: npm не всегда хоистит в корень. */
function packageVersion(name, places) {
  for (const place of places) {
    const rel = `${place}/node_modules/${name}/package.json`.replace(/^\.\//, "");
    const text = readText(rel);
    if (text === null) continue;
    try {
      return { version: JSON.parse(text).version ?? null, from: rel };
    } catch {
      return { version: null, from: rel };
    }
  }
  return null;
}

function changelogTop() {
  const text = readText("CHANGELOG.md");
  if (text === null) return null;
  const match = text.match(/^## \[([^\]]+)\](?:\s*-\s*(\S+))?/m);
  return match ? { version: match[1], date: match[2] ?? null } : null;
}

function androidVersion() {
  const text = readText("apk/android/app/build.gradle");
  if (text === null) return null;
  const code = text.match(/versionCode\s+(\d+)/);
  const name = text.match(/versionName\s+"([^"]+)"/);
  return { versionCode: code ? Number(code[1]) : null, versionName: name ? name[1] : null };
}

function npmWrapperVersion() {
  const text = readText("npm/remotai/package.json");
  if (text === null) return null;
  try { return JSON.parse(text).version ?? null; } catch { return null; }
}

function distIdentity() {
  const dist = path.join(root, "apk/dist");
  if (!existsSync(dist)) return null;
  const index = path.join(dist, "index.html");
  const assetsDir = path.join(dist, "assets");
  const assets = existsSync(assetsDir)
    ? readdirSync(assetsDir)
      .filter(name => name.endsWith(".js"))
      .sort()
      .map(name => {
        const file = path.join(assetsDir, name);
        return { name, bytes: statSync(file).size, sha256: sha256(file) };
      })
    : [];
  return { indexHtml: existsSync(index) ? sha256(index) : null, assets };
}

/**
 * Версия агента так, как её ставят -ldflags в scripts/publish-release.ps1:
 * `-X tgcontrol/internal/version.Version=<версия> -X
 * tgcontrol/internal/version.Commit=<git rev-parse --short HEAD>`. Локальное
 * дерево — не выпуск: версия — значение по умолчанию из version.go («dev»),
 * commit — HEAD (короткий, как в ldflags; полный — в git.commit).
 */
function agentIdentity() {
  const rel = "internal/version/version.go";
  const text = readText(rel);
  const match = text === null ? null : text.match(/^\s*Version\s*=\s*"([^"\\]*)"\s*$/m);
  const version = match ? match[1] : null;
  const commit = run("git", ["rev-parse", "--short", "HEAD"]);
  let reason = null;
  if (text === null) reason = `нет ${rel}`;
  else if (version === null) reason = `в ${rel} не найден литерал Version = "…"`;
  else if (commit === null) reason = "git недоступен";
  return {
    source: "local-tree",
    version,
    commit,
    ldflags: version !== null && commit !== null
      ? `-X tgcontrol/internal/version.Version=${version} -X tgcontrol/internal/version.Commit=${commit}`
      : null,
    reason,
  };
}

const FEATURES_FILE = "apk/src/ptyTerm/runtime/TerminalControls.ts";

/**
 * Значения по умолчанию terminalFeatures() — разбором литерала defaultFeatures
 * в TerminalControls.ts, без исполнения TS. Разбор строгий: литерал обязан
 * быть плоским `return { имя: true|false|"строка", … };`, а набор полей —
 * совпадать с interface TerminalFeatures (boolean-поля — только true/false).
 * Что-то не так — defaults: null и причина, а не догадка. На устройстве
 * значения может переопределить localStorage (storageKey).
 */
function terminalFeatureDefaults() {
  const text = readText(FEATURES_FILE);
  const out = { source: FEATURES_FILE, sha256: fileHash(FEATURES_FILE), storageKey: null, defaults: null, reason: null };
  const fail = reason => ({ ...out, defaults: null, reason });
  if (text === null) return fail(`нет ${FEATURES_FILE}`);
  const key = text.match(/^const FEATURES_KEY = "([^"\\]+)";\s*$/m);
  out.storageKey = key ? key[1] : null;

  const literal = text.match(/function defaultFeatures\(\): TerminalFeatures \{\s*return \{([^{}]*)\};\s*\}/);
  if (!literal) return fail("не найден литерал `function defaultFeatures(): TerminalFeatures { return { … }; }`");
  const defaults = {};
  for (const raw of literal[1].split(",")) {
    const entry = raw.trim();
    if (entry === "") continue; // висящая запятая
    const m = entry.match(/^([A-Za-z_$][\w$]*)\s*:\s*(true|false|"[^"\\]*"|'[^'\\]*')$/);
    if (!m) return fail(`нераспознанное поле литерала defaultFeatures: ${JSON.stringify(entry.slice(0, 80))}`);
    if (Object.hasOwn(defaults, m[1])) return fail(`поле ${m[1]} в литерале defaultFeatures дважды`);
    defaults[m[1]] = m[2] === "true" ? true : m[2] === "false" ? false : m[2].slice(1, -1);
  }

  const iface = text.match(/export interface TerminalFeatures \{\r?\n([\s\S]*?)\r?\n\}/);
  if (!iface) return fail("не найден `export interface TerminalFeatures { … }`");
  const body = iface[1].replace(/\/\*[\s\S]*?\*\//g, "").replace(/\/\/[^\n]*/g, "");
  const types = {};
  for (const m of body.matchAll(/^\s*([A-Za-z_$][\w$]*)\??\s*:\s*([^;]+);/gm)) types[m[1]] = m[2].trim();
  const missing = Object.keys(types).filter(k => !Object.hasOwn(defaults, k));
  const extra = Object.keys(defaults).filter(k => !Object.hasOwn(types, k));
  if (missing.length || extra.length) {
    return fail(`литерал defaultFeatures и interface TerminalFeatures расходятся: нет [${missing.join(", ")}], лишние [${extra.join(", ")}]`);
  }
  const badType = Object.keys(types).filter(k => (types[k] === "boolean") !== (typeof defaults[k] === "boolean"));
  if (badType.length) return fail(`тип значения по умолчанию не совпадает с interface: ${badType.join(", ")}`);
  return { ...out, defaults };
}

function gitIdentity() {
  const commit = run("git", ["rev-parse", "HEAD"]);
  const branch = run("git", ["rev-parse", "--abbrev-ref", "HEAD"]);
  const status = run("git", ["status", "--porcelain"]);
  const changed = status === null ? null : status.split("\n").filter(Boolean).length;
  return { commit, branch, dirty: changed === null ? null : changed > 0, changedPaths: changed };
}

export function buildIdentity() {
  return {
    format: "remotai-build-identity",
    v: 1,
    generatedAt: new Date().toISOString(),
    git: gitIdentity(),
    lockfiles: {
      "package-lock.json": fileHash("package-lock.json"),
      "go.sum": fileHash("go.sum"),
      "apk/package.json": fileHash("apk/package.json"),
    },
    tools: {
      node: process.version,
      go: run("go", ["version"]),
    },
    packages: {
      "@xterm/xterm": packageVersion("@xterm/xterm", ["apk", "."]),
      "@xterm/headless": packageVersion("@xterm/headless", [".", "apk"]),
      "@xterm/addon-webgl": packageVersion("@xterm/addon-webgl", [".", "apk"]),
    },
    versions: {
      npmWrapper: npmWrapperVersion(),
      changelog: changelogTop(),
      android: androidVersion(),
    },
    agent: agentIdentity(),
    terminalFeatures: terminalFeatureDefaults(),
    dist: distIdentity(),
  };
}

// CLI только при прямом запуске: проба, импортирующая buildIdentity(), не
// должна получить чужой stdout, перезапись своего --out или exit(2).
const direct = process.argv[1] && import.meta.url === pathToFileURL(path.resolve(process.argv[1])).href;
if (direct) {
  const args = process.argv.slice(2);
  const outAt = args.indexOf("--out");
  if (outAt >= 0 && !args[outAt + 1]) {
    console.error("usage: node scripts/build-identity.mjs [--out <файл>]");
    process.exit(2);
  }
  const json = JSON.stringify(buildIdentity(), null, 2);
  if (outAt >= 0) writeFileSync(path.resolve(process.cwd(), args[outAt + 1]), json + "\n");
  console.log(json);
}
