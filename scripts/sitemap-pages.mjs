/**
 * Какие страницы объявлены в карте сайта и где они лежат локально.
 *
 * Зачем отдельный модуль. 09.09.2026 страница `/claude-code.html` попала в
 * репозиторий, в `sitemap.xml`, в подвал главной и в `llms.txt`, прошла все
 * гейты — и на бою отдала 404. Причина: оба публикатора заливают ЖЁСТКИЙ
 * СПИСОК файлов (index.html, privacy, robots, sitemap, llms, og + превью), а
 * `server.html`, `uslugi.html` и блог попали на сайт когда-то руками. То есть
 * карта сайта обещала поисковику страницу, которой нет, — худший вид ошибки
 * для индексации, и заметить его можно было только на бою.
 *
 * Теперь список берётся ИЗ КАРТЫ САЙТА: она и есть объявленная поверхность.
 * Добавил страницу в sitemap — она обязана существовать локально и уехать на
 * бой. Проверку делает `check-landing-seo.mjs`, публикацию — оба скрипта.
 */
import { existsSync } from "node:fs";
import { readFileSync } from "node:fs";
import { resolve, dirname } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

const root = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const WWW = "tgcontrol-relay/deploy/www/";

/** Адрес на бою → локальный файл. Исключения названы явно, а не угаданы. */
export function localFor(pathname) {
  if (pathname === "/") return WWW + "index.html";
  // Две страницы живут в docs, а не в www, — так было до этого модуля, и
  // менять раскладку файлов ради стройности карты не стоит.
  if (pathname === "/privacy") return "docs/privacy-policy.html";
  if (pathname === "/offer.html") return "docs/offer.html";
  const clean = pathname.replace(/^\//, "");
  if (clean.endsWith("/")) return WWW + clean + "index.html";
  return WWW + clean;
}

/** Страницы из sitemap.xml: [{ url, pathname, local, remote }]. */
export function sitemapPages() {
  const xml = readFileSync(resolve(root, WWW + "sitemap.xml"), "utf8");
  return [...xml.matchAll(/<loc>(https:\/\/remotai\.ru([^<]*))<\/loc>/g)].map(([, url, pathname]) => ({
    url,
    pathname: pathname || "/",
    local: localFor(pathname || "/"),
    // На сервере файл лежит по тому же пути, что в адресе; корень — index.html.
    remote: (pathname || "/") === "/" ? "index.html" : (pathname || "/").replace(/^\//, "").replace(/\/$/, "/index.html"),
  }));
}

/** Чего не хватает на диске — то, что на бою станет 404. */
export function missingPages() {
  return sitemapPages().filter((page) => !existsSync(resolve(root, page.local)));
}

// Imports must not consume the publisher's CLI flags or print a second inventory.
const isMain = process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href;
if (isMain && process.argv[2] === "--list") {
  for (const page of sitemapPages()) console.log(`${page.remote}\t${page.local}`);
} else if (isMain && process.argv[2] === "--check") {
  const missing = missingPages();
  if (missing.length) {
    console.error("в карте сайта объявлены страницы без локального файла:");
    for (const page of missing) console.error(`  ${page.url} → ${page.local}`);
    process.exit(1);
  }
  console.log(`страниц в карте сайта: ${sitemapPages().length}, все на месте`);
}
