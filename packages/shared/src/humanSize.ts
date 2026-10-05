// Человеческий размер в байтах — ОДИН формат на всё приложение.
//
// Копий этой функции было четыре (Файлы, SFTP-файлы, «Система», шторка
// скачивания), и все печатали латинские «B/KB/MB/GB» с точкой как десятичным
// разделителем: «21.5 GB / 31.2 GB», «174 B». В русском интерфейсе это чужие
// единицы и чужая пунктуация, поэтому формат один: «174 Б», «1,4 КБ»,
// «21,5 ГБ». Единицы — из словаря, а не зашиты в код.
import { t } from "./i18n";

const UNIT_KEYS = ["unit.bytes", "unit.kb", "unit.mb", "unit.gb", "unit.tb"] as const;

/**
 * Размер файла, папки или тома по-русски.
 *
 * null/undefined/не-число → пустая строка: сервер отдаёт `size: null` для папок
 * и для файлов, которые не смог прочитать, — рисовать там «0 Б» значит врать.
 */
export function humanSize(bytes: number | null | undefined): string {
  if (bytes === null || bytes === undefined || !Number.isFinite(bytes)) return "";
  const sign = bytes < 0 ? "−" : "";
  let value = Math.abs(bytes);
  let unit = 0;
  while (value >= 1024 && unit < UNIT_KEYS.length - 1) {
    value /= 1024;
    unit++;
  }
  // Округление до одного знака могло дать «1024 КБ» — это уже мегабайт.
  if (unit < UNIT_KEYS.length - 1 && Number(value.toFixed(1)) >= 1024) {
    value /= 1024;
    unit++;
  }
  // Байты — целыми: «174,0 Б» читается как сбой. Дальше один знак после
  // запятой, но без хвостового «,0» — «512 МБ», а не «512,0 МБ».
  const num = unit === 0
    ? String(Math.round(value))
    : value.toFixed(1).replace(/\.0$/, "").replace(".", ",");
  return `${sign}${num} ${t(UNIT_KEYS[unit])}`;
}
