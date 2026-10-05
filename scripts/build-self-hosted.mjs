import { spawn } from "node:child_process";
import { copyFileSync, readdirSync, mkdirSync, existsSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const root = path.dirname(path.dirname(fileURLToPath(import.meta.url)));
const relay = process.argv[2];
if (!relay) { console.error("Usage: node scripts/build-self-hosted.mjs https://remotai.example.com"); process.exit(2); }
const parsed = new URL(relay);
if (!["http:", "https:"].includes(parsed.protocol) || parsed.username || parsed.password || parsed.pathname !== "/" || parsed.search || parsed.hash) {
  throw new Error("Relay must be an HTTP(S) origin.");
}
const env = { ...process.env, VITE_BASE: "/miniapp/", VITE_RELAY_BASE: parsed.origin };
async function run(command, args) {
  await new Promise((resolve, reject) => {
    const child = spawn(command, args, { cwd: root, env, stdio: "inherit" });
    child.once("error", reject);
    child.once("exit", (code, signal) => code === 0 ? resolve() : reject(new Error(`${command} failed: exit=${code}, signal=${signal}`)));
  });
}
if (process.platform === "win32") await run("cmd.exe", ["/d", "/s", "/c", "npm run build --workspace=tgcontrol-apk"]);
else await run("npm", ["run", "build", "--workspace=tgcontrol-apk"]);
// Copy generated assets without fs.cpSync: Node 22 on Windows can abort inside
// its native recursive copy when this workspace has a Cyrillic path.
function copyTree(source, target) {
  mkdirSync(target, { recursive: true });
  for (const item of readdirSync(source, { withFileTypes: true })) {
    const from = path.join(source, item.name);
    const to = path.join(target, item.name);
    if (item.isDirectory()) copyTree(from, to);
    else if (item.isFile()) copyFileSync(from, to);
    else throw new Error(`Unsupported generated asset: ${item.name}`);
  }
}
const embed = path.join(root, "internal/web/miniapp_dist");
copyTree(path.join(root, "apk/dist"), embed);
mkdirSync(path.join(root, "build"), { recursive: true });
const binary = process.platform === "win32" ? "build/remotai-self-hosted.exe" : "build/remotai-self-hosted";
await run("go", ["build", "-trimpath", "-ldflags", "-s -w -X tgcontrol/internal/version.Version=self-hosted", "-o", binary, "./cmd/tgcontrol"]);
if (!existsSync(path.join(root, binary))) throw new Error("Agent binary was not created.");
console.log(`Built ${binary}. Pair it with: ${binary} pair --relay ${parsed.origin}`);
