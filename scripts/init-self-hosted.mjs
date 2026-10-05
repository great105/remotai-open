import { randomBytes } from "node:crypto";
import { writeFileSync, existsSync } from "node:fs";
import { fileURLToPath } from "node:url";
import path from "node:path";

const root = path.dirname(path.dirname(fileURLToPath(import.meta.url)));
const target = path.join(root, "deploy/self-hosted/.env");
const raw = process.argv[2];
if (!raw) {
  console.error("Usage: node scripts/init-self-hosted.mjs https://remotai.example.com");
  process.exit(2);
}
const url = new URL(raw);
const local = ["localhost", "127.0.0.1", "[::1]"].includes(url.hostname);
if (url.username || url.password || url.pathname !== "/" || url.search || url.hash ||
    !["http:", "https:"].includes(url.protocol) || (!local && url.protocol !== "https:")) {
  throw new Error("Use an HTTPS origin, or an HTTP localhost origin for a local trial.");
}
if (!local && url.port) throw new Error("Public deployment uses the standard HTTPS port 443.");
if (existsSync(target)) throw new Error("Configuration already exists; it was preserved. Edit deploy/self-hosted/.env to change it.");
const lines = [
  `PUBLIC_URL=${url.origin}`,
  `SITE_ADDRESS=${url.protocol}//${url.hostname}`,
  `JWT_HMAC_SECRET=${randomBytes(32).toString("hex")}`,
  `HTTP_BIND=${local ? "127.0.0.1" : "0.0.0.0"}`,
  `HTTP_PORT=${local ? (url.protocol === "http:" ? url.port || "80" : "18080") : "80"}`,
  `HTTPS_PORT=${local ? (url.protocol === "https:" ? url.port || "443" : "18443") : "443"}`,
  "BOT_TOKEN=", "BOT_USERNAME=", "SMTP_HOST=", "SMTP_PORT=587", "SMTP_USER=", "SMTP_PASS=", "SMTP_FROM=",
];
writeFileSync(target, lines.join("\n") + "\n", { flag: "wx", mode: 0o600 });
console.log("Created deploy/self-hosted/.env with a new private signing secret.");
console.log("Run: docker compose --env-file deploy/self-hosted/.env -f deploy/self-hosted/compose.yaml up -d --build");
console.log(`Open: ${url.origin}/app/`);
