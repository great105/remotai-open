/**
 * Куда можно уйти по адресу из ссылки — один список на весь клиент.
 *
 * Правило было записано выражением внутри `InfrastructureView` и понадобилось
 * второй раз: чип машины теперь передаёт «вернуться сюда после переключения»
 * (`?back=`), и вторая копия регулярного выражения разъехалась бы с первой на
 * ближайшей правке. Проверять снаружи React заодно стало можно тестом.
 *
 * Пускаем ТОЛЬКО собственные экраны: известный раздел плюс необязательный один
 * сегмент-идентификатор (терминал, SSH-хост, сессия). Строку запроса
 * (`ssh=1`, `host=`, `cwd=`) переносим как есть — её читают наши же экраны.
 * Всё прочее — внешний адрес, «//», якорь — уводит на главную.
 */
// `agents`, `account`, `plan`, `settings`, `guide` добавлены после аудита ИА
// 02.09.2026: без них цель `?next=/plan` (возврат ЮKassa, «Подписка» из
// настроек) резалась до главной, а возврат через чип машины с экрана «Агенты»
// приводил не в «Агенты», а на главную.
const ALLOWED = /^\/(?:infrastructure|devices|start|files|system|remote|support|panel|usage|agents|account|plan|settings|guide|sessions|ssh-files|ssh|pty|session|research)(?:\/[^/?#]+)?$/;

/** Разрешённый путь назначения или "/" — второго ответа тут быть не должно. */
export function safeNextPath(raw: string | null | undefined): string {
  const value = raw || "";
  if (!value || value.includes("#")) return "/";
  const cut = value.indexOf("?");
  const path = cut < 0 ? value : value.slice(0, cut);
  const query = cut < 0 ? "" : value.slice(cut);
  return ALLOWED.test(path) ? path + query : "/";
}

/** Цель входа переживает перезагрузку вкладки и возврат из Telegram/OAuth. */
export function loginNextPath(state: unknown, search: string): string {
  const next = state && typeof state === "object" && "next" in state ? state.next : undefined;
  return safeNextPath(typeof next === "string" ? next : new URLSearchParams(search).get("next"));
}
