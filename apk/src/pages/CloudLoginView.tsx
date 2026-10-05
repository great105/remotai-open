import { useEffect, useRef, useState } from "react";
import { Link, useNavigate, useLocation } from "react-router-dom";

import { saveConfig, isNativeApp, hasServerConfig } from "../config";
import { useGoBack } from "../navBack";
import { loginNextPath } from "../nextPath";
import { installedPairCode } from "../installedPair";
import { connectLan, parseLanInput } from "../pairPayload";
import { pairNative } from "../cloud/api";
import { trackAppOpen, trackRegisterSource, trackPairSuccess } from "../cloud/support";
import { startTelegramLogin, type TgLoginHandle } from "../cloud/tgLogin";
import { startOAuthLogin, type OAuthLoginHandle } from "../cloud/oauthLogin";
import { startEmailLogin, verifyEmailCode } from "../cloud/emailLogin";
import { getAuthProviders, type AuthProvider } from "../cloud/authProviders";
import { connectWS } from "../api";
import { resetCapabilities } from "../capabilities";
import { useToast } from "@tgcontrol/shared";
import { mapApiError, RELAY_BASE } from "@tgcontrol/shared";
import { IconQr } from "../components/icons";
import { InstallPwaBanner } from "../components/InstallPwaBanner";
import { QrCode } from "../components/QrCode";
import { currentDesktopDownload } from "../downloads";
import { getTelegram } from "../telegram";
import { t } from "../i18n";

