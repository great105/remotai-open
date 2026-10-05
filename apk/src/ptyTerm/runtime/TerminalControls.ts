export interface SizeViewer { id: string; cols: number; rows: number; canOwn: boolean }
export interface TerminalControls {
  revision: number;
  you: string;
  owner: string;
  leaseUntil: number;
  viewers: SizeViewer[];
  error: string;
}

/** Как PtyTermView хранит историю при стирании (ST-04): legacy — прежнее
 * правило; shadow — наблюдение: исполняет прежнее во ВСЕХ точках (стирание,
 * отложенное, граница поколения, листание reader, полнота документа; кнопки
 * «Очистить историю на этом устройстве» нет), новое только считается, а
 * расхождения уходят diag retention-shadow в журнал агента и в trace; policy —
 * новое правило и новые действия. */
export type RetentionMode = "legacy" | "shadow" | "policy";
const RETENTION_MODES: readonly RetentionMode[] = ["legacy", "shadow", "policy"];

/** Кто выбирает исполнителя жеста прокрутки (ST-02/03, план 9.1): новое правило;
 * прежнее правило (откат); прежнее правило исполняет, новое только считается и
 * расхождения пишутся в журнал. Двойное исполнение намерения (I-01) не
 * возвращается ни в одном режиме. */
export type NavigationMode = "v2" | "legacy" | "shadow";
const NAVIGATION_MODES: readonly NavigationMode[] = ["v2", "legacy", "shadow"];

export interface TerminalFeatures {
  sizeOwner: boolean;
  agentHistory: boolean;
  /** Временная шкала событий клиента, только метаданные (ST-01, I-15). */
  trace: boolean;
  /** Координатор восстановления после разрыва (ST-05). */
  recoveryV1: boolean;
  retention: RetentionMode;
  /**
   * Показ замены целиком (ST-06, замер probe-terminal-flicker): маркер reset на
   * WebGL удерживает прежнюю картинку (RIS+BEGIN DEC 2026) до кадра экрана или
   * сторожа 1000 мс; снимок пишется двумя записями вместо четырёх (голова
   * RIS+история, хвост досылка+кадр, 2026 только на WebGL); клики по
   * удерживаемой картинке гасятся; потеря контекста WebGL сразу уводит на DOM
   * и даёт одну повторную попытку WebGL при возврате на передний план.
   * false — прежние записи по шагам и трёхсекундное ожидание аддона. Читается
   * при открытии терминала.
   */
  presentation: boolean;
  /**
   * Команды как блоки по OSC 133 (ST-10, T-39): обработчик OSC 133 в нормальном
   * буфере, «Копировать команду / вывод», «↑ К команде» (только локальным путём
   * навигации), пометка «блоки до восстановления недоступны». Разметку шлёт
   * оболочка при серверной настройке shell_integration (по умолчанию выкл.) —
   * без неё ряд пуст. false — OSC 133 не слушается вовсе. Читается при
   * открытии терминала.
   * ⚠ ПО УМОЛЧАНИЮ ВЫКЛЮЧЕН (волна 6, план 9 / PR-08+): обработчик принимает
   * ЛЮБУЮ разметку OSC 133, а её шлют и сами оболочки/промпты (starship,
   * shell integration VS Code и т.п.) — кнопки ST-10 появились бы в
   * стабилизационном выпуске без отдельной приёмки. Включается явно,
   * {"commandBlocks":true} в том же ключе или экраном «Функции терминала».
   */
  commandBlocks: boolean;
  /**
   * Двумерный viewport (ST-10, T-17/T-32): сетка шире коробки сдвигается по X
   * пальцем (блокировка оси: вертикаль — прежняя прокрутка), колесом/трекпадом
   * (deltaX) и за курсором при вводе; PTY при этом не меняет размер. false —
   * прежнее: правая часть срезана, горизонтальный жест не наш. Читается при
   * открытии терминала.
   * ⚠ ПО УМОЛЧАНИЮ ВЫКЛЮЧЕН (волна 4, план PR-08+): ST-10 не смешивается со
   * стабилизацией — новое поведение жестов включается явно, {"viewportPan":true}
   * в том же ключе или экраном «Функции терминала».
   */
  viewportPan: boolean;
  /**
   * Контракт вместимости (ST-08, I-10, T-32): кадр на запрос, ушедший ПОСЛЕ
   * доставки нашей вместимости в этот же сокет, принимается как сетка PTY по
   * обеим осям (обрывает круг «кадр → resync → запрос»); «уже отправлено»
   * привязано к сокету; под клавиатурой высота в отчёт не выдумывается, а
   * неизвестная вместимость — не повод слать сетку терминала; новый сокет
   * под клавиатурой получает вместимость, измеренную без неё. false — прежнее.
   * Читается при открытии терминала.
   */
  capacity: boolean;
  /**
   * Плашки как перекрытие (ST-08, I-09, T-31): вопрос агента, полоса загрузки,
   * уведомление о ширине и рост поля ввода сдвигают видимое окно к курсору, а
   * не меняют размер PTY. false — прежний fit + resize на каждую плашку.
   * Читается при открытии терминала.
   */
  occlusion: boolean;
  /** Пауза по ВСЕЙ очереди клиента, а не только по xterm (ST-09). */
  flowBacklog: boolean;
  navigation: NavigationMode;
  /**
   * Остатки ST-07 (I-08, T-11, T-18): путь загруженного файла сверяет цель ввода
   * после ожидания и при смене цели кладётся в черновик исходной сессии, а не
   * печатается сам в новое соединение; горизонтальный сдвиг гасит следующий
   * клик; копирование пути папки идёт через ClipboardService. false — прежнее.
   */
  inputSafety: boolean;
  /**
   * Одно касание — один клик приложению (ST-10, найдено пробой viewport pan,
   * воспроизводится и на baseline): касание при слежении за мышью уже ушло
   * кликом в ячейку (sendTapAsClick), а следом браузер шлёт совместимые
   * mousedown/mouseup, и xterm отдавал ВТОРОЙ клик — местами в соседней
   * ячейке. true — совместимые события мыши сразу после нашего клика xterm не
   * видит. false — прежнее (два клика). Читается при открытии терминала.
   */
  tapClickOnce: boolean;
}

