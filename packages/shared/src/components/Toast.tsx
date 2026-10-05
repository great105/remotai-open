import { useState, useCallback, createContext, useContext, useLayoutEffect, useRef } from "react";
import { t } from "../i18n";

type ToastType = "success" | "error" | "info";

/** Необязательная кнопка в тосте — обычно «Повторить» после сетевой ошибки. */
export interface ToastAction { label: string; onClick: () => void; }
export interface ToastOptions { action?: ToastAction; durationMs?: number; }

interface ToastItem {
  id: number;
  message: string;
  type: ToastType;
  removing?: boolean;
  action?: ToastAction;
}

interface ToastCtx {
  toast: (message: string, type?: ToastType, opts?: ToastOptions) => void;
  toastSuccess: (message: string, opts?: ToastOptions) => void;
  toastError: (message: string, opts?: ToastOptions) => void;
}

const ToastContext = createContext<ToastCtx>({
  toast: () => {},
  toastSuccess: () => {},
  toastError: () => {},
});

// Ошибка — единственный канал отказа и часто длинный текст (mapApiError даёт
// фразы по 100+ знаков): 2,8 с прочитать нереально.
const DURATION: Record<ToastType, number> = { success: 2800, info: 2800, error: 6000 };
// Тост с кнопкой живёт дольше остальных: «Отменить» после необратимого действия
// надо успеть заметить, прочитать и попасть по нему пальцем — за 2,8 с человек
// в лучшем случае дочитывает, ЧТО произошло, и кнопка уходит у него из-под
// пальца. У ошибки при этом остаются её 6 с, если они окажутся больше.
const ACTION_DURATION = 8000;
const MIN_REMAINING = 800; // после отпускания пальца всегда даём дочитать
const MAX_TOASTS = 3;
// Отбивка между шапкой экрана и первой плашкой.
const TOAST_GAP_PX = 8;
let _nextId = 0;

/**
 * Насколько опустить плашки, чтобы они не легли на шапку.
 *
 * Тосты фиксированы у верха и лежат ВЫШЕ шапки (z-index 999 против 100), а
 * нажатия принимают (pointer-events: auto): шесть секунд ошибки — это шесть
 * секунд, когда «←», ✕ терминала, переименование и шестерёнка не нажимаются,
 * хотя уйти назад человек хочет именно после отказа. В CSS высоту шапки не
 * записать — она разная (.page-header ≈ 54px, .usage-header/.infra-header ≥
 * 94px) и зависит от инсетов, поэтому меряем то, что реально стоит наверху:
 * полноширинную sticky/fixed-полосу, прижатую к верхней кромке экрана.
 * Ничего не нашли (полноэкранный «Экран ПК») — возвращаем 0, и работает
 * штатный top из CSS.
 */
const TOP_BAR_SELECTOR = ".page-header, .usage-header, .infra-header, .conn-banner, [class*='-header']";

function measureTopBars(): number {
  if (typeof document === "undefined" || typeof window === "undefined") return 0;
  const bars: DOMRect[] = [];
  for (const el of document.querySelectorAll<HTMLElement>(TOP_BAR_SELECTOR)) {
    const rect = el.getBoundingClientRect();
    // Полоса во всю ширину: заголовок внутри карточки под этот фильтр не попадёт.
    if (rect.height <= 0 || rect.width < window.innerWidth * 0.6) continue;
    const position = getComputedStyle(el).position;
    if (position !== "sticky" && position !== "fixed") continue;
    bars.push(rect);
  }
  // Полосы стоят стопкой: при обрыве связи шапка липнет не к нулю, а под
  // баннер «компьютер не в сети» (top: var(--connection-banner-height)).
  // Поэтому спускаемся по цепочке, пока находится следующая полоса, начатая у
  // текущего низа; ничего не нашли на самом верху — offset остаётся нулевым.
  let bottom = 0;
  for (let pass = 0; pass <= bars.length; pass++) {
    const next = bars.find((rect) => rect.top <= bottom + 2 && rect.bottom > bottom);
    if (!next) break;
    bottom = next.bottom;
  }
  return bottom > 0 ? Math.ceil(bottom) + TOAST_GAP_PX : 0;
}

type TimerEntry = { timer: ReturnType<typeof setTimeout> | null; remaining: number; startedAt: number };

