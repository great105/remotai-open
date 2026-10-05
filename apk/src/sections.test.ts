/**
 * Правило «один раздел — одно имя и одна подпись» проверяется здесь, а не
 * глазами на снимках экрана: разъезжались имена не разом, а по одному ключу за
 * релиз, и заметить это на скриншоте нельзя.
 *
 * Тест ходит в словарь и в App.tsx, но не в React и не в DOM — окружение node.
 */
import { describe, expect, it } from "vitest";
import { t } from "@tgcontrol/shared";
import { SECTIONS, SECTION_IDS, sectionDesc, sectionName, sectionShortName, type SectionId } from "./sections";

/** Ключ, которого нет в словаре, t() отдаёт как «⟦ключ⟧» (DEV) или как сам ключ. */
const missing = (value: string, key: string) => value === key || value.startsWith("⟦");

/**
 * Маршруты приложения списком, а не разбором App.tsx: читать соседний файл
 * отсюда нечем — в tsconfig apk нет типов node, а сам App.tsx тянет React и в
 * node-окружении не импортируется. Список сверяется с <Route path="…"> в
 * apk/src/App.tsx; если раздел переезжает, правятся оба места.
 */
const routes = new Set([
  "/", "/login", "/cloud-login", "/scan", "/infrastructure", "/agents", "/usage",
  "/devices", "/sessions", "/files", "/terminal", "/pty", "/ssh", "/ssh-files",
  "/system", "/account", "/plan", "/settings", "/support", "/guide", "/panel", "/remote",
]);

describe("таблица разделов", () => {
  it("SECTION_IDS перечисляет ровно то, что есть в SECTIONS", () => {
    expect([...SECTION_IDS].sort()).toEqual(Object.keys(SECTIONS).sort());
    for (const id of SECTION_IDS) expect(SECTIONS[id].id, id).toBe(id);
  });

  // Подпись обещает раздел, а маршрут ведёт в никуда — это хуже, чем отсутствие
  // двери: человек нажимает и получает пустой экран.
  it("маршрут каждого раздела есть среди маршрутов приложения", () => {
    for (const id of SECTION_IDS) {
      expect(routes.has(SECTIONS[id].path), `${id} → ${SECTIONS[id].path}`).toBe(true);
    }
  });

  // Две двери с одинаковой подписью читаются как одно место — ровно та ошибка,
  // из-за которой шторка «Ещё» выглядела повтором главной.
  it("подписи разделов попарно различны", () => {
    const descs = SECTION_IDS.map((id) => sectionDesc(id));
    expect(new Set(descs).size, descs.join(" | ")).toBe(descs.length);
    const keys = SECTION_IDS.map((id) => SECTIONS[id].descKey);
    expect(new Set(keys).size).toBe(keys.length);
  });

  it("имена и подписи есть в словаре", () => {
    for (const id of SECTION_IDS) {
      const def = SECTIONS[id];
      expect(missing(sectionName(id), def.nameKey), def.nameKey).toBe(false);
      expect(missing(sectionDesc(id), def.descKey), def.descKey).toBe(false);
      expect(missing(sectionShortName(id), def.shortNameKey ?? def.nameKey), id).toBe(false);
      if (def.descLocalKey) {
        expect(missing(t(def.descLocalKey), def.descLocalKey), def.descLocalKey).toBe(false);
      }
    }
  });

  // Короткая подпись — усечение того же имени, а не другое слово: то же правило,
  // что уже сторожит check-infrastructure-zones.mjs для вкладок.
  it("короткое имя — часть полного", () => {
    for (const id of SECTION_IDS) {
      if (!SECTIONS[id].shortNameKey) continue;
      const short = sectionShortName(id).toLowerCase();
      expect(sectionName(id).toLowerCase().includes(short), `${id}: «${short}»`).toBe(true);
    }
  });
});

describe("подпись по режиму", () => {
  // В локальном режиме (окно exe, LAN) аккаунта нет вовсе: обещать «все
  // компьютеры аккаунта» человеку, у которого одна машина по локальной сети, —
  // отправлять его искать список, которого не будет.
  it("«Мои компьютеры» вне облака описаны иначе и без слова «аккаунта»", () => {
    const cloud = sectionDesc("devices", { cloud: true });
    const local = sectionDesc("devices", { cloud: false });
    expect(local).not.toBe(cloud);
    expect(local.toLowerCase()).not.toContain("аккаунт");
    expect(cloud.toLowerCase()).toContain("аккаунт");
  });

  it("без указания режима берётся общая подпись", () => {
    expect(sectionDesc("devices")).toBe(sectionDesc("devices", { cloud: true }));
  });

  it("у раздела без локального варианта подпись одна на оба режима", () => {
    for (const id of SECTION_IDS) {
      if (SECTIONS[id].descLocalKey) continue;
      expect(sectionDesc(id, { cloud: false }), id).toBe(sectionDesc(id, { cloud: true }));
    }
  });
});

