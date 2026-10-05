import { getLocale } from "../locale";
import { useState } from "react";
import { t } from "../i18n";
import { useLicense } from "../hooks/useLicense";

export interface AccountLicenseInfo {
  tier: string;
  effective_tier?: string;
  beta?: boolean;
  billing_enabled?: boolean;
  devices_count: number;
  max_devices: number;
  /** Данные прочитаны из кэша, потому что облако не ответило (apk/cloud/api.ts). */
  cached_at?: number;
  /**
   * Цена платного плана в МИНОРНЫХ единицах (копейки/центы) и её валюта — так
   * её считает касса и так же отдаёт /api/license/pricing. Пока идёт бета,
   * релей эти поля не шлёт, и число на экране не появляется: выдумывать цену в
   * клиенте нельзя — она разъедется с реальным чеком.
   */
  price_monthly?: number;
  currency?: string;
  /** Сколько дней пробного Про осталось. Считает релей от первого облачного подключения. */
  trial_days_left?: number;
  /**
   * Работает ли удалённый доступ прямо сейчас. Решает релей (PROD-008) — клиент
   * не выводит это сам из тарифа и пробы, иначе правило разъедется с сервером.
   */
  cloud_allowed?: boolean;
}

/** Цена человеку: из копеек/центов — в «349 ₽/мес», без хвоста «,00». */
function formatPrice(minor: number, currency: string): string {
  try {
    return new Intl.NumberFormat(getLocale(), {
      style: "currency",
      currency,
      maximumFractionDigits: 0,
    }).format(minor / 100);
  } catch {
    // Неизвестный код валюты роняет Intl — лучше голое число, чем пустой экран.
    return String(Math.round(minor / 100));
  }
}

interface Props {
  versionLabel?: string;
  /** Cloud mode reads account truth even when the selected PC is offline. */
  cloud?: boolean;
  account?: AccountLicenseInfo;
  /**
   * Включены ли AI-функции у этого клиента. Матрица продавала «Оркестратор» и
   * «Исследователь» всем подряд, а в интерфейсе их нет и не появится: они
   * скрыты фича-флагом, который включается секретным пятикратным тапом по
   * версии. Обещать в сравнении планов то, чего человек не найдёт, нельзя —
   * поэтому эти две строки показываем только когда флаг уже включён (N12).
   */
  ai?: boolean;
}

function tierName(tier: string): string {
  const key = `license.${tier}`;
  const translated = t(key);
  return translated === key ? tier.charAt(0).toUpperCase() + tier.slice(1) : translated;
}

