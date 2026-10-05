/** Same normalization and bracketed-paste contract as the installed xterm. */
export function pastePayload(text: string, bracketed: boolean): string {
  const normalized = text.replace(/\r?\n/g, "\r");
  return bracketed ? `\x1b[200~${normalized}\x1b[201~` : normalized;
}

export function needsMultilineReview(text: string, bracketed: boolean): boolean {
  return !bracketed && /[\r\n]/.test(text);
}

export interface InputTransport { readyState: number; send(data: string | Uint8Array<ArrayBuffer>): void }
export interface InputTarget {
  transport: InputTransport | null;
  identity: string;
  bracketed: boolean;
}
export type InputResult = "sent" | "cancelled" | "stale" | "failed";

/**
 * Та же ли это цель ввода (ST-07, I-08): то же соединение, тот же процесс и то
 * же поколение, тот же режим вставки. Проверяется после КАЖДОГО ожидания —
 * review, чтения clipboard, загрузки файла: путь загруженной картинки после
 * переподключения к новому процессу не должен молча печататься в него.
 */
export function sameInputTarget(a: InputTarget, b: InputTarget): boolean {
  return a.transport === b.transport && a.identity === b.identity && a.bracketed === b.bracketed;
}

/** Capture at the original gesture, before reading a clipboard or opening a
 * dialog. A later process, connection or input mode must never receive it.
 */
export async function reviewedInput(target: InputTarget, current: () => InputTarget,
  text: string, submit: boolean, review: () => Promise<boolean>): Promise<InputResult> {
  const valid = () => sameInputTarget(target, current());
  if (!valid()) return "stale";
  if (needsMultilineReview(text, target.bracketed) && !await review()) return "cancelled";
  if (!valid()) return "stale";
  return transmitInput(target.transport, text
    ? { kind: "paste", text, bracketed: target.bracketed, submit }
    : { kind: "key", data: submit ? "\r" : "" }) ? "sent" : "failed";
}
export interface EnterKeyEvent {
  key: string; shiftKey?: boolean; ctrlKey?: boolean; metaKey?: boolean;
  /** nativeEvent.isComposing — состояние САМОГО нажатия, не React-флаг. */
  isComposing?: boolean; keyCode?: number;
}

/**
 * Что значит Enter в поле ввода (T-21, ST-07). Во время IME-композиции Enter
 * подтверждает композицию и никогда не отправляет: признак берём у события
 * (isComposing или keyCode 229), а не у нашего флага — на iOS compositionend
 * приходит ПОСЛЕ keydown (жалоба владельца 10.09.2026 с iPad).
 *   - поле «Сообщение/команда»: Enter без Shift отправляет, Shift+Enter — перевод строки;
 *   - большой композер: Enter — перевод строки (Shift+Enter на экранной
 *     клавиатуре нажать нечем), отправка только Ctrl/⌘+Enter.
 */
export function enterIntent(event: EnterKeyEvent, surface: "field" | "composer"): "submit" | "default" {
  if (event.key !== "Enter" || event.isComposing || event.keyCode === 229) return "default";
  if (surface === "composer") return event.ctrlKey || event.metaKey ? "submit" : "default";
  return event.shiftKey ? "default" : "submit";
}

export type InputIntent = { kind: "key"; data: string }
  | { kind: "paste"; text: string; bracketed: boolean; submit?: boolean }
  | { kind: "encodedPaste"; data: string };

/** Success means queued to this socket, never received or executed. No retries. */
export function transmitInput(transport: InputTransport | null, intent: InputIntent): boolean {
  if (transport?.readyState !== 1) return false;
  try {
    if (intent.kind === "key") transport.send(new TextEncoder().encode(intent.data));
    else {
      const text = intent.kind === "encodedPaste" ? intent.data
        : pastePayload(intent.text, intent.bracketed) + (intent.submit ? "\r" : "");
      // Enter is the tail of the SAME operation, also on older hosts.
      const message = JSON.stringify({ t: "paste", text });
      // Match the existing server's 1 MiB message limit, including JSON/UTF-8
      // overhead. Reject before send so the UI retains editable text instead
      // of reporting success immediately before the socket is closed.
      if (new TextEncoder().encode(message).byteLength > 1024 * 1024) return false;
      transport.send(message);
    }
    return true;
  } catch { return false; }
}
