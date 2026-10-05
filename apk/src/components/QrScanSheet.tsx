import { useCallback, useEffect, useRef, useState } from "react";
import type { ReactElement } from "react";
import jsQR from "jsqr";

import { SheetShell, useEscape, useToast } from "@tgcontrol/shared";
import { parsePairPayload } from "../cloud/pair";
import { parsePairingCode, parseTelegramLoginLink } from "../pairPayload";
import { getTelegram, haptic, hapticSuccess, scanQrInTelegram } from "../telegram";
import { t } from "../i18n";
import { tlog } from "../debuglog";

/**
 * Сканер QR ШТОРКОЙ поверх любого экрана.
 *
 * Зачем: «Добавить компьютер» уводило человека на отдельный маршрут `/scan` —
 * он терял из виду и список машин, и поле кода, а после подключения возвращался
 * не туда, откуда уходил. Камера теперь открывается прямо над тем экраном, где
 * человек нажал кнопку, и закрывается обратно в него.
 *
 * Кода сканирования в проекте ровно один: сам маршрут `/scan` (его открывают из
 * бота и с экрана входа) — тонкая обёртка над этой же шторкой (`ScanView`).
 *
 * Что шторка делает сама:
 *  • держит камеру ТОЛЬКО пока `open === true` и гасит поток при закрытии —
 *    иначе индикатор камеры на телефоне продолжает гореть над закрытой шторкой;
 *  • отличает код Remotai от чужого QR (ссылка, Wi-Fi, визитка) и объясняет
 *    словами, а не молчит;
 *  • в Telegram зовёт штатный сканер мини-приложения: getUserMedia там часто
 *    закрыт, и живое видео молча оставалось бы чёрным прямоугольником;
 *  • не долбит вызывающего одним и тем же кадром: повтор кода — не чаще раза
 *    в 4 секунды и не больше двух раз подряд, дальше «Сканирование остановлено».
 *
 * Чего шторка НЕ делает: не пейрит и не ходит в сеть. Она отдаёт код наружу —
 * что с ним делать (облачный пейринг, локальная сеть, тост об ошибке), решает
 * вызывающий экран. Ошибку он может вернуть обратно в `hint` — она встанет
 * подписью под видоискателем.
 */

/** Разобранный QR Remotai: облачный пейринг или прямое подключение по сети. */
export type ScanMatch =
  | { kind: "cloud"; relay: string; code: string; raw: string }
  | { kind: "lan"; url: string; token: string; raw: string }
  | { kind: "tglogin"; url: string; token: string; raw: string };

/**
 * Это код Remotai? Облачный QR — `remotai://pair?relay=…&code=…`, QR режима
 * «По локальной сети» — `tgcontrol://pair?url=…&token=…` или base64
 * «адрес|токен», QR входа с большого экрана — ссылка на бота с
 * `start=login_…`. Всё остальное — чужой QR.
 */
export function matchScanPayload(data: string): ScanMatch | null {
  const cloud = parsePairPayload(data);
  if (cloud) return { kind: "cloud", relay: cloud.relay, code: cloud.code, raw: data };
  const tg = parseTelegramLoginLink(data);
  if (tg) return { kind: "tglogin", url: tg.url, token: tg.token, raw: data };
  const lan = parsePairingCode(data);
  if (lan) return { kind: "lan", url: lan.url, token: lan.token, raw: data };
  return null;
}

/** Пауза перед повторной отдачей ТОГО ЖЕ кадра вызывающему. */
const REARM_MS = 4000;
/** Сколько раз подряд отдаём один и тот же код, прежде чем остановиться. */
const MAX_DELIVERIES = 2;

