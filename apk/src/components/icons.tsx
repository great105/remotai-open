/**
 * Линейные SVG-иконки в стиле лендинга remotai.ru — вместо разнобоя эмодзи.
 * stroke=currentColor, наследуют цвет текста/акцента из CSS.
 */

interface IconProps {
  size?: number;
  className?: string;
}

function svgProps({ size = 20, className }: IconProps) {
  return {
    width: size,
    height: size,
    viewBox: "0 0 24 24",
    fill: "none",
    stroke: "currentColor",
    strokeWidth: 2,
    strokeLinecap: "round" as const,
    strokeLinejoin: "round" as const,
    className,
    // Inline-SVG стоит на базовой линии текста и в обычной (не flex) кнопке
    // уезжает вниз примерно на 3 px — рядом с подписью это видно как «значок
    // провалился». Сдвиг задаём здесь, а не в styles.css: там на весь файл
    // ровно одно правило со словом svg, общей опоры нет, а знаки уже стоят в
    // кнопках без display:flex (.pty-folder-btn, .pty-key-btn).
    // flex: 0 0 auto — во flex-контейнерах знак не должен сжиматься под
    // длинной подписью: сплющенный значок читается как другой значок.
    style: { verticalAlign: "-0.15em", flex: "0 0 auto" },
    "aria-hidden": true,
  };
}

/** Фирменный знак (слои — как на лендинге). */
export function IconLogo({ size = 24, className }: IconProps) {
  return (
    <svg width={size} height={size} viewBox="0 0 32 32" fill="none" className={className} aria-hidden>
      <path d="M16 6L6 11.5 16 17 26 11.5Z" stroke="currentColor" strokeWidth="2" strokeLinejoin="round" />
      <path d="M6 16l10 5.5L26 16" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" />
      <path d="M6 20.5l10 5.5 10-5.5" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  );
}

export function IconHome(p: IconProps) {
  return (
    <svg {...svgProps(p)}>
      <path d="m3 10 9-7 9 7v10a1.5 1.5 0 0 1-1.5 1.5h-15A1.5 1.5 0 0 1 3 20Z" />
      <path d="M9 21v-7h6v7" />
    </svg>
  );
}

export function IconFolder(p: IconProps) {
  return (
    <svg {...svgProps(p)}>
      <path d="M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v9a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2Z" />
    </svg>
  );
}

/** Контекстные подсказки: тот же линейный стиль, что у остальных действий. */
export function IconHelp(p: IconProps) {
  return (
    <svg {...svgProps(p)}>
      <circle cx="12" cy="12" r="9" />
      <path d="M9.7 9a2.3 2.3 0 0 1 4.5.7c0 1.8-2.2 2-2.2 3.3M12 16.5h.01" />
    </svg>
  );
}

export function IconBackspace(p: IconProps) {
  return (
    <svg {...svgProps(p)}>
      <path d="M9 5h12v14H9l-7-7Z" />
      <path d="m11 9 6 6m0-6-6 6" />
    </svg>
  );
}

export function IconTerminal(p: IconProps) {
  return (
    <svg {...svgProps(p)}>
      <path d="m5 7 4 4-4 4" />
      <path d="M12 17h7" />
    </svg>
  );
}

export function IconActivity(p: IconProps) {
  return (
    <svg {...svgProps(p)}>
      <path d="M22 12h-4l-3 9L9 3l-3 9H2" />
    </svg>
  );
}

export function IconScreen(p: IconProps) {
  return (
    <svg {...svgProps(p)}>
      <rect x="2" y="4" width="20" height="13" rx="2" />
      <path d="M8 21h8M12 17v4" />
    </svg>
  );
}

/** Компьютеры и серверы аккаунта, доступные с телефона. */
export function IconDevices(p: IconProps) {
  return (
    <svg {...svgProps(p)}>
      <rect x="2" y="4" width="14" height="11" rx="2" />
      <path d="M7 19h4M9 15v4" />
      <rect x="17.5" y="8" width="4.5" height="12" rx="1.4" />
      <path d="M19.25 17.5h1" />
    </svg>
  );
}

/** SSH-серверы: стойка, а не «ещё один терминал» — раздел живёт своей жизнью. */
export function IconServer(p: IconProps) {
  return (
    <svg {...svgProps(p)}>
      <rect x="3" y="4" width="18" height="7" rx="1.6" />
      <rect x="3" y="13" width="18" height="7" rx="1.6" />
      <path d="M7 7.5h.01M7 16.5h.01" />
    </svg>
  );
}

