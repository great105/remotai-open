import { t } from "@tgcontrol/shared";
export type InstallOS = "windows" | "macos" | "linux";

/** The target belongs to the computer being connected, not the controller. */
export function installOS(value: string | null): InstallOS {
  return value === "macos" || value === "linux" ? value : "windows";
}

export function pairInstructions(os: InstallOS, server: boolean, handheld: boolean) {
  if (server) return {
    text: t("ui.installflow.mefbaf7eb64"),
    recovery: t("ui.installflow.m45532df913"),
  };
  if (handheld) return {
    text: t("ui.installflow.m18ec02b6ec"),
    recovery: t("ui.installflow.m3e22ecc28e"),
  };
  return {
    text: t("ui.installflow.mc7ac6d42d9"),
    recovery: t("ui.installflow.m3ccfca79d3", { p0: (os === "macos" ? t("ui.installflow.m74418780d0") : os === "linux" ? t("ui.installflow.m3db95580a9") : t("ui.installflow.m659833abc2")) }),
  };
}
