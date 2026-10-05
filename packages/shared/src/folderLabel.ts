import { t } from "./i18n";
// Вынесено из apk/src/components/icons.tsx, чтобы общие компоненты
// (FolderNavSheet) не тащили весь набор SVG-иконок.

/** Русские имена для серверных quick-папок (сервер шлёт EN-ключи). */
export function folderLabel(name: string): string {
  switch (name) {
    case "CWD": return t("ui.folderlabel.mb4120c437d");
    case "Desktop": return t("ui.folderlabel.m651d54bb32");
    case "Downloads": return t("remote.vbDownloads");
    case "Documents": return t("ui.folderlabel.m9b19ad0c85");
    case "Home": return t("ui.folderlabel.m4d50e06534");
    default: return name;
  }
}