export function QrScanSheet({
  open, onClose, onCode, hint, onManual,
}: {
  open: boolean;
  onClose: () => void;
  /**
   * Распознанный код пейринга: уже проверено, что это код Remotai.
   *
   * Для облачного QR это КОРОТКИЙ код с экрана компьютера — ровно то, что
   * человек ввёл бы руками; для QR локальной сети — его payload целиком
   * (короткого кода там нет). Второй аргумент — разобранный QR: он нужен,
   * только если вызывающему важен адрес релея из самого QR или подключение
   * по локальной сети. Обработчик вида `(code) => pair(code)` тоже подходит.
   */
  onCode: (code: string, match: ScanMatch) => void;
  /** Подпись под видоискателем; по умолчанию — «Наведите камеру на QR-код». */
  hint?: string;
  /**
   * «Ввести код вручную». По умолчанию просто закрывает шторку — поле кода
   * остаётся на экране вызывающего. Маршрут `/scan` открывают и там, где поля
   * рядом нет, поэтому ему нужен свой путь.
   */
  onManual?: () => void;
}): ReactElement | null {
  const { toast } = useToast();
  const videoRef = useRef<HTMLVideoElement>(null);
  const canvasRef = useRef<HTMLCanvasElement>(null);
  // Пока код отдан наружу, кадры не читаем: rAF-цикл видит тот же QR каждый
  // кадр и без этого устроил бы шторм пейрингов.
  const busyRef = useRef(false);
  const deliveredRef = useRef<{ raw: string; count: number }>({ raw: "", count: 0 });
  const foreignAtRef = useRef(0); // дебаунс подсказки «это не код Remotai»
  const rearmRef = useRef(0);
  // Вызывающий часто передаёт стрелку прямо в JSX: держим её в ref, иначе
  // каждый рендер родителя перезапускал бы камеру.
  const onCodeRef = useRef(onCode);
  onCodeRef.current = onCode;
  const hintRef = useRef(hint);
  hintRef.current = hint;

  const [busy, setBusy] = useState(false);
  const [stalled, setStalled] = useState(false);
  const [cameraError, setCameraError] = useState("");
  const [note, setNote] = useState("");
  const [attempt, setAttempt] = useState(0); // bump → перезапуск камеры/сканера
  const [tgIdle, setTgIdle] = useState(false); // сканер Telegram закрыт человеком

  const tg = getTelegram();
  const tgScanner = !!tg?.initData && typeof tg.showScanQrPopup === "function";

  useEscape(open, onClose);

  const deliver = useCallback((data: string) => {
    if (busyRef.current) return;
    const match = matchScanPayload(data);
    if (!match) {
      // Чужой QR: подсказываем раз в 4 секунды, чтобы не спамить на каждом кадре.
      const now = Date.now();
      if (now - foreignAtRef.current > REARM_MS) {
        foreignAtRef.current = now;
        setNote(t("scan.foreign"));
        toast(t("scan.foreign"), "info");
      }
      return;
    }
    const same = deliveredRef.current.raw === data;
    if (same && deliveredRef.current.count >= MAX_DELIVERIES) {
      // Тот же код не подошёл дважды — третий раз ничего не изменит: он
      // использован, просрочен или телефон не в той сети. Останавливаемся и
      // предлагаем решение кнопками вместо молчаливой камеры.
      busyRef.current = true;
      setBusy(false);
      setStalled(true);
      return;
    }
    deliveredRef.current = { raw: data, count: same ? deliveredRef.current.count + 1 : 1 };
    busyRef.current = true;
    setBusy(true);
    setNote("");
    hapticSuccess();
    onCodeRef.current(match.kind === "cloud" ? match.code : match.raw, match);
    window.clearTimeout(rearmRef.current);
    rearmRef.current = window.setTimeout(() => {
      busyRef.current = false;
      setBusy(false);
    }, REARM_MS);
  }, [toast]);

  // Открытие — всегда с чистого листа: прошлые «остановлено» и счётчик кадров
  // не должны встречать человека при следующем открытии шторки.
  useEffect(() => {
    if (!open) {
      window.clearTimeout(rearmRef.current);
      return;
    }
    busyRef.current = false;
    deliveredRef.current = { raw: "", count: 0 };
    foreignAtRef.current = 0;
    setBusy(false);
    setStalled(false);
    setNote("");
    setCameraError("");
  }, [open]);

  useEffect(() => () => window.clearTimeout(rearmRef.current), []);

  // ── Камера. Живёт строго внутри open: закрыли шторку — поток остановлен. ──
  useEffect(() => {
    if (!open || tgScanner) return;
    let stream: MediaStream | null = null;
    let raf = 0;
    let cancelled = false;

    const tick = () => {
      if (cancelled) return;
      const video = videoRef.current;
      const canvas = canvasRef.current;
      if (video && canvas && video.readyState === video.HAVE_ENOUGH_DATA && !busyRef.current) {
        const w = video.videoWidth, h = video.videoHeight;
        if (w && h) {
          canvas.width = w; canvas.height = h;
          const ctx = canvas.getContext("2d", { willReadFrequently: true });
          if (ctx) {
            ctx.drawImage(video, 0, 0, w, h);
            const img = ctx.getImageData(0, 0, w, h);
            const code = jsQR(img.data, img.width, img.height, { inversionAttempts: "dontInvert" });
            if (code && code.data) deliver(code.data);
          }
        }
      }
      raf = requestAnimationFrame(tick);
    };

    (async () => {
      // Браузер отдаёт камеру только защищённой странице. Клиент открывают и по
      // http://192.168.…:8080 — там getUserMedia не существует вовсе, и молчать
      // об этом нельзя: человек ждал бы видео на чёрном экране.
      const insecure = typeof window !== "undefined" && window.isSecureContext === false;
      if (!navigator.mediaDevices?.getUserMedia || insecure) {
        setCameraError(t("scan.cameraUnavailable"));
        tlog("scan:camera-unavailable", { secure: typeof window !== "undefined" ? window.isSecureContext : null });
        return;
      }
      try {
        stream = await navigator.mediaDevices.getUserMedia({
          video: { facingMode: { ideal: "environment" } },
          audio: false,
        });
        if (cancelled) { stream.getTracks().forEach((track) => track.stop()); return; }
        const video = videoRef.current;
        if (video) {
          video.srcObject = stream;
          video.setAttribute("playsinline", "true");
          await video.play().catch(() => {});
        }
        raf = requestAnimationFrame(tick);
      } catch (e) {
        const name = (e as Error)?.name;
        setCameraError(name === "NotAllowedError" || name === "SecurityError"
          ? t("scan.cameraDenied")
          : t("scan.cameraUnavailable"));
        tlog("scan:camera-error", { name });
      }
    })();

    return () => {
      cancelled = true;
      cancelAnimationFrame(raf);
      stream?.getTracks().forEach((track) => track.stop());
      const video = videoRef.current;
      if (video) {
        try { video.pause(); } catch { /* уже остановлено */ }
        video.srcObject = null;
      }
    };
  }, [open, attempt, tgScanner, deliver]);

  // ── Telegram: штатный сканер мини-приложения (свою камеру там не открыть). ──
  useEffect(() => {
    if (!open || !tgScanner) return;
    let cancelled = false;
    setTgIdle(false);
    (async () => {
      const raw = await scanQrInTelegram(hintRef.current || t("scan.hint"));
      if (cancelled) return;
      setTgIdle(true); // сканер закрыт — оставляем кнопку «Сканировать QR-код»
      if (raw) deliver(raw);
    })();
    return () => {
      cancelled = true;
      try { getTelegram()?.closeScanQrPopup?.(); } catch { /* уже закрыт */ }
    };
  }, [open, attempt, tgScanner, deliver]);

  if (!open) return null;

  const rescan = () => {
    haptic();
    busyRef.current = false;
    deliveredRef.current = { raw: "", count: 0 };
    setStalled(false);
    setBusy(false);
    setNote("");
    // Камеру дёргаем, только если она и была мертва: живой поток перезапускать
    // ради повторного чтения кадра незачем.
    if (cameraError || tgScanner) { setCameraError(""); setAttempt((a) => a + 1); }
  };

  const line = busy ? t("scan.connecting") : (cameraError || hint || t("scan.hint"));
  const sub = stalled ? t("scan.stopped") : note;

  return (
    <SheetShell
      open={open}
      onClose={onClose}
      overlayClassName="qr-scan-overlay"
      className="qr-scan-sheet"
      labelledBy="qr-scan-title"
    >
      <style>{QR_SCAN_CSS}</style>
      <div className="qr-scan-head">
        <h2 className="qr-scan-title" id="qr-scan-title">{t("scan.title")}</h2>
        <button className="qr-scan-close" onClick={() => { haptic(); onClose(); }} aria-label={t("modal.close")}>
          {"✕"}
        </button>
      </div>

      <div className="qr-scan-stage">
        {tgScanner ? (
          <div className="qr-scan-fallback">{t("scan.tgNote")}</div>
        ) : cameraError ? (
          <div className="qr-scan-fallback">{cameraError}</div>
        ) : (
          <>
            <video ref={videoRef} className="qr-scan-video" muted playsInline />
            <div className={`qr-scan-frame${busy ? " ok" : ""}`} />
          </>
        )}
        <canvas ref={canvasRef} style={{ display: "none" }} />
      </div>

      <div className="qr-scan-hint" role="status" aria-live="polite">{line}</div>
      {sub && <div className="qr-scan-note">{sub}</div>}

      <div className="qr-scan-actions">
        {(stalled || cameraError || (tgScanner && tgIdle)) && (
          <button className={`qr-scan-btn${stalled || tgScanner ? " qr-scan-btn-primary" : ""}`} onClick={rescan}>
            {tgScanner && !stalled ? t("scan.open") : `↻ ${stalled ? t("scan.rescan") : t("scan.retry")}`}
          </button>
        )}
        <button className="qr-scan-btn" onClick={() => { haptic(); (onManual ?? onClose)(); }}>
          {t("scan.manual")}
        </button>
      </div>
    </SheetShell>
  );
}

