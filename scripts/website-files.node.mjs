import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import { sitemapPages } from "./sitemap-pages.mjs";
import { websiteFiles } from "./website-files.mjs";

function run(args) {
  const result = spawnSync(process.execPath, args, { encoding: "utf8" });
  assert.ifError(result.error);
  assert.equal(result.status, 0, result.stderr);
  assert.equal(result.stderr, "");
  return result.stdout;
}

for (const [module, inventory] of [
  ["sitemap-pages.mjs", sitemapPages],
  ["website-files.mjs", websiteFiles],
]) {
  const url = new URL(module, import.meta.url);
  test(`${module} CLI emits each deployable path exactly once`, () => {
    const output = run([fileURLToPath(url), "--list"]);
    const rows = output.trim().split(/\r?\n/);
    assert.deepEqual(rows, inventory().map(file => `${file.remote}\t${file.local}`));
    assert.equal(new Set(rows.map(row => row.split("\t")[0])).size, rows.length);
  });

  for (const flag of ["--list", "--check"]) {
    test(`importing ${module} does not run its CLI for ${flag}`, () => {
      const code = `await import(${JSON.stringify(url.href)}); process.stdout.write("import completed");`;
      assert.equal(run(["--input-type=module", "-e", code, "fixture", flag]), "import completed");
    });
  }
}
