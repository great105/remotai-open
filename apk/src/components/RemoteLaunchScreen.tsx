import { t } from "../i18n";
import { isOnPCPanel } from "../config";
import { BottomNav } from "./BottomNav";
import { IconScreen } from "./icons";

/**
 * Экран запуска удалённого управления — то, что человек видит ДО нажатия.
 *
 * Что здесь и почему: тап по вкладке «Экран компьютера» раньше СРАЗУ отдавал
 * управление — сокет открывался при монтировании, рабочий стол начинал
 * стримиться, палец по картинке уже кликал по чужим окнам. Теперь между
 * «зашёл» и «управляю» стоит одно осознанное нажатие.
 *
 * Сюда же переехало всё, что раньше всплывало ПОСЛЕ подключения и потому
 * читалось как поломка: старт-панель виртуального браузера для сервера без
 * монитора и предупреждения «служба Windows не видит рабочий стол» / «нет
 * дисплея». Их место — до нажатия, рядом с кнопкой.
 *
 * Живёт отдельным файлом: `RemoteView.tsx` — самый большой экран продукта, и
 * эта его часть ни одного из 40 состояний стрима не касается, а читается и
 * правится чаще прочих (это первое, что видит человек).
 */
export function RemoteLaunchScreen({
  deviceName,
  linkState,
  knownDisplays,
  launchNotice,
  serviceMode,
  noDisplay,
  platform,
  vbGate,
  vbRunning,
  vbStarting,
  vbHint,
  vbError,
  onBack,
  onStart,
  onRecheck,
  onVbStart,
  onVbStop,
}: {
  deviceName: string;
  /** Состояние связи с машиной: выясняем / в сети / выключена. */
  linkState: "checking" | "online" | "offline";
  /** Сколько мониторов было в прошлый сеанс (0 — неизвестно). */
  knownDisplays: number;
  /** Почему прошлая попытка не удалась (пусто — попыток ещё не было). */
  launchNotice: string;
  serviceMode: boolean;
  noDisplay: boolean;
  /** ОС компьютера. На macOS «нет дисплея» значит совсем другое — см. ниже. */
  platform?: string;
  /** "needed" — у машины нет экрана вовсе, нужен виртуальный браузер. */
  vbGate: string;
  vbRunning: boolean;
  vbStarting: boolean;
  vbHint: string;
  vbError: string;
  onBack: () => void;
  onStart: () => void;
  onRecheck: () => void;
  onVbStart: () => void;
  onVbStop: () => void;
}) {
  return (
    <div className="page remote-launch-page">
      <div className="page-header">
        <button className="back-btn" aria-label={t("generic.back")} onClick={onBack}>
          {"←"}
        </button>
        {/* DeviceChip здесь намеренно НЕТ: имя машины и её состояние стоят
            крупно в карточке ниже, а чип повторил бы их 11-м кеглем — ровно
            то дублирование, за которое экраны и переделывали. */}
        <div className="page-header-context">
          <h1>{t("remote.launchTitle")}</h1>
        </div>
      </div>

      <div className="page-content remote-launch">
        <div className="remote-launch-card">
          {/* Линейная иконка вместо эмодзи 🖥: эмодзи рисуется системным
              шрифтом и на каждой платформе выглядит по-своему — всегда цветной
              и всегда спорит с мятно-графитовой палитрой бренда. display:flex
              здесь потому, что у прежнего эмодзи размер задавал font-size, и
              инлайновый SVG в той же строке оставлял под собой пустую полосу
              под базовой линией. */}
          <div className="remote-launch-icon" style={{ display: "flex" }} aria-hidden="true">
            <IconScreen size={44} />
          </div>
          {/* Безымянная машина (LAN, адрес вместо имени) зовётся «Этот
              компьютер», а не заголовком раздела: «Экран компьютера» строкой
              ниже шапки с тем же текстом — это не имя, это эхо. */}
          <div className="remote-launch-name">{deviceName || t("devices.chipThisPc")}</div>
          <div className={`remote-launch-status ${linkState}`}>
            {/* Пока выясняем — крутится спиннер, а не цветная точка: точка
                утверждает, а мы ещё не знаем. */}
            {linkState === "checking" ? (
              <span className="spinner spinner-sm" style={{ width: 12, height: 12 }} aria-hidden="true" />
            ) : (
              <span className="remote-launch-dot" aria-hidden="true" />
            )}
            <span>
              {linkState === "online"
                ? t("remote.launchOnline")
                : linkState === "offline" ? t("conn.pcOffline") : t("remote.launchChecking")}
              {/* Мониторы — по прошлому сеансу: ради этой строки ПК не
                  беспокоим, но если их два, знать это заранее полезно. Рядом с
                  «Компьютер не в сети» это цифры ни о чём, поэтому только у
                  живой машины. */}
              {linkState === "online" && knownDisplays > 0
                ? ` · ${t("remote.launchMonitors", { n: knownDisplays })}`
                : ""}
            </span>
          </div>
          {/* Обещание «вы увидите рабочий стол» у выключенной машины звучит
              издевательством: там нужно не обещание, а условие, при котором
              экран поднимется. */}
          <p className="remote-launch-desc">
            {linkState === "offline" ? t("remote.launchDescOffline") : t("remote.launchDesc")}
          </p>

          {/* Человек СИДИТ ЗА ЭТИМ компьютером (окно Remotai на ПК): экран
              самого себя внутри себя — бесполезная трансляция и лишняя
              нагрузка. Не запрещаем (иногда это проверяют намеренно), но
              говорим прямо (аудит онбординга 30.08.2026). */}
          {isOnPCPanel() && linkState !== "offline" && (
            <p className="remote-launch-self">{t("remote.launchSelf")}</p>
          )}

          {launchNotice ? (
            <div className="remote-launch-notice">{launchNotice}</div>
          ) : null}

          {/* Сервер без монитора: включать нечего, пока не поднят виртуальный
              экран, — поэтому главной кнопкой там становится «Запустить
              браузер» ниже, а не «Включить экран». */}
          {vbGate !== "needed" && (
            linkState === "offline" ? (
              // Выключенный компьютер: яркая кнопка «Включить экран» вела в
              // тупик — строкой выше уже написано, что машины нет в сети. Тут
              // работает то же правило, что на главной (disabled={pcOffline}),
              // а вместо неё человеку даётся действие, которое ему доступно.
              <>
                <button className="btn btn-primary remote-launch-start" disabled>
                  {t("remote.launchStart")}
                </button>
                <button className="btn btn-secondary remote-launch-start" onClick={onRecheck}>
                  {"↻ "}{t("offline.retry")}
                </button>
              </>
            ) : (
              <button className="btn btn-primary remote-launch-start" onClick={onStart}>
                {launchNotice ? t("remote.launchStartAgain") : t("remote.launchStart")}
              </button>
            )
          )}
        </div>

        {/* Обе беды — про НАСТРОЙКУ живой машины, поэтому у выключенной их не
            показываем: пришли они последним удачным ответом и рядом с
            «Компьютер не в сети» читались бы как вторая, противоречащая
            версия происходящего. */}
        {serviceMode && linkState !== "offline" && (
          <div className="remote-launch-warn">{t("remote.serviceMode")}</div>
        )}
        {noDisplay && vbGate !== "needed" && linkState !== "offline" && (
          // На macOS «дисплей не найден» — НЕ про заблокированный компьютер.
          //
          // Захвата экрана у мак-агента пока нет вовсе: библиотека снимка
          // собирается без CGO и честно отвечает «ноль дисплеев». Прежний текст
          // советовал разблокировать компьютер и включить монитор — человек с
          // маком делал бы это бесконечно, потому что и то и другое у него уже
          // так. Терминалы, файлы и мониторинг на маке работают полностью, и
          // сказать надо именно это.
          <div className="remote-launch-warn">
            {t(platform === "darwin" ? "remote.noDisplayMac" : "remote.noDisplay")}
          </div>
        )}

        {vbGate === "needed" && (
          <div className="remote-launch-vb">
            <div className="remote-vb-icon" aria-hidden="true">{"🌐"}</div>
            <div className="remote-vb-title">{t("remote.vbTitle")}</div>
            <div className="remote-vb-desc">{t("remote.vbDesc")}</div>
            {vbHint ? <div className="remote-vb-hint">{vbHint}</div> : null}
            {vbError ? <div className="remote-vb-error">{vbError}</div> : null}
            <button
              className="btn btn-primary remote-vb-start"
              onClick={onVbStart}
              disabled={vbStarting}
            >
              {vbStarting ? t("remote.vbStarting") : t("remote.vbStart")}
            </button>
          </div>
        )}

        {vbRunning && (
          <button className="btn remote-launch-vbstop" onClick={onVbStop}>
            {t("remote.vbStop")}
          </button>
        )}
      </div>

      <BottomNav active="remote" />
    </div>
  );
}
