import { useState, useEffect, type CSSProperties, type ComponentType } from "react";
import { useNavigate } from "react-router-dom";
import { SheetShell, useEscape } from "@tgcontrol/shared";
import { onConnectionChange } from "../api";
import { haptic } from "../telegram";
import { t } from "../i18n";
import { getMode, isOnPCPanel } from "../config";
import { sectionDesc, type SectionId } from "../sections";
import { GUIDE_GROUPS, GUIDE_SECTIONS, matchesGuideQuery, searchGuideShortcuts, type GuideSection } from "../guide/sections";
import { listSshHostsCached } from "../sshCommon";
import { useCapabilities } from "../hooks/useCapabilities";
import { useSupportUnread } from "../hooks/useSupportUnread";
import {
  IconHome, IconDevices, IconFolder, IconTerminal, IconActivity, IconScreen,
  IconGear, IconServer, IconMore, IconGauge, IconSliders, IconDoc, IconStar, IconChat, IconRobot } from "./icons";

/** Какая вкладка подсвечена. Тип экспортируем: его же берёт скелет ожидания
 *  маршрута в App.tsx, который рисует ту же навигацию, пока грузится чанк. */
export type NavTab =
  | "home" | "sessions" | "devices" | "files" | "terminal" | "system"
  | "remote" | "panel" | "ssh" | "usage" | "hermes" | "settings" | "guide" | "support" | "plan" | "more";

interface Props {
  active: NavTab;
}

interface NavItem {
  id: NavTab;
  Icon: ComponentType<{ size?: number; className?: string }>;
  labelKey: string;
  /** Подпись для узких экранов (≤390px) — усечение того же имени. */
  shortLabelKey?: string;
  /**
   * Имя РАЗДЕЛА длиннее, чем помещается в пункт панели. Тогда на самой панели
   * стоит короткий синоним, а полное имя остаётся в шторке «Ещё» и в
   * aria-label. Так «Как работать с системой» не занимает в сайдбаре три
   * строки и не выдавливает нижние пункты за край.
   */
  navLabelKey?: string;
  /** Маршрут раздела. У «Ещё» его нет: вкладка открывает шторку. */
  path: string;
  /**
   * Раздел в общей таблице (apk/src/sections.ts). Из неё шторка берёт
   * пояснение — своего текста у неё больше нет: подписи одного раздела жили в
   * двух местах словаря и разъехались («Процессор, память, диски и запущенные
   * программы» здесь против «Процессор, память, процессы; сон и выключение» на
   * главной), а человек читает два описания одного места как два разных места.
   */
  sectionId?: SectionId;
}

// `shortLabelKey` — подпись для узких экранов (≤390px): место выигрываем словом
// покороче, а не кеглем (ниже 10px подписи сливаются в серую кашу). Переключение
// делает CSS (.nav-label-full / .nav-label-short), поэтому подписи не прыгают на
// ресайзе и не зависят от JS-замеров. Полная подпись остаётся в aria-label.
const HOME_ITEM: NavItem = { id: "home", Icon: IconHome, labelKey: "nav.home", path: "/" };
// Терминал сразу после «Главной»: терминалы — основной способ управления
// (UX-аудит ТОП-10 #9), файлы вторичны.
const TERMINAL_ITEM: NavItem = { id: "terminal", Icon: IconTerminal, labelKey: "nav.terminal", path: "/pty" };
const HERMES_ITEM: NavItem = { id: "hermes", Icon: IconRobot, labelKey: "nav.hermes", path: "/hermes" };
const FILES_ITEM: NavItem = { id: "files", Icon: IconFolder, labelKey: "nav.files", path: "/files" };
const REMOTE_ITEM: NavItem = { id: "remote", Icon: IconScreen, labelKey: "nav.remote", shortLabelKey: "nav.remote.short", path: "/remote" };

