import { useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";
import { getServerAccess, serverAccessState, t } from "@tgcontrol/shared";
import type { ServerAccess } from "@tgcontrol/shared";
import { getSelectedDeviceId, getServerUrl, isOnPCPanel } from "../config";

/** Same notice for SSH terminals and files, including the local interface. */
export function ServerAccessNotice() {
  const navigate = useNavigate();
  const [access, setAccess] = useState<ServerAccess | null>(null);
  const [error, setError] = useState<string>();
  const [revision, setRevision] = useState(0);
  const device = getSelectedDeviceId();
  const server = getServerUrl();
  const onPC = isOnPCPanel();
  useEffect(() => {
    let active = true;
    setAccess(null);
    setError(undefined);
    getServerAccess().then((result) => { if (active) setAccess(result); })
      .catch((err: unknown) => {
        if (active) setError((err as { code?: string })?.code || "unavailable");
      });
    return () => { active = false; };
  }, [device, server, revision]);
  useEffect(() => {
    const refresh = () => setRevision((value) => value + 1);
    window.addEventListener("focus", refresh);
    return () => window.removeEventListener("focus", refresh);
  }, []);
  const state = serverAccessState(access, error);
  if (state === "loading") return <p className="settings-advanced-hint" role="status">{t("ssh.access.loading")}</p>;
  if (state === "allowed") return null;
  return (
    <div className="server-access-notice" role="status" style={{ border: "1px solid var(--tg-hint)", borderRadius: 12, padding: 14, marginBottom: 12 }}>
      <strong>{t("ssh.access.title")}</strong>
      <p style={{ margin: "8px 0", lineHeight: 1.5 }}>
        {state === "trial"
          ? t("ssh.access.trial", { days: access?.trial_days ?? 30 })
          : t(`ssh.access.${state}`)}
      </p>
      <p className="settings-advanced-hint" style={{ marginBottom: 12 }}>{t("ssh.access.local")}</p>
      <div style={{ display: "flex", flexWrap: "wrap", gap: 8 }}>
        {state === "account" && <button className="btn btn-primary btn-sm" style={{ minHeight: 44 }} onClick={() => navigate(onPC ? "/panel" : "/cloud-login")}>{t(onPC ? "ssh.access.openPanel" : "ssh.access.connectAccount")}</button>}
        {state !== "unavailable" && <button className="btn btn-secondary btn-sm" style={{ minHeight: 44 }} onClick={() => navigate("/account")}>{t("ssh.access.plans")}</button>}
        {state !== "trial" && <button className="btn btn-secondary btn-sm" style={{ minHeight: 44 }} onClick={() => setRevision((value) => value + 1)}>{t("ssh.access.retry")}</button>}
      </div>
    </div>
  );
}
