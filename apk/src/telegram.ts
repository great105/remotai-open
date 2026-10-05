/** Платформенный адаптер. Единый клиент работает в трёх средах; адаптер сам
 *  определяет окружение в рантайме:
 *   - Telegram Mini App  → window.Telegram.WebApp (initData, нативные haptics/confirm/biometry);
 *   - нативный APK       → Capacitor (@capacitor/haptics), токен из сохранённого пейринга;
 *   - окно exe / браузер → local-token мост (?token=), in-app диалоги.
 *  Не-Telegram ветки (APK/exe/web) — основной проверяемый путь; Telegram-ветка
 *  активируется только внутри реального Telegram-клиента. */

import { Haptics, ImpactStyle, NotificationType } from "@capacitor/haptics";
import { getLocalTokenInitData, confirmDialog, type DialogOptions } from "@tgcontrol/shared";
import { getServerConfig } from "./config";

declare global {
  interface Window {
    Telegram?: { WebApp: TelegramWebApp };
  }
}

interface TelegramWebApp {
  initData: string;
  /** Тема клиента Telegram. НЕ читаем осознанно: палитра у продукта одна —
   *  тёмная, и она одинакова во всех четырёх интерфейсах. Смешивать чужой
   *  светлый текст с нашими тёмными фонами карточек нельзя (main.tsx снимает
   *  --tg-theme-* при старте). Поле оставлено для диагностики. */
  colorScheme: "light" | "dark";
  themeParams: Record<string, string>;
  /** Цвета обвязки самого Telegram (Bot API 6.1+/7.10+ для нижней полосы) —
   *  без них вокруг тёмного мини-аппа остаётся белая рамка клиента. */
  setHeaderColor?: (color: string) => void;
  setBackgroundColor?: (color: string) => void;
  setBottomBarColor?: (color: string) => void;
  isExpanded: boolean;
  ready: () => void;
  expand: () => void;
  close: () => void;
  enableClosingConfirmation?: () => void;
  disableClosingConfirmation?: () => void;
  isFullscreen?: boolean;
  requestFullscreen?: () => void;
  disableVerticalSwipes?: () => void;
  /** Нативный сканер QR (Bot API 6.4+) — надёжнее веб-камеры внутри Telegram. */
  showScanQrPopup?: (params: { text?: string }, cb?: (text: string) => boolean | void) => void;
  closeScanQrPopup?: () => void;
  BackButton: {
    isVisible: boolean;
    show: () => void;
    hide: () => void;
    onClick: (cb: () => void) => void;
    offClick: (cb: () => void) => void;
  };
  HapticFeedback: {
    impactOccurred: (style: "light" | "medium" | "heavy" | "rigid" | "soft") => void;
    notificationOccurred: (type: "error" | "success" | "warning") => void;
    selectionChanged: () => void;
  };
  showConfirm: (message: string, cb?: (ok: boolean) => void) => void;
  version?: string;
  platform?: string;
  isVersionAtLeast?: (version: string) => boolean;
  // Safe-area (Bot API 8.0+). safeAreaInset — устройство (чёлка), contentSafeAreaInset —
  // дополнительный отступ под UI Telegram (его шапка с «✕ Закрыть» на iPad).
  safeAreaInset?: { top: number; bottom: number; left: number; right: number };
  contentSafeAreaInset?: { top: number; bottom: number; left: number; right: number };
  onEvent?: (event: string, cb: () => void) => void;
  offEvent?: (event: string, cb: () => void) => void;
}

export function getTelegram(): TelegramWebApp | null {
  return (typeof window !== "undefined" && window.Telegram?.WebApp) || null;
}

/** true только внутри настоящего Telegram-клиента (подписанный initData + Bot API),
 *  а не в exe/браузере, где telegram-web-app.js рапортует версию 6.0 с пустым initData. */
function isRealTelegram(): boolean {
  const tg = getTelegram();
  return !!tg?.initData && typeof tg.isVersionAtLeast === "function";
}

/** "token:<API_TOKEN>" | подписанный Telegram initData | "" — для auth-слоёв.
 *  Telegram → tg.initData; APK → токен пейринга; exe/браузер → ?token= мост. */