// ── Разделы, которые на телефоне живут в шторке «Ещё» ──────────────────────
// Жалоба владельца продукта: «захожу с телефона — вкладки SSH не видно, только
// когда на экране управления нажимаю» и «не понимаю, где управление другими
// компами и серверами». Так и было: «SSH-серверы» гасил CSS-костыль до 768px и
// показывал только пока человек УЖЕ внутри раздела, а «Мои компьютеры» в
// локальном режиме не было вовсе — попасть в оба раздела из навигации телефона
// было нельзя в принципе. Теперь у них есть постоянная дверь: пятая вкладка
// «Ещё» со списком разделов, у каждого — пояснение, что внутри.
//
// «Система» снова имеет именную дверь: переходы с главной вели сразу к
// «Питанию», а монитор процессов оставался невидимым. Поиск над списком
// находит конкретные действия; при пустом запросе здесь только разделы.
// «SSH-серверы» в списке показываются после добавления первого сервера, а
// поиск ведёт в пустой экран с кнопкой «Добавить SSH-сервер» и до этого.
const DEVICES_ITEM: NavItem = {
  id: "devices", Icon: IconDevices, labelKey: "nav.devices", shortLabelKey: "nav.devices.short",
  path: "/infrastructure", sectionId: "devices",
};
const SSH_ITEM: NavItem = {
  id: "ssh", Icon: IconServer, labelKey: "nav.ssh", shortLabelKey: "nav.ssh.short",
  path: "/ssh", sectionId: "ssh",
};
const SYSTEM_ITEM: NavItem = { id: "system", Icon: IconActivity, labelKey: "nav.system", path: "/system", sectionId: "system" };
// Раздел «Агенты»: аккаунты нейросетей, лимиты подписок, что установлено и как
// запускается. Адрес /usage остался алиасом — на него ведут кнопки бота и гид.
const USAGE_ITEM: NavItem = { id: "usage", Icon: IconGauge, labelKey: "agents.title", path: "/agents", sectionId: "usage" };
// «Панель ПК» — управление этим компьютером. Имеет смысл только на клиенте,
// открытом на самом ПК по loopback (окно exe): её эндпоинты loopback-only.
const PANEL_ITEM: NavItem = { id: "panel", Icon: IconSliders, labelKey: "nav.panel", path: "/panel", sectionId: "panel" };
const SETTINGS_ITEM: NavItem = { id: "settings", Icon: IconGear, labelKey: "settings.title", path: "/settings", sectionId: "settings" };
// Личный кабинет — в шторке «Ещё» и в сайдбаре, рядом с настройками.
//
// Без этого пункта путь к деньгам с главной был в ТРИ нажатия: «Ещё» →
// «Настройки» → «Подписка», и замер probe-pay-path это поймал. Деньги — не
// подраздел настроек, а отдельный разговор, и открываться должны так же прямо,
// как всё остальное. sectionId даёт строке подпись из общей таблицы: до волны 3
// аудита ИА (02.09.2026, P1-1) кабинет был единственной строкой шторки без
// пояснения, что внутри.
const PLAN_ITEM: NavItem = {
  id: "plan", Icon: IconStar, labelKey: "account.title", path: "/account", sectionId: "account",
};
// Гид — такой же раздел, как остальные, и его дверь обязана стоять в общем
// каталоге. До этого «Как работать с системой» жило только строкой на главной:
// человек, ушедший с главной, справку по продукту найти не мог, а на широком
// экране её не было вовсе (UX-аудит 2026-08-23, NIELSEN-12, HOLISTIC-7).
const GUIDE_ITEM: NavItem = {
  id: "guide", Icon: IconDoc, labelKey: "guide.title", navLabelKey: "nav.guide",
  path: "/guide", sectionId: "guide",
};
// Чат с поддержкой — своя дверь в навигации. До волны 3 аудита ИА (02.09.2026,
// P1-2) до него добирались только через настройки, а точка «есть ответ» висела
// на «Главной», где ответа нет. В сайдбаре пункт зовётся коротко («Поддержка»),
// полное имя — в шторке и для скринридера, как у гида.
const SUPPORT_ITEM: NavItem = {
  id: "support", Icon: IconChat, labelKey: "settings.help.supportChat", navLabelKey: "nav.support",
  path: "/support", sectionId: "support",
};

/**
 * Содержимое шторки «Ещё», в порядке показа. «SSH-серверы» и «Панель ПК» —
 * с условием (см. moreItems в компоненте): первые только тому, у кого есть
 * хотя бы один сервер, вторая только в окне на самом ПК.
 */
const MORE_ITEMS: NavItem[] = [
  DEVICES_ITEM, REMOTE_ITEM, USAGE_ITEM, SYSTEM_ITEM, PLAN_ITEM, SETTINGS_ITEM, GUIDE_ITEM, SUPPORT_ITEM, SSH_ITEM, PANEL_ITEM,
];

const MORE_SEARCH_ALIASES: Partial<Record<NavTab, string[]>> = {
  guide: [t("ui.bottomnav.m050adc9123"), t("ui.bottomnav.m59cebb5a70"), t("ui.bottomnav.mb6846e0341")],
  support: [t("ui.bottomnav.m597d4fefc0"), t("ui.bottomnav.m9983122d67")],
};

