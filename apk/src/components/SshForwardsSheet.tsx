// Шторка «Проброс портов»: активные SSH-туннели агента + создание нового.
// Открывается из секции «Серверы» (SshSection). Строки пока хардкодом
// по-русски, как в соседних SSH-компонентах; подписи кнопок подтверждения —
// из общего словаря (confirm.btn.*), они одни на всё приложение.

import { useEffect, useRef, useState } from "react";
import type { KeyboardEvent as ReactKeyboardEvent } from "react";
import {
  getSshForwards, createSshForward, deleteSshForward, deleteSshForwardSpec,
} from "../api";
import type { SshForward, SshForwardInput, SshForwardType, SshHost } from "../api";
import { haptic, hapticSuccess, tgConfirm } from "../telegram";
import { t } from "../i18n";
import { useToast, useEscape, mapApiError, isPcOffline, OfflineState } from "@tgcontrol/shared";
import { getSshPassword, runWithSshTrust, sshErrorText, sshTargetLabel } from "../sshCommon";

const TYPE_META: Record<SshForwardType, { badge: string; title: string; desc: string }> = {
  local: {
    badge: "L",
    get title() { return t("ui.sshforwardssheet.m0a5f4ba135"); },
    get desc() { return t("ui.sshforwardssheet.m7d17f0244c"); },
  },
  remote: {
    badge: "R",
    get title() { return t("ui.sshforwardssheet.mcfa48b420b"); },
    get desc() { return t("ui.sshforwardssheet.m2bcee762ea"); },
  },
  dynamic: {
    badge: "SOCKS",
    get title() { return t("ui.sshforwardssheet.m49c327a3ba"); },
    get desc() { return t("ui.sshforwardssheet.m1f70cf16d3"); },
  },
};

/**
 * Причина падения туннеля приходит от агента сырым английским текстом Go
 * («listen tcp 127.0.0.1:5432: bind: Only one usage of each socket address…»).
 * Весь остальной SSH-модуль такой текст в интерфейс не пускает (sshApiText),
 * и строка проброса — не исключение: частые причины называем по-русски и с
 * действием, остальное уходит в общую фразу.
 */
function forwardErrorText(f: SshForward): string {
  const raw = f.error || "";
  if (/already in use|only one usage of each socket address|eaddrinuse/i.test(raw)) {
    return t("ssh.forward.errPortBusy", { port: f.bind_port });
  }
  if (/permission denied|access is denied|eacces/i.test(raw)) {
    return t("ssh.forward.errPortDenied", { port: f.bind_port });
  }
  if (/administratively prohibited|forwarding not permitted|allowtcpforwarding|open failed/i.test(raw)) {
    return t("ssh.forward.errServerRefused");
  }
  if (/connection refused|econnrefused/i.test(raw)) {
    return t("ssh.forward.errTargetRefused");
  }
  if (/no route to host|timed out|i\/o timeout|timeout/i.test(raw)) {
    return t("ssh.forward.errUnreachable");
  }
  // Русский текст агент иногда пишет сам — его показываем как есть. Чужую
  // латиницу не пускаем: гонять её через mapApiError нельзя, там «connection
  // reset» из туннеля был бы принят за выключенный компьютер.
  if (/[а-яё]/i.test(raw)) return raw;
  return t("ssh.forward.errGeneric");
}

/**
 * Ловушка фокуса шторки — то же правило, что в DialogHost и в соседних шторках
 * продукта (AgentLaunchSheet, «Терминалы»). Без неё Tab из формы проброса
 * уходит в список серверов под затемнением и «нажимает» невидимые кнопки, а
 * скринридер продолжает читать страницу под оверлеем.
 */
function trapTabInSheet(e: ReactKeyboardEvent<HTMLDivElement>) {
  if (e.key !== "Tab") return;
  const items = [...e.currentTarget.querySelectorAll<HTMLElement>(
    'button:not([disabled]), input:not([disabled]), textarea:not([disabled]), select:not([disabled]), a[href], [tabindex]:not([tabindex="-1"])',
  )];
  if (items.length === 0) return;
  const index = items.indexOf(document.activeElement as HTMLElement);
  const next = e.shiftKey
    ? (index <= 0 ? items.length - 1 : index - 1)
    : (index < 0 || index === items.length - 1 ? 0 : index + 1);
  e.preventDefault();
  items[next]?.focus();
}

