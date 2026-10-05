/**
 * Разделы приложения: одно имя и одна подпись на раздел — данные, а не разметка.
 *
 * Зачем модуль. В один и тот же раздел ведёт несколько дверей (шторка «Ещё»,
 * блок «Возможности» на главной, строки в настройках, карточка в «Системе»,
 * кнопка гида), и каждая дверь описывала его СВОИМИ словами. «Система» была
 * «Процессор, память, диски и запущенные программы» в шторке и «Процессор,
 * память, процессы; сон и выключение» на главной; у раздела агентов описаний
 * набралось четыре — при том что комментарий в словаре прямо требовал одного.
 * Человек читает четыре описания одного экрана как четыре разных экрана.
 *
 * Правило: имя и подпись раздела берутся ТОЛЬКО отсюда — sectionName() и
 * sectionDesc(). Ни литералом в JSX, ни собственным ключом словаря мимо этой
 * таблицы: именно так подписи и разъехались. Новый текст добавляется в
 * packages/shared/src/i18n.ts канон-константой и попадает во все двери сразу.
 *
 * Почему вне React: правило проверяется тестом в node, без DOM (sections.test.ts).
 * По той же причине сюда НЕ импортируется ./config — он тянет @capacitor/core, и
 * тест перестал бы запускаться. Режим приходит параметром.
 *
 * Чего здесь нет: пунктов навигации и их порядка. Они остаются в
 * apk/src/components/BottomNav.tsx — этот файл разбирают как ТЕКСТ два гейта
 * релиза (check-infrastructure-zones.mjs и check-ui-surface.mjs), и вынос
 * NavItem или path сюда уронил бы выпуск.
 */
import { t } from "./i18n";

/**
 * Разделы, у которых есть больше одной двери.
 *
 * `account` и `support` добавлены волной 3 аудита ИА 02.09.2026 (P1-1, P1-2):
 * у личного кабинета двери — шторка «Ещё», сайдбар, строка в настройках и
 * кнопка пробы на главной; у чата поддержки — шторка, сайдбар и настройки.
 * До этого у обоих не было ни одной подписи, а имя кабинета жило в трёх ключах
 * тремя разными словами («Подписка», «Что дальше», «Личный кабинет»).
 */
export type SectionId =
  | "devices" | "ssh" | "system" | "usage" | "panel" | "settings" | "guide" | "account" | "support";

export interface SectionDef {
  id: SectionId;
  /** Маршрут раздела — тот же, что в App.tsx; сверяется тестом. */
  path: string;
  /** Имя раздела. Одно на все двери. */
  nameKey: string;
  /** Усечение того же имени для узких мест. Не другое слово. */
  shortNameKey?: string;
  /** Подпись: что человек внутри сделает. */
  descKey: string;
  /**
   * Вариант подписи для локального режима (окно exe, LAN). Нужен там, где
   * облачная формулировка становится неправдой: аккаунта в локальном режиме нет
   * вовсе, и «все компьютеры аккаунта» обещает список, которого не будет.
   */
  descLocalKey?: string;
  /**
   * Живая цифра о текущем состоянии раздела. ДОПОЛНЯЕТ подпись, а не заменяет
   * её: раньше при четырёх серверах на главной стояло только «4 сохранённых
   * сервера — открыть терминал или файлы», и что это за раздел, человек из
   * строки не узнавал.
   */
  countKey?: string;
}

export const SECTIONS: Record<SectionId, SectionDef> = {
  devices: {
    id: "devices",
    path: "/infrastructure",
    nameKey: "nav.devices",
    shortNameKey: "nav.devices.short",
    descKey: "section.devices.desc",
    descLocalKey: "section.devices.desc.local",
  },
  ssh: {
    id: "ssh",
    path: "/ssh",
    nameKey: "nav.ssh",
    shortNameKey: "nav.ssh.short",
    descKey: "section.ssh.desc",
    countKey: "section.ssh.count",
  },
  system: {
    id: "system",
    path: "/system",
    nameKey: "nav.system",
    descKey: "section.system.desc",
  },
  usage: {
    id: "usage",
    path: "/agents",
    nameKey: "agents.title",
    descKey: "section.usage.desc",
  },
  panel: {
    id: "panel",
    path: "/panel",
    nameKey: "nav.panel",
    descKey: "section.panel.desc",
  },
  settings: {
    id: "settings",
    path: "/settings",
    nameKey: "settings.title",
    descKey: "section.settings.desc",
  },
  guide: {
    id: "guide",
    path: "/guide",
    nameKey: "guide.title",
    // shortNameKey здесь нет намеренно: «Справка» (nav.guide) — не усечение
    // имени «Как работать с системой», а короткий синоним для самой панели.
    // Его показывает BottomNav через navLabelKey, и путать одно с другим нельзя.
    descKey: "section.guide.desc",
  },
  account: {
    id: "account",
    path: "/account",
    // Имя экрана, а не «Подписка»: дверь зовётся так же, как место, куда ведёт.
    // plan.title и home.trial.btn обязаны совпадать с ним — сверяет тест.
    nameKey: "account.title",
    descKey: "section.account.desc",
  },
  support: {
    id: "support",
    path: "/support",
    nameKey: "settings.help.supportChat",
    // shortNameKey здесь нет намеренно: «Поддержка» (nav.support) — короткий
    // синоним для пункта сайдбара, а не усечение имени «Чат с поддержкой».
    // Его, как и «Справку» у гида, BottomNav показывает через navLabelKey.
    descKey: "section.support.desc",
  },
};

/** Порядок нужен только тестам и обходам: двери сами решают, что показывать. */
export const SECTION_IDS: SectionId[] = [
  "devices", "ssh", "system", "usage", "panel", "settings", "guide", "account", "support",
];

/** Имя раздела — то же самое в любой двери. */
export function sectionName(id: SectionId): string {
  return t(SECTIONS[id].nameKey);
}

/**
 * Короткое имя для узких мест. Если усечения у раздела нет, отдаём полное имя:
 * пустая подпись в панели хуже длинной.
 */
export function sectionShortName(id: SectionId): string {
  const short = SECTIONS[id].shortNameKey;
  return short ? t(short) : sectionName(id);
}

export interface SectionDescOpts {
  /**
   * Облачный режим. `false` — окно exe или LAN: там у раздела может быть своя,
   * честная формулировка. `undefined` — режим неизвестен, берём общую подпись.
   */
  cloud?: boolean;
  /**
   * Сколько сейчас внутри. `null` — не знаем (ПК выключен, запрос не дошёл):
   * тогда подпись говорит о разделе и не врёт нулём.
   */
  count?: number | null;
}

/**
 * Подпись раздела. Живая цифра приписывается через « · » — подпись остаётся
 * первой, потому что она отвечает на вопрос «что это за место», а цифра только
 * уточняет. Ноль не показываем: «0 сохранённых серверов» отговаривает заходить
 * ровно там, где раздел и надо открыть, чтобы первый сервер завести.
 */
export function sectionDesc(id: SectionId, opts?: SectionDescOpts): string {
  const def = SECTIONS[id];
  const local = opts?.cloud === false && def.descLocalKey;
  const base = t(local ? (def.descLocalKey as string) : def.descKey);
  const count = opts?.count;
  if (def.countKey && typeof count === "number" && count > 0) {
    return `${base} · ${t(def.countKey, { n: count })}`;
  }
  return base;
}
