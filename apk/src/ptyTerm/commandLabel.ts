/**
 * Как НАЗВАТЬ команду человеку в вопросе, а не показать её целиком.
 *
 * Аудит онбординга 30.08.2026: диалог «в терминале работает агент — команда
 * уйдёт ему сообщением» печатал 2579 символов, из них ~96 % — служебная
 * обёртка PowerShell (`$env:…; try { claude } finally { … }`). Сам вопрос
 * оказывался в конце простыни, которую никто не дочитывает, — то есть
 * подтверждение переставало быть подтверждением.
 *
 * Здесь из команды достаётся её СУТЬ: то, что на самом деле запустится.
 * Обёртки известны наперёд, потому что их строит соседний модуль
 * (`agentLaunch.ts`): Windows — `try { … } finally { … }`, POSIX — проверка
 * `if command -v … then … fi` и префикс переменных окружения.
 */

/** Ядро запуска: команда без обёрток, если её удалось узнать. */
function launchCore(flat: string): string {
  // Windows: настоящий запуск живёт внутри try { … }, но и там ему
  // предшествуют присваивания `$env:NAME='…';` — их тоже снимаем.
  const inTry = flat.match(/try \{\s*(.+?)\s*\} finally/);
  if (inTry) {
    // Значение в одинарных кавычках PowerShell: внутренний апостроф удвоен.
    return inTry[1].replace(/^(?:\$env:[A-Za-z_][A-Za-z0-9_]*=(?:'(?:[^']|'')*'|\S+);\s*)+/, "");
  }
  // POSIX-проверка наличия CLI: `if command -v claude …; then claude; else …`.
  const afterThen = flat.match(/;\s*then\s+([^;]+);/);
  if (afterThen) return afterThen[1].trim();
  // POSIX-префикс окружения: `env -u A -u B VAR='…' claude --flag`.
  const noEnv = flat
    .replace(/^env(?:\s+-u\s+\S+)*\s+/, "")
    .replace(/^(?:[A-Za-z_][A-Za-z0-9_]*=(?:'(?:[^']|'\\'')*'|\S*)\s+)+/, "");
  return noEnv;
}

/**
 * Короткая подпись команды для вопроса. Пустую строку не выдумываем: если
 * ядра не нашлось, честно обрезаем исходную команду.
 */
/**
 * Хуки агента (`--settings '…'` у Claude, `-c 'notify=…'` у Codex) — такая же
 * служебная часть запуска, как обёртка PowerShell: путь к файлу настроек в
 * AppData съел бы всю подпись, а человеку он ничего не говорит.
 */
function withoutHookArgs(flat: string): string {
  return flat
    .replace(/\s--settings\s+(?:'(?:[^']|'')*'|\S+)/g, "")
    .replace(/\s-c\s+(?:'notify=(?:[^']|'')*'|notify=\S+)/g, "");
}

export function shortCommandLabel(cmd: string, limit = 60): string {
  const flat = withoutHookArgs((cmd || "").replace(/\s+/g, " ").trim());
  if (!flat) return "";
  if (flat.length <= limit) return flat;
  const core = launchCore(flat).trim() || flat;
  if (core.length <= limit) return core;
  return `${core.slice(0, limit - 1).trimEnd()}…`;
}