export function LicenseSection({ versionLabel, cloud = false, account, ai = false }: Props) {
  const { license: localLicense } = useLicense(!cloud);
  const [matrixOpen, setMatrixOpen] = useState(false);
  const effectiveTier = account?.effective_tier || account?.tier || localLicense?.effective_tier || localLicense?.tier || "free";
  // Имя полки — из одного словаря тарифов: «Про · бесплатно в бете» здесь то же
  // слово, что и в «Агентах» и в бейдже UpgradePrompt, а не латинское «Pro»
  // (аудит ИА 02.09.2026, P0-8).
  const planLabel = !cloud && !account ? t("license.free") : account?.beta && account.tier === "free" && effectiveTier === "pro"
    ? t("license.beta.badge")
    : tierName(effectiveTier);
  const ready = cloud ? !!account : !!localLicense;
  const devicesUsed = account?.devices_count ?? localLicense?.active_devices?.length;
  /**
   * Предел устройств. `device_slots` заполняет только активация ключа, поэтому у
   * живого агента он часто 0 при `limits.max_devices: 10` — из-за этого в
   * подписке писалось «1 из 0». Ноль и отсутствие значения здесь означают «без
   * ограничений» (так же его читает useLicense.canUse), поэтому берём первый
   * ПОЛОЖИТЕЛЬНЫЙ источник: лимит аккаунта → лимит локальной лицензии →
   * device_slots. Не нашли ни одного — так и говорим, а не выдумываем число.
   */
  const devicesMax = [account?.max_devices, localLicense?.limits?.max_devices, localLicense?.device_slots]
    .find((value): value is number => typeof value === "number" && value > 0);
  /**
   * В матрице колонка «Локально» — это лимит БЕСПЛАТНОГО плана, а не текущего.
   * Подставлять сюда лимит аккаунта можно лишь когда человек и так на нём,
   * иначе у аккаунта «Флит» в колонке «Локально» оказывалось «10 устройств».
   */
  const freeDevicesLabel = t("license.unlimited");
  /**
   * Метка появляется ТОЛЬКО когда данные читаются из кэша (облако не ответило):
   * `cached_at` теперь проставляет лишь ветка фолбэка в getMe. Раньше штамп
   * ставился на каждый удачный запрос, и «Данные аккаунта · на 15:32» висело
   * всегда — человек не мог отличить свежий план от устаревшего (N171).
   */
  const cachedLabel = account?.cached_at
    ? new Date(account.cached_at).toLocaleTimeString(getLocale(), { hour: "2-digit", minute: "2-digit" })
    : "";

  /**
   * Две строки, ради которых «Подписку» и открывают: сколько это стоит и что
   * будет, когда лимит кончится. Раньше экран не отвечал ни на один из этих
   * вопросов — он показывал название плана и «1 из 10», а деньги и последствие
   * человек додумывал сам (§6.5).
   *
   * Про деньги говорим по факту, а не по намерению: биллинг включён и цена
   * пришла — печатаем цену; включён без цены — говорим только то, что знаем
   * наверняка (что стало платным, а что осталось бесплатным); выключен — «идёт
   * бета». Единственный случай, когда о деньгах молчим, — уже оплаченная
   * локальная подписка: про неё выше стоит «Действует до …», и вторая строка
   * про бету прямо ей противоречила бы.
   */
  const betaFree = account?.beta ?? localLicense?.beta ?? false;
  const paidLocalPlan = !cloud && !betaFree && effectiveTier !== "free" && !!localLicense?.valid_until;
  const priceNote = account?.billing_enabled
    ? (typeof account.price_monthly === "number"
        ? t("license.pricing.monthly", { price: formatPrice(account.price_monthly, account.currency || "RUB") })
        : t("license.pricePaid"))
    : paidLocalPlan
      ? ""
      : t("license.priceNow");
  const limitNote = devicesMax ? t("license.devicesLimitNote", { max: devicesMax }) : "";
  /**
   * Печатается ОДИН раз на экране: под карточкой подписки, а если карточки нет
   * (данные ещё не приехали) — под «Сравнением планов», где вопрос про деньги
   * возникает ровно так же.
   */
  /**
   * Граница денег объясняется ВСЕГДА, а не только когда включена касса: человек,
   * упёршийся в «удалённый доступ выключен», должен тут же прочитать, что дома
   * ничего не изменилось. Тому, у кого удалёнка выключена, объяснение идёт
   * первым — это его текущее состояние, а не сноска.
   */
  const cloudOff = account?.cloud_allowed === false;
  const planNote = priceNote || limitNote || account ? (
    <div className="settings-advanced-hint">
      {cloudOff && <div style={{ marginBottom: 6 }}>{t("license.cloudOffNote")}</div>}
      {priceNote && <div>{priceNote}</div>}
      {limitNote && <div style={{ marginTop: priceNote ? 6 : 0 }}>{limitNote}</div>}
      {account && <div style={{ marginTop: 6 }}>{t("license.homeFree")}</div>}
    </div>
  ) : null;

  return (
    <>
      {ready && (
        <div className="setting-group">
          <div className="setting-label">{t("license.title")}</div>
          <div className="settings-info-card">
            <div className="settings-info-row">
              <span className="settings-info-label">{t("license.currentPlan")}</span>
              <span className="settings-info-value" style={{ fontWeight: 600, color: effectiveTier === "free" ? "var(--tg-hint)" : effectiveTier === "pro" ? "#58a6ff" : "#3fb950" }}>
                {planLabel}
              </span>
            </div>
            {typeof devicesUsed === "number" && (
              <div className="settings-info-row">
                <span className="settings-info-label">{t("license.devices")}</span>
                <span className="settings-info-value">
                  {devicesMax
                    ? t("license.devicesUsed", { used: devicesUsed, total: devicesMax })
                    : t("license.devicesUnlimited", { used: devicesUsed })}
                </span>
              </div>
            )}
            {/* Проба: человек должен видеть срок ДО того, как он кончится, —
                иначе «перестало работать» приходит без предупреждения. */}
            {typeof account?.trial_days_left === "number" && account.trial_days_left > 0 && (
              <div className="settings-info-row">
                <span className="settings-info-label">
                  {account.trial_days_left === 1
                    ? t("license.trialLeftOne")
                    : t("license.trialLeft", { days: account.trial_days_left })}
                </span>
              </div>
            )}
            {/* Работает ли удалённое подключение — главный факт этого экрана.
                Ответ берём у сервера, а не выводим из тарифа своими силами. */}
            {typeof account?.cloud_allowed === "boolean" && (
              <div className="settings-info-row">
                <span className="settings-info-label">
                  {account.cloud_allowed ? t("license.cloudOn") : t("license.cloudOff")}
                </span>
              </div>
            )}
            {cachedLabel && (
              <div className="settings-info-row">
                <span className="settings-info-label">{t("license.cachedOffline", { time: cachedLabel })}</span>
              </div>
            )}
            {localLicense?.valid_until && !cloud && (
              <div className="settings-info-row">
                <span className="settings-info-label">
                  {localLicense.cancelled_at
                    ? t("license.cancelled", { date: new Date(localLicense.valid_until).toLocaleDateString(getLocale()) })
                    : t("license.validUntil", { date: new Date(localLicense.valid_until).toLocaleDateString(getLocale()) })}
                </span>
              </div>
            )}
            {versionLabel && (
              <div className="settings-info-row">
                <span className="settings-info-label">{t("license.version")}</span>
                <span className="settings-info-value" style={{ fontSize: 12 }}>{versionLabel}</span>
              </div>
            )}
          </div>
          {planNote}
        </div>
      )}

      <div className="setting-group">
        <button
          className="settings-advanced-head"
          onClick={() => setMatrixOpen((value) => !value)}
          aria-expanded={matrixOpen}
        >
          <span className="settings-advanced-title">{t("license.matrix.title")}</span>
          <span className={`settings-advanced-chevron${matrixOpen ? " open" : ""}`} aria-hidden>{"›"}</span>
        </button>
        {!ready && planNote}
        {matrixOpen && (
          <div className="feature-matrix" style={{ marginTop: 8 }}>
            <div className="feature-matrix-header">
              <span className="feature-matrix-cell feature-name" />
              {/* Заголовки колонок — имена полок из словаря (license.free /
                  license.pro), а не латинские литералы: «Free» и «Pro» здесь
                  были вторым словарём тарифов (аудит ИА 02.09.2026, P0-8). */}
              <span className="feature-matrix-cell tier-label">{t("license.free")}</span>
              <span className="feature-matrix-cell tier-label">{t("license.pro")}</span>
            </div>
            {[
              // «12fps 1280p» человеку не говорит ничего: он выбирает не кадры и
              // не пиксели, а то, как будет выглядеть экран его компьютера на
              // телефоне. Пишем результат словами (§6.5).
              { key: "remoteDesktop", free: t("license.free"), pro: t("license.matrix.remotePro") },
              { key: "terminals", free: t("license.unlimited"), pro: t("license.unlimited") },
              { key: "servers", free: "—", pro: "✓" },
              { key: "fileWrite", free: "✓", pro: "✓" },
              { key: "allAgents", free: "✓", pro: "✓" },
              // Оркестратор и Исследователь живут за фича-флагом AI: пока он
              // выключен, этих экранов в приложении нет — и в сравнении планов
              // им тоже не место (N12).
              ...(ai
                ? [
                    { key: "orchestrator", free: "—", pro: "✓" },
                    { key: "researcher", free: "—", pro: "✓" },
                  ]
                : []),
              { key: "devices", free: freeDevicesLabel, pro: "5" },
            ].map((row) => (
              <div className="feature-matrix-row" key={row.key}>
                <span className="feature-matrix-cell feature-name">{t(`license.matrix.${row.key}`)}</span>
                <span className={`feature-matrix-cell ${effectiveTier === "free" ? "current-tier" : ""}`}>{row.free}</span>
                <span className={`feature-matrix-cell ${effectiveTier !== "free" ? "current-tier" : ""}`}>{row.pro}</span>
              </div>
            ))}
          </div>
        )}
      </div>
    </>
  );
}