// ⚠ Форма этих двух объявлений — контракт scripts/build-identity.mjs (ST-00):
// он читает ключ и литерал умолчаний регуляркой, без исполнения TS.
const FEATURES_KEY = "remotai.terminal.features.v1";
export const TERMINAL_FEATURES_KEY = FEATURES_KEY;

function defaultFeatures(): TerminalFeatures {
  return { sizeOwner: true, agentHistory: true, trace: true, recoveryV1: true, retention: "policy",
    presentation: true, commandBlocks: false, viewportPan: false, capacity: true, occlusion: true, flowBacklog: true,
    navigation: "v2", inputSafety: true, tapClickOnce: true };
}

export function defaultTerminalFeatures(): TerminalFeatures { return defaultFeatures(); }

export type FeatureName = keyof TerminalFeatures;
type BooleanFeature = { [K in FeatureName]: TerminalFeatures[K] extends boolean ? K : never }[FeatureName];
const BOOLEAN_FEATURES: readonly BooleanFeature[] = ["sizeOwner", "agentHistory", "trace", "recoveryV1", "presentation",
  "commandBlocks", "viewportPan", "capacity", "occlusion", "flowBacklog", "inputSafety", "tapClickOnce"];

/**
 * Все переключатели в порядке экрана «Функции терминала» (план 9.1): у каждого
 * либо вкл/выкл, либо выбор из известных значений. Новая функция — новая
 * строка здесь, иначе её нельзя откатить с телефона.
 */
export const FEATURE_ORDER: readonly FeatureName[] = ["trace", "recoveryV1", "retention", "presentation", "navigation",
  "capacity", "occlusion", "flowBacklog", "inputSafety", "tapClickOnce", "sizeOwner", "agentHistory", "commandBlocks",
  "viewportPan"];
export const FEATURE_CHOICES: Readonly<Partial<Record<FeatureName, readonly string[]>>> = {
  retention: RETENTION_MODES, navigation: NAVIGATION_MODES,
};

