import { useCallback, useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";
import { t, useToast } from "@tgcontrol/shared";
import { onConnectionChange } from "../api";
import { getMode, getSelectedDeviceId, getSelectedDeviceName } from "../config";
import type { CloudDevice } from "../cloud/api";
import { getDevicePtySummaries, listDevicesCached, type DeviceRuntimeSummary } from "../deviceSummaries";
// Что написано на плитке и в каком порядке плитки стоят — правило, а не
// разметка: живёт в deviceTiles.ts и проверяется без React (deviceTiles.test.ts).
import { orderTiles, TONE_CLASS } from "../deviceTiles";
import { humanDeviceName, selectDevice } from "../devices";
import { haptic, hapticSuccess } from "../telegram";

/**
 * Такт опроса чужих машин — тот же, что у сводки на главной (HomeActivity).
 * Совпадение намеренное: оба потребителя ходят через общий кэш
 * (deviceSummaries: 10 с на сводку, 60 с на список устройств, dedupe летящих
 * запросов), поэтому второй блок на экране не стоит ни одного лишнего запроса.
 */
const POLL_MS = 30_000;

/**
 * Сколько ждём переключения, прежде чем снять пометку «переключаю». Смена
 * машины — это реконнект живого канала и перезапрос возможностей (0,5–2 с);
 * плитка обязана сказать, что она думает, а не замереть. Таймер — страховка на
 * случай, если канал так и не доложит о себе.
 */
const SWITCH_TIMEOUT_MS = 6_000;

/**
 * Сколько живёт кнопка «Вернуться на …» под плитками.
 *
 * Отмена действия существует в продукте ровно в одном месте — после смены
 * машины, — и жила она только в тосте: 2,8 с у ВЕРХНЕГО края экрана. Человек,
 * случайно тапнувший плитку соседней машины, понимает это по сменившемуся
 * содержимому, а пока большой палец дотягивается до верха шестидюймового
 * экрана, «Отменить» уже нет. Поэтому дубль отмены стоит там, где случился тап,
 * и живёт заметно дольше. Тост остаётся — он сообщает сам факт переключения.
 */
const UNDO_MS = 20_000;

/**
 * Переключатель машин на главной.
 *
 * Зачем он здесь, а не в шапке каждого экрана: смена управляемой машины — это
 * не навигация, а выбор рабочего места, и делают его, начиная работу. Главная
 * для этого и открыта. На остальных экранах остаётся ЧИП-указатель (DeviceChip)
 * — он отвечает на «где я», но не переключает.
 *
 * Раньше переключиться можно было только через `/infrastructure` — экран
 * администрирования парка (зоны, теги, роли, приглашения, аудит). Смена машины
 * стоила похода в настройки и возврата, а состояние соседних машин при этом
 * было не видно вовсе: человек переключался вслепую.
 *
 * В ряду стоит и ТЕКУЩАЯ машина — первой и с пометкой «Управляете сейчас».
 * Без неё ряд читался как «какие-то другие компьютеры»: где я сейчас, он не
 * говорил, а после тапа менялся местами сам с собой (машина, которую выбрали,
 * из ряда исчезала, а прежняя появлялась) — переключение выглядело как сбой,
 * а не как результат. Теперь тап переносит пометку на тапнутую плитку, и
 * модель «нажал — управляю ею» видна прямо в ряду.
 *
 * С ОДНОЙ машиной плиток нет — выбирать не из чего. Но строка «Все компьютеры»
 * остаётся ВСЕГДА, в том числе в локальном режиме: это единственная дверь в
 * список машин с главной, а добавляют вторую машину именно оттуда.
 */
export function DeviceSwitcher() {
  const { toastSuccess } = useToast();
  const navigate = useNavigate();
  const [devices, setDevices] = useState<CloudDevice[]>([]);
  const [summaries, setSummaries] = useState<Record<string, DeviceRuntimeSummary>>({});
  // id машины, на которую переключаемся прямо сейчас. Конфиг меняется сразу,
  // поэтому «Переключаю…» пишет уже ПЕРВАЯ плитка (тапнутая машина стала
  // текущей), а связь с ней ещё догоняет; остальные на это время выключены.
  const [switching, setSwitching] = useState("");
  // Машина, с которой только что ушли: пока она здесь, под плитками стоит
  // «Вернуться на …» (см. UNDO_MS).
  const [undoTo, setUndoTo] = useState<CloudDevice | null>(null);
  // Тик раз в минуту: «не в сети 3 ч» должно стареть само, без ответа сервера.
  const [, setTick] = useState(0);

  const load = useCallback(async () => {
    if (getMode() !== "cloud") return;
    try {
      const list = await listDevicesCached();
      setDevices(list);
      const selected = getSelectedDeviceId();
      setSummaries(await getDevicePtySummaries(list.filter((d) => d.id !== selected)));
    } catch {
      // Краткий обрыв релея не должен стирать последнюю известную картину.
    }
  }, []);

  useEffect(() => {
    let poll = 0;
    let tick = 0;
    // В фоне не опрашиваем: телефон в кармане не должен будить чужие машины.
    const start = () => {
      if (poll) return;
      void load();
      poll = window.setInterval(() => void load(), POLL_MS);
      tick = window.setInterval(() => setTick((n) => n + 1), 60_000);
    };
    const stop = () => {
      window.clearInterval(poll); poll = 0;
      window.clearInterval(tick); tick = 0;
    };
    const onVis = () => (document.hidden ? stop() : start());
    if (!document.hidden) start();
    document.addEventListener("visibilitychange", onVis);
    return () => { stop(); document.removeEventListener("visibilitychange", onVis); };
  }, [load]);

  // Пометка «переключаю» снимается по факту: живой канал доложил, что связь с
  // новой машиной есть. Таймер — только страховка от вечной пометки.
  useEffect(() => {
    if (!switching) return;
    const off = onConnectionChange((state) => {
      if (state.connected) { setSwitching(""); void load(); }
    });
    const timer = window.setTimeout(() => setSwitching(""), SWITCH_TIMEOUT_MS);
    return () => { off(); window.clearTimeout(timer); };
  }, [switching, load]);

  // Кнопка отмены гаснет сама: висеть вечно она не должна, а «отменить» через
  // полчаса после переключения — уже не отмена, а второе переключение.
  useEffect(() => {
    if (!undoTo) return;
    const timer = window.setTimeout(() => setUndoTo(null), UNDO_MS);
    return () => window.clearTimeout(timer);
  }, [undoTo]);

  const goTo = useCallback((device: CloudDevice) => {
    setSwitching(device.id);
    selectDevice(device.id, device);
  }, []);

  const switchTo = useCallback((device: CloudDevice) => {
    const from = devices.find((d) => d.id === getSelectedDeviceId());
    haptic();
    goTo(device);
    hapticSuccess();
    // Отмена живёт в двух местах: тост сообщает факт («Работаем на …»), а
    // кнопка под плитками остаётся под пальцем — см. UNDO_MS.
    setUndoTo(from || null);
    // Тост с «Отменить» вместо диалога подтверждения: человек уже сказал, куда
    // хочет, и спрашивать разрешение на это же — лишний вопрос. А вот вернуться
    // одним нажатием после случайного тапа — нужно.
    toastSuccess(t("devices.switcher.switched", { name: deviceName(device) }), from
      ? {
        action: {
          label: t("devices.switcher.undo"),
          onClick: () => { setUndoTo(null); goTo(from); },
        },
      }
      : undefined);
  }, [devices, goTo, toastSuccess]);

  // Локальный режим (окно на ПК, телефон по QR) управляет одной машиной —
  // плиток там нет. Дверь в список машин ниже рисуется всё равно: добавляют
  // вторую машину именно там, и другого пути с главной нет.
  const cloud = getMode() === "cloud";
  const selected = getSelectedDeviceId();
  const others = !cloud
    ? []
    : orderTiles(devices.filter((device) => device.id !== selected), summaries, deviceName);
  // Имя текущей машины: из списка, а пока он не доехал — из конфига (то же имя
  // показывает чип в шапке рабочих экранов). Адрес именем не считаем — см.
  // humanDeviceName; запасное слово здесь пустое намеренно: плитка «Управляете
  // сейчас» существует, чтобы НАЗВАТЬ машину, и без имени её рисовать незачем.
  const currentDevice = devices.find((device) => device.id === selected);
  const currentName = currentDevice ? deviceName(currentDevice) : humanDeviceName(getSelectedDeviceName(), "");

  // Машина, на которую предлагаем вернуться, могла сама оказаться выбранной
  // (человек уже нажал «Отменить» в тосте) — тогда кнопке отмены здесь не место.
  const undoDevice = undoTo && undoTo.id !== selected ? undoTo : null;

  return (
    <>
    {others.length > 0 && (
      <>
        {/* Что делает тап — словами и один раз. Плитка сама этого не скажет:
            кнопка с именем машины и строкой состояния одинаково похожа и на
            «переключиться», и на «открыть подробности». */}
        <div className="device-tiles-hint">{t("devices.switcher.tapHint")}</div>
        <div className="device-tiles" role="group" aria-label={t("devices.switcher.a11y")}>
          {currentName && (
            /* Не кнопка: нажимать «я и так здесь» нечего, а disabled-кнопка
               читалась бы как «машина недоступна». */
            <div
              className={`device-tile current${switching ? " switching" : ""}${currentDevice && !currentDevice.online ? " offline" : ""}`}
              aria-current="true"
            >
              <span className="device-tile-top">
                <span className="device-tile-mark" aria-hidden>{"✓"}</span>
                <span className="device-tile-name">{currentName}</span>
              </span>
              <span className="device-tile-state">
                {/* Состояние ТЕКУЩЕЙ машины плитка не показывала вовсе: у
                    соседних было «в сети / не в сети», а у той, которой
                    управляешь, — только «Управляете сейчас». Из-за этого полоса
                    «не в сети» сверху выглядела сообщением ни о ком: человек
                    переключился на сервер, а понять, он ли молчит, было нечем. */}
                {switching
                  ? t("devices.switcher.switching")
                  : currentDevice && !currentDevice.online
                    ? t("devices.tile.offline")
                    : t("devices.switcher.current")}
              </span>
            </div>
          )}
          {others.map(({ device, text, tone }) => (
            <button
              key={device.id}
              type="button"
              className={["device-tile", TONE_CLASS[tone]].filter(Boolean).join(" ")}
              disabled={!!switching}
              onClick={() => switchTo(device)}
            >
              <span className="device-tile-top">
                <span className={`device-tile-dot ${device.online ? "online" : "offline"}`} aria-hidden />
                <span className="device-tile-name">{deviceName(device)}</span>
              </span>
              <span className="device-tile-state">{text}</span>
            </button>
          ))}
        </div>
      </>
    )}
    {/* Тап по плитке ПЕРЕКЛЮЧАЕТ машину — и на этом всё: полного списка машин
        с главной было не достать вовсе (раздел уехал с нижней панели телефона
        в «Ещё», а ряд показывает только соседей и только в облаке). Строка под
        рядом — явный путь в «Мои компьютеры»: добавить ещё один компьютер или
        сервер, переименовать, отвязать. */}
    <button
      type="button"
      className="device-tiles-all"
      onClick={() => { haptic(); navigate("/infrastructure"); }}
    >
      {t("devices.switcher.all")}
      <span aria-hidden>{"→"}</span>
    </button>
    {undoDevice && (
      <button
        type="button"
        className="device-undo"
        disabled={!!switching}
        onClick={() => { haptic(); setUndoTo(null); goTo(undoDevice); }}
      >
        {"↩ "}{t("devices.switcher.undoTo", { name: deviceName(undoDevice) })}
      </button>
    )}
    </>
  );
}

/**
 * Одно имя машины на все места сразу: плитка, тост «Работаем на …» и кнопка
 * «Вернуться на …». Через общий фильтр — чтобы машина, чей hostname пришёл
 * адресом, не звалась «127.0.0.1» ни в одном из них.
 */
function deviceName(device: CloudDevice): string {
  return humanDeviceName(device.name || device.hostname, t("home.act.deviceUnnamed"));
}