/**
 * «Ещё» — пятая вкладка телефона: за ней список остальных разделов.
 * Сетка 2×2, а не три точки: рядом с «Главной», «Терминалом» и «Файлами» точки
 * читаются как многоточие («идёт загрузка»), а плитки — как «тут ещё разделы».
 */
export function IconMore(p: IconProps) {
  return (
    <svg {...svgProps(p)}>
      <rect x="3.5" y="3.5" width="7" height="7" rx="1.6" />
      <rect x="13.5" y="3.5" width="7" height="7" rx="1.6" />
      <rect x="3.5" y="13.5" width="7" height="7" rx="1.6" />
      <rect x="13.5" y="13.5" width="7" height="7" rx="1.6" />
    </svg>
  );
}

/**
 * Лимиты AI-подписок: доля круга — «сколько окна уже израсходовано».
 * Тот же смысл, что у знака ◔ на кнопке «Лимиты» в шапке «Моих компьютеров»,
 * — раздел узнаётся по одной картинке в обоих местах.
 */
export function IconGauge(p: IconProps) {
  return (
    <svg {...svgProps(p)}>
      <circle cx="12" cy="12" r="9" />
      <path d="M12 3v9h9" />
    </svg>
  );
}

/** Панель ПК: ползунки — настройка самой машины, не приложения (там шестерня). */
export function IconSliders(p: IconProps) {
  return (
    <svg {...svgProps(p)}>
      <path d="M3.5 7h17M3.5 12h17M3.5 17h17" />
      <path d="M8 5v4M16 10v4M10.5 15v4" />
    </svg>
  );
}

export function IconList(p: IconProps) {
  return (
    <svg {...svgProps(p)}>
      <path d="M8 6h13M8 12h13M8 18h13" />
      <path d="M3 6h.01M3 12h.01M3 18h.01" />
    </svg>
  );
}

export function IconGear(p: IconProps) {
  return (
    <svg {...svgProps(p)}>
      <circle cx="12" cy="12" r="3" />
      <path d="M19.4 15a1.7 1.7 0 0 0 .34 1.87l.06.06a2 2 0 1 1-2.83 2.83l-.06-.06a1.7 1.7 0 0 0-1.87-.34 1.7 1.7 0 0 0-1.03 1.56V21a2 2 0 1 1-4 0v-.09a1.7 1.7 0 0 0-1.11-1.56 1.7 1.7 0 0 0-1.87.34l-.06.06a2 2 0 1 1-2.83-2.83l.06-.06a1.7 1.7 0 0 0 .34-1.87 1.7 1.7 0 0 0-1.56-1.03H3a2 2 0 1 1 0-4h.09A1.7 1.7 0 0 0 4.65 8.9a1.7 1.7 0 0 0-.34-1.87l-.06-.06a2 2 0 1 1 2.83-2.83l.06.06a1.7 1.7 0 0 0 1.87.34h.01A1.7 1.7 0 0 0 10.05 3V3a2 2 0 1 1 4 0v.09a1.7 1.7 0 0 0 1.03 1.56 1.7 1.7 0 0 0 1.87-.34l.06-.06a2 2 0 1 1 2.83 2.83l-.06.06a1.7 1.7 0 0 0-.34 1.87v.01a1.7 1.7 0 0 0 1.56 1.03H21a2 2 0 1 1 0 4h-.09a1.7 1.7 0 0 0-1.51.95Z" />
    </svg>
  );
}

export function IconCamera(p: IconProps) {
  return (
    <svg {...svgProps(p)}>
      <path d="M4 8h2.4l1.4-2.2A1.5 1.5 0 0 1 9.06 5h5.88a1.5 1.5 0 0 1 1.26.8L17.6 8H20a1.5 1.5 0 0 1 1.5 1.5V18a1.5 1.5 0 0 1-1.5 1.5H4A1.5 1.5 0 0 1 2.5 18V9.5A1.5 1.5 0 0 1 4 8Z" />
      <circle cx="12" cy="13.5" r="3.2" />
    </svg>
  );
}

export function IconRepeat(p: IconProps) {
  return (
    <svg {...svgProps(p)}>
      <path d="m17 2 4 4-4 4" />
      <path d="M3 11v-1a4 4 0 0 1 4-4h14" />
      <path d="m7 22-4-4 4-4" />
      <path d="M21 13v1a4 4 0 0 1-4 4H3" />
    </svg>
  );
}