/**
 * Локальный откат по одной функции без выпуска: в localStorage лежит
 * {"имя":false}. Формат v1 прежний: включённую по умолчанию функцию выключает
 * только ЯВНОЕ false, выключенную по умолчанию (viewportPan, commandBlocks)
 * включает только ЯВНОЕ true; всё остальное (нет ключа, порченое значение, не объект, не JSON,
 * нет storage) — дефолт. Иначе порченая запись молча выключила бы исправление.
 */
export function terminalFeatures(storage?: Pick<Storage, "getItem">): TerminalFeatures {
  const features = defaultTerminalFeatures();
  try {
    const parsed: unknown = JSON.parse((storage ?? localStorage).getItem(FEATURES_KEY) || "{}");
    if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) return features;
    const value = parsed as Record<string, unknown>;
    for (const key of BOOLEAN_FEATURES) {
      if (features[key] ? value[key] === false : value[key] === true) features[key] = !features[key];
    }
    if (RETENTION_MODES.includes(value.retention as RetentionMode)) features.retention = value.retention as RetentionMode;
    if (NAVIGATION_MODES.includes(value.navigation as NavigationMode)) features.navigation = value.navigation as NavigationMode;
    return features;
  } catch { return defaultTerminalFeatures(); }
}

/**
 * Запись экрана «Функции терминала» в тот же ключ v1: только ОТЛИЧИЯ от
 * умолчаний (включённое по умолчанию — явным false, выключенное — явным true,
 * режимы — известной строкой). Нет отличий — ключ снимается. Значение, которое
 * парсер не примет, не пишется вовсе. false — хранилище недоступно (приватный
 * режим): человек должен узнать, что откат не записан.
 */
export function writeTerminalFeatures(storage: Pick<Storage, "setItem" | "removeItem"> | null | undefined,
  features: TerminalFeatures): boolean {
  if (!storage) return false;
  const defaults = defaultTerminalFeatures();
  const out: Record<string, boolean | string> = {};
  for (const key of BOOLEAN_FEATURES) if (typeof features[key] === "boolean" && features[key] !== defaults[key]) out[key] = features[key];
  if (features.retention !== defaults.retention && RETENTION_MODES.includes(features.retention)) out.retention = features.retention;
  if (features.navigation !== defaults.navigation && NAVIGATION_MODES.includes(features.navigation)) out.navigation = features.navigation;
  try {
    if (Object.keys(out).length === 0) storage.removeItem(FEATURES_KEY);
    else storage.setItem(FEATURES_KEY, JSON.stringify(out));
    return true;
  } catch { return false; }
}

/** «Сбросить к умолчаниям»: ключ снимается целиком. */
export function resetTerminalFeatures(storage: Pick<Storage, "removeItem"> | null | undefined): boolean {
  if (!storage) return false;
  try { storage.removeItem(FEATURES_KEY); return true; } catch { return false; }
}

/** Unknown extension versions stay on the existing protocol/UI. */
export function parseTerminalControls(value: unknown): TerminalControls | null {
  if (!value || typeof value !== "object") return null;
  const v = value as Record<string, unknown>;
  if (v.t !== "terminal-controls" || v.v !== 1 || !Array.isArray(v.capabilities)
    || !v.capabilities.includes("size-owner-v1") || !Number.isSafeInteger(v.revision)
    || Number(v.revision) < 0 || typeof v.you !== "string" || !v.you
    || typeof v.owner !== "string" || !Array.isArray(v.viewers) || v.viewers.length > 256) return null;
  const viewers: SizeViewer[] = [];
  for (const item of v.viewers) {
    if (!item || typeof item !== "object" || typeof item.id !== "string" || !item.id
      || !Number.isSafeInteger(item.cols) || !Number.isSafeInteger(item.rows)
      || item.cols < 0 || item.rows < 0) return null;
    viewers.push({ id: item.id, cols: item.cols, rows: item.rows, canOwn: item.can_own === true });
  }
  if (!viewers.some(item => item.id === v.you) || v.owner && !viewers.some(item => item.id === v.owner)) return null;
  return { revision: Number(v.revision), you: v.you, owner: v.owner,
    leaseUntil: typeof v.lease_until === "number" ? v.lease_until : 0,
    viewers, error: typeof v.error === "string" ? v.error : "" };
}
