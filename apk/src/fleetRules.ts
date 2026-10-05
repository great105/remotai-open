/**
 * Правила экрана «Мои компьютеры» — отдельно от самого экрана.
 *
 * Эти три правила переписывались в 2.39.0, 2.39.1 и 2.40.0, и каждый раз их
 * проверял живой прогон с телефона: они жили внутри `InfrastructureView.tsx`
 * (2900 строк) и в отрыве от React не вызывались, а значит и не тестировались.
 * Здесь они лежат чистыми функциями от данных — их проверяет
 * `fleetRules.test.ts`, и регресс вида «пустое пространство снова считается
 * структурой парка» падает в CI, а не на телефоне владельца.
 *
 * Зависимостей нет намеренно: типы описаны структурно (ровно те поля, которые
 * правило читает), поэтому модуль не тянет ни `cloud/api`, ни i18n.
 */

/** Ровно то, что правилу нужно знать об устройстве. `CloudDevice` подходит. */
export type FleetDevice = {
  workspace_id: string;
  agent_version?: string | null;
  online?: boolean;
};

/** Сравнение версий «x.y.z»: >0 если a новее b. Больше трёх чисел релиз не даёт. */
export function compareVersions(a: string, b: string): number {
  const left = a.split(".").map((part) => parseInt(part, 10) || 0);
  const right = b.split(".").map((part) => parseInt(part, 10) || 0);
  for (let i = 0; i < 3; i += 1) {
    const diff = (left[i] || 0) - (right[i] || 0);
    if (diff !== 0) return diff > 0 ? 1 : -1;
  }
  return 0;
}

/**
 * Что на самом деле происходит с версией агента.
 *
 * Красный бейдж «Агент устарел» был тревогой без выхода: человек с телефона
 * ничего с версией сделать не может, а правда в том, что Remotai обновляется
 * САМ. Поэтому состояний четыре, и красное — ровно одно:
 *  • `unknown` — сравнивать не с чем (нет версии или манифест релея недоступен);
 *    выдумывать сравнение нельзя, показываем версию без всякой пометки;
 *  • `current`  — версия не отстала;
 *  • `auto`     — отстала, но обновление идёт своим ходом (и у выключенной
 *    машины оно просто ещё не начиналось) — спокойная вторичная строка;
 *  • `stuck`    — машина В СЕТИ, а самообновление отстало на целую мажорную
 *    версию: своим ходом это уже не догонится, значит нужен красный И действие.
 */
export type AgentUpdateState = "unknown" | "current" | "auto" | "stuck";

export function agentUpdateState(device: FleetDevice, latestVersion: string): AgentUpdateState {
  const version = (device.agent_version || "").trim();
  if (!version || !latestVersion) return "unknown";
  if (compareVersions(latestVersion, version) <= 0) return "current";
  const major = (value: string) => parseInt(value.split(".")[0], 10) || 0;
  if (device.online && major(latestVersion) > major(version)) return "stuck";
  return "auto";
}

/**
 * Сколько пространств РЕАЛЬНО заняты машинами.
 *
 * Считается по содержимому, а не по числу заведённых пространств: у владельца
 * их было два, во втором ноль машин — и человек с ОДНИМ компьютером получал
 * первый экран из пустого «Личное · 0 в сети · 0 всего» и пяти счётчиков.
 * Всё, что лежит вне пространств самого человека, считается одним общим
 * («Общий доступ»), иначе три расшаренные машины из трёх чужих компаний
 * раскладывали бы экран, в котором человеку нечего выбирать.
 */
export function populatedWorkspaceCount(
  devices: readonly FleetDevice[] | null | undefined,
  directWorkspaceIDs: ReadonlySet<string>,
  sharedWorkspaceID: string,
): number {
  const ids = new Set<string>();
  for (const device of devices || []) {
    ids.add(directWorkspaceIDs.has(device.workspace_id) ? device.workspace_id : sharedWorkspaceID);
  }
  return ids.size;
}

/**
 * «Простой парк» — тот, в котором человеку нечего выбирать, и организационный
 * слой (пространства, зоны, фильтры, групповые действия) на экран не выходит.
 *
 * Пространства и зоны считаются ОДНИМ правилом — по содержимому. Пустая зона
 * структурой парка не является: одной такой хватало, чтобы путь до карточки
 * своей машины вырос с 698 до 987 px.
 */
export function isSimpleFleet(populatedWorkspaces: number, populatedZones: number): boolean {
  return populatedWorkspaces <= 1 && populatedZones === 0;
}
