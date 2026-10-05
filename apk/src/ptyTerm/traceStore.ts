/**
 * Трасса и запись вывода живут ДОЛЬШЕ экрана терминала (ST-01, ревью S1).
 *
 * Раньше TerminalTrace и ByteRecorder лежали в useRef экземпляра PtyTermView.
 * «Открыть заново» (App.tsx пересоздаёт экран по ключу `${id}:${viewGeneration}`)
 * и уход с экрана создавали новый экземпляр — и всё, что происходило ДО
 * переоткрытия, пропадало ровно тогда, когда человек переоткрывал терминал
 * из-за проблемы. Теперь кольцо хранится на уровне модуля по id терминала:
 * новый экземпляр того же терминала продолжает ту же трассу (seq не
 * начинается заново), а запись вывода — ту же запись.
 *
 * Хранилище ограничено: последние TRACE_STORE_LIMIT терминалов, вытесняется
 * давно не открывавшийся. Вытесненная запись вывода выключается и стирается —
 * данных открытого вывода в памяти страницы не остаётся (I-15). Модуль без
 * React и DOM: проверяется в node.
 */
import { TerminalTrace } from "./terminalTrace";
import { ByteRecorder } from "./traceRecording";

export const TRACE_STORE_LIMIT = 4;

/** LRU по ключу: acquire поднимает ключ наверх, лишние снизу вытесняются. */
export class TraceStore<T> {
  private readonly slots = new Map<string, T>();

  constructor(
    private readonly create: () => T,
    private readonly limit = TRACE_STORE_LIMIT,
    private readonly evict?: (slot: T, key: string) => void,
  ) {}

  /** Слот терминала: тот же объект для того же id, пока он не вытеснен. */
  acquire(key: string): T {
    const id = String(key ?? "");
    let slot = this.slots.get(id);
    if (slot !== undefined) {
      this.slots.delete(id);
      this.slots.set(id, slot);
      return slot;
    }
    slot = this.create();
    this.slots.set(id, slot);
    const limit = Math.max(1, Math.floor(this.limit));
    while (this.slots.size > limit) {
      const oldest = this.slots.keys().next().value as string;
      const old = this.slots.get(oldest) as T;
      this.slots.delete(oldest);
      this.evict?.(old, oldest);
    }
    return slot;
  }

  has(key: string): boolean { return this.slots.has(String(key ?? "")); }
  get size(): number { return this.slots.size; }
  /** Ключи от давнего к свежему. */
  keys(): string[] { return [...this.slots.keys()]; }
  /** Пары [id, слот] от давнего к свежему — без изменения порядка LRU. */
  entries(): [string, T][] { return [...this.slots.entries()]; }
}

export interface TerminalTraceSlot {
  trace: TerminalTrace;
  rec: ByteRecorder;
}

export function createTraceSlot(): TerminalTraceSlot {
  return { trace: new TerminalTrace(), rec: new ByteRecorder() };
}

/** Вытеснение: запись вывода выключить и стереть, события стереть. */
export function releaseTraceSlot(slot: TerminalTraceSlot): void {
  slot.rec.disable();
  slot.rec.clear();
  slot.trace.clear();
}

/**
 * «Удалить запись» (T-37, волна 4): запись ВЫВОДА выключается и стирается во
 * ВСЕХ терминалах страницы — удаление, которое оставляет чужой вывод в памяти,
 * не удаление. События стирает вызывающий только у своего терминала.
 * Возвращает, у скольких ДРУГИХ терминалов было что стирать (для честного
 * текста уведомления).
 */
export function wipeAllRecordings(store: TraceStore<TerminalTraceSlot>, ownKey: string): { others: number } {
  let others = 0;
  for (const [key, slot] of store.entries()) {
    const had = slot.rec.enabled || slot.rec.snapshot().chunks.length > 0;
    slot.rec.disable();
    slot.rec.clear();
    if (had && key !== ownKey) others++;
  }
  return { others };
}

/** Одно хранилище на страницу: трассы терминалов этой вкладки. */
export const terminalTraceStore = new TraceStore<TerminalTraceSlot>(createTraceSlot, TRACE_STORE_LIMIT, releaseTraceSlot);
