// Шторка «SSH-ключи»: ключи, которые живут внутри Remotai на выбранном
// компьютере. Здесь их создают, приносят готовыми, ставят на сервер и
// назначают серверу.
//
// Приватная часть ключа сюда не приходит и уйти отсюда не может: клиент видит
// только имя, тип и отпечаток. «Скопировать» копирует ПУБЛИЧНЫЙ ключ — тот,
// который и полагается раздавать.

import { useEffect, useState } from "react";
import {
  getSshKeys, importSshKey, generateSshKey, renameSshKey, deleteSshKey, installSshKey,
} from "../api";
import type { SshKey, SshHost } from "../api";
import { haptic, hapticSuccess, tgConfirm } from "../telegram";
import { t } from "../i18n";
import { useToast, useEscape } from "@tgcontrol/shared";
import { sshErrorText, sshTargetLabel, invalidateSshHostsCache } from "../sshCommon";

interface Props {
  open: boolean;
  onClose: () => void;
  /** Серверы — чтобы ключ можно было поставить, не уходя отсюда. */
  hosts: SshHost[];
  /** Список серверов мог измениться (ключ назначен) — перечитать. */
  onHostsChanged?: () => void;
}

// Переименование — свой режим, а не системный prompt: в окне exe (WebView2)
// нативные prompt/confirm подавлены и молча возвращают null.
type Mode = "list" | "generate" | "import" | "install" | "rename";

