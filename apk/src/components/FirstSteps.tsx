import { useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";
import { isAgentKind, useToast } from "@tgcontrol/shared";
import { listPtySessions } from "../api";
import { haptic } from "../telegram";
import { t } from "../i18n";
import {
  dismissFirstSteps, homeStepIds, lastKnownPhonePaired, markHomeStep, rememberPhonePaired,
  restoreFirstSteps, useHomeProgress, type HomeStep,
} from "../homeProgress";
import { isOnPCPanel } from "../config";

interface StepMeta {
  titleKey: string;
  descKey: string;
  path: string;
}

const META: Record<HomeStep, StepMeta> = {
  terminal: { titleKey: "home.steps.terminal", descKey: "home.steps.terminalDesc", path: "/pty" },
  agent:    { titleKey: "home.steps.agent",    descKey: "home.steps.agentDesc",    path: "/pty?new=1&agent=1" },
  remote:   { titleKey: "home.steps.remote",   descKey: "home.steps.remoteDesc",   path: "/remote" },
  files:    { titleKey: "home.steps.files",    descKey: "home.steps.filesDesc",    path: "/files" },
};

interface Props {
  /**
   * ПК не в сети (признак приходит с главной — живой канал + провал health).
   * Шаги требуют компьютера, поэтому гасим их, а прогресс показываем последний
   * известный: обнулять чеклист человеку, который всё настроил, — враньё.
   */
  pcOffline?: boolean;
  /**
   * Показывать чек-лист даже после «Скрыть». Нужно справке: там он не
   * подсказка новичку на главной, а часть руководства — человек пришёл
   * читать, как работать, и «первые шаги» отвечают ровно на этот вопрос.
   * Скрытие на главной справку не касается.
   */
  ignoreDismissed?: boolean;
}

/**
 * «Первые шаги» — чеклист освоения для нового пользователя.
 *
 * Правила поведения: галочки ставятся по факту (см. homeProgress), блок сам
 * исчезает, когда все шаги пройдены, и его можно скрыть вручную. Так главная
 * учит новичка, но не занимает место у того, кто уже работает.
 */
export function FirstSteps({ pcOffline = false, ignoreDismissed = false }: Props) {
  const navigate = useNavigate();
  const { toastSuccess } = useToast();
  const { done, dismissed } = useHomeProgress();
  const onPC = isOnPCPanel();
  // На телефоне/в облаке шаг «телефон привязан» выполнен по определению; в окне
  // exe его знает только сам ПК — и до ответа берём последнее известное
  // значение, иначе у недоступного агента чеклист скатывался в «0 из 3».
  const [phonePaired, setPhonePaired] = useState(
    onPC ? (lastKnownPhonePaired() ?? false) : true,
  );
  // Показанное значение — из кэша, а не из ответа ПК.
  const [pairedStale, setPairedStale] = useState(false);

  // Факты о пройденных шагах собирает сам чек-лист, а не экран под ним.
  // Раньше «терминал открыт» и «агент запущен» отмечала только сводка главной
  // (HomeActivity), и один и тот же чек-лист показывал «2 из 3» на главной и
  // «0 из 3» в справке — то самое расхождение двух чек-листов, ради устранения
  // которого их и сводили в один.
  useEffect(() => {
    let alive = true;
    void listPtySessions()
      .then((r) => {
        if (!alive) return;
        const list = r.sessions || [];
        if (list.length > 0) markHomeStep("terminal");
        if (list.some((s) => isAgentKind(s.agent_kind))) markHomeStep("agent");
      })
      .catch(() => { /* ПК не ответил — прогресс остаётся прежним, врать нечем */ });
    return () => { alive = false; };
  }, []);

  useEffect(() => {
    if (!onPC) return;
    let alive = true;
    fetch("/api/setup/status")
      .then((r) => r.ok ? r.json() : Promise.reject())
      .then((s) => {
        if (!alive) return;
        const paired = !!s.relay_configured;
        setPhonePaired(paired);
        setPairedStale(false);
        rememberPhonePaired(paired);
      })
      .catch(() => {
        // Агент не ответил: помечаем прогресс устаревшим, только если кэш
        // вообще есть — иначе честнее промолчать (клиент открыт впервые).
        if (alive) setPairedStale(lastKnownPhonePaired() !== undefined);
      });
    return () => { alive = false; };
  }, [onPC]);

  const steps = homeStepIds();
  const doneCount = (phonePaired ? 1 : 0) + steps.filter((id) => done[id]).length;
  const total = steps.length + 1;
  if ((dismissed && !ignoreDismissed) || doneCount >= total) return null;

  return (
    <div className="home-steps">
      <div className="home-steps-head">
        <span className="home-guide-title" style={{ padding: 0 }}>{t("home.steps.title")}</span>
        <span className="home-steps-count">{t("home.steps.count", { done: doneCount, total })}</span>
        {/* «Скрыть» было необратимым: чеклист новичка исчезал навсегда, а
            вернуть его было нечем — при зоне нажатия 48×20 промах стоил всего
            обучения. Диалог здесь был бы лишним кликом (данные не теряются),
            поэтому отмена живёт в тосте, а второй путь — строка «Показать
            "Первые шаги"» в настройках, если тост уже уехал. */}
        <button
          className="home-steps-dismiss"
          onClick={() => {
            haptic();
            dismissFirstSteps();
            toastSuccess(t("home.steps.hidden"), {
              action: { label: t("home.steps.restore"), onClick: () => restoreFirstSteps() },
            });
          }}
        >
          {t("home.steps.hide")}
        </button>
      </div>

      {/* «Включите компьютер — шаги станут доступны» отсюда убрано: про
          выключенный ПК на главной уже сказано баннером связи и карточкой
          готовности, а шаги и без слов погашены (disabled + .offline). */}
      {pairedStale && <div className="home-steps-note">{t("home.steps.stale")}</div>}

      {/* На телефоне этот шаг никуда не ведёт (обработчик работает только в
          окне на ПК) — поэтому и рисуем его строкой, а не кнопкой: новичок,
          который знакомится с чеклистом и тапает первую строку, получал не
          отклик и не переход, а молчание. Это первое взаимодействие с
          приложением после привязки. */}
      {onPC ? (
        <button
          className={`home-step${phonePaired ? " done" : ""}`}
          onClick={() => { haptic(); navigate("/panel"); }}
        >
          <span className="home-step-mark">{phonePaired ? "✓" : "○"}</span>
          <span className="home-step-text">
            <span className="home-step-title">{t("home.steps.pcPair")}</span>
            {!phonePaired && <span className="home-step-desc">{t("home.steps.pcPairDesc")}</span>}
          </span>
          {!phonePaired && <span className="home-guide-chevron">{"›"}</span>}
        </button>
      ) : (
        <div className={`home-step home-step-static${phonePaired ? " done" : ""}`}>
          <span className="home-step-mark">{phonePaired ? "✓" : "○"}</span>
          <span className="home-step-text">
            <span className="home-step-title">{t("home.steps.paired")}</span>
          </span>
        </div>
      )}

      {steps.map((id) => {
        const meta = META[id];
        const isDone = !!done[id];
        return (
          <button
            key={id}
            // Шаг ведёт на экран, который без ПК ничего не покажет: с
            // выключенным компьютером кнопка гаснет, а не заводит в пустоту.
            className={`home-step${isDone ? " done" : ""}${pcOffline ? " offline" : ""}`}
            disabled={pcOffline}
            onClick={() => { haptic(); navigate(meta.path); }}
          >
            <span className="home-step-mark">{isDone ? "✓" : "○"}</span>
            <span className="home-step-text">
              <span className="home-step-title">{t(meta.titleKey)}</span>
              {/* В окне НА КОМПЬЮТЕРЕ описания «прямо с телефона» читаются
                  про кого-то другого: человек сидит за этой машиной. Тексты
                  для этой поверхности свои (аудит онбординга 30.08.2026). */}
              {!isDone && <span className="home-step-desc">{t(onPC ? `${meta.descKey}PC` : meta.descKey)}</span>}
            </span>
            {!isDone && <span className="home-guide-chevron">{"›"}</span>}
          </button>
        );
      })}
    </div>
  );
}