interface Props {
  open: boolean;
  onClose: () => void;
  hosts: SshHost[];          // сохранённые серверы для выбора (из SshSection)
  presetHost?: SshHost | null; // хост, из меню которого открыли шторку
}

export function SshForwardsSheet({ open, onClose, hosts, presetHost }: Props) {
  useEscape(open, onClose);
  const { toastSuccess, toastError } = useToast();
  const [forwards, setForwards] = useState<SshForward[]>([]);
  const [loading, setLoading] = useState(false);
  const [loadErr, setLoadErr] = useState(false);
  const [offline, setOffline] = useState(false);
  const busyRef = useRef(false);
  const sheetRef = useRef<HTMLDivElement | null>(null);
  const [busy, setBusy] = useState(false);
  // Форма создания.
  const [serverSel, setServerSel] = useState("manual"); // id хоста или "manual"
  const [manualHost, setManualHost] = useState("");
  const [manualPort, setManualPort] = useState("22");
  const [manualUser, setManualUser] = useState("");
  const [password, setPassword] = useState("");
  const [keyPassphrase, setKeyPassphrase] = useState("");
  const [proxyPassword, setProxyPassword] = useState("");
  const [type, setType] = useState<SshForwardType>("local");
  // Пароль ключа и пароль бастиона — вторичные поля, как в форме сервера.
  const [moreAuthOpen, setMoreAuthOpen] = useState(false);
  const [allowLAN, setAllowLAN] = useState(false);
  const [bindPort, setBindPort] = useState("");
  const [targetHost, setTargetHost] = useState("");
  const [targetPort, setTargetPort] = useState("");
  const [savedSpecs, setSavedSpecs] = useState<SshForwardInput[]>(() => {
    try {
      const value = JSON.parse(localStorage.getItem("ssh.forwardSpecs") || "[]");
      return Array.isArray(value) ? value : [];
    } catch {
      return [];
    }
  });

  const refresh = async (silent = false) => {
    if (!silent) setLoading(true);
    try {
      const d = await getSshForwards();
      setForwards(d.forwards || []);
      if (Array.isArray(d.specs)) setSavedSpecs(d.specs);
      setLoadErr(false);
      setOffline(false);
    } catch (e) {
      // Раньше любая ошибка означала «Remotai на компьютере не отвечает —
      // обновите его»: при спящем ПК это отправляло человека проверять
      // обновления вместо того, чтобы включить компьютер. Тот же разбор, что
      // и в списке серверов (SshSection).
      setForwards([]);
      const pcOffline = isPcOffline(e);
      setOffline(pcOffline);
      setLoadErr(!pcOffline);
    }
    if (!silent) setLoading(false);
  };

  useEffect(() => {
    if (!open) return;
    void refresh();
    // Открытие из меню хоста — сразу подставляем его в форму.
    if (presetHost && presetHost.source) {
      setServerSel(presetHost.id);
      setPassword(getSshPassword(presetHost));
    }
  }, [open, presetHost]);

  useEffect(() => {
    if (!open) return;
    const timer = window.setInterval(() => void refresh(true), 5000);
    return () => window.clearInterval(timer);
  }, [open]);

  // Фокус внутрь шторки при открытии и возврат его на кнопку, с которой её
  // позвали: без этого клавиатура остаётся на странице под затемнением.
  useEffect(() => {
    if (!open) return;
    const opener = document.activeElement as HTMLElement | null;
    sheetRef.current?.focus();
    return () => {
      if (opener && typeof opener.focus === "function" && document.contains(opener)) opener.focus();
    };
  }, [open]);

  if (!open) return null;

  const selectedHost = hosts.find((h) => h.id === serverSel) || null;

  // Проброс, который прямо сейчас поднят, восстанавливать нечего — повтор упрётся
  // в «порт занят». Все остальные предлагаем ВСЕГДА: раньше кнопки «Восстановить»
  // прятались, стоило подняться хотя бы одному туннелю, и после перезагрузки ПК
  // поднять оставшиеся два было нечем.
  const restorableSpecs = savedSpecs.filter(
    (spec) => !forwards.some((f) => f.type === spec.type && f.bind_port === spec.bind_port),
  );

  const buildInput = (trustHost: boolean): SshForwardInput | null => {
    const bp = parseInt(bindPort, 10);
    if (!Number.isFinite(bp) || bp <= 0) {
      // «Порт привязки (bind port)» — калька плюс английский оригинал, а полей
      // «Порт» в форме три. Ошибка обязана назвать ТО поле, которое пустует, и
      // сторону, где порт откроется: у «Удалённого» это сам SSH-сервер.
      toastError(type === "remote" ? t("ssh.forward.needBindPortRemote") : t("ssh.forward.needBindPort"));
      return null;
    }
    const base: SshForwardInput = {
      host: "", user: "",
      type,
      bind_addr: allowLAN ? "0.0.0.0" : "127.0.0.1",
      bind_port: bp,
      allow_lan: allowLAN || undefined,
      trust_host: trustHost || undefined,
    };
    if (type !== "dynamic") {
      const tp = parseInt(targetPort, 10);
      if (!targetHost.trim() || !Number.isFinite(tp) || tp <= 0) {
        toastError(t("ssh.forward.needTarget"));
        return null;
      }
      base.target_host = targetHost.trim();
      base.target_port = tp;
    }
    if (selectedHost) {
      base.host = selectedHost.host;
      base.host_id = selectedHost.id;
      base.port = selectedHost.port || 22;
      base.user = selectedHost.user;
      base.identity_file = selectedHost.identity_file || undefined;
      base.proxy_jump = selectedHost.proxy_jump || undefined;
      base.password = password || getSshPassword(selectedHost) || undefined;
      base.key_passphrase = keyPassphrase || undefined;
      base.proxy_password = proxyPassword || undefined;
    } else {
      if (!manualHost.trim() || !manualUser.trim()) {
        toastError(t("ssh.form.needHostUser"));
        return null;
      }
      const mp = parseInt(manualPort, 10);
      base.host = manualHost.trim();
      base.port = Number.isFinite(mp) && mp > 0 ? mp : 22;
      base.user = manualUser.trim();
      base.password = password || undefined;
      base.key_passphrase = keyPassphrase || undefined;
      base.proxy_password = proxyPassword || undefined;
    }
    return base;
  };

  const submit = async (trustHost: boolean) => {
    const input = buildInput(trustHost);
    if (!input) return;
    await createSshForward(input);
    const safeSpec: SshForwardInput = {
      ...input,
      password: undefined,
      key_passphrase: undefined,
      proxy_password: undefined,
      trust_host: undefined,
    };
    const signature = JSON.stringify(safeSpec);
    const nextSpecs = [safeSpec, ...savedSpecs.filter((x) => JSON.stringify(x) !== signature)].slice(0, 20);
    setSavedSpecs(nextSpecs);
    localStorage.setItem("ssh.forwardSpecs", JSON.stringify(nextSpecs));
    hapticSuccess();
    toastSuccess(t("ui.sshforwardssheet.m3b765c1a5a"));
    setBindPort("");
    setTargetHost("");
    setTargetPort("");
    await refresh();
  };

  const restoreSpec = (spec: SshForwardInput) => {
    setServerSel(spec.host_id || "manual");
    setManualHost(spec.host || "");
    setManualPort(String(spec.port || 22));
    setManualUser(spec.user || "");
    setType(spec.type);
    setAllowLAN(!!spec.allow_lan);
    setBindPort(String(spec.bind_port || ""));
    setTargetHost(spec.target_host || "");
    setTargetPort(spec.target_port ? String(spec.target_port) : "");
    toastSuccess(t("ui.sshforwardssheet.m2e7e80fcab"));
  };

  const handleCreate = async () => {
    if (busyRef.current) return;
    busyRef.current = true;
    setBusy(true);
    try {
      const target = selectedHost || {
        host: manualHost.trim(), port: parseInt(manualPort, 10) || 22, user: manualUser.trim(),
      };
      await runWithSshTrust(target, submit);
    } catch (e: any) {
      toastError(sshErrorText(e));
    } finally {
      busyRef.current = false;
      setBusy(false);
    }
  };

  const handleDelete = async (f: SshForward) => {
    if (!(await tgConfirm(t("ui.sshforwardssheet.mce6cc6969d", { p0: (f.bind_addr), p1: (f.bind_port) }), { danger: true, confirmText: t("confirm.btn.delete") }))) return;
    try {
      await deleteSshForward(f.id);
      haptic("medium");
      setForwards((prev) => prev.filter((x) => x.id !== f.id));
    } catch (e: any) {
      toastError(mapApiError(e));
    }
  };

  return (
    <div className="modal-overlay" onClick={(e) => { if (e.target === e.currentTarget) onClose(); }}>
      {/* Шторка объявлена диалогом (role + aria-modal + ловушка фокуса), как
          DialogHost и «Инфраструктура». Esc и системная «Назад» уже закрывают
          её через useEscape выше. */}
      <div
        ref={sheetRef}
        className="modal-sheet ssh-sheet ssh-fwd-sheet"
        role="dialog"
        aria-modal="true"
        aria-labelledby="ssh-fwd-title"
        tabIndex={-1}
        onKeyDown={trapTabInSheet}
      >
        <div className="help-sheet-header">
          <div className="modal-title" id="ssh-fwd-title">{t("ssh.forwards")}</div>
          <button className="icon-btn" onClick={onClose} aria-label={t("pty.searchClose")}>{"✕"}</button>
        </div>

        {/* Активные пробросы */}
        {loading ? (
          <div className="loading-center"><div className="spinner" /></div>
        ) : offline ? (
          <OfflineState compact onRetry={() => { haptic(); void refresh(); }} />
        ) : loadErr ? (
          <div className="ssh-empty">{t("ssh.forward.agentUnavailable")}</div>
        ) : forwards.length === 0 ? (
          <div className="ssh-empty">{t("ui.sshforwardssheet.m652d02b6c4")}</div>
        ) : (
          <div className="ssh-fwd-list">
            {forwards.map((f) => (
              <div key={f.id} className="ssh-fwd-item">
                <span className={`ssh-fwd-badge ${f.type}`}>{TYPE_META[f.type]?.badge || f.type}</span>
                <div className="ssh-fwd-info">
                  <div className="ssh-fwd-route">
                    {f.bind_addr}:{f.bind_port}
                    {f.type === "dynamic"
                      ? " → SOCKS"
                      : ` → ${f.target_host || "?"}:${f.target_port || "?"}`}
                  </div>
                  <div className="ssh-fwd-sub">
                    {f.server}
                    {f.status === "error" ? "" : t("ui.sshforwardssheet.m2886c10373")}
                  </div>
                  {f.status === "error" && (
                    <div className="ssh-fwd-error">{forwardErrorText(f)}</div>
                  )}
                  {f.access && (
                    <div className="ssh-fwd-access">
                      {t("ui.sshforwardssheet.m5e509bd1c5")}{f.access}
                      <button className="ssh-inline-action" onClick={() => {
                        void navigator.clipboard?.writeText(f.access);
                        toastSuccess(t("ui.sshforwardssheet.m314016f996"));
                      }}>{t("pty.copy")}</button>
                      {f.type !== "dynamic" && (
                        <button className="ssh-inline-action" onClick={() => {
                          window.open(`http://${f.access}`, "_blank", "noopener");
                        }}>{t("remote.browserOpen")}</button>
                      )}
                    </div>
                  )}
                </div>
                <button className="ssh-icon-btn" title={t("mcp.delete")} aria-label={t("mcp.delete")}
                  onClick={() => void handleDelete(f)}>{"✕"}</button>
              </div>
            ))}
          </div>
        )}

        {/* Форма создания */}
        <div className="ssh-fwd-form-title">{t("ui.sshforwardssheet.m0c528f0238")}</div>
        {restorableSpecs.length > 0 && (
          <div className="ssh-forward-restore">
            <div className="ssh-sheet-hint">{t("ssh.forward.restoreHint")}</div>
            {restorableSpecs.slice(0, 5).map((spec, index) => (
              <span key={(spec as any).id || index} className="ssh-forward-restore-row">
                {/* LOCAL/REMOTE/DYNAMIC — имена из ssh(1), а не из этой формы:
                    в чипах выше тот же проброс называется «Локальный». */}
                <button className="btn btn-secondary" onClick={() => restoreSpec(spec)}>
                  {t("ui.sshforwardssheet.mc195495cf2")}{TYPE_META[spec.type]?.title || spec.type} · {spec.bind_port}
                </button>
                {(spec as any).id && (
                  <button className="ssh-icon-btn" title={t("ssh.forward.forgetSpec")} aria-label={t("ssh.forward.forgetSpec")}
                    onClick={async () => {
                      await deleteSshForwardSpec((spec as any).id);
                      void refresh(true);
                    }}>{"✕"}</button>
                )}
              </span>
            ))}
          </div>
        )}
        <div className="ssh-type-chips">
          {(Object.keys(TYPE_META) as SshForwardType[]).map((tp) => (
            <button
              key={tp}
              className={`ssh-type-chip${type === tp ? " active" : ""}`}
              onClick={() => { haptic(); setType(tp); }}
            >
              {TYPE_META[tp].title}
            </button>
          ))}
        </div>
        <div className="ssh-type-desc">{TYPE_META[type].desc}</div>

        <select
          className="modal-input ssh-select"
          value={serverSel}
          onChange={(e) => {
            setServerSel(e.target.value);
            const h = hosts.find((x) => x.id === e.target.value);
            setPassword(h ? getSshPassword(h) : "");
          }}
        >
          <option value="manual">{t("ui.sshforwardssheet.m15524a0a1e")}</option>
          {hosts.map((h) => (
            <option key={h.id} value={h.id}>
              {h.name ? `${h.name} (${sshTargetLabel(h)})` : sshTargetLabel(h)}
            </option>
          ))}
        </select>

        {!selectedHost && (
          <>
            {/* Те же слова, что в карточке сервера: «Хост» против «Адрес
                сервера» заставляли гадать, одно это поле или разные. */}
            <input className="modal-input" placeholder={t("ssh.form.host")}
              value={manualHost} onChange={(e) => setManualHost(e.target.value)}
              autoCapitalize="off" autoCorrect="off" />
            <div className="ssh-sheet-row">
              <label className="ssh-form-field ssh-sheet-port-field">
                <span>{t("ssh.forward.serverPort")}</span>
                <input className="modal-input ssh-sheet-port" inputMode="numeric"
                  value={manualPort} onChange={(e) => setManualPort(e.target.value)} />
              </label>
              <input className="modal-input" placeholder={t("ssh.form.user")}
                value={manualUser} onChange={(e) => setManualUser(e.target.value)}
                autoCapitalize="off" autoCorrect="off" />
            </div>
          </>
        )}
        <label className="ssh-form-field">
          <span>{t("ssh.form.password")}</span>
          <input className="modal-input" type="password" placeholder={t("ssh.form.password")}
            value={password} onChange={(e) => setPassword(e.target.value)} />
        </label>
        <div className="ssh-sheet-hint">{t("ssh.form.passwordHint")}</div>
        {/* Три видимых поля «пароль» подряд читались как «введи пароль трижды»:
            в форме сервера это уже вылечено раскрывашкой — здесь тот же приём и
            те же слова. */}
        <button className="ssh-manual-toggle" onClick={() => { haptic(); setMoreAuthOpen(!moreAuthOpen); }}>
          {moreAuthOpen ? `▾ ${t("ssh.form.moreAuthHide")}` : `▸ ${t("ssh.form.moreAuthShow")}`}
        </button>
        {moreAuthOpen && (
          <>
            <label className="ssh-form-field">
              <span>{t("ssh.form.keyPass")}</span>
              <input className="modal-input" type="password" placeholder={t("ssh.form.keyPass")}
                value={keyPassphrase} onChange={(e) => setKeyPassphrase(e.target.value)} />
            </label>
            <label className="ssh-form-field">
              <span>{t("ssh.form.jumpPass")}</span>
              <input className="modal-input" type="password" placeholder={t("ssh.form.jumpPass")}
                value={proxyPassword} onChange={(e) => setProxyPassword(e.target.value)} />
            </label>
          </>
        )}

        {/* У «Удалённого» проброса порт слушает САМ SSH-сервер
            (ssh_forward.go: client.Listen), а не компьютер с Remotai. Общий
            тумблер врал в обе стороны: «Только на этом ПК» = 127.0.0.1 на
            сервере (коллега не подключится, а проброс числится активным),
            «Другим устройствам в сети» = 0.0.0.0 на публичном сервере. */}
        <div className="ssh-sheet-row">
          <label className="settings-toggle-row ssh-bind-toggle">
            <span>{type === "remote"
              ? (allowLAN ? t("ssh.forward.bindRemoteOn") : t("ssh.forward.bindRemoteOff"))
              : (allowLAN ? t("ssh.forward.bindLanOn") : t("ssh.forward.bindLanOff"))}</span>
            <input type="checkbox" checked={allowLAN} onChange={(e) => setAllowLAN(e.target.checked)} />
          </label>
          {/* Полей «Порт» в форме три (SSH-сервера, свой, целевой) — без подписи
              человек не знал, в каком из них стоит. */}
          <label className="ssh-form-field ssh-sheet-port-field">
            <span>{type === "remote" ? t("ssh.forward.bindPortRemote") : t("ssh.forward.bindPortLocal")}</span>
            <input className="modal-input ssh-sheet-port" inputMode="numeric"
              value={bindPort} onChange={(e) => setBindPort(e.target.value)} />
          </label>
        </div>
        <div className="ssh-sheet-hint">
          {type === "remote" ? t("ssh.forward.bindHintRemote") : t("ssh.forward.bindHint")}
        </div>
        {allowLAN && (
          <div className="ssh-bind-warning">
            {type === "remote"
              ? t("ssh.forward.warnRemote")
              : type === "dynamic"
              ? t("ssh.forward.warnSocks")
              : t("ssh.forward.warnLocal")}
          </div>
        )}
        {type !== "dynamic" && (
          <>
            <div className="ssh-sheet-row">
              <input className="modal-input" placeholder={t("ssh.forward.targetHost")}
                value={targetHost} onChange={(e) => setTargetHost(e.target.value)}
                autoCapitalize="off" autoCorrect="off" />
              <label className="ssh-form-field ssh-sheet-port-field">
                <span>{t("ssh.forward.targetPort")}</span>
                <input className="modal-input ssh-sheet-port" inputMode="numeric"
                  value={targetPort} onChange={(e) => setTargetPort(e.target.value)} />
              </label>
            </div>
            <div className="ssh-sheet-hint">{t("ssh.forward.targetHint")}</div>
          </>
        )}

        {/* Проброс поднимает сам ПК: при выключенном компьютере кнопка не
            должна обещать невозможное — раньше она возвращала «agent offline». */}
        {offline && <div className="ssh-sheet-hint">{t("ssh.forward.offlineHint")}</div>}
        {/* Про смертность туннеля надо знать ДО того, как его создали: раньше
            эта строка появлялась, только когда активных пробросов ноль, то есть
            никогда в момент создания первого. */}
        <div className="ssh-sheet-hint">{t("ssh.forward.notPersistent")}</div>
        <button className="btn btn-primary ssh-sheet-submit" disabled={busy || offline}
          onClick={() => void handleCreate()}>
          {busy ? t("ui.sshforwardssheet.m5e17213144") : t("ui.sshforwardssheet.m5ac75e6894")}
        </button>
      </div>
    </div>
  );
}