export function SshKeysSheet({ open, onClose, hosts, onHostsChanged }: Props) {
  const { toastSuccess, toastError } = useToast();
  const [keys, setKeys] = useState<SshKey[]>([]);
  const [foreign, setForeign] = useState(false);
  const [loading, setLoading] = useState(false);
  const [mode, setMode] = useState<Mode>("list");
  const [busy, setBusy] = useState(false);
  // Ключ, который сейчас ставят на сервер.
  const [installKey, setInstallKey] = useState<SshKey | null>(null);

  // Поля форм
  const [name, setName] = useState("");
  const [keyType, setKeyType] = useState<"ed25519" | "rsa">("ed25519");
  const [passphrase, setPassphrase] = useState("");
  const [pasted, setPasted] = useState("");
  const [path, setPath] = useState("");
  const [installHostId, setInstallHostId] = useState("");
  const [installPassword, setInstallPassword] = useState("");

  // Escape закрывает по одному слою: из формы — в список, из списка — наружу.
  // Иначе случайный Esc в форме терял бы уже набранный ключ.
  useEscape(open, () => {
    if (mode === "list") onClose();
    else backToList();
  });

  const load = async () => {
    setLoading(true);
    try {
      const r = await getSshKeys();
      setKeys(r.keys || []);
      setForeign(!!r.foreign);
    } catch (e: any) {
      setKeys([]);
      toastError(sshErrorText(e));
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    if (!open) return;
    setMode("list");
    void load();
  }, [open]);

  if (!open) return null;

  const backToList = () => {
    setMode("list");
    setName("");
    setPassphrase("");
    setPasted("");
    setPath("");
    setInstallKey(null);
    setInstallPassword("");
  };

  const doGenerate = async () => {
    if (busy) return;
    setBusy(true);
    try {
      const { key } = await generateSshKey({ name: name.trim(), type: keyType, passphrase });
      hapticSuccess();
      toastSuccess(t("ssh.keys.created", { name: key.name }));
      backToList();
      await load();
    } catch (e: any) {
      toastError(sshErrorText(e));
    } finally {
      setBusy(false);
    }
  };

  const doImport = async () => {
    if (busy) return;
    if (!pasted.trim() && !path.trim()) {
      toastError(t("ssh.keys.needKey"));
      return;
    }
    setBusy(true);
    try {
      const { key } = await importSshKey({
        name: name.trim(),
        private_key: pasted.trim() || undefined,
        path: path.trim() || undefined,
        passphrase: passphrase || undefined,
      });
      hapticSuccess();
      toastSuccess(t("ssh.keys.imported", { name: key.name }));
      backToList();
      await load();
    } catch (e: any) {
      toastError(sshErrorText(e));
    } finally {
      setBusy(false);
    }
  };

  const doDelete = async (key: SshKey) => {
    // Удаление ключа меняет способ входа на серверах — об этом и предупреждаем,
    // а не спрашиваем безличное «удалить элемент?».
    if (!(await tgConfirm(t("ssh.keys.deleteAsk", { name: key.name }),
      { danger: true, confirmText: t("confirm.btn.delete") }))) return;
    try {
      const r = await deleteSshKey(key.id);
      haptic("medium");
      toastSuccess(r.detached_hosts > 0
        ? t("ssh.keys.deletedDetached", { n: r.detached_hosts })
        : t("ssh.keys.deleted"));
      if (r.detached_hosts > 0) {
        invalidateSshHostsCache();
        onHostsChanged?.();
      }
      await load();
    } catch (e: any) {
      toastError(sshErrorText(e));
    }
  };

  const openRename = (key: SshKey) => {
    haptic();
    setInstallKey(key); // тот же «ключ, с которым сейчас работают»
    setName(key.name);
    setMode("rename");
  };

  const doRename = async () => {
    if (busy || !installKey) return;
    if (!name.trim()) {
      toastError(t("ssh.keys.needName"));
      return;
    }
    setBusy(true);
    try {
      await renameSshKey(installKey.id, name.trim());
      hapticSuccess();
      toastSuccess(t("ssh.keys.renamed"));
      // Имя ключа видно в карточке сервера — кэш серверов протух.
      invalidateSshHostsCache();
      onHostsChanged?.();
      backToList();
      await load();
    } catch (e: any) {
      toastError(sshErrorText(e));
    } finally {
      setBusy(false);
    }
  };

  const copyPublic = async (key: SshKey) => {
    try {
      await navigator.clipboard.writeText(key.public_key);
      hapticSuccess();
      toastSuccess(t("ssh.keys.copied"));
    } catch {
      toastError(t("ssh.install.copyFailed"));
    }
  };

  const openInstall = (key: SshKey) => {
    if (hosts.length === 0) {
      toastError(t("ssh.keys.installNoHosts"));
      return;
    }
    haptic();
    setInstallKey(key);
    setInstallHostId(hosts[0].id);
    setInstallPassword("");
    setMode("install");
  };

  const doInstall = async () => {
    if (busy || !installKey) return;
    setBusy(true);
    try {
      const r = await installSshKey(installKey.id, {
        host_id: installHostId,
        password: installPassword || undefined,
        // Ключ, поставленный на сервер, но не назначенный ему, ничего не
        // меняет для человека — назначаем сразу.
        assign: true,
      });
      hapticSuccess();
      toastSuccess(!r.assigned
        ? t("ssh.keys.installedNotAssigned")
        : r.already
        ? t("ssh.keys.installedAlready")
        : t("ssh.keys.installed"));
      invalidateSshHostsCache();
      onHostsChanged?.();
      backToList();
    } catch (e: any) {
      toastError(sshErrorText(e));
    } finally {
      setBusy(false);
    }
  };

  const title = mode === "generate" ? t("ssh.keys.generateTitle")
    : mode === "import" ? t("ssh.keys.importTitle")
    : mode === "install" ? t("ssh.keys.installTitle", { key: installKey?.name || "" })
    : mode === "rename" ? t("ssh.keys.rename")
    : t("ssh.keys.title");

  return (
    <div className="modal-overlay" onClick={(e) => { if (e.target === e.currentTarget) onClose(); }}>
      <div className="modal-sheet ssh-keys-sheet">
        <div className="help-sheet-header">
          {mode !== "list" && (
            <button className="icon-btn" onClick={() => { haptic(); backToList(); }}
              aria-label={t("generic.back")}>{"←"}</button>
          )}
          <div className="modal-title">{title}</div>
          <button className="icon-btn" onClick={onClose} aria-label={t("modal.close")}>{"✕"}</button>
        </div>

        {mode === "list" && (
          <>
            <p className="ssh-keys-intro">{t("ssh.keys.intro")}</p>
            {/* Пустой список и «ключи есть, но не открываются» — разные беды.
                Вторую нельзя показывать пустотой: человек решит, что ключи
                пропали, и заведёт их заново вместо входа в свою учётку. */}
            {foreign && <div className="ssh-keys-foreign">{t("ssh.keys.foreign")}</div>}
            {loading ? (
              <div className="loading-center"><div className="spinner" /></div>
            ) : keys.length === 0 ? (
              <div className="ssh-empty">{t("ssh.keys.empty")}</div>
            ) : (
              <div className="ssh-keys-list">
                {keys.map((k) => (
                  <div className="ssh-key-item" key={k.id}>
                    <div className="ssh-key-info">
                      <div className="ssh-key-name">
                        {k.name}
                        <span className="ssh-badge saved">{shortKeyType(k.type)}</span>
                        {k.encrypted && <span className="ssh-badge">{t("ssh.keys.encrypted")}</span>}
                      </div>
                      {/* Отпечаток — то, чем ключ сверяют с сервером; без него
                          два ключа с похожими именами неразличимы. */}
                      <div className="ssh-key-fp">{k.fingerprint}</div>
                      {k.generated && <div className="ssh-key-hint">{t("ssh.keys.generatedHint")}</div>}
                    </div>
                    <div className="ssh-host-actions ssh-host-actions-wrap">
                      <button className="ssh-host-connect" onClick={() => openInstall(k)}>
                        {`⬆ ${t("ssh.keys.install")}`}
                      </button>
                      <button className="ssh-host-files" onClick={() => void copyPublic(k)}>
                        {`⧉ ${t("ssh.keys.copyPublic")}`}
                      </button>
                      <button className="ssh-icon-btn" title={t("ssh.keys.rename")}
                        aria-label={`${t("ssh.keys.rename")}: ${k.name}`}
                        onClick={() => openRename(k)}>{"✎"}</button>
                      <button className="ssh-icon-btn" title={t("ssh.keys.delete")}
                        aria-label={`${t("ssh.keys.delete")}: ${k.name}`}
                        onClick={() => void doDelete(k)}>{"🗑"}</button>
                    </div>
                  </div>
                ))}
              </div>
            )}
            <div className="ssh-keys-actions">
              <button className="btn btn-primary" onClick={() => { haptic(); setMode("generate"); }}>
                {t("ssh.keys.generate")}
              </button>
              <button className="btn btn-secondary" onClick={() => { haptic(); setMode("import"); }}>
                {t("ssh.keys.import")}
              </button>
            </div>
          </>
        )}

        {mode === "generate" && (
          <>
            <input className="modal-input" placeholder={t("ssh.keys.name")} autoFocus
              value={name} onChange={(e) => setName(e.target.value)} />
            <label className="ssh-form-field">
              <span>{t("ssh.keys.type")}</span>
              <select className="modal-input" value={keyType}
                onChange={(e) => setKeyType(e.target.value as "ed25519" | "rsa")}>
                <option value="ed25519">{t("ssh.keys.typeEd25519")}</option>
                <option value="rsa">{t("ssh.keys.typeRsa")}</option>
              </select>
            </label>
            <label className="ssh-form-field">
              <span>{t("ssh.keys.passphrase")}</span>
              <input className="modal-input" type="password" placeholder={t("ssh.keys.passphrase")}
                value={passphrase} onChange={(e) => setPassphrase(e.target.value)} />
            </label>
            <div className="ssh-sheet-hint">{t("ssh.keys.passphraseHint")}</div>
            <button className="btn btn-primary ssh-sheet-submit" disabled={busy}
              onClick={() => void doGenerate()}>
              {busy ? t("ssh.form.saving") : t("ssh.keys.generate")}
            </button>
          </>
        )}

        {mode === "import" && (
          <>
            <input className="modal-input" placeholder={t("ssh.keys.name")} autoFocus
              value={name} onChange={(e) => setName(e.target.value)} />
            <label className="ssh-form-field">
              <span>{t("ssh.keys.pasteLabel")}</span>
              <textarea className="modal-input ssh-key-textarea" rows={6}
                placeholder={t("ssh.keys.pastePlaceholder")}
                value={pasted} onChange={(e) => setPasted(e.target.value)}
                autoCapitalize="off" autoCorrect="off" spellCheck={false} />
            </label>
            <div className="ssh-sheet-hint">{t("ssh.keys.pasteHint")}</div>
            <label className="ssh-form-field">
              <span>{t("ssh.keys.pathLabel")}</span>
              <input className="modal-input" placeholder="~/.ssh/id_ed25519"
                value={path} onChange={(e) => setPath(e.target.value)}
                autoCapitalize="off" autoCorrect="off" />
            </label>
            <label className="ssh-form-field">
              <span>{t("ssh.keys.passphrase")}</span>
              <input className="modal-input" type="password" placeholder={t("ssh.keys.passphrase")}
                value={passphrase} onChange={(e) => setPassphrase(e.target.value)} />
            </label>
            <button className="btn btn-primary ssh-sheet-submit" disabled={busy}
              onClick={() => void doImport()}>
              {busy ? t("ssh.form.saving") : t("ssh.keys.import")}
            </button>
          </>
        )}

        {mode === "rename" && installKey && (
          <>
            <input className="modal-input" placeholder={t("ssh.keys.name")} autoFocus
              value={name} onChange={(e) => setName(e.target.value)}
              onKeyDown={(e) => { if (e.key === "Enter") { e.preventDefault(); void doRename(); } }} />
            <div className="ssh-key-fp">{installKey.fingerprint}</div>
            <button className="btn btn-primary ssh-sheet-submit" disabled={busy}
              onClick={() => void doRename()}>
              {busy ? t("ssh.form.saving") : t("ssh.form.save")}
            </button>
          </>
        )}

        {mode === "install" && installKey && (
          <>
            <label className="ssh-form-field">
              <span>{t("ssh.keys.installPick")}</span>
              <select className="modal-input" value={installHostId}
                onChange={(e) => setInstallHostId(e.target.value)}>
                {hosts.map((h) => (
                  <option key={h.id} value={h.id}>
                    {h.name || sshTargetLabel(h)}
                  </option>
                ))}
              </select>
            </label>
            <label className="ssh-form-field">
              <span>{t("ssh.keys.installPassword")}</span>
              <input className="modal-input" type="password" autoFocus
                placeholder={t("ssh.form.password")}
                value={installPassword} onChange={(e) => setInstallPassword(e.target.value)} />
            </label>
            <div className="ssh-sheet-hint">{t("ssh.keys.installHint")}</div>
            <button className="btn btn-primary ssh-sheet-submit" disabled={busy}
              onClick={() => void doInstall()}>
              {busy ? t("ssh.keys.installing") : t("ssh.keys.install")}
            </button>
          </>
        )}
      </div>
    </div>
  );
}

/** «ssh-ed25519» → «ed25519»: в бейдже важен род ключа, а не префикс. */
function shortKeyType(type: string): string {
  return type.replace(/^ssh-/, "").replace(/^ecdsa-sha2-/, "ecdsa ");
}