/**
 * Разделы, за которые на телефоне отвечает вкладка «Ещё» (подсветка).
 * «ssh» остаётся подсвеченным и тогда, когда строка условно скрыта: в раздел
 * можно прийти через поиск.
 */
const MORE_IDS = new Set<NavTab>(MORE_ITEMS.map((item) => item.id));

const MORE_ITEM: NavItem = { id: "more", Icon: IconMore, labelKey: "nav.more", path: "" };

/**
 * Один пункт панели. `wideOnly` — раздел, у которого на телефоне вкладки нет
 * (он в шторке «Ещё»), а в сайдбаре ≥768px место вертикальное и не кончается,
 * поэтому там он стоит отдельным пунктом. `narrowOnly` — наоборот: сама «Ещё»
 * нужна только там, где навигация горизонтальная полоса.
 * Оба варианта ВСЕГДА в разметке, показ решает медиазапрос (.nav-tab-wide /
 * .nav-tab-narrow) — состав панели не зависит от JS-замеров ширины и верен уже
 * в первом кадре, включая поворот экрана.
 */
interface NavSlot {
  item: NavItem;
  wideOnly?: boolean;
  narrowOnly?: boolean;
}

// Точка «есть непрочитанный ответ поддержки». Стили инлайном намеренно:
// styles.css правит другой исполнитель, а точка не должна ждать его правки —
// иначе индикатор молча не появится. Цвет/фон — те же переменные, что у
// .settings-badge и .bottom-nav.
const NAV_ICON_STYLE: CSSProperties = { position: "relative" };
const UNREAD_DOT_STYLE: CSSProperties = {
  position: "absolute",
  top: -1,
  right: -5,
  width: 8,
  height: 8,
  borderRadius: "50%",
  background: "var(--tg-destructive)",
  boxShadow: "0 0 0 2px var(--tg-section-bg)", // отбивка от иконки
};