export function IconLock(p: IconProps) {
  return (
    <svg {...svgProps(p)}>
      <rect x="4.5" y="10.5" width="15" height="10" rx="2" />
      <path d="M8 10.5V7a4 4 0 0 1 8 0v3.5" />
    </svg>
  );
}

export function IconMoon(p: IconProps) {
  return (
    <svg {...svgProps(p)}>
      <path d="M21 12.8A8.5 8.5 0 1 1 11.2 3 6.6 6.6 0 0 0 21 12.8Z" />
    </svg>
  );
}

export function IconRestart(p: IconProps) {
  return (
    <svg {...svgProps(p)}>
      <path d="M3 12a9 9 0 1 0 3-6.7" />
      <path d="M3 4v5h5" />
    </svg>
  );
}

export function IconPower(p: IconProps) {
  return (
    <svg {...svgProps(p)}>
      <path d="M12 2v8" />
      <path d="M18.4 6.6a9 9 0 1 1-12.77.04" />
    </svg>
  );
}

export function IconQr(p: IconProps) {
  return (
    <svg {...svgProps(p)}>
      <rect x="3" y="3" width="7" height="7" rx="1" />
      <rect x="14" y="3" width="7" height="7" rx="1" />
      <rect x="3" y="14" width="7" height="7" rx="1" />
      <path d="M14 14h3v3h-3zM21 14v.01M21 21h-4M14 21v-3" />
    </svg>
  );
}

/**
 * AI-агент. Заменяет 🤖 на плитках и кнопках «Запустить AI-агента»: эмодзи
 * рисуется системным шрифтом и на Android/iOS/Windows выглядит по-разному —
 * три вида одной кнопки в продукте с мятно-графитовой палитрой.
 */
export function IconRobot(p: IconProps) {
  return (
    <svg {...svgProps(p)}>
      <rect x="3.5" y="8" width="17" height="12" rx="3" />
      <path d="M12 4.5V8" />
      <path d="M12 3.2h.01" />
      <path d="M9 13h.01M15 13h.01" />
      <path d="M9.5 16.6h5" />
    </svg>
  );
}

/**
 * Избранное. `filled` — уже в избранном: заливка вместо второго глифа, чтобы
 * «в избранном» и «не в избранном» были одной картинкой в двух состояниях
 * (раньше это были два разных символа ★ и ☆).
 */
export function IconStar({ filled, ...p }: IconProps & { filled?: boolean }) {
  return (
    <svg {...svgProps(p)} fill={filled ? "currentColor" : "none"}>
      <path d="m12 3.6 2.6 5.28 5.82.85-4.21 4.1.99 5.8L12 16.9l-5.2 2.73.99-5.8-4.21-4.1 5.82-.85Z" />
    </svg>
  );
}

/** Недавняя папка — время, а не «часы» как прибор. */
export function IconClock(p: IconProps) {
  return (
    <svg {...svgProps(p)}>
      <circle cx="12" cy="12" r="9" />
      <path d="M12 7.2V12l3.4 2" />
    </svg>
  );
}

/**
 * Группа терминалов — стопка карточек, а НЕ папка: «папкой» на экране
 * терминалов зовётся настоящий каталог компьютера, и один значок 📁 на два
 * смысла заставлял человека гадать, что он открывает.
 */
export function IconGroup(p: IconProps) {
  return (
    <svg {...svgProps(p)}>
      <rect x="3" y="8" width="14" height="12" rx="2" />
      <path d="M7 5h12a2 2 0 0 1 2 2v10" />
    </svg>
  );
}

/**
 * Три знака для плиток «Быстрого доступа» в файлах. Раньше там стояли системные
 * эмодзи (⬇️ 📄 💾): они рисуются шрифтом ОС, то есть на Android, iOS и Windows
 * выглядят по-разному и всегда цветные — рядом с линейными мятными иконками это
 * читается как чужой элемент, случайно попавший на экран.
 */
export function IconDownload(p: IconProps) {
  return (
    <svg {...svgProps(p)}>
      <path d="M12 4v10" />
      <path d="m8 11 4 4 4-4" />
      <path d="M5 18h14" />
    </svg>
  );
}

export function IconDoc(p: IconProps) {
  return (
    <svg {...svgProps(p)}>
      <path d="M14 3H7a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V8z" />
      <path d="M14 3v5h5" />
    </svg>
  );
}

