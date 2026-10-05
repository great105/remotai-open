import { useCallback, useRef, useState } from "react";
import { useLocation, useNavigate, useSearchParams } from "react-router-dom";

import { QrScanSheet, type ScanMatch } from "../components/QrScanSheet";
import { hapticSuccess } from "../telegram";
import { runPair } from "../cloud/pair";
import { approveQrLogin } from "../cloud/tgLogin";
import { connectLan } from "../pairPayload";
import { openExternalLink } from "../openExternal";
import { backFallback, canStepBack } from "../navBack";
import { hasServerConfig } from "../config";
import { mapApiError, useToast } from "@tgcontrol/shared";
import { t } from "../i18n";
import { tlog } from "../debuglog";

/**
 * Маршрут `/scan` — тонкая обёртка над шторкой сканера (`QrScanSheet`).
 *
 * Сама камера, разбор QR и объяснение чужого кода живут в шторке: её открывают
 * прямо над «Моими компьютерами», не уводя человека с экрана. Отдельный маршрут
 * остаётся рабочим, потому что на него ведут ссылки снаружи приложения — из
 * бота и с экранов входа, где никакого списка машин ещё нет.
 *
 * Здесь — только то, что делать с распознанным кодом: облачный QR
 * (`remotai://pair?relay=…&code=…`) подтверждаем на релее и уходим в
 * приложение; QR режима «По локальной сети» подключаем напрямую к ПК, как при
 * ручном вводе кода в LoginView.
 */
export function ScanView() {
  const navigate = useNavigate();
  const location = useLocation();
  const [searchParams] = useSearchParams();
  // Пришли с LAN-экрана (LoginView) — ручной ввод там свой, по коду «По
  // локальной сети»; облачное поле кода ему не поможет.
  const fromLan = !!(location.state as { lan?: boolean } | null)?.lan;
  const { toastError } = useToast();
  const busyRef = useRef(false);
  // Успели увидеть QR локальной сети — значит ручной ввод человеку нужен
  // LAN-овый (длинный base64), а не облачное поле на 8 символов.
  const sawLanRef = useRef(false);
  const [error, setError] = useState("");
  const workspaceId = searchParams.get("workspace") || undefined;
  const deviceType = searchParams.get("type") === "server" ? "server" : "computer";
  const zoneId = searchParams.get("zone") || undefined;

  /** Закрытие шторки = уход с маршрута: шаг назад, а без истории — на главную. */
  const close = useCallback(() => {
    if (canStepBack(location)) navigate(-1);
    else navigate(backFallback(location), { replace: true });
  }, [location, navigate]);

  const onCode = useCallback((_code: string, match: ScanMatch) => {
    if (busyRef.current) return;
    busyRef.current = true;
    if (match.kind === "lan") sawLanRef.current = true;
    setError("");
    void (async () => {
      try {
        if (match.kind === "tglogin") {
          // QR входа с большого экрана. Телефон уже вошёл — подтверждаем сами,
          // компьютер заберёт доступ своим обычным опросом. Если релей ещё не
          // знает эту дверь (старая версия на бою), остаётся прежний путь —
          // открыть бота, там человек нажмёт «Запустить».
          if (await approveQrLogin(match.token)) {
            hapticSuccess();
            setError(t("scan.qrLoginApproved"));
            return;
          }
          await openExternalLink(match.url);
          setError(t("scan.tgLoginOpened"));
          return;
        }
        if (match.kind === "lan") {
          // QR режима «По локальной сети»: url+token, как при ручном вводе кода.
          await connectLan(match.url, match.token);
        } else {
          await runPair(match.relay, match.code, { workspaceId, deviceType, zoneId });
        }
        navigate("/", { replace: true });
      } catch (e) {
        // Отказ показываем и тостом, и подписью под видоискателем: тост на
        // чёрном экране камеры человек часто не успевает прочитать.
        const msg = mapApiError(e);
        setError(msg);
        toastError(msg);
        tlog("scan:pair-error", { message: (e as Error)?.message, status: (e as { status?: number })?.status });
      } finally {
        busyRef.current = false;
      }
    })();
  }, [deviceType, navigate, toastError, workspaceId, zoneId]);

  /** «Ввести код вручную»: у маршрута поля кода рядом нет — ведём к нему. */
  const manual = useCallback(() => {
    // QR локальной сети даёт длинный код «адрес|токен» — его место на
    // LAN-экране, а не в облачном поле на 8 символов.
    if (fromLan || sawLanRef.current) {
      navigate("/login");
      return;
    }
    // Неавторизованному /infrastructure закрыт (RequireAuth уводит на
    // /cloud-login, теряя и add=1, и подсказку) — раньше человек молча
    // возвращался на экран входа со СВЁРНУТОЙ секцией кода и не понимал,
    // куда его вводить. Ведём туда сами и просим раскрыть поле кода.
    if (!hasServerConfig()) {
      navigate("/cloud-login", { state: { showCode: true } });
      return;
    }
    navigate(`/infrastructure?add=1${workspaceId ? `&workspace=${encodeURIComponent(workspaceId)}` : ""}&type=${deviceType}${zoneId ? `&zone=${encodeURIComponent(zoneId)}` : ""}`);
  }, [deviceType, fromLan, navigate, workspaceId, zoneId]);

  return (
    <QrScanSheet
      open
      onClose={close}
      onCode={onCode}
      onManual={manual}
      hint={error || t("scan.hintPc")}
    />
  );
}
