import { useCallback, useMemo, useState } from "react";
import { isPcOffline, mapApiError } from "../api-core";

export type LoadPhase = "loading" | "ok" | "offline" | "error";

/**
 * Состояние загрузки экрана с ЧЕСТНЫМ различием «пусто» и «компьютер не в сети».
 *
 * Раньше экраны глотали ошибку пустым catch и показывали пустое состояние:
 * список терминалов писал «Нет терминалов», когда на ПК работал Claude Code, а
 * «Система» крутила skeleton навсегда. Теперь ошибка классифицируется один раз
 * и одинаково на всех экранах, а поллинг можно останавливать по phase.
 */
export function useLoadState() {
  const [phase, setPhase] = useState<LoadPhase>("loading");
  const [message, setMessage] = useState("");

  const succeed = useCallback(() => {
    setPhase("ok");
    setMessage("");
  }, []);

  const fail = useCallback((e: unknown) => {
    if (isPcOffline(e)) {
      setPhase("offline");
      setMessage("");
      return;
    }
    setPhase("error");
    setMessage(mapApiError(e));
  }, []);

  /** Обёртка над загрузчиком: сама выставляет phase. Возвращает данные или null. */
  const run = useCallback(async <T,>(load: () => Promise<T>): Promise<T | null> => {
    try {
      const data = await load();
      succeed();
      return data;
    } catch (e) {
      fail(e);
      return null;
    }
  }, [succeed, fail]);

  // ВАЖНО: стабильная ссылка. Литерал на каждый рендер попадал в deps
  // useCallback/useEffect вызывающих экранов — эффект с поллингом пересоздавался
  // каждый рендер, и вместо одного запроса в 4 секунды экран делал тысячи
  // (ERR_INSUFFICIENT_RESOURCES в консоли).
  return useMemo(
    () => ({ phase, message, succeed, fail, run, offline: phase === "offline" }),
    [phase, message, succeed, fail, run],
  );
}