export function getInitData(): string {
  const tg = getTelegram();
  if (tg?.initData) return tg.initData;
  const cfg = getServerConfig();
  if (cfg) return `token:${cfg.token}`;
  return getLocalTokenInitData();
}

const CAP_IMPACT = { light: ImpactStyle.Light, medium: ImpactStyle.Medium, heavy: ImpactStyle.Heavy };

/** Haptic impact — Telegram HapticFeedback в Telegram, иначе Capacitor. */
export function haptic(type: "light" | "medium" | "heavy" = "light") {
  const tg = getTelegram();
  if (tg) { tg.HapticFeedback.impactOccurred(type); return; }
  Haptics.impact({ style: CAP_IMPACT[type] }).catch(() => {});
}

export function hapticSuccess() {
  const tg = getTelegram();
  if (tg) { tg.HapticFeedback.notificationOccurred("success"); return; }
  Haptics.notification({ type: NotificationType.Success }).catch(() => {});
}

export function hapticError() {
  const tg = getTelegram();
  if (tg) { tg.HapticFeedback.notificationOccurred("error"); return; }
  Haptics.notification({ type: NotificationType.Error }).catch(() => {});
}

/** Confirm — нативный Telegram showConfirm (только в настоящем Telegram, Bot API 6.2+
 *  и только для простого «да/нет» без опций), иначе in-app диалог (<DialogHost/>).
 *  В окне exe (WebView2) нативный confirm подавлен, поэтому единственный надёжный
 *  путь вне Telegram — DialogHost. */
export function tgConfirm(message: string, opts?: DialogOptions): Promise<boolean> {
  // Нативный showConfirm умеет только «ОК/Отмена»: он теряет и красный стиль,
  // и подпись-глагол, и заголовок. Поэтому при danger/своих подписях/заголовке
  // всегда рисуем свой <DialogHost/> — Telegram и APK ведут себя одинаково.
  const needsCustomUi = !!opts && (opts.danger === true || !!opts.confirmText || !!opts.cancelText || !!opts.title);
  const tg = getTelegram();
  if (!needsCustomUi && isRealTelegram() && typeof tg!.showConfirm === "function" && tg!.isVersionAtLeast!("6.2")) {
    return new Promise<boolean>((resolve) => {
      try { tg!.showConfirm(message, (ok) => resolve(!!ok)); }
      catch { confirmDialog(message, opts).then(resolve); }
    });
  }
  return confirmDialog(message, opts);
}

// ── Telegram BiometricManager (Bot API 7.2+) — подтверждение чувствительных действий ──
// Резолвит true и когда биометрия недоступна, чтобы не блокировать неподдерживающие
// клиенты (они используют обычный confirm).
type TgBiometricManager = {
  isInited: boolean;
  isBiometricAvailable: boolean;
  init(cb?: () => void): void;
  authenticate(params: { reason?: string }, cb: (ok: boolean) => void): void;
};

export function biometricConfirm(reason: string): Promise<boolean> {
  const bm = (getTelegram() as unknown as { BiometricManager?: TgBiometricManager })?.BiometricManager;
  if (!bm?.authenticate) return Promise.resolve(true);
  return new Promise((resolve) => {
    const run = () => {
      if (bm.isBiometricAvailable) bm.authenticate({ reason }, (ok) => resolve(!!ok));
      else resolve(true);
    };
    if (!bm.isInited) bm.init(run);
    else run();
  });
}

/**
 * Нативный сканер QR Telegram (Bot API 6.4+). Возвращает распознанный текст или
 * null, если сканер недоступен/закрыт. В Telegram веб-камера через getUserMedia
 * капризна, а этот путь — системный.
 */
export function scanQrInTelegram(text?: string): Promise<string | null> {
  const tg = getTelegram();
  if (!isRealTelegram() || typeof tg?.showScanQrPopup !== "function") return Promise.resolve(null);
  return new Promise((resolve) => {
    let done = false;
    try {
      tg.showScanQrPopup!({ text }, (raw) => {
        if (done) return true;
        done = true;
        try { tg.closeScanQrPopup?.(); } catch { /* уже закрыт */ }
        resolve(raw || null);
        return true; // закрыть попап
      });
    } catch {
      resolve(null);
    }
  });
}
