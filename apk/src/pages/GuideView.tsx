import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";
import { t } from "../i18n";
import { BottomNav } from "../components/BottomNav";
import { FirstSteps } from "../components/FirstSteps";
import { useGoBack } from "../navBack";
import { isOnPCPanel } from "../config";
import { haptic, hapticSuccess } from "../telegram";
import { useToast, getLanguage } from "@tgcontrol/shared";
import { markHomeStep, type HomeStep } from "../homeProgress";
import {
  GUIDE_GROUPS,
  GUIDE_SECTIONS,
  GUIDE_TROUBLES,
  resolveGuideTopic,
  searchGuide,
  type GuideSection,
} from "../guide/sections";

/**
 * «Как работать с системой» — гид по всем возможностям.
 *
 * Прежний гид был восемью абзацами подряд: их дочитывали до третьего. Здесь он
 * говорит языком самого продукта — как пульт, а не как документация:
 *
 *   • тот же чек-лист первых шагов, что и на Главной, — один список, один счёт;
 *   • разделы свёрнуты до заголовка и одной строки — сразу видно, что умеет
 *     система целиком, а подробности открываются нажатием;
 *   • примеры — настоящие команды моноширинным шрифтом, копируются одним тапом:
 *     ровно то, что человек и так переносит в терминал;
 *   • у раздела есть дверь: кнопка ведёт в тот самый экран, о котором читаешь.
 *
 * Поиск ищет по ВСЕМУ тексту раздела, а не по заголовку: человек ищет словом из
 * своей задачи — «пароль», «скилл», «батарея».
 *
 * Гид статичен: ему не нужен ни выбранный ПК, ни сеть, поэтому он в списке
 * DEVICE_GUARD_FREE и не гаснет при офлайне.
 */

/**
 * Отметки старого — РУЧНОГО — чек-листа гида и что с ними стало.
 *
 * До 28.08.2026 гид вёл свой список из пяти шагов, которые человек отмечал
 * пальцем, а «Первые шаги» на главной считали свой — по факту. Про одного и
 * того же человека выходило два ответа сразу: «2 из 3» там и «0 из 5» здесь
 * (NIELSEN-11). Список остался один, автоматический, и настоящий вопрос был не
 * «удалять ли данные», а что увидит человек, у которого здесь уже стоят
 * галочки. Молча их выбросить — значит показать ему, что обновление сбросило
 * прогресс: отметки исчезнут, а объяснения не будет.
 *
 * Поэтому две отметки, у которых в автоматическом списке есть ТОЧНОЕ
 * соответствие, переносятся: «Откройте терминал» → terminal, «Запустите
 * агента» → agent. Трём остальным («Спросите агента про Remotai», «Отвечайте
 * цифрой», «Отчёты в Telegram») переносить некуда — автоматический список их не
 * измеряет, — но их содержание никуда не делось, оно живёт разделами гида.
 *
 * Границы переноса. Он необратим (markHomeStep галочку не снимает), поэтому
 * переносим только то, что человек сам про себя заявил ровно теми же словами,
 * и НЕ додумываем: отметка «Спросите агента» не даёт галочку «Запустить
 * агента». Выполняется один раз — по флагу, а не по наличию ключа, иначе
 * заявленный, но не сделанный шаг возвращался бы после каждой чистки прогресса.
 * Старый ключ не удаляем: если перенос окажется неверным, разбираться будет не
 * по чему.
 */
const LEGACY_STEPS_KEY = "tgcontrol.guide.steps";
const LEGACY_ADOPTED_KEY = "tgcontrol.guide.stepsAdopted.v1";
const LEGACY_TO_HOME: Record<string, HomeStep> = {
  "open-terminal": "terminal",
  "run-agent": "agent",
};

function adoptLegacyGuideSteps(): void {
  try {
    if (localStorage.getItem(LEGACY_ADOPTED_KEY) === "1") return;
    localStorage.setItem(LEGACY_ADOPTED_KEY, "1");
    const raw = localStorage.getItem(LEGACY_STEPS_KEY);
    if (!raw) return;
    const parsed: unknown = JSON.parse(raw);
    if (!Array.isArray(parsed)) return;
    for (const id of parsed) {
      const step = typeof id === "string" ? LEGACY_TO_HOME[id] : undefined;
      if (step) markHomeStep(step);
    }
  } catch {
    /* приватный режим: хранилища нет — и переносить нечего */
  }
}