export function ToastProvider({ children }: { children: React.ReactNode }) {
  const [items, setItems] = useState<ToastItem[]>([]);
  // Отступ сверху меряем в момент появления плашки: шапка у каждого экрана
  // своя, а тост живёт поверх всех экранов сразу.
  const [topOffset, setTopOffset] = useState(0);
  const timersRef = useRef<Map<number, TimerEntry>>(new Map());

  const clearTimer = useCallback((id: number) => {
    const e = timersRef.current.get(id);
    if (e?.timer) clearTimeout(e.timer);
    timersRef.current.delete(id);
  }, []);

  const removeToast = useCallback((id: number) => {
    // Trigger exit animation
    setItems((prev) => prev.map((x) => (x.id === id ? { ...x, removing: true } : x)));
    setTimeout(() => setItems((prev) => prev.filter((x) => x.id !== id)), 250);
    clearTimer(id);
  }, [clearTimer]);

  const arm = useCallback((id: number, ms: number) => {
    const e = timersRef.current.get(id);
    if (e?.timer) clearTimeout(e.timer);
    timersRef.current.set(id, { timer: setTimeout(() => removeToast(id), ms), remaining: ms, startedAt: Date.now() });
  }, [removeToast]);

  /** Палец на тосте / курсор над ним — автозакрытие замирает. */
  const pause = useCallback((id: number) => {
    const e = timersRef.current.get(id);
    if (!e || !e.timer) return;
    clearTimeout(e.timer);
    const left = Math.max(MIN_REMAINING, e.remaining - (Date.now() - e.startedAt));
    timersRef.current.set(id, { timer: null, remaining: left, startedAt: 0 });
  }, []);

  const resume = useCallback((id: number) => {
    const e = timersRef.current.get(id);
    if (!e || e.timer) return;
    arm(id, e.remaining);
  }, [arm]);

  const toast = useCallback((message: string, type: ToastType = "info", opts?: ToastOptions) => {
    const id = ++_nextId;
    setItems((prev) => {
      const next = [...prev, { id, message, type, action: opts?.action }];
      // Remove oldest if over limit
      if (next.length > MAX_TOASTS) {
        clearTimer(next[0].id);
        return next.slice(1);
      }
      return next;
    });
    arm(id, opts?.durationMs ?? (opts?.action ? Math.max(ACTION_DURATION, DURATION[type]) : DURATION[type]));
  }, [arm, clearTimer]);

  const toastSuccess = useCallback((msg: string, opts?: ToastOptions) => toast(msg, "success", opts), [toast]);
  const toastError = useCallback((msg: string, opts?: ToastOptions) => toast(msg, "error", opts), [toast]);

  // Swipe-to-dismiss handler
  const touchState = useRef<{ id: number; startX: number; el: HTMLElement | null }>({ id: 0, startX: 0, el: null });

  const onTouchStart = (id: number, e: React.TouchEvent) => {
    touchState.current = { id, startX: e.touches[0].clientX, el: e.currentTarget as HTMLElement };
  };
  const onTouchMove = (e: React.TouchEvent) => {
    const { el, startX } = touchState.current;
    if (!el) return;
    const dx = e.touches[0].clientX - startX;
    if (Math.abs(dx) > 10) {
      el.style.transform = `translateX(${dx}px)`;
      el.style.opacity = `${Math.max(0, 1 - Math.abs(dx) / 150)}`;
    }
  };
  const onTouchEnd = () => {
    const { el, id } = touchState.current;
    if (!el) return;
    const dx = parseFloat(el.style.transform?.replace(/[^-\d.]/g, "") || "0");
    if (Math.abs(dx) > 60) {
      removeToast(id);
    } else {
      el.style.transform = "";
      el.style.opacity = "";
    }
    touchState.current = { id: 0, startX: 0, el: null };
  };

  // Тап по тосту больше ничего не закрывает: случайное касание при чтении
  // гасило сообщение об ошибке. Закрыть можно ✕ или свайпом вбок.
  const renderToast = (x: ToastItem) => (
    <div
      key={x.id}
      className={`toast toast-${x.type}${x.removing ? " toast-exit" : ""}`}
      onTouchStart={(e) => { pause(x.id); onTouchStart(x.id, e); }}
      onTouchMove={onTouchMove}
      onTouchEnd={() => { onTouchEnd(); resume(x.id); }}
      onTouchCancel={() => { onTouchEnd(); resume(x.id); }}
      // Пауза по наведению — только для НАСТОЯЩЕЙ мыши. После касания браузер
      // досылает совместимостную последовательность мыши (mouseenter приходит
      // уже ПОСЛЕ touchend), а mouseleave не наступает, пока палец не коснётся
      // чего-то ещё: тост, которого случайно коснулись, замирал на экране
      // навсегда. pointerType отсекает синтетику.
      onPointerEnter={(e) => { if (e.pointerType === "mouse") pause(x.id); }}
      onPointerLeave={(e) => { if (e.pointerType === "mouse") resume(x.id); }}
    >
      <span className="toast-icon">
        {x.type === "success" ? "✓" : x.type === "error" ? "✗" : "ℹ"}
      </span>
      <span className="toast-text">{x.message}</span>
      {x.action && (
        <button
          type="button"
          className="toast-action"
          onClick={() => { const a = x.action!; removeToast(x.id); a.onClick(); }}
        >
          {x.action.label}
        </button>
      )}
      <button
        type="button"
        className="toast-close"
        aria-label={t("toast.dismiss")}
        onClick={() => removeToast(x.id)}
      >
        {"✕"}
      </button>
    </div>
  );

  const errors = items.filter((x) => x.type === "error");
  const notices = items.filter((x) => x.type !== "error");

  // Меряем перед отрисовкой кадра — иначе первая плашка успевает мигнуть на
  // шапке. Пересчёт на каждое изменение списка: пока висел один тост, человек
  // мог уйти на экран с другой шапкой. Пустой список не трогаем.
  const shownCount = items.length;
  useLayoutEffect(() => {
    if (shownCount === 0) return;
    setTopOffset(measureTopBars());
  }, [shownCount]);

  return (
    <ToastContext.Provider value={{ toast, toastSuccess, toastError }}>
      {children}
      {/* Два live-region'а: ошибку скринридер обязан объявить немедленно,
          успех/инфо — дождавшись паузы. Оба смонтированы всегда, иначе
          вставка узла вместе с регионом не объявляется. */}
      <div className="toast-container" style={topOffset ? { top: topOffset } : undefined}>
        <div className="toast-region" role="alert" aria-live="assertive" aria-atomic="false">
          {errors.map(renderToast)}
        </div>
        <div className="toast-region" role="status" aria-live="polite" aria-atomic="false">
          {notices.map(renderToast)}
        </div>
      </div>
    </ToastContext.Provider>
  );
}

export function useToast() {
  return useContext(ToastContext);
}
