// Вынесено из apk/src/components/icons.tsx, чтобы общие компоненты
// (FolderNavSheet) не тащили весь набор SVG-иконок.

/** Русские имена для серверных quick-папок (сервер шлёт EN-ключи). */
export function folderLabel(name: string): string {
  switch (name) {
    case "CWD": return "Папка Remotai";
    case "Desktop": return "Рабочий стол";
    case "Downloads": return "Загрузки";
    case "Documents": return "Документы";
    case "Home": return "Домашняя";
    default: return name;
  }
}