describe("живая цифра дополняет подпись, а не заменяет её", () => {
  it("подпись остаётся первой, число приписывается через « · »", () => {
    const base = sectionDesc("ssh");
    expect(sectionDesc("ssh", { count: 4 })).toBe(`${base} · 4 сохранённых сервера`);
    expect(sectionDesc("ssh", { count: 1 })).toBe(`${base} · 1 сохранённый сервер`);
    expect(sectionDesc("ssh", { count: 2 })).toBe(`${base} · 2 сохранённых сервера`);
    expect(sectionDesc("ssh", { count: 5 })).toBe(`${base} · 5 сохранённых серверов`);
  });

  // null = «не знаем» (ПК выключен, запрос не дошёл), 0 = «пока ничего нет».
  // В обоих случаях строка обязана оставаться осмысленной сама по себе.
  it("без числа и с нулём подпись чистая, без хвоста", () => {
    const base = sectionDesc("ssh");
    expect(sectionDesc("ssh", { count: null })).toBe(base);
    expect(sectionDesc("ssh", { count: 0 })).toBe(base);
    expect(sectionDesc("ssh", {})).toBe(base);
    expect(base).not.toContain(" · ");
  });

  it("разделам без живой цифры число ничего не приписывает", () => {
    for (const id of SECTION_IDS) {
      if (SECTIONS[id].countKey) continue;
      expect(sectionDesc(id, { count: 7 }), id).toBe(sectionDesc(id));
    }
  });
});

/**
 * Двери, которые пока читают раздел собственными ключами словаря. Пока их не
 * перевели на sectionName/sectionDesc, значение обязано совпадать слово в слово:
 * именно так подписи и разъезжались — ключ жил своей жизнью, и никто не замечал.
 */
const NAME_ALIASES: Array<[SectionId, string[]]> = [
  ["devices", [
    "nav.devices", "infra.title", "devices.switcher.all", "devices.chipOpen",
    "settings.myComputers", "conn.myComputers", "offline.devices",
    "guide.action.openDevices", "settings.webClient",
  ]],
  ["ssh", ["nav.ssh", "settings.sshSection", "guide.action.openSsh"]],
  ["system", ["nav.system", "sys.title", "guide.action.openSystem"]],
  ["usage", [
    "agents.title", "usage.title", "sys.aiLimits", "infra.quotaOpenUsage",
    "settings.home.agents", "agents.helpTitle",
  ]],
  ["panel", ["nav.panel"]],
  ["settings", ["settings.title"]],
  ["guide", ["guide.title"]],
  // Личный кабинет звался тремя словами: «Подписка» в настройках, «Что дальше»
  // на плашке пробы, «Личный кабинет» на самом экране (аудит ИА 02.09.2026,
  // P1-1). Теперь все три двери обязаны нести имя экрана.
  ["account", ["account.title", "plan.title", "home.trial.btn"]],
  ["support", ["settings.help.supportChat"]],
];

// Ключей more.*.desc здесь больше нет: шторка «Ещё» берёт подпись через
// sectionDesc(), своего текста у неё не осталось. Остались двери, которые
// пока зовут раздел собственным ключом — их значения обязаны совпадать с
// подписью раздела слово в слово.
const DESC_ALIASES: Array<[SectionId, string[]]> = [
  // Пусто — и это цель, а не упущение: подпись раздела больше не живёт ни в
  // одном чужом ключе. Все двери (шторка «Ещё», карточка в «Системе», строка в
  // настройках) зовут sectionDesc(). Если здесь снова появится строка, значит
  // кто-то завёл второе описание того же места — именно так они и разъезжались.
];

describe("все двери зовут раздел одинаково", () => {
  for (const [id, keys] of NAME_ALIASES) {
    it(`имя раздела «${id}» одно во всех ключах`, () => {
      for (const key of keys) {
        expect(missing(t(key), key), `нет ключа ${key}`).toBe(false);
        expect(t(key), `${key} против имени раздела ${id}`).toBe(sectionName(id));
      }
    });
  }

  for (const [id, keys] of DESC_ALIASES) {
    it(`подпись раздела «${id}» одна во всех ключах`, () => {
      for (const key of keys) {
        expect(missing(t(key), key), `нет ключа ${key}`).toBe(false);
        expect(t(key), `${key} против подписи раздела ${id}`).toBe(sectionDesc(id));
      }
    });
  }
});
