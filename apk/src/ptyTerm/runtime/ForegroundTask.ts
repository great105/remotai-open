export interface VisibilitySource {
  readonly hidden: boolean;
  addEventListener(type: "visibilitychange", listener: () => void): void;
  removeEventListener(type: "visibilitychange", listener: () => void): void;
}

/**
 * Льгота после возврата страницы для сторожа тишины (ST-09, T-33). После
 * разморозки таймеры и доставка накопившихся hb идут в неизвестном порядке:
 * интервал сторожа может сработать раньше, чем доедет первое сообщение, и
 * сравнить с lastMsg, взятым ДО заморозки. Без льготы живой сокет закрывался
 * бы как мёртвый. Цена — обнаружение действительно мёртвого сокета позже на
 * эти 3 с.
 */
export const SILENCE_THAW_GRACE_MS = 3000;

/**
 * Мёртв ли сокет по тишине: сообщений не было дольше limitMs, и страница
 * видима уже не меньше graceMs. limitMs <= 0 — сторож выключен (сервер не
 * объявил hb). visibleSince — момент последнего возврата страницы (0, если
 * страница не уходила в фон).
 */
export function silenceVerdict(now: number, lastMsgAt: number, limitMs: number,
  visibleSince: number, graceMs: number = SILENCE_THAW_GRACE_MS): boolean {
  return limitMs > 0 && now - lastMsgAt > limitMs && now - visibleSince >= graceMs;
}

/** Запас разового таймера сверх льготы: таймер не должен прийти раньше неё. */
export const SILENCE_RECHECK_SLACK_MS = 20;

/**
 * Когда перепроверить тишину после возврата или разморозки (ST-09, T-33): ровно
 * по истечении льготы, а не на следующем тике сторожа. Льгота лишь откладывает
 * приговор; без разовой перепроверки мёртвый сокет после фона закрывался
 * следующим 5-секундным тиком — через 6,6–6,9 с вместо ~3 с (замер ревьюера
 * 14.09), тогда как до льготы — за миллисекунды.
 * Возвращает задержку в мс или null: сторож выключен (limitMs <= 0) или
 * страница не возвращалась (visibleSince <= 0).
 */
export function silenceRecheckDelay(now: number, visibleSince: number, limitMs: number,
  graceMs: number = SILENCE_THAW_GRACE_MS): number | null {
  if (!(limitMs > 0) || !(visibleSince > 0) || !Number.isFinite(now)) return null;
  return Math.max(0, visibleSince + graceMs - now) + SILENCE_RECHECK_SLACK_MS;
}

/**
 * Разморозка по опозданию тика сторожа (ST-09, T-33). Страницу морозят без
 * visibilitychange: CDP Page.setWebLifecycleState, остановленные таймеры
 * WebView, долгий захват главного потока. Видимая страница, чей интервальный
 * тик опоздал больше чем на два периода, не выполняла и доставку сообщений —
 * накопившиеся hb ещё впереди, и тишина до этого момента ничего не доказывает.
 * Возвращает момент разморозки (now) или null. Скрытую страницу так не судим:
 * там таймеры троттлятся, а сообщения идут, и опоздание ничего не значит.
 */
export function thawedAt(prevTickAt: number, now: number, intervalMs: number, visible: boolean): number | null {
  if (!visible || !(prevTickAt > 0) || !(intervalMs > 0)) return null;
  return now - prevTickAt > intervalMs * 2 ? now : null;
}

/** No timer in the background and no overlapping asynchronous polls. */
export function foregroundTask(task: () => void | Promise<void>, interval: number,
  visibility: VisibilitySource = document): () => void {
  let stopped = false, running = false;
  let timer: ReturnType<typeof setTimeout> | undefined;
  const clear = () => { if (timer !== undefined) clearTimeout(timer); timer = undefined; };
  const tick = async () => {
    clear();
    if (stopped || visibility.hidden || running) return;
    running = true;
    try { await task(); }
    catch { /* A transient poll failure must not end foreground scheduling. */ }
    finally {
      running = false;
      if (!stopped && !visibility.hidden) timer = setTimeout(tick, interval);
    }
  };
  const changed = () => { clear(); if (!visibility.hidden) void tick(); };
  visibility.addEventListener("visibilitychange", changed);
  void tick();
  return () => { stopped = true; clear(); visibility.removeEventListener("visibilitychange", changed); };
}
