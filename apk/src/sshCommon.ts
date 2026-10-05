// Общие мелочи SSH-UI: пароли текущей страницы (в localStorage их нет и не
// будет — сохранённые пароли живут на ПК у агента), человеческие тексты
// ошибок, формат «N дней назад».

export interface SshTarget {
  id?: string;
  host: string;
  port?: number;
  user: string;
  identity_file?: string;
  proxy_jump?: string;
}

import { forgetSshKnownHost, getSshHosts } from "./api";
import type { SshHost } from "./api";
import { getMode, getSelectedDeviceId } from "./config";
import { tgConfirm } from "./telegram";
import { t } from "./i18n";
import { isPcOffline, mapApiError } from "@tgcontrol/shared";

const _passwords = new Map<string, string>();

function key(t: SshTarget): string {
  return `${t.user}@${t.host}:${t.port || 22}`;
}

export function getSshPassword(t: SshTarget): string {
  return _passwords.get(key(t)) || "";
}

export function setSshPassword(t: SshTarget, password: string) {
  if (password) _passwords.set(key(t), password);
}

/**
 * Забыть пароль этой вкладки.
 *
 * Обязателен вместе с «Забыть» на агенте: пароль живёт в двух местах, и если
 * очистить только компьютер, следующее нажатие «Подключиться» подставит его из
 * памяти страницы — сервер откроется молча, а агент запомнит пароль заново.
 * Человек при этом уверен, что доступ убран.
 */
export function forgetSshPassword(t: SshTarget) {
  _passwords.delete(key(t));
}

/**
 * mapApiError, но без утечки сырого английского текста агента и релея.
 *
 * И агент, и релей кладут в `error` машинный код плюс англоязычный msg
 * («agent offline», «sftp list: … permission denied»). mapApiError переводит
 * известные коды и статусы, но последней строкой возвращает именно msg — и
 * раньше он уходил в тост как есть. Здесь такой текст заменяем на `fallback`.
 */
export function sshApiText(e: any, fallback: string): string {
  const mapped = mapApiError(e);
  // Перевод нашёлся (по коду, офлайну или статусу) — текст уже человеческий.
  if (mapped !== String(e?.message ?? "")) return mapped;
  // Сервер ответил по-русски (например, текст с самого агента) — показываем.
  if (/[а-яё]/i.test(mapped)) return mapped;
  return fallback;
}

// Машинные code от /api/ssh/* → RU-текст (для тостов и форм).
export function sshErrorText(e: any): string {
  // Выключенный ПК-посредник важнее любого SSH-кода: до сервера просто некому
  // дозвониться, а человеку надо включить компьютер, а не менять пароль.
  if (isPcOffline(e)) return mapApiError(e);
  switch (e?.code) {
    case "auth_failed":
      return t("ssh.error.authFailed")
        + (Array.isArray(e?.tried) && e.tried.length
          ? ` ${t("ssh.error.authTried", { list: e.tried.join(", ") })}`
          : "");
    case "key_encrypted":
      return t("ssh.error.keyEncrypted");
    // Ключ из хранилища подвёл не тем, что «не подошёл»: его либо удалили,
    // либо он зашифрован другой учётной записью Windows. Оба случая требуют
    // от человека разных действий, поэтому и текста два.
    case "key_locked":
      return t("ssh.error.keyLocked");
    case "key_gone":
      return t("ssh.error.keyGone");
    case "key_invalid":
      return t("ssh.error.keyInvalid");
    case "key_bad_passphrase":
      return t("ssh.error.keyBadPassphrase");
    case "key_file_missing":
      return t("ssh.error.keyFileMissing");
    case "key_install_failed":
      return t("ssh.error.keyInstallFailed");
    case "host_key_unknown":
      return t("ssh.error.hostKeyUnknown");
    case "host_key_mismatch":
      return t("ssh.error.hostKeyMismatch");
    case "unreachable":
      return t("ssh.error.unreachable");
    case "bad_request":
      return t("ssh.error.badRequest");
    case "unsafe_bind":
      return t("ssh.error.unsafeBind");
    default:
      // Файловые коды агента (not_found/no_permission/disk_full…) переводит
      // общая таблица mapApiError — дублировать её здесь не нужно.
      return sshApiText(e, t("ssh.error.generic"));
  }
}

/**
 * Один TOFU/MITM-поток для терминала, SFTP и форвардов. Неизвестный ключ можно
 * принять после показа отпечатка; изменившийся — только после отдельного
 * опасного подтверждения и удаления старой записи из private known_hosts.
 */