/** Диск (C:, D:) — корпус с индикатором, а не дискета из 1998 года. */
export function IconDrive(p: IconProps) {
  return (
    <svg {...svgProps(p)}>
      <rect x="3" y="5" width="18" height="14" rx="2" />
      <path d="M3 12h18" />
      <circle cx="7.5" cy="15.5" r="1" />
    </svg>
  );
}

/* ══════════════════════════════════════════════════════════════════════════
   Второй заход набора (P2-6): знаки, которыми продукт до сих пор обходился
   эмодзи и типографскими символами (✕ ✎ 🗑 ⏳ ⚠ ↻ ⋮ ⠿ ＋ …). У каждого
   объяснено, ПОЧЕМУ выбрана такая картинка: без этого следующий агент
   нарисует рядом четвёртую папку и третью галку.
   ══════════════════════════════════════════════════════════════════════════ */

/**
 * Закрыть. Один крест на весь продукт: карточка терминала, шторки, плашки,
 * сброс поиска. Крест, а НЕ корзина: закрытие ничего не стирает — лог живёт
 * ещё несколько минут и его предлагают сохранить, а корзина обещала бы
 * удаление, которого не происходит.
 */
export function IconClose(p: IconProps) {
  return (
    <svg {...svgProps(p)}>
      <path d="M6 6l12 12M18 6L6 18" />
    </svg>
  );
}

const CHEVRON_PATH = {
  right: "M9 5l7 7-7 7",
  down: "M5 9l7 7 7-7",
  up: "M5 15l7-7 7 7",
  left: "M15 5l-7 7 7 7",
} as const;

/**
 * Раскрывашка: ▸ ▾ ▴ ◂ во всём продукте означали одно — «свернуть/развернуть».
 * Один компонент с направлением, а не четыре знака: иначе половина экранов
 * рисует треугольник, половина — стрелку, и они читаются как разные действия.
 */
export function IconChevron({ dir = "down", ...p }: IconProps & { dir?: keyof typeof CHEVRON_PATH }) {
  return (
    <svg {...svgProps(p)}>
      <path d={CHEVRON_PATH[dir]} />
    </svg>
  );
}

const ARROW_PATH = {
  up: ["M12 19V5", "M6 11l6-6 6 6"],
  down: ["M12 5v14", "M6 13l6 6 6-6"],
  left: ["M19 12H5", "M11 6l-6 6 6 6"],
  right: ["M5 12h14", "M13 6l6 6-6 6"],
} as const;

/**
 * Перемещение (↑ ↓ в списке). Стрелка с ХВОСТОМ, в отличие от «птички»
 * IconChevron: у неё смысл «свернуть», а не «передвинуть», и на одном экране
 * эти два действия обязаны выглядеть по-разному.
 */
export function IconArrow({ dir = "right", ...p }: IconProps & { dir?: keyof typeof ARROW_PATH }) {
  const [line, head] = ARROW_PATH[dir];
  return (
    <svg {...svgProps(p)}>
      <path d={line} />
      <path d={head} />
    </svg>
  );
}

/** Готово / выбрано / пункт пройден — вместо ✓ ✅ ☑ в двенадцати файлах. */
export function IconCheck(p: IconProps) {
  return (
    <svg {...svgProps(p)}>
      <path d="M5 12.8l4.6 4.7L19 6.5" />
    </svg>
  );
}

/**
 * Обновить/повторить (↻ 🔄): круг из ДВУХ стрелок — «сделать то же ещё раз».
 * НЕ путать с IconRestart выше: тот означает «перезагрузить компьютер», у него
 * одна стрелка, и подменять их местами нельзя — цена ошибки разная.
 */
export function IconRefresh(p: IconProps) {
  return (
    <svg {...svgProps(p)}>
      <path d="M20.5 12a8.5 8.5 0 0 1-14.4 6.1" />
      <path d="M3.5 12a8.5 8.5 0 0 1 14.4-6.1" />
      <path d="M18.2 2.6v3.6h-3.6" />
      <path d="M5.8 21.4v-3.6h3.6" />
    </svg>
  );
}

/**
 * «Агент задал вопрос и ждёт» (⏳). Песочные часы, а не таймер: ждут ЧЕЛОВЕКА,
 * а не машину, — время идёт впустую, пока не ответишь.
 */
