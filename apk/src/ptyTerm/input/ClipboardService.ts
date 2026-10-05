import { Capacitor } from "@capacitor/core";
import { Clipboard } from "@capacitor/clipboard";

export interface ClipboardAdapter { write(text: string): Promise<void>; read(): Promise<string> }

export function legacyCopy(text: string, doc: Document = document): boolean {
  const active = doc.activeElement as HTMLElement | null;
  const selection = doc.getSelection();
  const ranges = selection ? Array.from({ length: selection.rangeCount }, (_, i) => selection.getRangeAt(i).cloneRange()) : [];
  const field = doc.createElement("textarea");
  field.value = text;
  field.readOnly = true;
  field.style.cssText = "position:fixed;opacity:0;left:0;top:0;pointer-events:none";
  try {
    doc.body.appendChild(field);
    field.select();
    return doc.execCommand("copy") === true;
  } catch { return false; }
  finally {
    field.remove();
    active?.focus({ preventScroll: true });
    if (selection) { selection.removeAllRanges(); ranges.forEach(r => selection.addRange(r)); }
  }
}

export function createClipboardService(adapter: ClipboardAdapter, fallback?: (text: string) => boolean) {
  return {
    async write(text: string): Promise<boolean> {
      try { await adapter.write(text); return true; }
      catch { try { return fallback?.(text) === true; } catch { return false; } }
    },
    read: () => adapter.read(),
  };
}

export const terminalClipboard = createClipboardService({
  async write(text) {
    if (Capacitor.isNativePlatform()) await Clipboard.write({ string: text });
    else await navigator.clipboard.writeText(text);
  },
  async read() {
    if (!Capacitor.isNativePlatform()) return navigator.clipboard.readText();
    const result = await Clipboard.read();
    return result.type === "text/plain" || result.type === "text" ? result.value : "";
  },
}, text => !Capacitor.isNativePlatform() && legacyCopy(text));
