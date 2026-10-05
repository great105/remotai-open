/** A link from the installed app only fills the form; it never confirms pairing. */
export function installedPairCode(path: string): string {
  const [route, query] = path.split("?");
  if (route !== "/infrastructure") return "";
  const params = new URLSearchParams(query);
  if (params.get("add") !== "1") return "";
  const code = (params.get("code") || "").trim().toUpperCase();
  if (!/^[A-Z0-9]{4}-?[A-Z0-9]{4}$/.test(code)) return "";
  const compact = code.replace("-", "");
  return `${compact.slice(0, 4)}-${compact.slice(4)}`;
}
