export const SETTINGS_SECTIONS = ["computer", "local", "notifications", "connection", "account", "help", "advanced"] as const;
export type SettingsSection = typeof SETTINGS_SECTIONS[number];

/** Unknown bookmarks fall back to the index; old account links still open logins. */
export function settingsSection(search: string, focus?: string): SettingsSection | null {
  const value = new URLSearchParams(search).get("section");
  if (SETTINGS_SECTIONS.some(section => section === value)) return value as SettingsSection;
  return focus === "logins" ? "account" : null;
}

export function settingsPath(section: SettingsSection): string {
  return `/settings?section=${section}`;
}
