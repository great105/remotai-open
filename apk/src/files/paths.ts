/**
 * Пути файлового менеджера: Windows и POSIX одновременно.
 *
 * Телефон работает с чужой файловой системой, и правила её путей — не деталь
 * разметки: тем же кодом сравниваются папки («я уже здесь?»), собираются
 * адреса файлов и считается родитель для кнопки «вверх». Ошибка здесь тихая —
 * человек просто попадает не туда, поэтому правила вынесены из
 * `FilesView.tsx` (2293 строки) и проверяются тестом.
 */

/**
 * Ключ тома, чтобы не дёргать disk-info на каждую папку: на Windows это буква
 * диска, на UNC — \\server\share, на POSIX — первый сегмент пути (для корней
 * монтирования — два, иначе /mnt/a и /mnt/b слиплись бы в один ключ).
 */
export function volumeKey(p: string): string {
  const drive = /^([A-Za-z]):/.exec(p);
  if (drive) return drive[1].toUpperCase() + ":";
  if (p.startsWith("\\\\") || p.startsWith("//")) {
    return p.split(/[/\\]/).filter(Boolean).slice(0, 2).join("/");
  }
  const seg = p.split("/").filter(Boolean);
  if (seg.length === 0) return "/";
  const depth = seg[0] === "mnt" || seg[0] === "media" || seg[0] === "Volumes" ? 2 : 1;
  return "/" + seg.slice(0, depth).join("/");
}

/**
 * Один и тот же путь, записанный по-разному (хвостовой слэш, регистр буквы
 * диска), обязан считаться одним и тем же местом — иначе «Вставить» в текущую
 * папку выглядит как перенос в другую.
 */
export function samePathKey(p: string): string {
  const trimmed = p.length > 1 ? p.replace(/[\\/]+$/, "") : p;
  return (trimmed || p).toLowerCase();
}

/** Путь до файла внутри папки — в разделителях самой папки. */
export function joinPath(dir: string, name: string): string {
  const sep = dir.includes("\\") && !dir.includes("/") ? "\\" : "/";
  return dir.endsWith("/") || dir.endsWith("\\") ? dir + name : dir + sep + name;
}

/** Родительская папка. Корни (C:\ и /) остаются корнями, а не пустой строкой. */
export function parentOf(path: string): string {
  const trimmed = path.replace(/[\\/]+$/, "");
  const idx = Math.max(trimmed.lastIndexOf("/"), trimmed.lastIndexOf("\\"));
  if (idx < 0) return path;
  // Сохраняем корень C:\ и POSIX /.
  if (idx === 2 && /^[A-Za-z]:/.test(trimmed)) return trimmed.slice(0, 3);
  if (idx === 0) return "/";
  return trimmed.slice(0, idx);
}