export function IconHourglass(p: IconProps) {
  return (
    <svg {...svgProps(p)}>
      <path d="M7 3h10M7 21h10" />
      <path d="M8 3v3.2c0 1.2.6 2.3 1.6 3L12 11l2.4-1.8c1-.7 1.6-1.8 1.6-3V3" />
      <path d="M8 21v-3.2c0-1.2.6-2.3 1.6-3L12 13l2.4 1.8c1 .7 1.6 1.8 1.6 3V21" />
    </svg>
  );
}

/**
 * «Работает, но вывода нет N минут» (⏱). Секундомер, а не часы: IconClock
 * означает «недавняя папка», и один циферблат на два смысла заставил бы
 * гадать, о чём говорит карточка.
 */
export function IconStopwatch(p: IconProps) {
  return (
    <svg {...svgProps(p)}>
      <circle cx="12" cy="13.8" r="7.4" />
      <path d="M12 10v3.8l2.4 1.5" />
      <path d="M9.6 2.6h4.8M12 2.6v3.8" />
    </svg>
  );
}

/** Ошибка и предупреждение (⚠ ⚠️) — треугольник, а не круг: круг у нас статус. */
export function IconWarning(p: IconProps) {
  return (
    <svg {...svgProps(p)}>
      <path d="M10.3 3.9L2.6 17.4A2 2 0 0 0 4.3 20.4h15.4a2 2 0 0 0 1.7-3L13.7 3.9a2 2 0 0 0-3.4 0Z" />
      <path d="M12 9.4v4.2" />
      <path d="M12 17.1h.01" />
    </svg>
  );
}

/**
 * «Связь с терминалом потеряна» (⚯): разорванное звено. Процесс на компьютере
 * ЖИВ — оборвался только канал, поэтому не крест (тот означал бы «закрыт») и
 * не предупреждение (это не ошибка работы).
 */
export function IconUnlink(p: IconProps) {
  return (
    <svg {...svgProps(p)}>
      <path d="M9.8 13.6l-2.4 2.5a3.6 3.6 0 0 1-5.1-5.1l2.5-2.4" />
      <path d="M14.2 10.4l2.4-2.5a3.6 3.6 0 0 1 5.1 5.1l-2.5 2.4" />
    </svg>
  );
}

/** Подключиться к терминалу с клавиатуры (⌨). */
export function IconKeyboard(p: IconProps) {
  return (
    <svg {...svgProps(p)}>
      <rect x="2.5" y="6" width="19" height="12" rx="2" />
      <path d="M6.5 9.6h.01M10 9.6h.01M13.5 9.6h.01M17 9.6h.01M6.5 13.2h.01M17 13.2h.01" />
      <path d="M9.6 13.2h4.8" />
    </svg>
  );
}

/** Добавить (＋ ➕): плюс, а не «звёздочка нового» — добавляют в существующий список. */
export function IconPlus(p: IconProps) {
  return (
    <svg {...svgProps(p)}>
      <path d="M12 5v14M5 12h14" />
    </svg>
  );
}

/** Отправить файл НА компьютер (⬆ 📥) — пара к IconDownload, стрелка от полки вверх. */
export function IconUpload(p: IconProps) {
  return (
    <svg {...svgProps(p)}>
      <path d="M12 15V5" />
      <path d="M8 9l4-4 4 4" />
      <path d="M5 18h14" />
    </svg>
  );
}

/** Ключ доступа (🔑): SSH-ключи, ключ API, ключ подключения телефона. */
export function IconKey(p: IconProps) {
  return (
    <svg {...svgProps(p)}>
      <circle cx="7.6" cy="12" r="3.6" />
      <path d="M11.2 12h9.3" />
      <path d="M17.6 12v3.4M20.5 12v2.4" />
    </svg>
  );
}

/**
 * Удалить НАВСЕГДА (🗑). Только там, где данные исчезают без возврата: файл,
 * ключ, запись. Закрытие терминала корзиной НЕ обозначаем — см. IconClose.
 */
export function IconTrash(p: IconProps) {
  return (
    <svg {...svgProps(p)}>
      <path d="M4 6.8h16" />
      <path d="M9 6.8V5a1.6 1.6 0 0 1 1.6-1.6h2.8A1.6 1.6 0 0 1 15 5v1.8" />
      <path d="M6.6 6.8l.9 12.3a1.9 1.9 0 0 0 1.9 1.7h5.2a1.9 1.9 0 0 0 1.9-1.7l.9-12.3" />
      <path d="M10.4 10.6v6.4M13.6 10.6v6.4" />
    </svg>
  );
}

