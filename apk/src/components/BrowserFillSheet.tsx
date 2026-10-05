/**
 * «Заполнить» — то, ради чего с телефона можно РЕГИСТРИРОВАТЬСЯ на сайтах,
 * открытых на сервере.
 *
 * Без этого регистрация выглядит так: набрать почту по буквам через удалённый
 * экран, придумать пароль в голове, ввести его дважды и запомнить навсегда —
 * записать его некуда, потому что менеджер паролей телефона о сайтах сервера
 * не знает. Здесь: «Заполнить анкетой», «Придумать пароль», «Сохранить вход»,
 * список сохранённых входов для этого сайта.
 *
 * Пароль НИКОГДА не приходит на телефон: шит называет запись идентификатором, а
 * значение подставляет агент прямо в поле страницы. Единственное исключение —
 * только что придуманный пароль: его показываем один раз, чтобы человек мог
 * записать или скопировать, если не хочет доверять хранилищу.
 */

import { useEffect, useState } from "react";
import type { BrowserLogin, BrowserProfileData } from "../api";
import {
  deleteBrowserLogin, fillBrowserForm, getBrowserForm, saveBrowserLogin, saveBrowserProfile,
} from "../api";
import { mapApiError, t, useEscape, useToast } from "@tgcontrol/shared";
import { haptic, hapticSuccess } from "../telegram";

export interface BrowserFillSheetProps {
  onClose: () => void;
  /** Скопировать текст в буфер устройства (общий путь экрана). */
  onCopy: (text: string) => void;
}

const PROFILE_FIELDS: Array<{ key: keyof BrowserProfileData; label: string; type?: string }> = [
  { key: "first_name", label: "remote.profileFirstName" },
  { key: "last_name", label: "remote.profileLastName" },
  { key: "email", label: "remote.profileEmail", type: "email" },
  { key: "phone", label: "remote.profilePhone", type: "tel" },
  { key: "birthday", label: "remote.profileBirthday", type: "date" },
  { key: "country", label: "remote.profileCountry" },
  { key: "city", label: "remote.profileCity" },
  { key: "address", label: "remote.profileAddress" },
  { key: "zip", label: "remote.profileZip" },
];

