/**
 * Адресная строка виртуального браузера: то, что человек набрал, превращаем в
 * URL так же, как это делает настоящий браузер. Правило отдельно от компонента
 * и под тестами — гадать его на телефоне пришлось бы руками.
 */

/**
 * «Поиск или адрес»: пробелы или отсутствие точки — это запрос в поисковик,
 * иначе — адрес, которому не хватает только схемы. Явную схему (http://,
 * about:blank, localhost:8080) уважаем как есть.
 */
export function searchOrUrl(input: string): string {
  const text = input.trim();
  if (!text) return "";
  // Уже полный адрес (схема://) или служебная форма scheme:... — не трогаем.
  if (/^[a-z][a-z0-9+.-]*:/i.test(text)) return text;
  // Пробел в адресе невозможен, а запрос без пробелов, но без точки — тоже
  // запрос: «слон» ищут, а «слон.ру» открывают.
  if (/\s/.test(text) || !text.includes(".")) {
    return `https://www.google.com/search?q=${encodeURIComponent(text)}`;
  }
  return `https://${text}`;
}

/** Первая буква хоста для аватарки вкладки (дёшево, без фавиконок). */
export function tabLetter(host: string, title: string): string {
  const from = (host || title).trim();
  return from ? from[0].toUpperCase() : "•";
}

/** Стабильный цвет аватарки вкладки по имени хоста: одинаковый сайт —
 *  одинаковый кружок, и никаких запросов за картинками. */
export function tabAvatarColor(host: string): string {
  let hash = 0;
  for (let i = 0; i < host.length; i++) hash = (hash * 31 + host.charCodeAt(i)) >>> 0;
  return `hsl(${hash % 360} 45% 32%)`;
}
