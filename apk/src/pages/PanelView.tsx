import { BottomNav } from "../components/BottomNav";
import { useGoBack } from "../navBack";
import { sectionName } from "../sections";
import { t } from "../i18n";
import { haptic } from "../telegram";

/**
 * «Панель ПК» — управление этим компьютером: подключение телефона (QR/код),
 * обновление приложения, доступ с других устройств, отвязка телефонов.
 *
 * Не дублируем логику панели — встраиваем готовую страницу `/setup` во фрейме.
 * Клиент в окне exe и сама панель живут на одном loopback-origin
 * (`localhost:<порт>`), поэтому фрейм same-origin и его loopback-only
 * эндпоинты проходят. Вкладка, ведущая сюда, показывается только на on-PC
 * клиенте (см. isOnPCPanel в config.ts), так что фрейм здесь всегда валиден.
 *
 * ⚠ Шапка со своим заголовком и «←» — не украшение. Обход карты 04.09.2026
 * показал `/panel` единственным достижимым экраном с заголовком «(нет)»: фрейм
 * занимал экран целиком, и человек, попавший сюда, не получал ответа ни на
 * «где я», ни на «как назад» — выйти можно было только через нижнюю панель
 * (аудит ИА 02.09.2026, P1-7 — «фрейм без шапки и „←“»). Имя берём из
 * `sections.ts`, как все: одно место — одно имя.
 */
export function PanelView() {
  const stepBack = useGoBack();
  return (
    <div className="page panel-page">
      <div className="page-header">
        <button className="back-btn" aria-label={t("generic.back")} onClick={() => { haptic(); stepBack(); }}>
          {"←"}
        </button>
        <h1>{sectionName("panel")}</h1>
      </div>
      <iframe
        className="panel-frame"
        src="/setup?force=1&embed=1"
        title={sectionName("panel")}
      />
      <BottomNav active="panel" />
    </div>
  );
}
