import { useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import { mapApiError } from "@tgcontrol/shared";
import { connectLan, parseAccessLink, parseLanInput } from "../pairPayload";
import { parsePairPayload, runPair } from "../cloud/pair";
import { getRelayBase } from "../config";
import { t } from "../i18n";
import { IconLogo, IconQr } from "../components/icons";

/**
 * Облачный код с экрана ПК — ровно 8 знаков, дефис в середине только для
 * читаемости. Его тоже приносят на этот экран (человек не различает «облако» и
 * «локальную сеть»), поэтому узнаём его в лицо и пейримся через релей.
 */
const CLOUD_CODE_RE = /^[A-Za-z0-9]{4}-?[A-Za-z0-9]{4}$/;

export function LoginView() {
  const navigate = useNavigate();
  const [url, setUrl] = useState("");
  const [token, setToken] = useState("");
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(false);
  const [mode, setMode] = useState<"code" | "manual">("code");
  const [pasteCode, setPasteCode] = useState("");
  const [showToken, setShowToken] = useState(false);
  const pasteInputRef = useRef<HTMLInputElement>(null);

  const tryConnect = async (serverUrl: string, serverToken: string) => {
    setLoading(true);
    setError("");

    try {
      await connectLan(serverUrl, serverToken);
      navigate("/");
    } catch (e: any) {
      setError(e?.message || "Ошибка подключения");
    } finally {
      setLoading(false);
    }
  };

  const handlePaste = async () => {
    try {
      const text = await navigator.clipboard.readText();
      if (text && text.trim()) {
        setPasteCode(text);
        handleCodeSubmit(text);
        return;
      }
      // Буфер пуст — подсказываем, где взять код, и ведём к ручному вводу.
      setError("Буфер пуст — скопируйте код в панели Remotai на ПК");
      pasteInputRef.current?.focus();
    } catch {
      // Clipboard API может не сработать (нет разрешения, не HTTPS) — не затираем
      // текущую ошибку молчанием, а честно просим ввести код вручную.
      setError("Не удалось прочитать буфер обмена — введите код вручную");
      pasteInputRef.current?.focus();
    }
  };

  /** Вставили облачный код или облачный QR — путь к тому же компьютеру, просто
   *  через релей. Пейримся молча, вместо «неверного кода». */
  const tryCloud = async (relay: string, code: string) => {
    setLoading(true);
    setError("");
    try {
      await runPair(relay, code);
      navigate("/");
    } catch (e) {
      setError(mapApiError(e));
    } finally {
      setLoading(false);
    }
  };

  /**
   * Ввод в поля вкладки «Адрес и ключ». Если вставили ссылку из окна Remotai
   * целиком — раскладываем её на адрес и ключ, в какое бы поле её ни вставили.
   */
  const handleAccessInput = (field: "url" | "token", value: string) => {
    const link = parseAccessLink(value);
    if (link) {
      setUrl(link.url);
      setToken(link.token);
      setError("");
      return;
    }
    if (field === "url") setUrl(value);
    else setToken(value);
  };

  /**
   * Одна кнопка на всё, что человек мог скопировать на ПК. Панель кладёт в
   * буфер то код, то ссылку доступа «…/miniapp?token=…» — а эта ветка знала
   * только код, и её же подсказку «скопируйте код» ссылка встречала ответом
   * «Неверный код подключения». Теперь узнаём все три формата и облачный код.
   */
  const handleCodeSubmit = (code?: string) => {
    const value = (code || pasteCode).trim();
    const lan = parseLanInput(value);
    if (lan) {
      setUrl(lan.url);
      setToken(lan.token);
      tryConnect(lan.url, lan.token);
      return;
    }
    const cloud = parsePairPayload(value);
    if (cloud) {
      void tryCloud(cloud.relay, cloud.code);
      return;
    }
    if (CLOUD_CODE_RE.test(value)) {
      void tryCloud(getRelayBase(), value.toUpperCase());
      return;
    }
    setError("Это не похоже на код подключения. На ПК в панели Remotai выберите «По локальной сети» и нажмите «Скопировать код» — сюда подойдёт и код, и ссылка доступа целиком.");
  };

  return (
    <div className="login-page">
      <div className="login-card">
        <div className="login-logo"><IconLogo size={26} className="login-logo-mark" /> Remotai</div>
        <p className="login-subtitle">Подключение к компьютеру</p>

        {/* Primary path: scan the QR shown by the desktop app (cloud). */}
        {/* state.lan — чтобы кнопка «Ввести код вручную» на сканере вернула сюда
            (к LAN-коду), а не на облачный экран входа. */}
        <button className="login-btn login-btn-qr" onClick={() => navigate("/scan", { state: { lan: true } })}>
          <IconQr size={18} /> Сканировать QR-код
        </button>
        <small className="login-hint" style={{ display: "block", textAlign: "center", marginBottom: 14 }}>
          Запустите Remotai на компьютере и наведите камеру на QR
        </small>

        <div className="login-divider"><span>или без камеры</span></div>

        {/* Tab switcher */}
        <div className="login-tabs">
          <button
            className={`login-tab ${mode === "code" ? "active" : ""}`}
            onClick={() => { setMode("code"); setError(""); }}
          >
            По коду
          </button>
          <button
            className={`login-tab ${mode === "manual" ? "active" : ""}`}
            onClick={() => { setMode("manual"); setError(""); }}
          >
            Адрес и ключ
          </button>
        </div>

        {mode === "code" ? (
          <>
            {/* Quick paste mode */}
            <div className="login-steps">
              <div className="login-step">
                <span className="login-step-num">1</span>
                <span>Установите и откройте Remotai на компьютере</span>
              </div>
              <div className="login-step">
                <span className="login-step-num">2</span>
                <span>В панели Remotai выберите «По локальной сети»</span>
              </div>
              <div className="login-step">
                <span className="login-step-num">3</span>
                <span>Скопируйте код подключения и вставьте его ниже</span>
              </div>
            </div>

            <button
              className="login-btn login-btn-paste"
              onClick={handlePaste}
              disabled={loading}
            >
              {/* Многоточие одним символом — как везде в продукте («Открываем…»,
                  «Проверяем…»); три точки видны ровно на первом экране. */}
              {loading ? t("remote.connecting") : "Вставить код из буфера"}
            </button>

            <div className="login-divider">
              <span>или введите вручную</span>
            </div>

            <div className="login-field">
              <input
                ref={pasteInputRef}
                type="text"
                placeholder="Код подключения или ссылка с ПК"
                value={pasteCode}
                onChange={(e) => setPasteCode(e.target.value)}
                autoCapitalize="off"
                autoCorrect="off"
                spellCheck={false}
                style={{ fontFamily: "monospace", fontSize: 13 }}
              />
            </div>

            <button
              className="login-btn"
              onClick={() => handleCodeSubmit()}
              disabled={loading || !pasteCode.trim()}
            >
              {loading ? t("remote.connecting") : "Подключиться"}
            </button>
          </>
        ) : (
          <>
            {/* Manual mode */}
            <small className="login-hint" style={{ display: "block", marginBottom: 12 }}>
              На компьютере в окне Remotai раскройте «Зайти с другого компьютера» и нажмите на адрес — ссылка скопируется целиком, вместе с ключом. Вставьте её в любое из полей ниже: адрес и ключ разложатся сами.
            </small>

            <div className="login-field">
              <label>Адрес компьютера</label>
              <input
                type="text"
                inputMode="url"
                placeholder="192.168.1.100:8080"
                value={url}
                onChange={(e) => handleAccessInput("url", e.target.value)}
                autoCapitalize="off"
                autoCorrect="off"
                spellCheck={false}
              />
              <small className="login-hint">
                IP компьютера и порт Remotai (по умолчанию 8080)
              </small>
            </div>

            <div className="login-field">
              <label>Ключ доступа</label>
              <div className="login-input-row">
                <input
                  type={showToken ? "text" : "password"}
                  placeholder="Вставьте ссылку или ключ с компьютера"
                  value={token}
                  onChange={(e) => handleAccessInput("token", e.target.value)}
                  autoCapitalize="off"
                  autoCorrect="off"
                  autoComplete="off"
                  spellCheck={false}
                  style={{ fontFamily: "monospace", fontSize: 13 }}
                />
                <button
                  type="button"
                  className="login-eye"
                  onClick={() => setShowToken((v) => !v)}
                  aria-label={showToken ? "Скрыть ключ" : "Показать ключ"}
                >
                  {showToken ? "\u{1F648}" : "\u{1F441}"}
                </button>
              </div>
              <small className="login-hint">
                Ключ — вторая половина той же ссылки. Никому его не пересылайте: он даёт полный доступ к компьютеру.
              </small>
            </div>

            <button
              className="login-btn"
              onClick={() => tryConnect(url, token)}
              disabled={loading}
            >
              {loading ? t("remote.connecting") : "Подключиться"}
            </button>
          </>
        )}

        {error && <div className="login-error">{error}</div>}

        <button
          className="login-cloud-link"
          onClick={() => navigate("/cloud-login")}
        >
          Компьютер далеко? Подключиться через облако
        </button>
      </div>
    </div>
  );
}