/** Переименовать (✎ ✏️). */
export function IconPencil(p: IconProps) {
  return (
    <svg {...svgProps(p)}>
      <path d="M4 16.6V20h3.4L18.9 8.5a2.4 2.4 0 0 0-3.4-3.4Z" />
      <path d="M14.4 6.2l3.4 3.4" />
    </svg>
  );
}

/** Отправить в Telegram (✈ ✈️ ➤) — самолётик, узнаваемый жест мессенджера. */
export function IconSend(p: IconProps) {
  return (
    <svg {...svgProps(p)}>
      <path d="M21 3L10.4 13.6" />
      <path d="M21 3l-6.6 18-4-8-8-4Z" />
    </svg>
  );
}

/**
 * «Ещё действия» (⋮) — вертикальное троеточие у строки или карточки. Именно
 * вертикальное: горизонтальное «…» на этих же экранах означает «идёт загрузка».
 * От IconMore (сетка 2×2, пятая вкладка навигации) отличается смыслом: там
 * «остальные РАЗДЕЛЫ», здесь «остальные ДЕЙСТВИЯ над этим объектом».
 */
export function IconDots(p: IconProps) {
  return (
    <svg {...svgProps(p)}>
      <path d="M12 5.4h.01M12 12h.01M12 18.6h.01" />
    </svg>
  );
}

/** Ручка перетаскивания (⠿): шесть точек — общепринятый «схвати и тащи». */
export function IconGrip(p: IconProps) {
  return (
    <svg {...svgProps(p)}>
      <path d="M9 6h.01M15 6h.01M9 12h.01M15 12h.01M9 18h.01M15 18h.01" />
    </svg>
  );
}

/** Проброс портов (⇄): две встречные стрелки — трафик ходит в обе стороны. */
export function IconTunnel(p: IconProps) {
  return (
    <svg {...svgProps(p)}>
      <path d="M4 9h16" />
      <path d="M17 6l3 3-3 3" />
      <path d="M20 15H4" />
      <path d="M7 12l-3 3 3 3" />
    </svg>
  );
}

/** Поиск (🔍 ⌕). */
export function IconSearch(p: IconProps) {
  return (
    <svg {...svgProps(p)}>
      <circle cx="11" cy="11" r="6.5" />
      <path d="M15.8 15.8L20.5 20.5" />
    </svg>
  );
}

/**
 * Одно устройство типа «компьютер» (▣ 💻) — ноутбук с подставкой. НЕ
 * IconDevices (там пара «ПК + телефон», это раздел) и не IconScreen (там
 * монитор на ножке — «экран компьютера», то есть трансляция картинки).
 */
export function IconComputer(p: IconProps) {
  return (
    <svg {...svgProps(p)}>
      <rect x="4" y="4.6" width="16" height="11" rx="1.8" />
      <path d="M2.4 19h19.2" />
    </svg>
  );
}

/** Сеть и свой прокси (🌐): шар с меридианом — «наружу, в интернет». */
export function IconGlobe(p: IconProps) {
  return (
    <svg {...svgProps(p)}>
      <circle cx="12" cy="12" r="9" />
      <path d="M3.3 9.4h17.4M3.3 14.6h17.4" />
      <path d="M12 3c2.4 2.6 3.7 5.6 3.7 9s-1.3 6.4-3.7 9c-2.4-2.6-3.7-5.6-3.7-9S9.6 5.6 12 3Z" />
    </svg>
  );
}

/** Горячие команды (⚡): молния — «одним нажатием». */
export function IconBolt(p: IconProps) {
  return (
    <svg {...svgProps(p)}>
      <path d="M13.2 2.5L4.6 13.6H11l-.2 7.9 8.6-11.1H13l.2-7.9Z" />
    </svg>
  );
}

/**
 * Расформировать группу / «Без группы» (обратное к IconGroup): карточки
 * РАЗЪЕХАЛИСЬ и больше не лежат стопкой. Терминалы при этом остаются
 * открытыми — поэтому здесь нет ни креста, ни корзины: ничего не пропадает.
 */
export function IconUngroup(p: IconProps) {
  return (
    <svg {...svgProps(p)}>
      <rect x="2.5" y="11.5" width="10" height="10" rx="2" />
      <rect x="11.5" y="2.5" width="10" height="10" rx="2" />
    </svg>
  );
}

