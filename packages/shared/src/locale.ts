import { useSyncExternalStore } from "react";

export type Language = "ru" | "en";
export const LANGUAGE_STORAGE_KEY = "remotai.language";
const listeners = new Set<() => void>();

function supported(value: unknown): value is Language {
  return value === "ru" || value === "en";
}

export function initialLanguage(input: { saved?: string | null; query?: string | null; browser?: string }): Language {
  if (supported(input.query)) return input.query;
  if (supported(input.saved)) return input.saved;
  return input.browser?.toLowerCase().startsWith("ru") ? "ru" : input.browser ? "en" : "ru";
}

function browserLanguage(): Language {
  if (typeof window === "undefined") return "ru";
  let saved: string | null = null;
  try { saved = window.localStorage.getItem(LANGUAGE_STORAGE_KEY); } catch { /* Browser storage can be unavailable. */ }
  const query = new URLSearchParams(window.location?.search ?? "").get("lang");
  if (supported(query)) {
    try { window.localStorage.setItem(LANGUAGE_STORAGE_KEY, query); } catch { /* The URL still selects this language. */ }
  }
  return initialLanguage({
    saved,
    query,
    browser: window.navigator?.language,
  });
}

let language = browserLanguage();

function updateDocument(): void {
  if (typeof document !== "undefined" && document.documentElement) {
    document.documentElement.lang = language;
    const manifest = document.querySelector?.<HTMLLinkElement>('link[rel="manifest"]');
    if (manifest) manifest.href = manifest.href.replace(/manifest(?:\.en)?\.webmanifest$/, language === "en" ? "manifest.en.webmanifest" : "manifest.webmanifest");
  }
}

function syncQueryLanguage(next: Language): void {
  if (typeof window !== "undefined" && window.location?.href && typeof window.history?.replaceState === "function") {
    const url = new URL(window.location.href);
    if (url.searchParams.has("lang")) {
      url.searchParams.set("lang", next);
      window.history.replaceState(window.history.state, "", url);
    }
  }
}

export function getLanguage(): Language { return language; }
export function getLocale(): string { return language === "ru" ? "ru-RU" : "en-US"; }

export function setLanguage(next: Language): void {
  if (!supported(next)) return;
  if (typeof window !== "undefined") {
    try { window.localStorage.setItem(LANGUAGE_STORAGE_KEY, next); } catch { /* Keep the in-memory preference. */ }
    // An explicit landing link must not override a later manual choice on reload.
    syncQueryLanguage(next);
  }
  if (language === next) { updateDocument(); return; }
  language = next;
  updateDocument();
  for (const listener of listeners) listener();
}

export function subscribeLanguage(listener: () => void): () => void {
  listeners.add(listener);
  return () => { listeners.delete(listener); };
}

/** Re-render translated UI without remounting terminals or losing input. */
export function useLanguage(): Language {
  return useSyncExternalStore(subscribeLanguage, getLanguage, () => "ru");
}

updateDocument();
if (typeof window !== "undefined" && typeof window.addEventListener === "function") {
  window.addEventListener("storage", (event) => {
    if (event.key !== LANGUAGE_STORAGE_KEY || !supported(event.newValue) || event.newValue === language) return;
    language = event.newValue;
    syncQueryLanguage(language);
    updateDocument();
    for (const listener of listeners) listener();
  });
}
