export type InstallOS = "windows" | "macos" | "linux";

/** The target belongs to the computer being connected, not the controller. */
export function installOS(value: string | null): InstallOS {
  return value === "macos" || value === "linux" ? value : "windows";
}

export function pairInstructions(os: InstallOS, server: boolean, handheld: boolean) {
  if (server) return {
    text: "Во время установки в терминале появятся код подключения и QR. Оставьте этот терминал открытым и добавьте машину в аккаунт на следующем шаге. Установщик дождётся привязки и покажет результат.",
    recovery: "Если код истёк или вы закрыли терминал, запустите повторную привязку командой remotai pair от того же пользователя. Если команда не найдена, используйте полный путь к remotai из вывода установщика.",
  };
  if (handheld) return {
    text: "Пройдите установку на нужном компьютере по переданной ссылке. Код подключения появится на нём. Держите код перед собой и продолжайте на этом устройстве.",
    recovery: "Если код истёк, получите новый на подключаемом компьютере по инструкции для его системы.",
  };
  return {
    text: "В окне Remotai выберите «Подключить этот компьютер», затем «Через интернет». Появятся код и QR.",
    recovery: `Если закрыли окно или код истёк, откройте Remotai ${os === "macos" ? "из папки «Программы»" : os === "linux" ? "из меню приложений" : "из меню «Пуск»"} и получите новый код в «Панели ПК».`,
  };
}
