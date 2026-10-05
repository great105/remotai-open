/**
 * Imperative, promise-based dialogs (shared).
 *
 * Replaces the browser's native alert()/confirm()/prompt() with in-app modal UI
 * that can be awaited from anywhere — including plain util functions with no
 * access to React context (e.g. telegram.ts / platform()).
 *
 * Critical for the exe панель: WebView2 подавляет нативные alert/confirm/prompt
 * (они молча возвращают false), поэтому window.confirm() там не работает —
 * единственный надёжный путь подтверждений — этот in-app диалог.
 *
 * <DialogHost/> (mounted once at the app root) registers the emitter; calls made
 * before it mounts are queued and flushed on registration.
 */

export type DialogKind = "confirm" | "alert" | "prompt";

export interface DialogOptionsBase {
  title?: string;
  cancelText?: string;
  /** prompt: initial input value. */
  defaultValue?: string;
  /** prompt: input placeholder. */
  placeholder?: string;
  /** prompt: use a multi-line textarea instead of a single-line input. */
  multiline?: boolean;
}

/** Обычный диалог: подпись кнопки необязательна (дефолт «Подтвердить»/«ОК»). */
export interface DialogOptionsSafe extends DialogOptionsBase {
  danger?: false;
  confirmText?: string;
}

/**
 * Деструктивный: красная кнопка ОБЯЗАНА нести глагол действия («Удалить»,
 * «Выключить», «Закрыть терминал») — «Подтвердить» на красной кнопке не
 * говорит, что именно произойдёт, а отменить это уже нельзя. Поэтому
 * confirmText здесь обязателен на уровне типа.
 */
export interface DialogOptionsDanger extends DialogOptionsBase {
  danger: true;
  confirmText: string;
}

export type DialogOptions = DialogOptionsSafe | DialogOptionsDanger;

/** Union нельзя расширить через `interface extends` — только пересечением. */
export type DialogRequest = DialogOptions & {
  id: number;
  kind: DialogKind;
  message: string;
  resolve: (value: boolean | string | null | void) => void;
};

type Emitter = (req: DialogRequest) => void;

let emitter: Emitter | null = null;
let queued: DialogRequest[] = [];
let nextId = 1;

/** Registered by <DialogHost/> on mount; pass null on unmount. */
export function setDialogEmitter(fn: Emitter | null): void {
  emitter = fn;
  if (fn && queued.length) {
    const pending = queued;
    queued = [];
    pending.forEach(fn);
  }
}

function request(kind: DialogKind, message: string, opts: DialogOptions = {}): Promise<boolean | string | null | void> {
  return new Promise((resolve) => {
    // as: спред union'а TS не сводит обратно к union автоматически.
    const req = { id: nextId++, kind, message, resolve, ...opts } as DialogRequest;
    if (emitter) emitter(req);
    else queued.push(req); // host not mounted yet — flush once it registers
  });
}

/** Ask the user to confirm. Resolves true (confirm) / false (cancel or Escape). */
export function confirmDialog(message: string, opts?: DialogOptions): Promise<boolean> {
  return request("confirm", message, opts) as Promise<boolean>;
}

/** Show an informational alert. Resolves when dismissed. */
export function alertDialog(message: string, opts?: DialogOptions): Promise<void> {
  return request("alert", message, opts) as Promise<void>;
}

/** Ask for text input. Resolves the string, or null on cancel/Escape. */
export function promptDialog(message: string, opts?: DialogOptions): Promise<string | null> {
  return request("prompt", message, opts) as Promise<string | null>;
}
