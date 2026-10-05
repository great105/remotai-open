import { normalizeScrollOverride, type ScrollOverride } from "./sessionRuntime";

// A terminal ID alone is not unique across connected computers. JSON keeps
// contexts/IDs containing separators from colliding with another pair.
const key = (context: string, id: string) => `pty.scrollMode.v1:${JSON.stringify([context, id])}`;

/** New and legacy terminals start on Auto; only a person's choice is saved. */
export function readScrollPreference(
  context: string,
  id: string,
  storage?: Pick<Storage, "getItem">,
): ScrollOverride {
  if (!context || !id) return "auto";
  try {
    return normalizeScrollOverride((storage ?? globalThis.localStorage).getItem(key(context, id)) ?? undefined);
  } catch { return "auto"; }
}

export function saveScrollPreference(
  context: string,
  id: string,
  value: ScrollOverride,
  storage?: Pick<Storage, "setItem">,
): void {
  if (!context || !id) return;
  try {
    (storage ?? globalThis.localStorage).setItem(key(context, id), value);
  } catch { /* Storage may be unavailable; the current view still works. */ }
}
