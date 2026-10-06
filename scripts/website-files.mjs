// Both publishers use the same complete static surface, including translations.
import { existsSync, readFileSync } from "node:fs";
import { resolve } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import { sitemapPages } from "./sitemap-pages.mjs";
const root = fileURLToPath(new URL("../", import.meta.url));
const www = "tgcontrol-relay/deploy/www/";
export function websiteFiles() {
  const files = new Map(sitemapPages().map(p => [p.remote, { remote: p.remote, local: p.local }]));
  for (const name of ["robots.txt", "sitemap.xml", "llms.txt", "og.png", "og-en.png", "blog/blog.css", "blog/rss.xml", "en/blog/rss.xml", "status.html"]) {
    files.set(name, { remote: name, local: www + name });
  }
  for (const page of sitemapPages()) {
    const html = readFileSync(resolve(root, page.local), "utf8");
    for (const [, name] of html.matchAll(/src="\/(preview-[a-z0-9-]+\.png)"/g)) files.set(name, { remote: name, local: www + name });
  }
  for (const file of files.values()) {
    if (!/^[A-Za-z0-9._/-]+$/.test(file.remote) || file.remote.split("/").some(p => !p || p === "." || p === "..")) throw new Error(`Unsafe website path: ${file.remote}`);
    if (!existsSync(resolve(root, file.local))) throw new Error(`Missing website file: ${file.local}`);
  }
  return [...files.values()];
}
const isMain = process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href;
if (isMain && process.argv[2] === "--list") for (const f of websiteFiles()) console.log(`${f.remote}\t${f.local}`);
