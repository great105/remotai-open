/**
 * Какие виды {t:"diag"} уходят в журнал агента (волна 4, совместимость I-14).
 *
 * Агент 2.71.1 и старше знает только виды, которые слал клиент до плана
 * стабилизации; любой незнакомый он печатает форматом alt-scroll, и строка
 * журнала — `what=trace-mark alt=<nil> … байт=0`: seq метки «Зафиксировать
 * проблему» теряется, журнал забивается пустыми строками (flow раз в 5–30 с,
 * recovery на каждом соединении, nav на каждом решении). Поэтому новые виды
 * уходят только агенту, который доказал, что он новый; до этого — и у старого
 * агента всегда — они остаются в трассе клиента (sendDiag пишет трассу первой).
 *
 * Признак нового агента — без новых параметров URL (I-14):
 *   • {t:"screen-capability", v:1} — ответ на объявленный screen-request-v1;
 *   • поля /state, которых у старого агента нет: fg_started, history_retention.
 * Модуль чистый: проверяется в node.
 */

/** Виды, которые знал агент до ST-01 (клиент ae70c36 слал ровно их). */
export const LEGACY_DIAG_KINDS: readonly string[] = [
  "tg-chrome", "upload", "snapshot", "snapshot-stale", "snapshot-geometry-stale", "snapshot-adopt", "snapshot-width",
  "alt-scroll", "alt-scroll-page-dead", "alt-scroll-verdict-restored",
];
const LEGACY: ReadonlySet<string> = new Set(LEGACY_DIAG_KINDS);

/** Уходит ли diag этого вида в сокет: прежние — всегда, новые — только новому агенту. */
export function diagReachesAgent(what: string, peerIsNew: boolean): boolean {
  return peerIsNew || LEGACY.has(what);
}

/** Поля /state, по которым агент виден как новый (их нет у 2.71.1). */
export function stateShowsNewAgent(state: Record<string, unknown> | null | undefined): boolean {
  if (!state || typeof state !== "object") return false;
  return typeof state.fg_started === "number" || state.history_retention === "honor" || state.history_retention === "preserve";
}
