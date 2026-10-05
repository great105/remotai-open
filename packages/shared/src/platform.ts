// Платформенный реестр: единственная точка, через которую общие компоненты
// получают платформенные возможности (haptic, диалоги, системная «Назад»).
// Каждое приложение регистрирует свою реализацию в main.tsx через setPlatform():
//   apk     — Capacitor (Haptics, DialogHost, Android back);
//   miniapp — Telegram WebApp (HapticFeedback, showConfirm).
// Дефолты безопасны (no-op / window.confirm), поэтому общий код работает
// и до регистрации, и в голом браузере.

export interface PlatformAdapter {
  haptic(type?: "light" | "medium" | "heavy"): void;
  hapticSuccess(): void;
  hapticError(): void;
  confirm(message: string): Promise<boolean>;
  /** Обработчик системной «Назад» (Android). Возвращает функцию отписки. */
  pushBackHandler(handler: () => boolean): () => void;
  /**
   * Сохранить/расшарить блоб как файл на устройстве.
   * apk — Capacitor cache + системный share sheet (см. apk/src/saveFile.ts);
   * Telegram/веб — Web Share API с файлами, фолбэк на <a download>;
   * дефолт — классический <a download> (обычный браузер).
   * Бросает и на реальных ошибках, и при отмене share-листа пользователем —
   * отмену отфильтровывать через isSaveCancel().
   *
   * "failed" — сохранить НЕ удалось (мобильный Telegram: blob-якорь там молча
   * теряет файл). Вызывающий обязан сказать об этом и предложить доставку
   * файла ботом, а не рапортовать «Скачано» (находки N51/N117).
   */
  saveBlob(blob: Blob, name: string): Promise<"shared" | "downloaded" | "failed">;
  /** true, если ошибка — отмена share-листа пользователем (не показывать toast). */
  isSaveCancel(e: unknown): boolean;
}

const noop = () => {};

/** Безопасный дефолт сохранения: классический <a download> с blob-URL. */
export async function saveBlobViaAnchor(blob: Blob, name: string): Promise<"downloaded"> {
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = name;
  document.body.appendChild(a);
  a.click();
  a.remove();
  window.setTimeout(() => URL.revokeObjectURL(url), 4000);
  return "downloaded";
}

let current: PlatformAdapter = {
  haptic: noop,
  hapticSuccess: noop,
  hapticError: noop,
  confirm: (message) => Promise.resolve(window.confirm(message)),
  pushBackHandler: () => noop,
  saveBlob: saveBlobViaAnchor,
  isSaveCancel: (e) => /abort|cancel|dismiss/i.test((e as any)?.message || String(e ?? "")),
};

export function setPlatform(p: Partial<PlatformAdapter>): void {
  current = { ...current, ...p };
}

export function platform(): PlatformAdapter {
  return current;
}