/**
 * Показать скрытое (👁) и спрятать обратно (🙈). Пара делается ОДНОЙ картинкой
 * в двух состояниях, а не двумя разными зверьками: 👁 и 🙈 — рисунки из разных
 * миров, и человек не понимал, что это одна кнопка-переключатель.
 */
export function IconEye(p: IconProps) {
  return (
    <svg {...svgProps(p)}>
      <path d="M2.5 12S6 5.5 12 5.5 21.5 12 21.5 12 18 18.5 12 18.5 2.5 12 2.5 12Z" />
      <circle cx="12" cy="12" r="3.2" />
    </svg>
  );
}

export function IconEyeOff(p: IconProps) {
  return (
    <svg {...svgProps(p)}>
      <path d="M2.5 12S6 5.5 12 5.5 21.5 12 21.5 12 18 18.5 12 18.5 2.5 12 2.5 12Z" />
      <path d="M4.5 4.5l15 15" />
    </svg>
  );
}

/**
 * Пароль НЕ сохранён (🔓) — пара к IconLock: тот же корпус замка, но дужка
 * откинута. Именно пара, а не два разных знака: строка «сохранён / только на
 * сеанс» читается взглядом по одной детали, а не по двум картинкам.
 */
export function IconUnlock(p: IconProps) {
  return (
    <svg {...svgProps(p)}>
      <rect x="4.5" y="10.5" width="15" height="10" rx="2" />
      <path d="M8 10.5V7a4 4 0 0 1 7.6-1.7" />
    </svg>
  );
}

/**
 * Скопировать (📋): два листа внахлёст — «стало два». Буфер обмена планшеткой
 * не рисуем: планшетка в продукте нигде не значит «копировать», а лист поверх
 * листа объясняет действие без подписи.
 */
export function IconCopy(p: IconProps) {
  return (
    <svg {...svgProps(p)}>
      <rect x="9" y="9" width="11" height="11" rx="2" />
      <path d="M5.5 15H4.5A1.5 1.5 0 0 1 3 13.5v-9A1.5 1.5 0 0 1 4.5 3h9A1.5 1.5 0 0 1 15 4.5v1" />
    </svg>
  );
}

/**
 * Закрепить (📌) — канцелярская кнопка. НЕ путать с IconStar: звезда означает
 * «избранное» (мой личный список), а кнопка — «приколоть вот к этому месту»
 * (аккаунт нейросети за папкой терминала). Смыслы разные, значки тоже.
 */
export function IconPin(p: IconProps) {
  return (
    <svg {...svgProps(p)}>
      <path d="M8.5 3.5h7" />
      <path d="M10 3.5v6.3l-2.4 3.2a1 1 0 0 0 .8 1.6h7.2a1 1 0 0 0 .8-1.6L14 9.8V3.5" />
      <path d="M12 14.6V20.5" />
    </svg>
  );
}

/**
 * Разговор с человеком (💬): поддержка, обсуждение, ответ живого человека.
 * Только про ЛЮДЕЙ — переписку с агентом обозначает IconRobot, иначе «написать
 * в поддержку» и «поговорить с Claude» слились бы в одну картинку.
 */
export function IconChat(p: IconProps) {
  return (
    <svg {...svgProps(p)}>
      <path d="M18.5 4H5.5A1.5 1.5 0 0 0 4 5.5v15l3.8-3.5h10.7A1.5 1.5 0 0 0 20 15.5v-10A1.5 1.5 0 0 0 18.5 4Z" />
    </svg>
  );
}

/**
 * Знак «Поделиться» из панели Safari на iPhone — квадрат со стрелкой вверх.
 * Нужен буквально: подсказка про установку на главный экран называет кнопку,
 * у которой НЕТ подписи, и словами «квадратик со стрелкой» её каждый находит
 * по-своему. Рисуем ровно то, что человек ищет глазами внизу экрана.
 */
export function IconIosShare(p: IconProps) {
  return (
    <svg {...svgProps(p)}>
      <path d="M12 3v12" />
      <path d="m8 7 4-4 4 4" />
      <path d="M6 11H5a1 1 0 0 0-1 1v8a1 1 0 0 0 1 1h14a1 1 0 0 0 1-1v-8a1 1 0 0 0-1-1h-1" />
    </svg>
  );
}

// folderLabel переехал в @tgcontrol/shared (packages/shared/src/folderLabel.ts).
