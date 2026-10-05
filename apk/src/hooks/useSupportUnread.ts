import { useEffect, useState } from "react";
import { usePolling } from "./usePolling";
import {
  getUnread,
  onUnread,
  refreshSupportUnread,
  supportChatAvailable,
} from "../supportUnread";

/** Ответ поддержки — не realtime-событие: раз в минуту достаточно, а с гейтом
 *  по видимости в кармане запросов нет вовсе. */
const POLL_MS = 60_000;

/**
 * Число непрочитанных ответов поддержки.
 *
 * `poll` включают ТОЛЬКО долгоживущие экраны-держатели (нижняя навигация,
 * экран настроек), остальные подписываются на готовое значение. Даже если
 * поллеров случайно окажется два, лишнего запроса не будет: троттлинг и
 * дедуп in-flight сидят в самом refreshSupportUnread.
 */
export function useSupportUnread(poll = false): number {
  const [n, setN] = useState(getUnread);

  useEffect(() => onUnread(setN), []);

  // В usePolling передаём именно модульную функцию — стабильную ссылку
  // (грабля 2.28.1: стрелка из тела компонента пересоздавала таймер).
  usePolling(refreshSupportUnread, POLL_MS, {
    enabled: poll && supportChatAvailable(),
  });

  return n;
}