/**
 * CSS живёт рядом с компонентом (как и раньше у экрана `/scan`): шторка
 * открывается поверх ЛЮБОГО экрана всех четырёх сборок, и её вид не должен
 * зависеть от того, доехал ли до цели общий styles.css.
 */
const QR_SCAN_CSS = `
.qr-scan-overlay { position:fixed; inset:0; z-index:300; background:rgba(0,0,0,.72);
  display:flex; align-items:stretch; justify-content:center; }
.qr-scan-sheet { position:relative; display:flex; flex-direction:column; width:100%;
  background:#0b0f0e; color:#fff; overflow:hidden;
  padding-top:env(safe-area-inset-top); padding-bottom:env(safe-area-inset-bottom); }
/* Планшет и десктоп: карточка по центру, а не камера во весь монитор. */
@media (min-width:768px) {
  .qr-scan-overlay { align-items:center; padding:24px; }
  .qr-scan-sheet { max-width:520px; max-height:min(760px,92vh); border-radius:20px; }
  .qr-scan-stage { min-height:300px; }
}
.qr-scan-head { display:flex; align-items:center; gap:8px; padding:6px 6px 6px 16px; }
.qr-scan-title { flex:1; margin:0; font-size:16px; font-weight:700; color:#fff; }
.qr-scan-close { min-width:44px; min-height:44px; border:0; border-radius:12px;
  background:transparent; color:#fff; font-size:17px; line-height:1; }
.qr-scan-stage { position:relative; flex:1 1 auto; min-height:220px; background:#000;
  display:flex; align-items:center; justify-content:center; overflow:hidden; }
.qr-scan-video { position:absolute; inset:0; width:100%; height:100%; object-fit:cover; }
.qr-scan-frame { position:relative; width:min(62vw,240px); height:min(62vw,240px); border-radius:24px;
  border:3px solid rgba(255,255,255,.9); box-shadow:0 0 0 4000px rgba(0,0,0,.45);
  transition:border-color .2s; }
.qr-scan-frame.ok { border-color:#2ee6b0; }
/* Нет камеры (Telegram, http-адрес, запрет) — на её месте объяснение словами. */
.qr-scan-fallback { padding:24px 20px; text-align:center; font-size:15px; line-height:1.45;
  color:rgba(255,255,255,.86); }
.qr-scan-hint { padding:14px 16px 0; text-align:center; font-size:15px; color:#fff; }
.qr-scan-note { padding:6px 16px 0; text-align:center; font-size:13px; color:rgba(255,255,255,.78); }
.qr-scan-actions { display:flex; flex-wrap:wrap; gap:10px; justify-content:center; padding:14px 16px; }
.qr-scan-btn { flex:1 1 44%; max-width:240px; min-height:44px; padding:12px 16px; border-radius:12px;
  border:1px solid rgba(255,255,255,.3); background:rgba(255,255,255,.12);
  color:#fff; font-size:15px; font-weight:600; }
.qr-scan-btn-primary { background:#2ee6b0; border-color:#2ee6b0; color:#06120e; }
`;