export default function BrowserFillSheet({ onClose, onCopy }: BrowserFillSheetProps) {
  // Системная «Назад» закрывает лист, а не уводит с экрана браузера: лист
  // существует, только пока смонтирован (аудит ИА 02.09.2026, P0-6).
  useEscape(true, onClose);
  const [host, setHost] = useState("");
  const [logins, setLogins] = useState<BrowserLogin[]>([]);
  const [profile, setProfile] = useState<BrowserProfileData>({});
  const [signup, setSignup] = useState(false);
  const [hasForm, setHasForm] = useState(false);
  const [busy, setBusy] = useState(false);
  const [loading, setLoading] = useState(true);
  const [editProfile, setEditProfile] = useState(false);
  const [newPassword, setNewPassword] = useState("");
  const { toastError, toastSuccess } = useToast();

  const reload = async () => {
    try {
      const form = await getBrowserForm();
      setHost(form.host);
      setLogins(form.logins || []);
      setProfile(form.profile || {});
      setSignup(!!form.signup);
      setHasForm(!!form.has_form);
    } catch (e: any) {
      toastError(mapApiError(e));
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => { void reload(); }, []);

  const run = async (fn: () => Promise<void>) => {
    if (busy) return;
    setBusy(true);
    try {
      await fn();
    } catch (e: any) {
      toastError(mapApiError(e));
    } finally {
      setBusy(false);
    }
  };

  const useLogin = (id: string) => run(async () => {
    const res = await fillBrowserForm({ login_id: id });
    hapticSuccess();
    toastSuccess(t("remote.fillDone", { n: res.filled }));
    onClose();
  });

  const fillProfile = () => run(async () => {
    const res = await fillBrowserForm({ profile: true });
    hapticSuccess();
    toastSuccess(t("remote.fillDone", { n: res.filled }));
  });

  const makePassword = () => run(async () => {
    const res = await fillBrowserForm({ new_password: 20 });
    if (res.password) setNewPassword(res.password);
    hapticSuccess();
    toastSuccess(t("remote.fillNewPasswordDone"));
  });

  const fillSignup = () => run(async () => {
    const res = await fillBrowserForm({ profile: true, new_password: 20 });
    if (res.password) setNewPassword(res.password);
    hapticSuccess();
    toastSuccess(t("remote.fillDone", { n: res.filled }));
  });

  const saveCurrent = () => run(async () => {
    await saveBrowserLogin({});
    setNewPassword("");
    hapticSuccess();
    toastSuccess(t("remote.fillSaved"));
    await reload();
  });

  const forget = (id: string) => run(async () => {
    haptic("light");
    await deleteBrowserLogin(id);
    setLogins((list) => list.filter((item) => item.id !== id));
  });

  const storeProfile = () => run(async () => {
    const res = await saveBrowserProfile(profile);
    setProfile(res.profile || profile);
    setEditProfile(false);
    hapticSuccess();
  });

  return (
    <div className="remote-sheet-backdrop" onClick={onClose}>
      <div className="remote-menu-sheet vb-fill-sheet" onClick={(e) => e.stopPropagation()}>
        <div className="remote-quick-section">
          {t("remote.fillTitle")}{host ? ` · ${host}` : ""}
        </div>

        {signup && <div className="vb-fill-hint">{t("remote.fillSignupHint")}</div>}

        {newPassword && (
          <div className="vb-fill-password">
            <code>{newPassword}</code>
            <button className="btn btn-sm" onClick={() => { onCopy(newPassword); toastSuccess(t("remote.ctxCopied")); }}>
              {t("remote.passwordCopy")}
            </button>
          </div>
        )}

        {hasForm && (
          <>
            {signup && (
              <button className="remote-quick-row" disabled={busy} onClick={fillSignup}>
                <span className="remote-quick-row-icon">{"✨"}</span>
                <span className="remote-quick-row-label">{t("remote.fillProfile")} + {t("remote.fillNewPassword")}</span>
              </button>
            )}
            <button className="remote-quick-row" disabled={busy} onClick={fillProfile}>
              <span className="remote-quick-row-icon">{"👤"}</span>
              <span className="remote-quick-row-label">{t("remote.fillProfile")}</span>
            </button>
            <button className="remote-quick-row" disabled={busy} onClick={makePassword}>
              <span className="remote-quick-row-icon">{"🔑"}</span>
              <span className="remote-quick-row-label">{t("remote.fillNewPassword")}</span>
            </button>
            <button className="remote-quick-row" disabled={busy} onClick={saveCurrent}>
              <span className="remote-quick-row-icon">{"💾"}</span>
              <span className="remote-quick-row-label">{t("remote.fillSaveCurrent")}</span>
            </button>
          </>
        )}

        <div className="remote-quick-section">{t("remote.fillLogins")}</div>
        {loading && <div className="vb-fill-empty">…</div>}
        {!loading && logins.length === 0 && <div className="vb-fill-empty">{t("remote.fillNoLogins")}</div>}
        {logins.map((login) => (
          <div key={login.id} className="vb-fill-login">
            <button className="vb-fill-login-use" disabled={busy} onClick={() => void useLogin(login.id)}>
              <span className="vb-fill-login-name">{login.login || login.host}</span>
              <span className="vb-fill-login-host">{login.host}</span>
            </button>
            <button className="vb-fill-login-forget" aria-label={t("remote.fillForget")}
              onClick={() => void forget(login.id)}>×</button>
          </div>
        ))}

        <button className="remote-quick-row" onClick={() => setEditProfile((v) => !v)}>
          <span className="remote-quick-row-icon">{"📇"}</span>
          <span className="remote-quick-row-label">{t("remote.fillProfileEdit")}</span>
          <span className="remote-menu-check">{editProfile ? "▴" : "▾"}</span>
        </button>

        {editProfile && (
          <div className="vb-fill-profile">
            <div className="vb-fill-profile-hint">{t("remote.profileDataHint")}</div>
            {PROFILE_FIELDS.map((field) => (
              <label key={String(field.key)} className="vb-fill-field">
                <span>{t(field.label)}</span>
                <input
                  type={field.type || "text"}
                  value={(profile[field.key] as string) || ""}
                  autoCapitalize="off" spellCheck={false}
                  onChange={(e) => setProfile((p) => ({ ...p, [field.key]: e.target.value }))} />
              </label>
            ))}
            <button className="btn btn-primary btn-sm" disabled={busy} onClick={storeProfile}>
              {t("remote.profileSave")}
            </button>
          </div>
        )}
      </div>
    </div>
  );
}
