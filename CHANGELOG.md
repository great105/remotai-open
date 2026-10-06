# Changelog

All notable changes to this project will be documented in this file.

The format is based on Keep a Changelog.

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
