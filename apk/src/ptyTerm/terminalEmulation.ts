/**
 * Параметры эмуляции терминала, от которых зависит СЕМАНТИКА буфера, а не
 * внешний вид: сколько строк истории переживает прокрутку и какой ширины
 * символ Unicode (раздел 6.1 плана: «совпадающие версии и настройки browser
 * xterm и headless, Unicode фиксируется в fixture»).
 *
 * Почему отдельный модуль. Раньше эти числа жили литералом в `new Terminal`
 * компонента (PtyTermView), а стенд сравнения (snapshotConformance.test.ts)
 * создавал свой headless со своими числами. Разойдись они — стенд сравнивал
 * бы не тот терминал, что у человека в телефоне, и зелёный прогон ничего бы
 * не доказывал. Теперь оба берут одну константу.
 *
 * Unicode. Клиент не грузит unicode-аддонов, значит активна встроенная
 * таблица xterm версии '6': эмодзи 😀 шириной 1. Go-зеркало считает ширину
 * по графемам (charmbracelet/x/vt, GraphemeWidth): 😀 шириной 2. Это
 * задокументированный разрыв unicode-width-v6-vs-grapheme (реестр
 * EXPECTED_MIRROR_GAPS в terminalConformance.ts); сменить версию здесь можно
 * только вместе с загрузкой аддона в компоненте и пересмотром реестра.
 *
 * Тема, шрифт, мигание курсора и smoothScrollDuration сюда НЕ входят: они
 * меняют картинку, а не состояние буфера.
 *
 * Чистый модуль: ни xterm, ни DOM — проверяется в node.
 */

export const TERMINAL_EMULATION = Object.freeze({
  /** Строк истории у клиентского xterm (у Go-зеркала потолок 500 — разрыв scrollback-cap-500). */
  scrollback: 10000,
  /** Активная таблица ширин xterm: встроенная '6', аддоны не грузятся. */
  unicodeVersion: "6",
  /** Версия, на которой сверены приватные поля адаптера conformance. */
  xtermVersion: "6.0.0",
} as const);

/** Опции конструктора Terminal, влияющие на семантику буфера. */
export function terminalEmulationOptions(): { scrollback: number } {
  return { scrollback: TERMINAL_EMULATION.scrollback };
}

/**
 * Чем фактическая эмуляция отличается от объявленной. Пустой список — совпала.
 * Поле, которое вызывающий измерить не смог (undefined), не сравнивается:
 * например, `term.unicode` без allowProposedApi недоступен.
 */
export function emulationMismatches(actual: {
  scrollback?: number;
  unicodeVersion?: string;
  xtermVersion?: string;
}): string[] {
  const out: string[] = [];
  if (actual.scrollback !== undefined && actual.scrollback !== TERMINAL_EMULATION.scrollback) {
    out.push(`scrollback ${actual.scrollback} != ${TERMINAL_EMULATION.scrollback}`);
  }
  if (actual.unicodeVersion !== undefined && actual.unicodeVersion !== TERMINAL_EMULATION.unicodeVersion) {
    out.push(`unicode ${actual.unicodeVersion} != ${TERMINAL_EMULATION.unicodeVersion}`);
  }
  if (actual.xtermVersion !== undefined && actual.xtermVersion !== TERMINAL_EMULATION.xtermVersion) {
    out.push(`xterm ${actual.xtermVersion} != ${TERMINAL_EMULATION.xtermVersion}`);
  }
  return out;
}