// Переносим при загрузке чанка гида, до первого рендера: в useEffect галочка
// проставилась бы уже на глазах у человека — «сама собой», без причины.
adoptLegacyGuideSteps();

export function GuideView() {
  const navigate = useNavigate();
  // «←» зовёт то же правило, что и системная «Назад» (см. navBack.ts).
  const stepBack = useGoBack();
  const { toast } = useToast();

  const [searchParams] = useSearchParams();
  const routeQuery = searchParams.get("q") ?? "";
  const [query, setQuery] = useState(routeQuery);
  useEffect(() => setQuery(routeQuery), [routeQuery]);

  // Тема из адреса: подсказки по экрану (HelpSheet) ведут сюда с
  // `?topic=<группа>`, и гид открывается на разделах ТОГО экрана, откуда
  // пришли, а не с начала — до этого между «Подсказками по экрану» и справкой
  // не было ни одной двери (аудит ИА 02.09.2026, P1-5, P1-31; волна 3, п. 5).
  // Незнакомое значение молча даёт обычный гид: битая ссылка не должна
  // показывать пустой экран.
  const focus = useMemo(
    () => resolveGuideTopic(GUIDE_SECTIONS, GUIDE_GROUPS, searchParams.get("topic")),
    [searchParams],
  );
  // Первый раздел темы раскрыт с первого кадра: раскрывать его в useEffect —
  // значит показать сначала свёрнутый, а потом «сам собой» открывшийся.
  const [openId, setOpenId] = useState<string>(focus?.sectionId ?? "");
  const focusRef = useRef<HTMLElement | null>(null);
  useEffect(() => {
    if (!focus) return;
    setOpenId(focus.sectionId);
    // Блок стоит сразу под поиском, но `page-content` может помнить прокрутку
    // — доводим до вида, не двигая то, что и так видно.
    focusRef.current?.scrollIntoView({ block: "nearest" });
  }, [focus]);

  const found = useMemo(() => searchGuide(GUIDE_SECTIONS, query, t), [query, getLanguage()]);

  // Во время поиска разделы раскрыты: искали текст внутри — прятать найденное
  // за вторым нажатием значит заставить искать дважды.
  const searching = query.trim().length > 0;

  const copyCommand = useCallback(async (command: string) => {
    try {
      await navigator.clipboard.writeText(command);
      hapticSuccess();
      toast(t("guide.copied"));
    } catch {
      toast(command); // буфер недоступен — показываем строку, её можно выделить
    }
  }, [toast]);

  const renderSection = (section: GuideSection) => {
    const open = searching || openId === section.id;
    return (
      <div key={section.id} className={`guide-card${open ? " open" : ""}`}>
        <button
          className="guide-card-head"
          aria-expanded={open}
          onClick={() => {
            haptic("light");
            setOpenId(open && !searching ? "" : section.id);
          }}
        >
          <span className="guide-card-icon" aria-hidden>{section.icon}</span>
          <span className="guide-card-titles">
            <span className="guide-card-title">{t(section.titleKey)}</span>
            <span className="guide-card-lead">{t(section.leadKey)}</span>
          </span>
          <span className="guide-card-chev" aria-hidden>{open ? "▴" : "▾"}</span>
        </button>

        {open && (
          <div className="guide-card-body">
            {section.bodyKeys.map((key) => (
              <p key={key} className="guide-p">{t(key)}</p>
            ))}

            {section.examples && section.examples.length > 0 && (
              <div className="guide-examples">
                <div className="guide-examples-title">{t("guide.examples")}</div>
                {section.examples.map((example) => (
                  <button
                    key={example.command}
                    className="guide-cmd"
                    onClick={() => void copyCommand(example.command)}
                    title={t("guide.copy")}
                  >
                    <span className="guide-cmd-text">{example.command}</span>
                    <span className="guide-cmd-label">{t(example.labelKey)}</span>
                    <span className="guide-cmd-copy" aria-hidden>⧉</span>
                  </button>
                ))}
              </div>
            )}

            {/* «Панель ПК» — фрейм loopback-страницы; с телефона и из Telegram
                кнопка вела на пустой экран без шапки и «←» (аудит ИА
                02.09.2026, P0-5). Дверь показываем там же, где её показывает
                навигация: только в окне на ПК. */}
            {section.actions?.filter((action) => action.route !== "/panel" || isOnPCPanel()).map((action) => (
              <button
                key={action.route}
                className="guide-open-btn"
                onClick={() => { haptic(); navigate(action.route); }}
              >
                {t(action.labelKey)} <span aria-hidden>→</span>
              </button>
            ))}
          </div>
        )}
      </div>
    );
  };

  return (
    <div className="page">
      <div className="page-header">
        <button className="back-btn" aria-label={t("generic.back")} onClick={() => { haptic(); stepBack(); }}>
          {"←"}
        </button>
        <h1>{t("guide.title")}</h1>
      </div>

      <div className="page-content home-page-content">
        <div className="guide-wrap">
          <p className="guide-subtitle">{t("guide.subtitle")}</p>
          <button className="btn btn-secondary" style={{ minHeight: 44, marginBottom: 16 }} onClick={() => navigate("/start")}>{t("ui.guideview.ma8b43b8b9e")}</button>

          <input
            className="guide-search"
            value={query}
            aria-label={t("guide.search")}
            placeholder={t("guide.search")}
            autoCapitalize="off"
            autoCorrect="off"
            spellCheck={false}
            onChange={(e) => setQuery(e.target.value)}
          />

          {/* «По этому экрану»: разделы группы из `?topic=` — первыми, до
              общего списка. При поиске блока нет: человек ищет по всему гиду,
              и «откуда пришёл» ему уже не важно (аудит ИА 02.09.2026, P1-31).
              Ниже — «Все темы»: первые шаги и остальные группы как обычно. */}
          {focus && !searching && (
            <>
              <section ref={focusRef} className="guide-group">
                <h2 className="guide-h2 guide-group-title">{t("guide.forScreen")}</h2>
                {GUIDE_SECTIONS.filter((section) => section.group === focus.group).map(renderSection)}
              </section>
              <h2 className="guide-h2 guide-group-title">{t("guide.allTopics")}</h2>
            </>
          )}

          {/* Первые шаги — ТОТ ЖЕ блок и тот же счётчик, что на Главной. Свой
              список гид больше не ведёт: два чек-листа отвечали про одного
              человека по-разному (NIELSEN-11). Блок сам исчезает, когда шаги
              пройдены или скрыты на Главной, — и это правильно: за справкой
              возвращается тот, кому чек-лист новичка уже не нужен. */}
          {!searching && (
            // Обёртка нужна только ради отступа снизу (`.guide-group` — это
            // margin-bottom и ничего больше): у карточки чек-листа своего
            // отступа нет, и заголовок первой группы прилипал к ней вплотную.
            // ignoreDismissed: в справке чек-лист показываем и тому, кто скрыл
            // его на главной — здесь он часть руководства, а не подсказка.
            <div className="guide-group"><FirstSteps ignoreDismissed /></div>
          )}

          {/* Разделы по группам. Пустая группа заголовок не рисует — при поиске
              иначе висят подписи без содержимого. */}
          {GUIDE_GROUPS.map((group) => {
            // Группа темы уже показана выше, в «По этому экрану»; второй раз
            // те же карточки — это «остальные группы» перестали бы быть остальными.
            if (focus && !searching && group.id === focus.group) return null;
            const items = found.filter((section) => section.group === group.id);
            if (items.length === 0) return null;
            return (
              <section key={group.id} className="guide-group">
                <h2 className="guide-h2 guide-group-title">{t(group.titleKey)}</h2>
                {items.map(renderSection)}
              </section>
            );
          })}

          {found.length === 0 && <p className="guide-empty">{t("guide.nothing")}</p>}

          {!searching && (
            <section className="guide-group">
              <h2 className="guide-h2 guide-group-title">{t("guide.troubleTitle")}</h2>
              {GUIDE_TROUBLES.map((trouble) => (
                <div key={trouble.id} className="guide-trouble">
                  <div className="guide-trouble-q">{t(trouble.symptomKey)}</div>
                  <div className="guide-trouble-a">{t(trouble.answerKey)}</div>
                </div>
              ))}
            </section>
          )}

          {/* Дверь в поддержку в конце: дочитавший и не нашедший ответ — ровно
              тот, кому она нужна. */}
          <button className="home-guide-row" onClick={() => { haptic(); navigate("/support"); }}>
            <span className="home-guide-icon" aria-hidden>{"💬"}</span>
            <span className="home-guide-text">
              <span className="home-guide-row-title">{t("guide.supportRow")}</span>
              <span className="home-guide-row-desc">{t("guide.supportRowDesc")}</span>
            </span>
            <span className="home-guide-chevron">{"›"}</span>
          </button>
        </div>
      </div>
      <BottomNav active="guide" />
    </div>
  );
}
