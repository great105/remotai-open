// Транспортный реестр API — по образцу platform.ts. Общие endpoint-обёртки
// (api-endpoints.ts) ходят в сеть ТОЛЬКО через зарегистрированный транспорт.
// Каждое приложение регистрирует свою реализацию в начале своего api.ts:
//   apk     — api<T>() с cloud-роутингом через релей, в LAN — X-API-Token;
//   miniapp — httpClient с заголовком X-Telegram-Init-Data.
// Вся app-специфика (auth-заголовки, cloud/LAN ветвление, лимиты загрузок)
// живёт в адаптерах приложений, а не здесь.

export interface UploadOptions {
  /** Отмена пользователем. Транспорт обязан оборвать активный XHR/fetch. */
  signal?: AbortSignal;
  /** Включить детерминированные чанки и продолжение с server offset. */
  resumable?: boolean;
}

export interface ApiTransport {
  /** Обычные JSON-вызовы. */
  request<T>(path: string, init?: RequestInit): Promise<T>;
  /** Multipart-загрузка (upload-функции); auth-заголовки — в адаптере приложения.
      onProgress (pct 0–100) — опционально; cloud-адаптер может звать только финальные 100. */
  uploadForm<T>(
    path: string,
    form: FormData,
    onProgress?: (pct: number) => void,
    options?: UploadOptions,
  ): Promise<T>;
  /** BASE для url-builder'ов (downloadUrl, ptyExportUrl). */
  base(): string;
  /** Строка initData для query-параметров (download/export URL). */
  authQuery(): string;
}

let current: ApiTransport | null = null;

export function setApiTransport(t: ApiTransport): void {
  current = t;
}

/** Внутренний геттер для api-endpoints.ts. */
export function transport(): ApiTransport {
  if (!current) {
    throw new Error(
      "@tgcontrol/shared: ApiTransport не зарегистрирован — вызовите setApiTransport(...) в api.ts приложения",
    );
  }
  return current;
}
