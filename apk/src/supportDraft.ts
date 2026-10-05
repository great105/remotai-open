/**
 * Черновик обращения в поддержку — один ключ на всё приложение.
 *
 * Раньше ключ жил внутри `SupportView`, и это было верно, пока писать в него
 * умел только сам экран. Теперь черновик заполняет ещё и карточка тарифа
 * («Оставить заявку на Про»), поэтому ключ и правило стали общими: две копии
 * строки `"support.draft"` разъехались бы на первой же правке.
 */
export const SUPPORT_DRAFT_KEY = "support.draft";

export function readSupportDraft(): string {
  try {
    return localStorage.getItem(SUPPORT_DRAFT_KEY) || "";
  } catch {
    return "";
  }
}

export function writeSupportDraft(text: string): void {
  try {
    if (text) localStorage.setItem(SUPPORT_DRAFT_KEY, text);
    else localStorage.removeItem(SUPPORT_DRAFT_KEY);
  } catch { /* квота или приватное окно */ }
}

/**
 * Положить заготовку обращения, НЕ затирая начатое человеком.
 *
 * Возвращает то, что окажется в поле. Уже набранный текст важнее нашей
 * заготовки: потерять чужой черновик хуже, чем не подставить свой.
 */
export function prefillSupportDraft(text: string): string {
  const existing = readSupportDraft().trim();
  if (existing) return existing;
  writeSupportDraft(text);
  return text;
}
