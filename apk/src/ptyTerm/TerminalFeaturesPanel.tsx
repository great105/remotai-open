/**
 * «Функции терминала» (план 9.1, волна 4): откат отдельной функции без выпуска
 * — и на телефоне. Раньше ключ remotai.terminal.features.v1 можно было записать
 * только через DevTools, а в APK и Telegram их нет: «откат без выпуска» был
 * неправдой на главной цели плана.
 *
 * Экран для поддержки: все переключатели terminalFeatures() с тем, что
 * действует в ЭТОМ открытом терминале, и тем, что записано. Запись — в тот же
 * ключ (TerminalControls.writeTerminalFeatures: только отличия от умолчаний),
 * применяется при следующем открытии терминала — живых таймеров старого пути в
 * открытом экземпляре не появляется (правило 9.1 «на согласованной границе»).
 */
import { useState } from "react";
import { t } from "../i18n";
import {
  FEATURE_CHOICES, FEATURE_ORDER, defaultTerminalFeatures, resetTerminalFeatures, terminalFeatures, writeTerminalFeatures,
  type FeatureName, type TerminalFeatures,
} from "./runtime/TerminalControls";

/** Подписи — литералами (scripts/check-i18n.mjs видит ключи в таблице). */
const FEATURE_LABELS: Record<FeatureName, string> = {
  trace: "pty.featureTrace",
  recoveryV1: "pty.featureRecoveryV1",
  retention: "pty.featureRetention",
  presentation: "pty.featurePresentation",
  navigation: "pty.featureNavigation",
  capacity: "pty.featureCapacity",
  occlusion: "pty.featureOcclusion",
  flowBacklog: "pty.featureFlowBacklog",
  inputSafety: "pty.featureInputSafety",
  tapClickOnce: "pty.featureTapClickOnce",
  sizeOwner: "pty.featureSizeOwner",
  agentHistory: "pty.featureAgentHistory",
  commandBlocks: "pty.featureCommandBlocks",
  viewportPan: "pty.featureViewportPan",
};
const VALUE_LABELS: Record<string, string> = {
  policy: "pty.featureValuePolicy",
  v2: "pty.featureValueV2",
  legacy: "pty.featureValueLegacy",
  shadow: "pty.featureValueShadow",
};

function featureStorage(): Storage | null {
  try { return typeof localStorage === "undefined" ? null : localStorage; } catch { return null; }
}

export function TerminalFeaturesPanel({ active, onBack, onToast }: {
  /** Что действует в открытом терминале (прочитано при его открытии). */
  active: TerminalFeatures;
  onBack: () => void;
  onToast: (text: string) => void;
}) {
  const [stored, setStored] = useState<TerminalFeatures>(() => terminalFeatures(featureStorage() ?? undefined));
  const save = (next: TerminalFeatures) => {
    if (!writeTerminalFeatures(featureStorage(), next)) { onToast(t("pty.featuresSaveFailed")); return; }
    setStored(terminalFeatures(featureStorage() ?? undefined));
    onToast(t("pty.featuresSaved"));
  };
  const reset = () => {
    if (!resetTerminalFeatures(featureStorage())) { onToast(t("pty.featuresSaveFailed")); return; }
    setStored(defaultTerminalFeatures());
    onToast(t("pty.featuresResetDone"));
  };
  return (
    <>
      <p className="pty-trace-note">{t("pty.featuresNote")}</p>
      <div className="pty-features-list">
        {FEATURE_ORDER.map((name) => {
          const value = stored[name];
          const pending = value !== active[name];
          const choices = FEATURE_CHOICES[name];
          return (
            <div key={name} className="pty-feature-row" data-feature={name} data-pending={pending ? "1" : "0"}>
              {choices ? (
                <label className="pty-feature-choice">
                  <span>{t(FEATURE_LABELS[name])}</span>
                  <select
                    value={String(value)}
                    onChange={(e) => save({ ...stored, [name]: e.target.value } as TerminalFeatures)}
                  >
                    {choices.map((choice) => (
                      <option key={choice} value={choice}>{`${choice} — ${t(VALUE_LABELS[choice])}`}</option>
                    ))}
                  </select>
                </label>
              ) : (
                <label className="pty-trace-capture">
                  <input
                    type="checkbox"
                    checked={value === true}
                    onChange={(e) => save({ ...stored, [name]: e.target.checked } as TerminalFeatures)}
                  />
                  <span>{t(FEATURE_LABELS[name])}</span>
                </label>
              )}
              {pending && <span className="pty-feature-pending">{t("pty.featuresPending")}</span>}
            </div>
          );
        })}
      </div>
      <button className="pty-export-option" onClick={reset}>{"↺ "}{t("pty.featuresReset")}</button>
      <button className="pty-export-option" onClick={onBack}>{"← "}{t("pty.traceBack")}</button>
    </>
  );
}
