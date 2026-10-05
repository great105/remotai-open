import { getLocale } from "@tgcontrol/shared";
import { useEffect, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import { mapApiError } from "@tgcontrol/shared";
import { t } from "../i18n";
import { BottomNav } from "../components/BottomNav";
import { useGoBack } from "../navBack";
import { haptic } from "../telegram";
import { openExternalLink } from "../openExternal";
import { QrCode } from "../components/QrCode";
import { getMode, isNativeApp } from "../config";
import { getTelegram } from "../telegram";
import { isAnalyticsEnabled } from "../cloud/support";
import { reachMetrikaGoal } from "../cloud/metrika";
import { trialLine } from "../plan";
import {
  getMe,
  getIdentities,
  listDevices,
  getSubscription,
  unbindCard,
  createCheckout,
  setBillingEmail,
  type CloudMe,
  type CloudIdentity,
  type CloudDevice,
  type CloudSubscription,
} from "../cloud/api";

/**
 * Личный кабинет: аккаунт, устройства и деньги в одном месте.
 *
 * Владелец 01.09.2026: «давай всё вот в личный кабинет — там привязанный
 * telegram, привязанное устройство, оплата, то есть вынесем всё такое».
 *
 * До этого «всё такое» было рассыпано: способы входа — в настройках, устройства
 * — на отдельном экране инфраструктуры, подписка — сперва внутри раздела
 * «Агенты», потом отдельным экраном. Человек, желавший ответить себе на вопрос
 * «что у меня за аккаунт и за что я плачу», обходил три места.
 *
 * Здесь один экран и три ответа подряд: кто я → чем управляю → за что плачу.
 * Маршрут `/plan` оставлен алиасом: на него возвращает ЮKassa после оплаты и
 * ведут прежние ссылки.
 */
export function AccountView() {
  const goBack = useGoBack();
  const navigate = useNavigate();
  const local = getMode() !== "cloud";

  const [me, setMe] = useState<CloudMe | null>(null);
  const [ids, setIds] = useState<CloudIdentity[]>([]);
  const [devices, setDevices] = useState<CloudDevice[]>([]);
  const [sub, setSub] = useState<CloudSubscription | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [loadErrors, setLoadErrors] = useState({ profile: false, identities: false, devices: false, subscription: false });
  const loadSeq = useRef(0);
  const [notYet, setNotYet] = useState(false);
  const [busy, setBusy] = useState(false);
  const [unbound, setUnbound] = useState(false);
  // Почта для чека. Спрашиваем, только если сервер сказал, что не знает её:
  // у большинства она уже сохранена с прошлой оплаты, и поле лишнее.
  const [askEmail, setAskEmail] = useState<string | null>(null);
  const [email, setEmail] = useState("");
  const [paymentMethod, setPaymentMethod] = useState<"sbp" | "bank_card">("sbp");
  const [checkingPayment, setCheckingPayment] = useState(false);
  const [checkoutOpened, setCheckoutOpened] = useState(false);
  // Ссылка кассы остаётся на экране, а не только во вкладке. ⚠ 11.09.2026
  // покупатель с VPN на компьютере шесть раз нажал «Оплатить»: вкладка ЮKassa
  // через VPN не грузилась, без VPN не открывался наш сайт, а ссылку он не
  // видел больше нигде. С QR оплата уходит на телефон; повторное открытие
  // берёт ту же ссылку и не плодит платежи.
  const [checkoutUrl, setCheckoutUrl] = useState<string | null>(null);
  const [linkCopied, setLinkCopied] = useState(false);
  // Оплата как цель Метрики (веб на remotai.ru): `checkout_start` — ушли в
  // кассу, `payment_success` — после возврата срок доступа сдвинулся. Сервер
  // узнаёт об оплате вебхуком, а клиент — только по изменившемуся paid_until,
  // поэтому сравниваем с тем, что было в момент ухода в кассу, и шлём один раз.
  const paidUntilAtCheckoutRef = useRef<string | null | undefined>(undefined);
  const paymentGoalSentRef = useRef(false);
  const metrikaDeps = () => ({
    nativeApp: isNativeApp,
    telegramMiniApp: Boolean(getTelegram()?.initData),
    analyticsEnabled: isAnalyticsEnabled(),
  });
  const notePaymentOutcome = (value: { paid_until: string | null } | null) => {
    if (!value || paidUntilAtCheckoutRef.current === undefined || paymentGoalSentRef.current) return;
    if (value.paid_until && value.paid_until !== paidUntilAtCheckoutRef.current) {
      paymentGoalSentRef.current = true;
      setCheckoutUrl(null);
      reachMetrikaGoal("payment_success", metrikaDeps());
    }
  };

  const load = () => {
    const seq = ++loadSeq.current;
    setLoading(true);
    setNotYet(false);
    setLoadErrors({ profile: false, identities: false, devices: false, subscription: false });
    setError("");
    // Четыре независимых запроса: ни один не должен уронить экран целиком.
    // Аккаунт может быть без устройств, касса — не подключена, и это НЕ ошибки.
    Promise.allSettled([getMe(), getIdentities(), listDevices(), getSubscription()])
      .then(([m, i, d, s]) => {
        if (seq !== loadSeq.current) return;
        setMe(m.status === "fulfilled" ? m.value : null);
        setIds(i.status === "fulfilled" ? i.value.identities || [] : []);
        setDevices(d.status === "fulfilled" ? d.value.devices || [] : []);
        const subscriptionFailed = s.status === "rejected";
        const code = subscriptionFailed ? ((s.reason as any)?.status ?? (s.reason as any)?.code) : null;
        if (s.status === "fulfilled") setSub(s.value);
        else {
          setSub(null);
          if (code === 404 || code === 501) setNotYet(true);
        }
        setLoadErrors({
          profile: m.status === "rejected",
          identities: i.status === "rejected",
          devices: d.status === "rejected",
          subscription: subscriptionFailed && code !== 404 && code !== 501,
        });
      })
      .finally(() => { if (seq === loadSeq.current) setLoading(false); });
  };
  useEffect(load, []);

  // Payment opens outside the app. Returning to the existing tab must refresh
  // access too; the provider's return URL may open a different browser.
  useEffect(() => {
    if (!checkoutOpened) return;
    let alive = true;
    const refresh = () => {
      if (document.visibilityState === "hidden") return;
      void getSubscription().then((value) => { if (alive) { setSub(value); notePaymentOutcome(value); } }).catch(() => {});
    };
    window.addEventListener("focus", refresh);
    document.addEventListener("visibilitychange", refresh);
    return () => {
      alive = false;
      window.removeEventListener("focus", refresh);
      document.removeEventListener("visibilitychange", refresh);
    };
  }, [checkoutOpened]);

  const checkPayment = async () => {
    setCheckingPayment(true);
    setError("");
    try { const value = await getSubscription(); setSub(value); notePaymentOutcome(value); }
    catch (e) { setError(mapApiError(e)); }
    finally { setCheckingPayment(false); }
  };

  // Буфер обмена может быть закрыт (WebView, старый браузер) — ссылка всё
  // равно видна текстом под кодом, поэтому отказ здесь не ошибка.
  const copyCheckout = async () => {
    if (!checkoutUrl) return;
    haptic();
    try { await navigator.clipboard.writeText(checkoutUrl); setLinkCopied(true); }
    catch { /* ссылка видна текстом ниже */ }
  };

  const pay = async (tier: string, withEmail?: string) => {
    if (busy) return;
    haptic();
    setBusy(true);
    try {
      setError("");
      const r = await createCheckout(tier, paymentMethod, withEmail);
      if (r.confirmation_url) {
        setAskEmail(null);
        paidUntilAtCheckoutRef.current = sub?.paid_until ?? null;
        paymentGoalSentRef.current = false;
        reachMetrikaGoal("checkout_start", metrikaDeps());
        setCheckoutOpened(true);
        setCheckoutUrl(r.confirmation_url);
        setLinkCopied(false);
        openExternalLink(r.confirmation_url);
      } else setError(t("billing.noLink"));
    } catch (e: any) {
      // ⚠ Сервер не знает почты — это не сбой, а недостающий шаг. До
      // 01.09.2026 здесь был тупик: отказ показывали текстом, ввести почту
      // было негде, и оплата не проходила ни у кого.
      if (e?.code === "email_required" || e?.code === "email_invalid") {
        setAskEmail(tier);
        setError(e.code === "email_invalid" ? mapApiError(e) : "");
      } else setError(mapApiError(e));
    } finally {
      setBusy(false);
    }
  };

  const saveEmail = async (value: string) => {
    if (busy) return;
    haptic();
    setBusy(true);
    try {
      await setBillingEmail(value);
      setSub((p) => (p ? { ...p, billing_email: value } : p));
      setAskEmail(null);
      setError("");
    } catch (e: any) {
      setError(mapApiError(e));
    } finally {
      setBusy(false);
    }
  };

  const unbind = async () => {
    if (busy) return;
    if (!window.confirm(t("billing.unbindConfirm"))) return;
    haptic();
    setBusy(true);
    try {
      const r = await unbindCard();
      setSub((p) => (p ? { ...p, card: null, auto_renew: false, paid_until: r.paid_until ?? p.paid_until } : p));
      setUnbound(true);
      setError("");
    } catch (e: any) {
      setError(mapApiError(e));
    } finally {
      setBusy(false);
    }
  };

  const paidUntil = sub?.paid_until ? new Date(sub.paid_until).toLocaleDateString(getLocale()) : "";
  const tier = sub?.tier || me?.effective_tier || me?.tier || "free";
  const trial = me && !sub?.founder ? trialLine(me, paidUntil) : null;
  const loadProblem = (messageKey: string) => (
    <div role="alert">
      <p className="plan-error">{t(messageKey)}</p>
      <button type="button" className="btn btn-secondary btn-sm" onClick={() => { haptic(); load(); }}>
        {t("support.retry")}
      </button>
    </div>
  );

  return (
    <div className="page plan-page">
      <div className="page-header">
        <button className="back-btn" aria-label={t("generic.back")} onClick={goBack}>{"←"}</button>
        <h1>{t("account.title")}</h1>
      </div>

      <div className="plan-body">
        {loading && <p className="plan-note">{t("generic.loading")}</p>}

        {/* Локальный режим: облачного аккаунта нет вовсе, и кабинет пуст не
            потому, что сломался. Говорим прямо и не рисуем пустых карточек. */}
        {!loading && local && (
          <section className="card">
            <p>{t("account.localOnly")}</p>
            <p className="plan-note">{t("plan.localFree")}</p>
            <button className="btn btn-primary" onClick={() => navigate("/cloud-login", { state: { next: "/account" } })}>{t("ui.accountview.md57fdc2469")}</button>
          </section>
        )}

        {!loading && !local && (
          <>
            {/* ── Кто я ─────────────────────────────────────────────────── */}
            <section className="card">
              <div className="plan-card-title">{t("account.who")}</div>
              {loadErrors.profile ? loadProblem("account.profileLoadError") : (
                <div className="plan-current-tier">
                  {me?.first_name || me?.username || t("account.noName")}
                </div>
              )}
              {loadErrors.identities ? loadProblem("account.identitiesLoadError") : ids.length > 0 ? (
                <ul className="account-list">
                  {ids.map((i) => (
                    <li key={`${i.provider}:${i.uid}`}>
                      <span className="account-badge">{providerName(i.provider)}</span>
                      <span className="account-line">{i.display || i.uid}</span>
                    </li>
                  ))}
                </ul>
              ) : (
                <p className="plan-note">{t("account.noIdentities")}</p>
              )}
              {/* Не просто «в настройки», а К СПИСКУ ВХОДОВ: он там свёрнут
                  и лежит тремя экранами ниже, и без указания настройки
                  открывались сверху — кнопка выглядела сломанной (аудит ИА
                  02.09.2026, P1-6). SettingsView читает `state.focus`. */}
              <button
                className="btn btn-secondary btn-sm"
                onClick={() => { haptic(); navigate("/settings", { state: { focus: "logins" } }); }}
              >
                {t("account.manageLogins")}
              </button>
            </section>

            {/* ── Чем управляю ──────────────────────────────────────────── */}
            <section className="card">
              <div className="plan-card-title">{t("account.devices")}</div>
              {loadErrors.devices ? loadProblem("account.devicesLoadError") : devices.length > 0 ? (
                <>
                  <ul className="account-list">
                    {devices.slice(0, 6).map((d) => (
                      <li key={d.id}>
                        <span className={`account-dot${d.online ? " on" : ""}`} aria-hidden />
                        <span className="account-line">{d.name || d.hostname}</span>
                      </li>
                    ))}
                  </ul>
                  {me?.max_devices ? (
                    <p className="plan-note">
                      {t("account.devicesCount", { used: String(devices.length), max: String(me.max_devices) })}
                    </p>
                  ) : null}
                </>
              ) : (
                <p className="plan-note">{t("account.noDevices")}</p>
              )}
              <button
                className="btn btn-secondary btn-sm"
                onClick={() => { haptic(); navigate("/infrastructure"); }}
              >
                {t("account.manageDevices")}
              </button>
            </section>

            {/* ── За что плачу ──────────────────────────────────────────── */}
            {loadErrors.subscription ? (
              <section className="card">
                <div className="plan-card-title">{t("plan.current")}</div>
                {loadProblem("billing.offline")}
              </section>
            ) : <section className="card plan-current">
              <div className="plan-card-title">{t("plan.current")}</div>
              <div className="plan-current-tier">
                {me?.self_hosted ? t("plan.selfHosted") : sub?.founder || me?.founder ? t("plan.founder") : trial ? t("plan.tier.trial") : t(`plan.tier.${tier}`)}
              </div>
              {paidUntil && <p className="plan-note">{t("billing.paidUntil", { date: paidUntil })}</p>}
              {/* Проба: живой прогон нового аккаунта 06.09.2026 — релей знал
                  «29 дней, до 6 октября», а кабинет писал одно слово «Про». */}
              {trial && (
                <p className="plan-note plan-trial-note">
                  {trial.days <= 1
                    ? t("plan.trialLastDay")
                    : trial.date
                      ? t("plan.trialUntil", { days: String(trial.days), date: trial.date })
                      : t("plan.trialLeft", { days: String(trial.days) })}
                </p>
              )}
              <p className="plan-note">{t(me?.self_hosted ? "plan.selfHostedNote" : "plan.localFree")}</p>
            </section>}

            {/* Способ оплаты и отвязка — обязательное условие ЮKassa: человек
                должен видеть свою карту и мочь отвязать её сам, в любой момент. */}
            {sub && !sub.self_hosted && (
              <section className="card">
                <div className="plan-card-title">{t("billing.cardTitle")}</div>
                {sub.card ? (
                  <>
                    <div className="plan-card-line">
                      {t("billing.cardBound", { type: sub.card.type || t("ui.accountview.m5736d0caff"), last4: sub.card.last4 })}
                    </div>
                    <p className="plan-note">{sub.auto_renew ? t("billing.autoRenew") : t("billing.noAutoRenew")}</p>
                    <button className="btn btn-secondary btn-sm" onClick={unbind} disabled={busy}>
                      {t("billing.unbind")}
                    </button>
                  </>
                ) : (
                  <p className="plan-note">{unbound ? t("billing.unbindDone") : t("billing.noCard")}</p>
                )}
                {/* Сохранённая почта. Владелец 02.09.2026: «почту надо
                    сохранять, чтобы не вводить 2 раза» — она и сохранялась, но
                    показать это было негде, а невидимое сохранение человек
                    считает отсутствующим. Рядом ход изменить: адрес меняют. */}
                {sub.billing_email && !askEmail && (
                  <>
                    <p className="plan-note">
                      {t("billing.emailSaved", { email: sub.billing_email })}
                    </p>
                    <button
                      className="btn btn-secondary btn-sm"
                      onClick={() => { haptic(); setEmail(sub.billing_email || ""); setAskEmail("change"); }}
                    >
                      {t("billing.emailChange")}
                    </button>
                  </>
                )}
              </section>
            )}

            {/* Почта для чека: появляется ровно тогда, когда сервер её
                запросил, и исчезает после успешного создания платежа. */}
            {askEmail && (
              <section className="card">
                <div className="plan-card-title">{t("billing.emailTitle")}</div>
                <input
                  className="input account-email"
                  type="email"
                  inputMode="email"
                  autoComplete="email"
                  placeholder={t("billing.emailPlaceholder")}
                  aria-label={t("billing.emailTitle")}
                  value={email}
                  onChange={(ev) => setEmail(ev.target.value)}
                />
                <button
                  className="btn btn-primary"
                  disabled={busy || !email.includes("@")}
                  onClick={() => {
                    // «change» — человек правит адрес, а не платит: запоминаем
                    // почту и возвращаем экран, не открывая кассу.
                    if (askEmail === "change") saveEmail(email.trim());
                    else pay(askEmail, email.trim());
                  }}
                >
                  {askEmail === "change" ? t("generic.save") : t("billing.emailContinue")}
                </button>
                <p className="plan-note">{t("billing.emailNote")}</p>
              </section>
            )}

            {checkoutUrl && (
              <section className="card plan-checkout" role="status">
                <div className="plan-card-title">{t("billing.openedTitle")}</div>
                <p className="plan-note">{t("billing.openedNote")}</p>
                <div style={{ display: "flex", justifyContent: "center", margin: "8px 0 12px" }}>
                  <QrCode value={checkoutUrl} size={200} title={t("billing.qrLabel")} />
                </div>
                <p className="plan-note" style={{ wordBreak: "break-all", userSelect: "all" }}>{checkoutUrl}</p>
                <button className="btn btn-secondary" onClick={() => { haptic(); openExternalLink(checkoutUrl); }}>
                  {t("billing.openAgain")}
                </button>
                <button className="btn btn-secondary" style={{ marginTop: 8 }} onClick={copyCheckout}>
                  {linkCopied ? t("billing.linkCopied") : t("billing.copyLink")}
                </button>
                <p className="plan-note">{t("billing.oneLinkNote")}</p>
              </section>
            )}

            {sub && !sub.founder && sub.billing_enabled && (
              <section className="card">
                <div className="plan-card-title">{t("plan.upgrade")}</div>
                <p className="plan-note">{t("billing.fixedPeriod")}</p>
                <label className="plan-note" htmlFor="payment-method">{t("billing.method")}</label>
                <select id="payment-method" className="input" value={paymentMethod}
                  disabled={busy} onChange={(e) => setPaymentMethod(e.target.value as "sbp" | "bank_card")}
                  style={{ display: "block", width: "100%", minHeight: 44, padding: "10px 12px", margin: "6px 0 12px", border: "1px solid var(--tg-hint)", borderRadius: 8, background: "var(--tg-secondary-bg)", color: "var(--tg-text)", font: "inherit" }}>
                  <option value="sbp">{t("billing.methodSbp")}</option>
                  <option value="bank_card">{t("billing.methodCard")}</option>
                </select>
                <button className="btn btn-primary" onClick={() => pay("pro")} disabled={busy}>
                  {t("plan.payPro")}
                </button>
                <button className="btn btn-secondary btn-sm plan-pay-fleet" onClick={() => pay("fleet")} disabled={busy}>
                  {t("plan.payFleet")}
                </button>
                <p className="plan-note">{t("plan.sbpNote")}</p>
                {/* Оплата — акцепт оферты: ссылка обязана быть рядом с кнопкой,
                    а не только в подвале сайта (касса и модерация рекламы). */}
                <p className="plan-note">
                  {t("billing.offerNote")}{" "}
                  <a href="https://remotai.ru/offer.html" onClick={(e) => { e.preventDefault(); openExternalLink("https://remotai.ru/offer.html"); }}>{t("billing.offerLink")}</a>
                  {" · "}
                  <a href="https://remotai.ru/privacy" onClick={(e) => { e.preventDefault(); openExternalLink("https://remotai.ru/privacy"); }}>{t("billing.privacyLink")}</a>
                </p>
              </section>
            )}

            {sub && (
              <section className="card">
                <div className="plan-card-title">{t("billing.helpTitle")}</div>
                <p className="plan-note">{t("billing.helpNote")}</p>
                <button className="btn btn-secondary" onClick={checkPayment} disabled={checkingPayment || busy}>
                  {checkingPayment ? t("generic.loading") : t("billing.checkPayment")}
                </button>
                <button className="btn btn-secondary btn-sm" style={{ marginTop: 8 }} onClick={() => navigate("/support")}>
                  {t("billing.contactSupport")}
                </button>
              </section>
            )}

            {/* Проверочный платёж. Живые карты и весь путь денег проверяются
                настоящей оплатой, а не рассуждениями, — но на десять рублей и
                на сутки. Кнопку показывает сервер (can_test_pay), он же не
                пускает эту полку никому, кроме администратора. */}
            {sub?.can_test_pay && !sub.self_hosted && (
              <section className="card">
                <div className="plan-card-title">{t("plan.payTest")}</div>
                <button className="btn btn-secondary" onClick={() => pay("test")} disabled={busy}>
                  {t("plan.payTest")}
                </button>
                <p className="plan-note">{t("plan.payTestNote")}</p>
              </section>
            )}

            {/* Кассы ещё нет — это ожидание, а не сбой, и говорим об этом прямо. */}
            {!me?.self_hosted && (notYet || (sub && !sub.founder && !sub.billing_enabled)) && (
              <section className="card"><p>{t("plan.notYet")}</p></section>
            )}

            {(sub?.founder || me?.founder) && (
              <section className="card"><p>{t("plan.founderNote")}</p></section>
            )}

            {error && <p className="plan-error">{error}</p>}
          </>
        )}
      </div>

      {/* Свой пункт шторки «Ещё» — «Личный кабинет»: с `active="more"` не
          подсвечивалось ничего и «Вы здесь» в шторке не было. */}
      <BottomNav active="plan" />
    </div>
  );
}

/** Человеческое имя способа входа: «telegram» само по себе ничего не говорит. */
function providerName(p: string): string {
  const key = `account.provider.${p}`;
  const named = t(key);
  return named === key ? p : named;
}

export default AccountView;
