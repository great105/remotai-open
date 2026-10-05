import { t } from "./i18n";

/**
 * Единый формат «сколько прошло» для всех экранов: «12с», «7м», «3ч», «2д».
 *
 * До этого одну и ту же величину считали пять раз по-разному: список терминалов
 * и легаси-карточки сессий печатали латинские «12m / 3h / 2d», а главная и экран
 * терминала — кириллические «12м / 3ч / 2д». Оба варианта подставлялись в один и
 * тот же ключ `pty.timeAgo` («{value} назад»), поэтому в русском интерфейсе один
 * факт читался на двух языках: «вывод 7м назад» на главной и «вывод 7m» в /pty.
 *
 * `nowMs` передают экраны с собственным тикером (список терминалов держит
 * `nowMs` в state, чтобы возраст вывода обновлялся только на видимой странице).
 */
export function formatAgoValue(ms: number, nowMs: number = Date.now()): string {
  const sec = Math.max(0, ((nowMs - ms) / 1000) | 0);
  if (sec < 60) return t("ui.ptytermview.mee89ed3363", { p0: (sec) });
  if (sec < 3600) return t("ui.ptytermview.m2cc2493765", { p0: ((sec / 60) | 0) });
  if (sec < 86400) return t("ui.ptytermview.mb190c47c3b", { p0: ((sec / 3600) | 0) });
  return t("ui.ptytermview.m991812ce32", { p0: ((sec / 86400) | 0) });
}

/** Готовая фраза «7м назад» — когда экрану не нужна отдельная величина. */
export function formatAgo(ms: number, nowMs?: number): string {
  return t("pty.timeAgo", { value: formatAgoValue(ms, nowMs) });
}

/**
 * Длительность завершившегося эпизода работы агента: «38 с», «4 мин 12 с»,
 * «2 ч 5 мин». Полные слова, а не сокращения formatAgoValue («4м»): здесь это
 * самостоятельный факт в плашке «агент закончил» и в системном уведомлении,
 * а не суффикс в строке списка. Единицы не склоняются («мин», «с», «ч») —
 * короткие формы читаются одинаково при любом числе.
 */
export function formatDurationMs(ms: number): string {
  const sec = Math.max(0, Math.round(ms / 1000));
  if (sec < 60) return t("ui.timeago.m2dd8ed31ee", { p0: (sec) });
  const min = (sec / 60) | 0;
  if (min < 60) {
    const rest = sec % 60;
    return rest > 0 ? t("ui.timeago.mc36d1394a0", { p0: (min), p1: (rest) }) : t("ui.timeago.mf3b7422bb3", { p0: (min) });
  }
  const h = (min / 60) | 0;
  const restMin = min % 60;
  return restMin > 0 ? t("ui.timeago.md24d7119e0", { p0: (h), p1: (restMin) }) : t("ui.timeago.m93c2902098", { p0: (h) });
}
