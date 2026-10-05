import { useState, type ReactNode } from "react";
import { t } from "../i18n";
import { haptic } from "../telegram";
import { currentInstallHint, snoozeInstallHint, type IosInstallHint } from "../installPwa";
import { IconIosShare } from "./icons";

/**
 * «Поставьте на экран „Домой“» — единственное место, где владелец iPhone узнаёт,
 * что у него может быть приложение с иконкой, а не вкладка браузера.
 *
 * Safari кнопку «Установить» не показывает и подсказок не даёт: без этой плашки
 * PWA-часть продукта (manifest, standalone, иконки — всё уже собрано) для
 * человека просто не существует. Показываем на двух экранах: вход (там это
 * важнее всего — см. ниже) и главная.
 *
 * `place="login"` добавляет строку про порядок действий. У приложения с главного
 * экрана ОТДЕЛЬНОЕ от Safari хранилище: если человек сначала войдёт в браузере,
 * а установит потом, в установленном приложении он окажется разлогинен и
 * прочтёт это как поломку, а не как устройство iOS.
 */
export function InstallPwaBanner({ place = "home" }: { place?: "home" | "login" }) {
  // Считаем один раз при монтировании: UA и display-mode за жизнь экрана не
  // меняются, а перерисовки здесь ни к чему.
  const [hint, setHint] = useState<IosInstallHint>(() => currentInstallHint());
  if (!hint) return null;

  // Обёртка живёт ЗДЕСЬ, а не в вызывающем экране: иначе на всех не-iOS
  // устройствах (то есть почти всегда) на главной оставался пустой div с
  // отступом — «дырка» над рабочей зоной без единого видимого элемента.
  const wrap = (inner: ReactNode) => (
    <div className={place === "login" ? "install-pwa-wrap login" : "install-pwa-wrap"}>{inner}</div>
  );

  const later = (
    <button
      className="app-update-later"
      onClick={() => { haptic(); snoozeInstallHint(); setHint(null); }}
    >
      {t("installPwa.later")}
    </button>
  );

  if (hint === "open-in-safari") {
    return wrap(
      <div className="install-pwa-banner">
        <div className="install-pwa-text">
          <b>{t("installPwa.safariTitle")}</b>
          <small>{t("installPwa.safariHint")}</small>
        </div>
        {later}
      </div>,
    );
  }

  return wrap(
    <div className="install-pwa-banner">
      <div className="install-pwa-text">
        <b>{t("installPwa.title")}</b>
        <small>{t("installPwa.subtitle")}</small>
        <ol className="install-pwa-steps">
          <li>
            {t("installPwa.step1before")}
            <IconIosShare size={18} className="install-pwa-share" />
            {t("installPwa.step1after")}
          </li>
          <li>{t("installPwa.step2")}</li>
          <li>{t("installPwa.step3")}</li>
        </ol>
        {place === "login" && <small className="install-pwa-order">{t("installPwa.beforeLogin")}</small>}
      </div>
      {later}
    </div>
  );
}
