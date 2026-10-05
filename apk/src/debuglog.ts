/**
 * Tagged diagnostic logger for the cloud/pairing flow.
 *
 * Debug builds enable WebView debugging, so these lines surface both in
 * chrome://inspect and in `adb logcat` (as `chromium: [INFO:CONSOLE] "[TGC] …"`).
 * That makes the pairing/cloud path observable and testable over ADB without a
 * human watching the screen.
 */
export function tlog(event: string, data?: unknown): void {
  let suffix = "";
  if (data !== undefined) {
    try { suffix = " " + JSON.stringify(data); } catch { suffix = " " + String(data); }
  }
  // console.info maps to logcat priority INFO under WebView debugging.
  console.info(`[TGC] ${event}${suffix}`);
}