export function BottomNav({ active }: Props) {
  const navigate = useNavigate();
  const [connected, setConnected] = useState(true);
  const [moreOpen, setMoreOpen] = useState(false);
  const [moreQuery, setMoreQuery] = useState("");
  const { remoteDesktop } = useCapabilities();
  const onPCPanel = isOnPCPanel();
  // Подпись раздела зависит от режима: в локальном «все компьютеры аккаунта»
  // обещает список, которого без аккаунта не будет. count сюда НЕ передаём —
  // иначе шторка потянула бы список серверов на каждом экране.
  const cloud = getMode() === "cloud";
  // Нижняя навигация есть на всех основных экранах и переживает переходы между
  // ними — это и делает её общим держателем поллинга непрочитанных ответов
  // поддержки. Экраны-потребители (шестерёнка главной) берут готовое число
  // через useSupportUnread() без запросов.
  const supportUnread = useSupportUnread(true);
  // Есть ли у аккаунта хоть один SSH-сервер: от этого зависит, показывать ли
  // строку «SSH-серверы» в шторке телефона. null — ещё не знаем (или ПК не
  // ответил): строки нет, дверь в раздел остаётся с главной и из «Моих
  // компьютеров». listSshHostsCached кэширует ответ на 60 с и склеивает
  // параллельные вызовы, поэтому навигация, смонтированная на каждом экране,
  // лишних запросов не даёт (аудит ИА 02.09.2026, волна 3).
  const [hasSshHosts, setHasSshHosts] = useState<boolean | null>(null);

  useEffect(() => {
    return onConnectionChange((state) => setConnected(state.connected));
  }, []);

  useEffect(() => {
    let alive = true;
    listSshHostsCached()
      .then((hosts) => { if (alive) setHasSshHosts(hosts.length > 0); })
      // Нет связи с ПК или режим без серверов — строки просто не будет.
      .catch(() => {});
    return () => { alive = false; };
  }, []);

  // Системная «Назад» и Esc закрывают шторку, а не уводят с экрана.
  useEscape(moreOpen, () => { setMoreOpen(false); setMoreQuery(""); });

  // Порядок в разметке даёт верный порядок на ОБЕИХ ширинах:
  //   телефон  — Главная · Терминалы · Hermes · Файлы · Ещё (ровно пять);
  //   Экран ПК доступен в «Ещё» и отдельно в широком сайдбаре.
  //   сайдбар  — Главная · Мои компьютеры · Терминалы · Файлы · Система ·
  //              Экран ПК · SSH-серверы · Агенты · Личный кабинет · [Панель ПК] ·
  //              Чат с поддержкой · Справка · Настройки.
  const slots: NavSlot[] = [{ item: HOME_ITEM }];
  // «Мои компьютеры» — и в локальном режиме тоже. Раньше пункт показывали
  // только в облаке, и при прямом подключении к своему ПК ответа на «где мои
  // остальные компьютеры и серверы» в навигации не было вовсе.
  slots.push({ item: DEVICES_ITEM, wideOnly: true });
  slots.push({ item: TERMINAL_ITEM });
  slots.push({ item: HERMES_ITEM });
  slots.push({ item: FILES_ITEM });
  slots.push({ item: SYSTEM_ITEM, wideOnly: true });
  // Headless-сервер (нет дисплея) не может стримить экран.
  if (remoteDesktop) slots.push({ item: REMOTE_ITEM, wideOnly: true });
  slots.push({ item: SSH_ITEM, wideOnly: true });
  // «Агенты» и «Настройки» в сайдбаре не стояли вовсе: на телефоне их держала
  // шторка «Ещё», а на ≥768px шторки нет — и оба раздела выпадали из навигации
  // насовсем. В «Агенты» на широком экране вела единственная дверь: блок
  // «Возможности» на главной (UX-аудит 2026-08-23, HOLISTIC-3, NIELSEN-2).
  // Порядок именно такой: сначала разделы, потом помощь и настройки внизу.
  // «Личный кабинет» и «Чат с поддержкой» в сайдбаре не стояли вовсе — на
  // широком экране до денег и до ответа поддержки навигация не вела (аудит ИА
  // 02.09.2026, P1-1, P1-2).
  slots.push({ item: USAGE_ITEM, wideOnly: true });
  slots.push({ item: PLAN_ITEM, wideOnly: true });
  if (onPCPanel) slots.push({ item: PANEL_ITEM, wideOnly: true });
  slots.push({ item: SUPPORT_ITEM, wideOnly: true });
  slots.push({ item: GUIDE_ITEM, wideOnly: true });
  slots.push({ item: SETTINGS_ITEM, wideOnly: true });
  slots.push({ item: MORE_ITEM, narrowOnly: true });

  // Две строки шторки с условием. «Панель ПК»: её эндпоинты loopback-only, с
  // телефона открывать нечего. «SSH-серверы»: только тому, у кого есть хотя бы
  // один, — остальным строка обещала раздел, в котором пусто (волна 3).
  const moreItems = MORE_ITEMS.filter((item) => {
    if (item.id === "remote") return remoteDesktop;
    if (item.id === "panel") return onPCPanel;
    if (item.id === "ssh") return hasSshHosts === true;
    return true;
  });
  // При поиске показываем и пустой SSH-раздел: там можно добавить первый сервер.
  // Совпадение по имени раздела идёт выше упоминаний в статьях гида.
  const navResults = MORE_ITEMS.filter((item) => {
    if (item.id === "panel" && !onPCPanel) return false;
    return [t(item.labelKey), ...(item.navLabelKey ? [t(item.navLabelKey)] : []),
      ...(MORE_SEARCH_ALIASES[item.id] ?? [])]
      .some((name) => matchesGuideQuery(moreQuery, name));
  });
  const moreResults = searchGuideShortcuts(GUIDE_SECTIONS, moreQuery, t);
  const visibleResults = moreResults.slice(0, Math.max(0, 6 - navResults.length));
  const resultCount = navResults.length + moreResults.length;

  // На /sessions (старый режим чата с агентом) намеренно не подсвечена ни одна
  // вкладка: этого экрана в навигации нет. Раньше он приравнивался к «Главной»,
  // и тап по подсвеченной вкладке уходил в ветку «уже здесь» — прокрутку
  // наверх вместо перехода, то есть уйти с экрана нижней панелью было нельзя
  // (UX-аудит 2026-08-23, HOLISTIC-12).
  const activeId: NavTab = active;
  // «Ещё» подсвечена, пока человек на любом из своих маршрутов. Ложной
  // подсветки соседей при этом нет: вкладка самого раздела на телефоне
  // скрыта медиазапросом (display: none — она уходит и из потока, и из дерева
  // доступности), а в сайдбаре скрыта уже сама «Ещё».
  const moreActive = MORE_IDS.has(activeId);

  const closeMore = () => { setMoreOpen(false); setMoreQuery(""); };

  const handleTab = (item: NavItem) => {
    haptic();
    if (item.id === "more") {
      if (moreOpen) closeMore(); else setMoreOpen(true);
      return;
    }
    if (activeId === item.id) {
      // Tap on active tab = scroll to top (iOS pattern)
      window.scrollTo({ top: 0, behavior: "smooth" });
    } else {
      navigate(item.path);
    }
  };

  const handleMoreItem = (item: NavItem) => {
    haptic();
    closeMore();
    // Человек уже в этом разделе — шторка просто закрывается, лишней
    // перезагрузки экрана не будет.
    if (activeId !== item.id) navigate(item.path);
  };

  const handleFeatureResult = (section: GuideSection) => {
    haptic();
    closeMore();
    const action = section.actions?.find((candidate) => candidate.route !== "/panel" || onPCPanel);
    navigate(action?.route ?? `/guide?topic=${encodeURIComponent(section.id)}`);
  };

  const openFullSearch = (browseTopics = false) => {
    const query = moreQuery.trim();
    haptic();
    closeMore();
    navigate(query && !browseTopics ? `/guide?q=${encodeURIComponent(query)}` : "/guide");
  };

  return (
    <>
    <nav className="bottom-nav" aria-label={t("nav.a11y")}>
      {slots.map(({ item, wideOnly, narrowOnly }) => {
        // Точка «есть ответ поддержки» висит на двери, которая к нему ведёт: на
        // телефоне это «Ещё» (чат — строка её шторки), в сайдбаре — сам пункт
        // «Чат с поддержкой». Раньше она стояла на «Главной», и связать точку с
        // поддержкой можно было только догадкой (аудит ИА 02.09.2026, P1-2).
        // Оба пункта всегда в разметке, показ решает медиазапрос — точка не
        // зависит от JS-замера ширины.
        const unreadDot = supportUnread > 0 && (item.id === MORE_ITEM.id || item.id === SUPPORT_ITEM.id);
        const label = t(item.labelKey);
        // Видимая подпись может быть короче имени раздела; скринридер и шторка
        // всегда получают полное имя.
        const navLabel = item.navLabelKey ? t(item.navLabelKey) : label;
        const shortLabel = item.shortLabelKey ? t(item.shortLabelKey) : navLabel;
        const isActive = item.id === "more" ? moreActive : activeId === item.id;
        const cls = [isActive ? "active" : "", wideOnly ? "nav-tab-wide" : "",
          narrowOnly ? "nav-tab-narrow" : ""].filter(Boolean).join(" ");
        return (
        <button key={item.id} data-nav-id={item.id}
          className={cls}
          onClick={() => handleTab(item)}
          aria-label={unreadDot ? `${label} · ${t("support.unreadA11y")}` : label}
          aria-haspopup={item.id === "more" ? "dialog" : undefined}
          aria-expanded={item.id === "more" ? moreOpen : undefined}
          // У «Ещё» страница не своя — она лишь держит группу разделов,
          // поэтому общее aria-current="true", а не "page".
          aria-current={isActive ? (item.id === "more" ? "true" : "page") : undefined}>
          <span className="nav-icon" aria-hidden="true" style={NAV_ICON_STYLE}>
            <item.Icon size={21} />
            {unreadDot && <span style={UNREAD_DOT_STYLE} />}
          </span>
          {/* Оба варианта в разметке всегда: показ решает медиазапрос, а
              скринридер берёт полное имя из aria-label выше (обе подписи
              скрыты от него, иначе он читал бы вкладку дважды). */}
          <span className="nav-label" aria-hidden="true">
            <span className="nav-label-full">{navLabel}</span>
            <span className="nav-label-short">{shortLabel}</span>
          </span>
        </button>
        );
      })}
      {/* Точка «нет связи». Была единственным английским словом в русской
          навигации (title="Disconnected"), причём на телефоне HTML-подсказку не
          вызвать вовсе — человек видел мигающий кружок неизвестного смысла.
          Теперь у неё русское имя, и его читает скринридер (role="img" +
          aria-label; без role точка остаётся декорацией и не озвучивается).
          Кликабельной НЕ делаем намеренно: чтобы попасть по 8px пальцем,
          пришлось бы растянуть зону нажатия на ~32px, а она лежит поверх
          последней вкладки и воровала бы её тапы. Словами причину и кнопку
          «Повторить» уже даёт ConnectionBanner сверху экрана. */}
      {!connected && (
        <span
          className="nav-conn-dot"
          role="img"
          aria-label={t("conn.offline")}
          title={t("conn.offline")}
        />
      )}
    </nav>

    {/* Шторка «Ещё». Полноценное окно, а не выпадашка: role="dialog",
        aria-modal, ловушка Tab и возврат фокуса на вкладку — всё это даёт
        SheetShell; системная «Назад» закрывает её через useEscape выше. */}
    <SheetShell
      open={moreOpen}
      onClose={closeMore}
      overlayClassName="more-sheet-overlay"
      className="more-sheet"
      labelledBy="more-sheet-title"
    >
      <div className="more-sheet-head">
        <h2 className="more-sheet-title" id="more-sheet-title">{t("nav.more")}</h2>
        <button className="more-sheet-close" onClick={closeMore} aria-label={t("modal.close")}>
          {"✕"}
        </button>
      </div>
      <label className="more-sheet-search-label" htmlFor="more-feature-search">{t("more.search")}</label>
      <div className="more-sheet-search-row">
        <input
          id="more-feature-search"
          type="search"
          className="more-sheet-search-input"
          value={moreQuery}
          placeholder={t("more.searchPlaceholder")}
          autoCapitalize="off"
          autoCorrect="off"
          spellCheck={false}
          onChange={(event) => setMoreQuery(event.target.value)}
        />
        {moreQuery && <button type="button" className="more-sheet-search-clear" onClick={() => setMoreQuery("")}>{t("more.searchClear")}</button>}
      </div>
      {moreQuery.trim() ? <div className="more-sheet-results">
        <span className="sr-only" role="status" aria-live="polite">{t("more.resultCount", { n: resultCount })}</span>
        {navResults.length > 0 && <div className="more-sheet-group-label">{t("more.sections")}</div>}
        {navResults.map((item) => <button key={item.id} type="button" className="more-sheet-result"
          onClick={() => handleMoreItem(item)}>
          <span className="more-sheet-result-title">{t(item.labelKey)}</span>
          <span className="more-sheet-result-desc">{item.sectionId ? sectionDesc(item.sectionId, { cloud }) : ""}</span>
        </button>)}
        {visibleResults.length > 0 && navResults.length > 0 && <div className="more-sheet-group-label">{t("more.features")}</div>}
        {visibleResults.map((section) => {
          const group = GUIDE_GROUPS.find((candidate) => candidate.id === section.group);
          return <button key={section.id} type="button" className="more-sheet-result" onClick={() => handleFeatureResult(section)}>
            <span className="more-sheet-result-title">{t(section.titleKey)}</span>
            <span className="more-sheet-result-desc">{group ? `${t(group.titleKey)} · ` : ""}{t(section.leadKey)}</span>
          </button>;
        })}
        {resultCount === 0 && <p className="more-sheet-empty">{t("more.noResults")}</p>}
        {(moreResults.length > visibleResults.length || resultCount === 0) && (
          <button type="button" className="more-sheet-all" onClick={() => openFullSearch(resultCount === 0)}>
            {t(resultCount === 0 ? "more.browseTopics" : "more.allResults")} →
          </button>
        )}
      </div> : <div className="more-sheet-list">
        {moreItems.map((item) => {
          const current = activeId === item.id;
          // Та же точка, что на вкладке «Ещё»: открыв шторку, человек должен
          // увидеть, КАКАЯ строка её несёт.
          const rowDot = supportUnread > 0 && item.id === SUPPORT_ITEM.id;
          return (
            <button
              key={item.id}
              data-more-id={item.id}
              className={`more-sheet-item${current ? " active" : ""}`}
              aria-current={current ? "page" : undefined}
              aria-label={rowDot ? `${t(item.labelKey)} · ${t("support.unreadA11y")}` : undefined}
              onClick={() => handleMoreItem(item)}
            >
              <span className="more-sheet-icon" aria-hidden="true" style={NAV_ICON_STYLE}>
                <item.Icon size={22} />
                {rowDot && <span style={UNREAD_DOT_STYLE} />}
              </span>
              <span className="more-sheet-text">
                <span className="more-sheet-name">{t(item.labelKey)}</span>
                {item.sectionId && (
                  <span className="more-sheet-desc">{sectionDesc(item.sectionId, { cloud })}</span>
                )}
              </span>
              {current
                ? <span className="more-sheet-current">{t("more.current")}</span>
                : <span className="more-sheet-mark" aria-hidden="true">{"›"}</span>}
            </button>
          );
        })}
      </div>}
    </SheetShell>
    </>
  );
}
