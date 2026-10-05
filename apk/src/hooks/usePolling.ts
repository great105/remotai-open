import { useEffect, useRef } from "react";

export interface PollingOptions {
  /** Поллить только когда true (например, открыта нужная вкладка). По умолчанию true. */
  enabled?: boolean;
  /** Дёргать load сразу: при старте и при возврате из фона. По умолчанию true. */
  immediate?: boolean;
}

/**
 * Периодическое обновление экрана с гейтом по видимости — один паттерн вместо
 * голых setInterval, разбросанных по экранам.
 *
 * Зачем: в WebView2 и в свёрнутом Telegram таймеры не троттлятся, поэтому
 * экраны продолжали будить радиомодуль телефона и занимать агента, пока
 * приложение лежало в кармане (каждый /api/system/stats — ~500 мс на ПК,
 * каждый /api/pty — снимок дерева процессов на КАЖДУЮ сессию). Здесь опрос
 * идёт только при `document.visibilityState === "visible"`, а при возврате из
 * фона данные обновляются сразу, без ожидания очередного тика.
 *
 * ГРАБЛЯ 2.28.0 (6549 запросов за 6 секунд, хотфикс 2.28.1): колбэк, собранный
 * в теле компонента, — НОВАЯ ссылка на каждый рендер. Стоило ему попасть в deps
 * эффекта с таймером — и таймер пересоздавался каждым рендером, а вместе с ним
 * уходил новый немедленный запрос. Поэтому здесь колбэк живёт в ref
 * (обновляется отдельным эффектом), а в deps эффекта-таймера ТОЛЬКО примитивы
 * enabled/intervalMs/immediate. Добавлять сюда `load` или объект `opts` нельзя
 * ни при каких обстоятельствах — вызывающий экран волен передавать хоть
 * стрелку-литерал `() => refresh(true)`.
 *
 * Колбэк можно передавать асинхронный: пока предыдущий вызов не завершился,
 * такт пропускается (наложения запросов нет). Запросы клиента ограничены
 * 30-секундным abort'ом в api-core, так что «вечно висящего» вызова не будет.
 */
export function usePolling(
  load: () => void | Promise<unknown>,
  intervalMs: number,
  opts: PollingOptions = {},
): void {
  // Деструктурируем ДО эффекта: в deps должны попасть примитивы, а не объект
  // opts — литерал `{ enabled: tab === "monitor" }` это новая ссылка на рендер.
  const { enabled = true, immediate = true } = opts;

  const loadRef = useRef(load);
  // Без deps: просто держим ссылку свежей после каждого рендера.
  useEffect(() => {
    loadRef.current = load;
  });

  // Компонент размонтирован — ни одного вызова load больше (грабля зомби-циклов
  // в PtyTermView: обработчик пережил размонтирование и продолжал жить).
  const disposed = useRef(false);
  useEffect(() => {
    disposed.current = false; // StrictMode в деве монтирует дважды
    return () => {
      disposed.current = true;
    };
  }, []);

  // Момент последнего запуска: гасит лишний немедленный запрос при перезапуске
  // эффекта (сменился intervalMs из-за ухода ПК в офлайн, дёрнулась вкладка).
  const lastFire = useRef(0);
  // Предыдущий вызов ещё в полёте — очередной такт пропускаем.
  const inFlight = useRef(false);

  useEffect(() => {
    // 0/NaN превратили бы setInterval в шторм (браузер зажимает такой период до 4 мс).
    if (!enabled || !Number.isFinite(intervalMs) || intervalMs <= 0) return;

    let timer = 0;

    const fire = () => {
      if (disposed.current || inFlight.current) return;
      lastFire.current = Date.now();
      let result: unknown;
      try {
        result = loadRef.current();
      } catch {
        return; // синхронное исключение в колбэке не должно убивать таймер
      }
      if (result instanceof Promise) {
        inFlight.current = true;
        const done = () => {
          inFlight.current = false;
        };
        result.then(done, done);
      }
    };

    const visible = () => document.visibilityState === "visible";

    const start = () => {
      if (timer) return;
      // Не чаще одного «немедленного» вызова на min(интервал, 2 с): перезапуск
      // эффекта или быстрое переключение вкладок туда-обратно не должны
      // превращаться в лишние запросы.
      if (immediate && Date.now() - lastFire.current >= Math.min(intervalMs, 2000)) fire();
      timer = window.setInterval(fire, intervalMs);
    };
    const stop = () => {
      if (timer) {
        window.clearInterval(timer);
        timer = 0;
      }
    };
    const onVis = () => (visible() ? start() : stop());

    if (visible()) {
      start();
    } else if (immediate) {
      // Смонтировались в фоне (приложение подняли из свёрнутого состояния):
      // один запрос всё же делаем, иначе экран встретит пользователя спиннером
      // или skeleton'ом. Интервал заведётся при первом же возврате в фокус.
      fire();
    }

    document.addEventListener("visibilitychange", onVis);
    return () => {
      stop();
      document.removeEventListener("visibilitychange", onVis);
    };
  }, [enabled, intervalMs, immediate]);
}