export async function runWithSshTrust<T>(
  target: SshTarget,
  action: (trustHost: boolean) => Promise<T>,
): Promise<T> {
  try {
    return await action(false);
  } catch (e: any) {
    const fingerprint = e?.fingerprint || t("ssh.trust.noFingerprint");
    if (e?.code === "host_key_unknown") {
      // Вопрос «доверять?» без способа на него ответить обесценивает защиту:
      // человек не знает, где взять эталон отпечатка, и жмёт «Доверять» не
      // глядя. Поэтому рядом с отпечатком говорим, с чем его сверить.
      const trusted = await tgConfirm(
        `${sshTargetLabel(target)}\n\n${t("ssh.trust.fingerprint")}\n${fingerprint}\n\n${t("ssh.trust.verifyHint", { host: target.host })}`,
        { title: t("ssh.trust.newTitle"), confirmText: t("confirm.btn.trust") },
      ).catch(() => false);
      if (!trusted) throw e;
      return action(true);
    }
    if (e?.code === "host_key_mismatch") {
      const expected = Array.isArray(e?.expected) ? e.expected.join("\n") : t("ssh.trust.mismatchUnknown");
      // Смена ключа и перехват соединения выглядят для человека одинаково, а
      // цена ошибки — пароль прод-сервера в чужих руках. Диалог обязан назвать
      // второй сценарий: иначе оба случая заканчиваются одним нажатием.
      const replace = await tgConfirm(
        `${sshTargetLabel(target)}\n\n${t("ssh.trust.mismatchWas")}\n${expected}\n\n${t("ssh.trust.mismatchNow")}\n${fingerprint}\n\n${t("ssh.trust.mismatchWarn")}\n\n${t("ssh.trust.mismatchAsk")}`,
        { danger: true, title: t("ssh.trust.mismatchTitle"), confirmText: t("confirm.btn.forgetOldKey") },
      ).catch(() => false);
      if (!replace) throw e;
      await forgetSshKnownHost(target.host, target.port || 22);
      return action(true);
    }
    throw e;
  }
}

/** ISO-время → «только что / 5 мин назад / 3 дн назад». */
export function sshLastAgo(iso?: string): string {
  if (!iso) return "";
  const ms = Date.parse(iso);
  if (!Number.isFinite(ms)) return "";
  const sec = Math.max(0, ((Date.now() - ms) / 1000) | 0);
  if (sec < 60) return t("chat.justNow");
  if (sec < 3600) return t("ui.infrastructureview.m8ceea3d01a", { p0: ((sec / 60) | 0) });
  if (sec < 86400) return t("ui.infrastructureview.mb748cc3575", { p0: ((sec / 3600) | 0) });
  const d = (sec / 86400) | 0;
  return t("ui.infrastructureview.m644d0bda1d", { p0: (d) });
}

/** Короткая подпись сервера: user@host:port (порт 22 не показываем). */
export function sshTargetLabel(t: SshTarget): string {
  const port = t.port || 22;
  return `${t.user}@${t.host}${port !== 22 ? `:${port}` : ""}`;
}

// ── Список серверов для общего списка машин ─────────────────────────
//
// SSH-серверы принадлежат ВЫБРАННОЙ машине с Remotai: их читает её агент
// (~/.ssh/config + ssh_hosts.json), телефон к серверу не ходит. Поэтому кэш
// живёт вместе с выбранной машиной — переключились на другой компьютер, и
// прошлый ответ выбрасываем, иначе под ноутбуком нарисовались бы серверы
// домашнего ПК.
//
// Ошибку не глотаем: «серверов нет» и «компьютер не в сети» — разные вещи, и
// решать, что показать, обязан вызывающий экран (у него есть isPcOffline).
// Неудачный ответ в кэш не попадает, поэтому следующий вызов сходит заново.

const SSH_HOSTS_TTL_MS = 60_000;

let sshHostsCache: { key: string; at: number; hosts: SshHost[] } | null = null;
let sshHostsInflight: { key: string; promise: Promise<SshHost[]> } | null = null;

/** Ключ кэша = режим + машина, с которой сейчас работают. */
function sshHostsKey(): string {
  return `${getMode()}:${getSelectedDeviceId()}`;
}

/** Сохранённые SSH-серверы текущего ПК. Кэш 60 с + дедупликация параллельных
 *  вызовов, чтобы общий список машин не добавлял запросов на каждый рендер. */
export function listSshHostsCached(force?: boolean): Promise<SshHost[]> {
  const key = sshHostsKey();
  if (!force && sshHostsCache && sshHostsCache.key === key
      && Date.now() - sshHostsCache.at < SSH_HOSTS_TTL_MS) {
    return Promise.resolve(sshHostsCache.hosts);
  }
  // Дедупликация: десять карточек, смонтированных одновременно, обязаны дать
  // один запрос. При force ждать чужой ответ нельзя — его могли запросить до
  // того, как сервер переименовали.
  if (!force && sshHostsInflight && sshHostsInflight.key === key) return sshHostsInflight.promise;
  const promise = getSshHosts()
    .then((r) => {
      const hosts = r.hosts || [];
      sshHostsCache = { key, at: Date.now(), hosts };
      return hosts;
    })
    .finally(() => {
      if (sshHostsInflight?.promise === promise) sshHostsInflight = null;
    });
  sshHostsInflight = { key, promise };
  return promise;
}

/** Сбросить кэш — звать после добавления/удаления/переименования сервера. */
export function invalidateSshHostsCache(): void {
  sshHostsCache = null;
  sshHostsInflight = null;
}
