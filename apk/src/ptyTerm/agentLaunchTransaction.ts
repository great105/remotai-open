/** Безопасная отметка аккаунта, которую можно хранить у PTY. */
export interface PtyAccountMarker {
  id: string;
  label: string;
}

interface AccountScopedLaunch {
  /** Уже запущенному агенту команда уйдёт как ввод, а не создаст новый CLI. */
  agentAlreadyRunning: boolean;
  /** Подтверждение и отправка ввода текущему агенту. Metadata здесь не меняется. */
  sendToExistingAgent: () => Promise<boolean>;
  previous: PtyAccountMarker;
  next: PtyAccountMarker;
  /** REST-фиксация metadata; false означает, что сервер её не принял. */
  persist: (marker: PtyAccountMarker) => Promise<boolean>;
  /** Синхронная постановка байтов в открытый WebSocket. */
  sendToShell: () => boolean;
}

/**
 * Связывает account metadata только с фактически начатым shell-launch.
 *
 * Важный порядок:
 * 1. у живого агента сначала confirm/send, без единой записи metadata;
 * 2. у idle shell сначала фиксируем аккаунт, затем отправляем CLI;
 * 3. если сокет закрылся в этом узком окне, возвращаем прежнюю metadata.
 */
export async function commitAccountScopedLaunch(input: AccountScopedLaunch): Promise<boolean> {
  if (input.agentAlreadyRunning) return input.sendToExistingAgent();

  if (!(await input.persist(input.next))) {
    throw new Error("Не удалось запомнить аккаунт терминала.");
  }
  try {
    if (input.sendToShell()) return true;
  } catch (error) {
    if (!(await input.persist(input.previous))) {
      throw new Error("Команда не отправлена, а прежний аккаунт терминала не удалось восстановить.");
    }
    throw error;
  }
  if (!(await input.persist(input.previous))) {
    throw new Error("Команда не отправлена, а прежний аккаунт терминала не удалось восстановить.");
  }
  return false;
}
