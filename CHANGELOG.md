# Changelog

All notable changes to this project will be documented in this file.

The format is based on Keep a Changelog.

## [2.75.17] - 2026-10-08

### Local capabilities and terminal navigation / Локальные возможности и терминалы

- Added Settings → Local capabilities for the selected computer: connect or replace the Windows recognition module, install and select models from its catalog, save language and terminology, and test a recording without terminal input. Model installation continues when leaving settings and can be resumed or cancelled.
- Added local voice input to terminal drafts, including phone recording and audio files, explicit transcript review, upload ownership checks and cancellation of the entire recognition process tree.
- Added computers as terminal folders with per-computer navigation, search and offline status. Folder selection supports sorting and filters and retains its layout with the keyboard open.
- Improved Hermes installation diagnostics and isolated build dependencies on Intel macOS; retained owned compilers across Hermes updates. Native installation and subsequent updates were checked on five OS/CPU targets.
- Fixed returning to the latest terminal output after viewport reflow, which could stop three rows short of the bottom.
- Fixed the agent Continue button disappearing when terminal state arrived before the agent catalog.
- Добавлен раздел «Настройки → Локальные возможности»: модуль Windows, каталог моделей, установка, выбор, язык, словарь и проверочная запись. Установка продолжается после выхода из раздела.
- Голосовая запись с телефона и аудиофайлы распознаются на выбранном ПК; проверенный текст добавляется в черновик без автоматической отправки.
- Компьютеры открываются как папки терминалов; в выборе папки появились сортировка и фильтры, устранено смещение окна при поиске и открытии клавиатуры.
- Исправлена подготовка Hermes на Intel macOS и сохранение нужного компилятора при обновлении; улучшена диагностика ошибок завершения установки.
- Исправлен возврат к последней строке терминала после перерасчёта размеров: прокрутка могла остановиться на три строки выше низа.
- Исправлено исчезновение кнопки продолжения агента, когда состояние терминала приходило раньше каталога агентов.

## [2.75.16] - 2026-10-07

### Hermes installation and terminal performance / Установка Hermes и скорость терминала

- Hermes setup retries the installer download from the official NousResearch GitHub repository when the official website denies access, is unavailable or returns an invalid response. Failed downloads do not replace the bootstrap script; cancellation stops further requests.
- Reduced the JavaScript required at startup by about 24% compressed by loading the terminal engine and WebGL when opening a terminal.
- Fixed output search failing when highlighting and counting matches in the terminal.
- Подготовка Hermes использует официальный GitHub, если основной сервер отвечает HTTP 403 или недоступен. Страницы отказа не сохраняются вместо установщика.
- Стартовая загрузка JavaScript уменьшена примерно на 24% в сжатом виде. Исправлена ошибка поиска и подсветки совпадений в выводе терминала.

## [2.75.15] - 2026-10-06

### All computers, language selection and terminal activity / Все компьютеры, язык и активность терминалов

- Terminals has an optional All computers view: each computer is a collapsible folder with its terminal cards. The selected view and collapsed folders are saved. Opening a card selects its computer and access role; Back returns to the combined list.
- The combined list supports search and shows offline computers, loading failures and empty lists separately. One slow computer does not delay the others.
- The website language switch stays visible in the phone header. In the application, Language is at the top of More and Settings.
- Codex automatic goal continuations and new turns started from the computer update terminal activity without requiring Enter through Remotai. A previous completion no longer leaves an active turn marked as ready.
- The open terminal and its card use the same runtime status. Idle redraws do not mean a new task started; a silent running turn stays working.
- Переключатель языка виден в шапке мобильного сайта, в начале «Ещё» и настроек приложения.
- Статус Codex обновляется при автоматическом продолжении цели без ввода через Remotai. Карточка и открытый терминал показывают единое состояние работы.
- В «Терминалах» можно включить «Все компьютеры»: машины показаны раскрывающимися папками со своими терминалами. Режим и свёрнутые папки сохраняются, есть поиск; карточка открывается на нужной машине.

## [2.75.14] - 2026-10-06

### English and Russian / Русский и английский

- The application supports English and Russian: sign-in, computers, terminals, files, Hermes, settings, billing and the built-in guide. Language selection is saved on the device.
- Switching language preserves open terminals, connections and drafts. Dates, numbers and plurals follow the selected language; user content stays unchanged.
- Added an English website, documentation, first-run wizard, Windows tray, macOS menus, shell installer and CLI help. Free self-hosting and the paid managed service remain available.
- Full publications now upload every declared website page and its language assets from one checked inventory.
- Приложение, мастер первого запуска, сайт и инструкции доступны на русском и английском. Выбор языка сохраняется; терминалы, соединения и черновики продолжают работать.
- Переведены меню Windows и macOS, командный установщик и справка командной строки. Полный выпуск загружает все страницы сайта, включая переводы.