const DEFAULT_RELAY = RELAY_BASE;
/** Адрес, который человек открывает на КОМПЬЮТЕРЕ, чтобы поставить Remotai. */
const SITE_HOST = RELAY_BASE.replace(/^https?:\/\//, "");

/**
 * Камеру предлагаем там, где ей есть чем сканировать: на телефоне. На большом
 * экране QR показан на этом же компьютере — «сканировать» его нечем, там
 * первичен ручной код (находка N94).
 */
function isPhoneLikeDevice(): boolean {
  if (isNativeApp) return true;
  if (typeof window === "undefined") return false;
  const coarse = window.matchMedia?.("(pointer: coarse)")?.matches ?? false;
  return coarse && window.innerWidth < 900;
}

/** Политика конфиденциальности живёт только на каноничном хосте — намеренно не
 *  берём адрес из поля «свой сервер»: у self-hosted релея /privacy нет. */
const PRIVACY_URL = RELAY_BASE + "/privacy";

/**
 * Экран входа. Набор методов приходит с релея (/v1/auth/providers): регион и
 * настроенные креды решают, что показать. Telegram-кнопка, OAuth-кнопки
 * (VK ID / Яндекс ID / Google) и вход по email-коду. Сканирование QR с экрана
 * компьютера — первичное действие на телефоне (мастер на ПК велит нажать именно
 * его); под тогглом «Ввести код вручную / свой сервер» — код подключения и
 * адрес самостоятельного релея. ScanView ведёт сюда с state.showCode=true.
 *
 * Внутри Telegram этот экран бесполезен: там уже авторизует подписанный
 * initData, а любая кнопка входа выбрасывает из мини-аппа. Поэтому в Telegram
 * вместо входа показываем единственное, что там работает — «закройте
 * мини-приложение и откройте его снова из чата с ботом» (находки N53/N130).
 */
export function CloudLoginView() {
  const navigate = useNavigate();
  const location = useLocation();
  const goBack = useGoBack();
  const { toastSuccess, toastError } = useToast();
  const [relayBase, setRelayBase] = useState<string>(DEFAULT_RELAY);
  const [code, setCode] = useState<string>("");
  // Узнали в поле кода локальный код/ссылку доступа — подключаемся напрямую к
  // компьютеру, минуя релей (он такой код честно не найдёт).
  const [lanInput, setLanInput] = useState<{ url: string; token: string } | null>(null);
  const [busy, setBusy] = useState(false);
  const [tgBusy, setTgBusy] = useState(false);
  const [oauthBusy, setOauthBusy] = useState<string | null>(null); // id провайдера в процессе
  // Со сканера («Ввести код вручную») приходят с раскрытой секцией кода и
  // отдельной подсказкой: раньше человек молча возвращался на свёрнутый тоггл.
  const fromScan = !!(location.state as { showCode?: boolean } | null)?.showCode;
  const [showCode, setShowCode] = useState<boolean>(fromScan);
  const [showAdvanced, setShowAdvanced] = useState(false);
  const [providers, setProviders] = useState<AuthProvider[] | null>(null);
  const phoneLike = isPhoneLikeDevice();
  const forSupport = loginNextPath(location.state, location.search).split("?")[0] === "/support";
  const forInstalledComputer = !!installedPairCode(loginNextPath(location.state, location.search));
  const inTelegram = !!getTelegram()?.initData;

  // Email-вход: два шага (почта → код).
  const [email, setEmail] = useState("");
  const [emailToken, setEmailToken] = useState<string | null>(null);
  const [emailCode, setEmailCode] = useState("");
  const [emailBusy, setEmailBusy] = useState(false);
  const [emailExpiresAt, setEmailExpiresAt] = useState("");
  const [emailResendAt, setEmailResendAt] = useState(0);
  const [emailResendLeft, setEmailResendLeft] = useState(0);

  // Вход по QR: ссылку бота не открываем, а показываем кодом — телефон её
  // сканирует и подтверждает, этот экран досматривает опрос до конца.
  const [qrLink, setQrLink] = useState<string | null>(null);
  const desktopDownload = phoneLike ? null : currentDesktopDownload();

  const tgHandleRef = useRef<TgLoginHandle | null>(null);
  const oauthHandleRef = useRef<OAuthLoginHandle | null>(null);

  useEffect(() => {
    getAuthProviders()
      .then((r) => setProviders(r.providers))
      .catch(() => setProviders([{ id: "telegram", kind: "deeplink", label: "Telegram" }]));
  }, []);

  useEffect(() => {
    if (!emailToken) return;
    const tick = () => setEmailResendLeft(Math.max(0, Math.ceil((emailResendAt - Date.now()) / 1000)));
    tick();
    const timer = window.setInterval(tick, 1000);
    return () => window.clearInterval(timer);
  }, [emailToken, emailResendAt]);

  const has = (id: string) => !!providers?.some((p) => p.id === id);
  const oauthProviders = (providers || []).filter((p) => p.kind === "oauth");

  const afterLogin = () => {
    trackAppOpen();
    trackRegisterSource();
    connectWS();
    // Человек шёл в чат поддержки или кабинет и упёрся во вход — после входа
    // ведём туда, а не на главную (аудит ИА 02.09.2026, P1-13). Цель кладёт
    // RequireAuth; чужой адрес safeNextPath режет до главной.
    navigate(loginNextPath(location.state, location.search));
  };

  /**
   * Вход через Telegram. `openBot=false` — режим QR: ссылку бота показываем
   * кодом на этом экране, а нажимает «Запустить» телефон. Всё остальное
   * (опрос, слияние анонимного аккаунта, завершение) — общее.
   */
  const handleTgLogin = async (openBot = true) => {
    setTgBusy(true);
    try {
      const h = await startTelegramLogin({ open: openBot });
      tgHandleRef.current = h;
      if (!openBot) setQrLink(h.deepLink);
      const res = await h.done;
      if (res.ok) {
        afterLogin();
        return;
      }
      if (res.reason === "expired" || res.reason === "timeout") {
        toastError("Время на вход вышло. Попробуйте ещё раз.");
      }
      // "cancelled" — пользователь сам нажал «Отмена», молча возвращаемся.
    } catch (e) {
      toastError(mapApiError(e));
    } finally {
      tgHandleRef.current = null;
      setTgBusy(false);
      setQrLink(null);
    }
  };

  const handleOAuthLogin = async (providerId: string) => {
    setOauthBusy(providerId);
    try {
      const h = await startOAuthLogin(providerId);
      oauthHandleRef.current = h;
      const res = await h.done;
      if (res.ok) {
        afterLogin();
        return;
      }
      if (res.reason === "expired" || res.reason === "timeout") {
        toastError("Время на вход вышло. Попробуйте ещё раз.");
      }
    } catch (e) {
      toastError(mapApiError(e));
    } finally {
      oauthHandleRef.current = null;
      setOauthBusy(null);
    }
  };

  const handleEmailStart = async () => {
    const clean = email.trim();
    if (!clean.includes("@")) {
      toastError("Введите адрес почты");
      return;
    }
    setEmailBusy(true);
    try {
      const result = await startEmailLogin(clean);
      setEmailToken(result.loginToken);
      setEmailExpiresAt(result.expiresAt);
      setEmailResendAt(Date.now() + 60_000);
    } catch (e) {
      toastError(mapApiError(e));
    } finally {
      setEmailBusy(false);
    }
  };

  const handleEmailVerify = async (override?: string) => {
    const clean = (override ?? emailCode).trim();
    if (clean.length !== 6) {
      toastError("Введите 6 цифр из письма");
      return;
    }
    setEmailBusy(true);
    try {
      await verifyEmailCode(emailToken!, clean);
      afterLogin();
    } catch (e) {
      toastError(mapApiError(e));
    } finally {
      setEmailBusy(false);
    }
  };

  /**
   * Ввод кода из письма. Полный код отправляем сами: на цифровой клавиатуре
   * Android клавиши Enter нет вовсе, а «Готово» её просто убирает — человек
   * вводил шесть цифр и не понимал, чем их отправить (находка N101).
   */
  const autoVerifiedRef = useRef("");
  const handleEmailCodeInput = (raw: string) => {
    const digits = raw.replace(/\D/g, "").slice(0, 6);
    setEmailCode(digits);
    if (digits.length === 6 && !emailBusy && autoVerifiedRef.current !== digits) {
      autoVerifiedRef.current = digits;
      void handleEmailVerify(digits);
    }
  };

  /**
   * Поле принимает не только облачный код. Панель ПК в режиме «По локальной
   * сети» кладёт в буфер длинный base64 «адрес|токен» или ссылку доступа
   * целиком, а сюда человека приводит единственная спасательная кнопка сканера
   * «Ввести код вручную». Обрезка до 9 символов делала такую вставку заведомо
   * непроходимой: релей отвечал «такого кода нет», и человек по кругу
   * перепроверял символы. Узнали локальный код — храним его целиком.
   */
  const handleCodeInput = (raw: string) => {
    const lan = parseLanInput(raw);
    if (lan) {
      setLanInput(lan);
      setCode(raw.trim());
      return;
    }
    setLanInput(null);
    setCode(raw.toUpperCase().slice(0, 9));
  };

  const submit = async () => {
    // Локальный код — прямое подключение к ПК, релею его показывать незачем.
    if (lanInput) {
      setBusy(true);
      try {
        await connectLan(lanInput.url, lanInput.token); // сам сохранит конфиг и поднимет WS
        navigate("/");
      } catch (e) {
        toastError(mapApiError(e));
      } finally {
        setBusy(false);
      }
      return;
    }
    const cleanCode = code.trim().toUpperCase();
    // Код — ровно 8 символов (дефис в середине — только для читаемости, релей
    // его выбрасывает сам). Порог стоял на 6: недобранный код уезжал на релей
    // и возвращался бессмысленным «Не найдено.» вместо подсказки (N95).
    if (cleanCode.replace(/[-\s]/g, "").length < 8) {
      toastError("В коде подключения 8 символов — проверьте, всё ли переписано с экрана компьютера.");
      return;
    }
    setBusy(true);
    try {
      const base = relayBase.trim().replace(/\/+$/, "");
      const r = await pairNative(base, cleanCode);
      saveConfig({
        mode: "cloud",
        relayBase: base,
        jwt: r.user_jwt,
        jwtExpiresAt: Date.parse(r.expires_at) || undefined,
        selectedDeviceId: r.device_id,
      });
      trackAppOpen();
      trackRegisterSource();
      trackPairSuccess();
      resetCapabilities(); // возможности новой машины ещё не известны
      connectWS();
      navigate("/");
    } catch (e) {
      toastError(mapApiError(e));
    } finally {
      setBusy(false);
    }
  };

  /**
   * «Откройте remotai.ru на компьютере» — адрес удобнее скопировать, чем
   * переписывать. Прямая ссылка на установщик с телефона бесполезна: она
   * качает .exe для Windows НА ТЕЛЕФОН (находка N96).
   */
  /** Скопировать строку в буфер с человеческим подтверждением. */
  const copyText = async (text: string, okMessage: string) => {
    try {
      await navigator.clipboard.writeText(text);
      toastSuccess(okMessage);
    } catch {
      toastError("Не удалось скопировать — выделите строку и скопируйте вручную.");
    }
  };

  const copySiteAddress = async () => {
    try {
      await navigator.clipboard.writeText(RELAY_BASE);
      toastSuccess(`Адрес скопирован. Откройте ${SITE_HOST} на компьютере и установите Remotai.`);
    } catch {
      toastError(`Не удалось скопировать. Наберите на компьютере ${SITE_HOST}`);
    }
  };

  const brand = (
    <div className="onb-brand">
      <span className="onb-mark" aria-hidden="true">
        <svg width="18" height="18" viewBox="0 0 24 24" fill="none">
          <path d="M4 8.5 12 4l8 4.5v7L12 20l-8-4.5z" stroke="#06120e" strokeWidth="2" strokeLinejoin="round" />
          <path d="M12 12v8M12 12l8-3.5M12 12 4 8.5" stroke="#06120e" strokeWidth="2" strokeLinejoin="round" />
        </svg>
      </span>
      <span className="onb-word">remotai</span>
    </div>
  );

  // В Telegram ни одна ветка входа не работает: аккаунт там задаёт сам Telegram
  // подписанным initData, а «Войти через Telegram» уводит в чат бота, где опрос
  // входа умирает вместе с закрытым мини-аппом. Единственное действие, которое
  // реально лечит истёкший initData и отозванный доступ, — открыть мини-апп
  // заново (находки N53/N130).
  if (inTelegram) {
    const tg = getTelegram();
    return (
      <div className="onb">
        {brand}
        <div className="onb-hero">
          <h1>Откройте Remotai заново</h1>
          <p>
            В Telegram вход происходит сам — отдельного входа здесь нет. Telegram передал устаревшие
            данные сеанса (мини-приложение провисело слишком долго) либо доступ к компьютеру отозвали,
            поэтому запросы больше не проходят.
          </p>
        </div>
        <div className="onb-actions">
          <button className="onb-tg" onClick={() => tg?.close()}>
            Закрыть мини-приложение
          </button>
          <p className="onb-hint">
            Затем вернитесь в чат с ботом Remotai и нажмите «Открыть» — данные сеанса обновятся сами,
            всё останется на месте.
          </p>
          <button
            className="btn btn-secondary"
            style={{ width: "100%", marginTop: 12 }}
            onClick={() => window.location.reload()}
          >
            Попробовать снова
          </button>
        </div>
        <div className="onb-foot">
          Если не помогло — переключитесь в Telegram на тот аккаунт, которым подключали компьютер,
          и снова откройте Remotai из чата с ботом.
        </div>
      </div>
    );
  }

  return (
    <div className="onb">
      {brand}

      {/* Сюда приходят и из ЖИВОГО приложения («Войти в аккаунт» при локальном
          подключении). Без «←» и без нижней панели выхода с экрана не было:
          системная «Назад» сворачивала приложение (аудит ИА 02.09.2026, P0-4).
          При пустой конфигурации экран — корень, и стрелка не нужна. */}
      {hasServerConfig() && (
        <div style={{ display: "flex", justifyContent: "flex-start" }}>
          <button
            type="button"
            className="btn btn-secondary btn-sm"
            aria-label={t("generic.back")}
            onClick={goBack}
          >
            {"← "}{t("generic.back")}
          </button>
        </div>
      )}

      <div className="onb-hero">
        <h1>{forSupport ? "Написать в поддержку" : forInstalledComputer ? "Подключить компьютер к аккаунту" : "Вход в Remotai"}</h1>
        <p>{forSupport ? "Войдите удобным способом, чтобы отправить сообщение и прочитать ответ в личном чате на сайте. При первом входе аккаунт создастся автоматически." : "Один аккаунт для управления с телефона, браузера и приложения на ПК. При первом входе аккаунт создастся автоматически."}</p>
        <p className="onb-hint">{forSupport ? "Устанавливать Remotai или подключать компьютер для обращения не нужно. После входа сразу откроется поддержка." : forInstalledComputer ? "Вы пришли из установленного Remotai. Войдите тем же способом, что на других устройствах. После входа подтвердите добавление компьютера — код уже подставлен." : "После входа выберите компьютер или подключите новый по его коду. На других устройствах используйте тот же способ входа."}</p>
        {!forSupport && !forInstalledComputer && <Link className="login-advanced-toggle" style={{ minHeight: 44, display: "inline-flex", alignItems: "center" }} to="/start">Впервые здесь? Выбрать, что подключить</Link>}
      </div>

      {/* Перед способами входа, а не после: у приложения с главного экрана
          отдельное от Safari хранилище, и вход, сделанный в браузере, туда не
          переедет. Кто установит после входа — окажется разлогинен и прочтёт
          это как поломку. Точка решения здесь. */}
      <InstallPwaBanner place="login" />

      <div className="onb-actions">
        {(providers === null || has("telegram")) && (
          <>
            <button className="onb-tg" disabled={tgBusy} onClick={() => void handleTgLogin()}>
              <svg width="20" height="20" viewBox="0 0 24 24" fill="none" aria-hidden="true">
                <path d="M21 4 3 11l5 2 2 6 3-4 5 4z" stroke="#06120e" strokeWidth="1.8" strokeLinejoin="round" />
              </svg>
              {!tgBusy ? "Войти через Telegram" : qrLink ? "Ждём подтверждения с телефона…" : "Открываем Telegram…"}
            </button>
            {/* Показать свой QR — второе действие того же входа. На большом
                экране Telegram может быть не установлен и не залогинен, зато
                телефон с ним всегда рядом: он сканирует код и подтверждает
                вход, а этот экран досматривает опрос. Раньше здесь была
                только зеркальная кнопка «Сканировать QR» — на компьютере ей
                нечего и нечем сканировать. */}
            {!phoneLike && !tgBusy && (
              <button
                className="btn btn-secondary"
                style={{ width: "100%", marginTop: 12 }}
                onClick={() => void handleTgLogin(false)}
              >
                <IconQr size={18} /> Показать QR — войти с телефона
              </button>
            )}
            {tgBusy && qrLink && (
              <div className="onb-fallback" style={{ marginTop: 12, textAlign: "center" }}>
                <div style={{ display: "flex", justifyContent: "center", padding: 12 }}>
                  <QrCode value={qrLink} size={220} title="QR-код для входа через Telegram" />
                </div>
                <p className="onb-hint" style={{ marginTop: 0 }}>
                  Наведите камеру телефона и нажмите «Запустить» в Telegram — вход на этом
                  компьютере завершится сам, страницу закрывать не нужно.
                </p>
                <button className="btn btn-secondary" style={{ width: "100%", marginTop: 12 }} onClick={() => tgHandleRef.current?.cancel()}>
                  Отмена
                </button>
              </div>
            )}
            {tgBusy && !qrLink && (
              <>
                <p className="onb-hint">Нажмите «Запустить» в Telegram и вернитесь в приложение — вход завершится сам.</p>
                <button className="btn btn-secondary" style={{ width: "100%", marginTop: 12 }} onClick={() => tgHandleRef.current?.cancel()}>
                  Отмена
                </button>
              </>
            )}
          </>
        )}

        {/* Мастер на ПК прямо велит «нажмите „Сканировать QR“», поэтому кнопка
            стоит на верхнем уровне, а не под тогглом «без аккаунта» (N94).
            На большом экране камеры для этого обычно нет — там первичен код. */}
        {phoneLike && (
          <>
            <button
              className="onb-tg"
              style={{
                marginTop: 12,
                background: "transparent",
                color: "var(--tg-text)",
                border: "1px solid var(--tg-border)",
                boxShadow: "none",
              }}
              onClick={() => navigate("/scan")}
            >
              <IconQr size={18} /> Сканировать QR-код с экрана компьютера
            </button>
            <p className="onb-hint">
              Сразу привяжем этот компьютер — аккаунт можно добавить позже. QR показывает окно Remotai
              на компьютере.
            </p>
          </>
        )}

        {oauthProviders.map((p) => (
          <button
            key={p.id}
            className="btn btn-secondary"
            style={{ width: "100%", marginTop: 12 }}
            disabled={oauthBusy !== null}
            onClick={() => void handleOAuthLogin(p.id)}
          >
            {oauthBusy === p.id ? `Открываем ${p.label}…` : `Войти через ${p.label}`}
          </button>
        ))}
        {oauthBusy && (
          <>
            <p className="onb-hint">Войдите в открывшемся окне и вернитесь в приложение — вход завершится сам.</p>
            <button className="btn btn-secondary" style={{ width: "100%", marginTop: 12 }} onClick={() => oauthHandleRef.current?.cancel()}>
              Отмена
            </button>
          </>
        )}

        {has("email") && (
          <div className="onb-fallback" style={{ marginTop: 12 }}>
            {!emailToken ? (
              <>
                <div className="login-field">
                  <label>Email</label>
                  <input
                    value={email}
                    onChange={(e) => setEmail(e.target.value)}
                    onKeyDown={(e) => { if (e.key === "Enter") void handleEmailStart(); }}
                    placeholder="you@example.ru"
                    autoCapitalize="off"
                    autoCorrect="off"
                    spellCheck={false}
                    inputMode="email"
                    enterKeyHint="send"
                  />
                </div>
                <button className="btn btn-secondary" style={{ width: "100%", marginTop: 12 }} disabled={emailBusy} onClick={() => void handleEmailStart()}>
                  {emailBusy ? "Отправляем…" : "Получить код на почту"}
                </button>
              </>
            ) : (
              <>
                <p className="onb-hint" style={{ marginBottom: 10 }}>
                  Код отправлен на <b>{email.trim()}</b> · действует 10 минут
                  {emailExpiresAt ? ` (до ${new Date(emailExpiresAt).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })})` : ""}.
                </p>
                <div className="login-field">
                  <label>Код из письма</label>
                  <input
                    value={emailCode}
                    onChange={(e) => handleEmailCodeInput(e.target.value)}
                    onKeyDown={(e) => { if (e.key === "Enter") void handleEmailVerify(); }}
                    placeholder="123456"
                    inputMode="numeric"
                    autoComplete="one-time-code"
                    enterKeyHint="done"
                    autoFocus
                    style={{ fontFamily: "var(--font-mono)", fontSize: 18, letterSpacing: 4, textAlign: "center" }}
                  />
                  <small className="login-hint">Шесть цифр из письма. Как введёте — проверим сами.</small>
                </div>
                <button className="btn btn-secondary" style={{ width: "100%", marginTop: 12 }} disabled={emailBusy} onClick={() => void handleEmailVerify()}>
                  {emailBusy ? "Проверяем…" : "Войти"}
                </button>
                <button
                  className="login-advanced-toggle"
                  disabled={emailBusy || emailResendLeft > 0}
                  onClick={() => void handleEmailStart()}
                >
                  {emailResendLeft > 0 ? `Отправить снова через ${emailResendLeft} с` : "Отправить код снова"}
                </button>
                <button className="login-advanced-toggle" onClick={() => { setEmailToken(null); setEmailCode(""); }}>
                  ◂ Другая почта
                </button>
              </>
            )}
          </div>
        )}

        <button className="login-advanced-toggle" aria-expanded={showCode} onClick={() => setShowCode(!showCode)}>
          {showCode ? "▾" : "▸"} Подключиться по коду с другого компьютера
        </button>
        {showCode && (
          <div className="onb-fallback">
            {fromScan && (
              <p className="onb-hint" style={{ marginTop: 0, marginBottom: 10 }}>
                Введите код с экрана компьютера — он под QR-кодом в окне Remotai.
              </p>
            )}
            {!phoneLike && (
              <button className="btn btn-secondary" style={{ width: "100%", marginBottom: 12 }} onClick={() => navigate("/scan")}>
                <IconQr size={18} /> Сканировать QR
              </button>
            )}
            <div className="login-field">
              <label htmlFor="login-pair-code">Код подключения с экрана компьютера</label>
              <input
                id="login-pair-code"
                value={code}
                onChange={(e) => handleCodeInput(e.target.value)}
                onKeyDown={(e) => { if (e.key === "Enter") void submit(); }}
                placeholder="XXXX-XXXX"
                autoCapitalize={lanInput ? "off" : "characters"}
                autoCorrect="off"
                spellCheck={false}
                inputMode="text"
                enterKeyHint="go"
                autoFocus={fromScan}
                // Длинный локальный код в разрядку по центру не читается —
                // показываем его как обычную моноширинную строку.
                style={lanInput
                  ? { fontFamily: "var(--font-mono)", fontSize: 13, letterSpacing: 0, textAlign: "left" }
                  : { fontFamily: "var(--font-mono)", fontSize: 18, letterSpacing: 2, textAlign: "center" }}
              />
              <small className="login-hint">
                {lanInput ? t("login.code.lanDetected") : "Код из 8 символов, буквы и цифры. Действует час."}
              </small>
            </div>
            <button className="btn btn-secondary" style={{ width: "100%", marginTop: 12 }} disabled={busy} onClick={() => void submit()}>
              {busy ? "Подключение…" : lanInput ? t("login.code.connectLan") : "Подключиться по коду"}
            </button>
            <p className="onb-hint">Это быстрый вход без регистрации. Чтобы затем видеть компьютер с других устройств и восстановить доступ, добавьте способ входа в «Личном кабинете».</p>

            {/* Кода ещё нет, потому что на компьютере ничего не установлено.
                На телефоне прямая ссылка на .exe бесполезна (N96) — там остаётся
                совет открыть сайт на компьютере. А в браузере НА компьютере тот
                же совет отправлял человека по кругу: он уже стоял на {SITE_HOST}
                на компьютере. Там даём ссылку прямо на установщик этой ОС. */}
            <div style={{ marginTop: 14 }}>
              <small className="login-hint" style={{ display: "block" }}>
                Код показывает окно Remotai на том компьютере, которым вы хотите управлять:
                он живёт час, дальше нужен новый.
              </small>
              {desktopDownload ? (
                <>
                  <small className="login-hint" style={{ display: "block", marginTop: 8 }}>
                    Хотите управлять <b>этим</b> компьютером? Браузер этого не умеет — на него нужно
                    поставить Remotai, и он сам покажет QR и код.
                  </small>
                  <a
                    className="btn btn-secondary"
                    style={{ width: "100%", marginTop: 10, display: "block", textAlign: "center" }}
                    href={desktopDownload.os === "linux" ? desktopDownload.guideUrl : desktopDownload.url}
                    download={desktopDownload.os !== "linux" || undefined}
                  >
                    {desktopDownload.os === "linux" ? "Выбрать установщик для Linux" : `↓ ${desktopDownload.label}`}
                  </a>
                  {(desktopDownload.os === "macos" || desktopDownload.os === "linux") && <small className="login-hint" style={{ display: "block", marginTop: 8 }}>{t("install.previousNativePackage")}</small>}
                  {desktopDownload.guideUrl && <a className="login-advanced-toggle" href={desktopDownload.guideUrl}>Открыть пошаговую установку и получить код</a>}
                  {desktopDownload.os === "windows" && desktopDownload.altUrl && (
                    <a
                      className="login-advanced-toggle"
                      style={{ display: "block" }}
                      href={desktopDownload.altUrl}
                      download
                    >
                      {desktopDownload.altLabel}
                    </a>
                  )}
                </>
              ) : (
                <>
                  <small className="login-hint" style={{ display: "block", marginTop: 8 }}>
                    Ещё не установили — откройте <b>{SITE_HOST}</b> на компьютере и скачайте приложение там.
                  </small>
                  <button className="login-advanced-toggle" onClick={() => void copySiteAddress()}>
                    ⧉ Скопировать адрес {SITE_HOST}
                  </button>
                </>
              )}
            </div>

            <button className="login-advanced-toggle" onClick={() => setShowAdvanced(!showAdvanced)}>
              {showAdvanced ? "▾" : "▸"} Собственный сервис связи Remotai (для администраторов)
            </button>
            {showAdvanced && (
              <div className="login-field">
                {/* «relay» — наше внутреннее слово: в продукте оно нигде не
                    объяснено, а поле видит человек, который поднял сервер сам. */}
                <label>Адрес сервиса связи Remotai</label>
                <small className="login-hint">Только если вы развёрнули собственный сервис Remotai. Для подключения обычного Linux-сервера или VPS оставьте стандартный адрес.</small>
                <input
                  value={relayBase}
                  onChange={(e) => setRelayBase(e.target.value)}
                  placeholder={DEFAULT_RELAY}
                  autoCapitalize="off"
                  autoCorrect="off"
                  spellCheck={false}
                  inputMode="url"
                />
              </div>
            )}
          </div>
        )}
      </div>

      <div className="onb-foot">
        {/* Правовая строка: «условия использования» намеренно не упоминаем —
            такой страницы нет, ссылка вела бы в никуда. */}
        <div>
          Входя, вы принимаете{" "}
          <a
            href={PRIVACY_URL}
            target="_blank"
            rel="noopener"
            style={{ color: "var(--tg-link)" }}
          >
            политику конфиденциальности
          </a>
          .
        </div>
        <div style={{ marginTop: 8 }}>
          {isNativeApp ? (
            <button className="login-cloud-link" style={{ margin: 0 }} onClick={() => navigate("/login")}>
              Компьютер рядом? Подключиться по локальной сети
            </button>
          ) : (
            "Локальная работа бесплатна · серверы и облако входят в Про"
          )}
        </div>
      </div>
    </div>
  );
}
