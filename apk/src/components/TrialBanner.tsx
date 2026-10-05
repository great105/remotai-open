import { useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";
import { t } from "../i18n";
import { getMode } from "../config";
import { getMe } from "../cloud/api";
import { planState, trialBannerOnTop, trialNotice, type TrialNotice } from "../plan";
import { haptic } from "../telegram";

/**
 * «Проба заканчивается» — там, где человек бывает каждый день.
 *
 * До этой плашки о конце пробного Про говорил ТОЛЬКО экран «Агенты» (аудит
 * путей 29.08.2026). В конце пробы туда не заходят, поэтому первым известием
 * становилось молчаливое выключение удалённого доступа.
 *
 * Три правила:
 *  1. Не раньше, чем за неделю (`trialNotice`): плашка, висящая месяц, к
 *     нужному дню превращается в мебель.
 *  2. Один ход, и он ведёт туда, где можно что-то сделать, — на экран тарифа.
 *  3. В последний день тон другой: это уже не напоминание, а срок. Тогда же
 *     плашка переезжает НАВЕРХ (`trialBannerOnTop`): внизу её кнопка уходила
 *     под нижнюю панель, и заплатить было нечем — замер `probe-trial-over-cta`.
 *
 * Экран рисует плашку в двух местах и передаёт `place`; показывается ровно
 * одно из них. Так место зависит от состояния, а состояние остаётся здесь и не
 * поднимается в главную ради одной строки.
 *
 * Отказ запроса молчаливый: главная не место для «не удалось получить профиль».
 */
export function TrialBanner({ place = "bottom" }: { place?: "top" | "bottom" }) {
  const navigate = useNavigate();
  const [notice, setNotice] = useState<TrialNotice | null>(null);

  useEffect(() => {
    if (getMode() !== "cloud") return;
    let cancelled = false;
    getMe()
      .then((me) => { if (!cancelled) setNotice(trialNotice(planState(me))); })
      .catch(() => { /* профиль не приехал — молчим */ });
    return () => { cancelled = true; };
  }, []);

  if (!notice?.show) return null;
  if (trialBannerOnTop(notice) !== (place === "top")) return null;

  return (
    <div className={`home-trial${notice.urgent ? " urgent" : ""}`}>
      <b className="home-trial-title">
        {notice.ended
          ? t("home.trial.ended")
          : notice.days <= 1
            ? t("home.trial.lastDay")
            : t("home.trial.left", { days: String(notice.days) })}
      </b>
      <p className="home-trial-note">
        {notice.ended ? t("home.trial.endedNote") : t("home.trial.note")}
      </p>
      <button
        className="btn btn-sm btn-primary"
        // Баннер ведёт туда, где можно ЗАПЛАТИТЬ, а не в раздел про ИИ-агентов:
        // человек нажимает его именно с намерением продлить.
        onClick={() => { haptic(); navigate("/plan"); }}
      >
        {t("home.trial.btn")}
      </button>
    </div>
  );
}
