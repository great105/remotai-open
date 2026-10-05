import { t } from "../i18n";
import { createCheckout } from "../api-endpoints";
import { useToast } from "./Toast";
import { useLicense } from "../hooks/useLicense";

interface Props {
  feature: string;
  requiredTier?: "pro" | "team";
  compact?: boolean;
}

export function UpgradePrompt({ feature, requiredTier = "pro", compact }: Props) {
  const { toastError } = useToast();
  const { license } = useLicense();
  // Имя полки — из словаря тарифов («Про» / «Флит»), не латиницей: один словарь
  // на всё приложение (аудит ИА 02.09.2026, P0-8).
  const tierLabel = t(requiredTier === "team" ? "license.team" : "license.pro");
  const checkoutKey = requiredTier === "team" ? "license.checkout.team" : "license.checkout.pro";

  const handleUpgrade = async () => {
    try {
      const { url } = await createCheckout(requiredTier, false);
      if (url) window.open(url, "_blank");
    } catch {
      toastError(t("license.error.checkout"));
    }
  };

  // Бета: фичи открыты, оплата выключена — показываем информирующий бейдж,
  // чтобы граница платного была видна с первого дня.
  if (license?.beta) {
    return <span className="upgrade-badge">{t("license.beta.badge")}</span>;
  }

  if (compact) {
    return (
      <span className="upgrade-badge" onClick={handleUpgrade}>
        {tierLabel}
      </span>
    );
  }

  return (
    <div className="upgrade-prompt">
      <div className="upgrade-prompt-icon">🔒</div>
      <div className="upgrade-prompt-text">
        <strong>{feature}</strong>
        <span>{t("license.upgrade.required", { tier: tierLabel })}</span>
      </div>
      <button className="upgrade-prompt-btn" onClick={handleUpgrade}>
        {t(checkoutKey)}
      </button>
    </div>
  );
}
