import { useCallback, useEffect, useState } from "react";
import { getBootReport, getNetworkStatus, controlVPN } from "../api";
import type { BootReport, NetworkStatus } from "@tgcontrol/shared";
import { t } from "../i18n";
import { haptic } from "../telegram";
import { useToast, mapApiError } from "@tgcontrol/shared";

/**
 * «Что случилось с компьютером» — одна карточка на два вопроса, которые до сих
 * пор оставались без ответа: ПОЧЕМУ его не было и ЧТО с его связью сейчас.
 *
 * Появляется в «Система → Питание», рядом с автозапуском: там же человек
 * разбирается, почему компьютер пропал и как сделать, чтобы возвращался сам.
 *
 * ПРАВИЛО ЭТОЙ КАРТОЧКИ: она молчит, когда сказать нечего. Первый запуск,
 * штатный перезапуск ради обновления и живой VPN при живой связи — не новости;
 * карточка-«мебель», которая всегда на экране, перестаёт читаться ровно к тому
 * моменту, когда в ней появляется важное.
 */
export function HealthCard() {
  const [boot, setBoot] = useState<BootReport | null>(null);
  const [net, setNet] = useState<NetworkStatus | null>(null);
  const [busy, setBusy] = useState(false);
  const [checking, setChecking] = useState(false);
  const { toastSuccess, toastError } = useToast();

  const load = useCallback(async (check = false) => {
    try {
      const [b, n] = await Promise.all([
        getBootReport().catch(() => null),
        getNetworkStatus(check).catch(() => null),
      ]);
      if (b?.available && b.report) setBoot(b.report); else setBoot(null);
      if (n) setNet(n);
    } catch {
      // Компьютер не отвечает — карточке нечего показать, и это не ошибка:
      // выше по экрану уже висит баннер «нет связи».
    }
  }, []);

  useEffect(() => { void load(false); }, [load]);

  const recheck = async () => {
    haptic();
    setChecking(true);
    await load(true);
    setChecking(false);
  };

  const toggleVPN = async (action: "start" | "stop") => {
    haptic();
    setBusy(true);
    try {
      const res = await controlVPN(action);
      setNet(res.status);
      toastSuccess(action === "stop" ? t("health.vpnStopped") : t("health.vpnStarted"));
    } catch (e) {
      toastError(mapApiError(e));
    } finally {
      setBusy(false);
    }
  };

  const vpn = net?.vpn ?? null;
  // Показываем отчёт о перерыве, только если он про НЕОБЫЧНОЕ. Про «первый
  // запуск» и штатный рестарт человеку читать нечего.
  const showBoot = Boolean(boot && boot.kind !== "first_run" && (boot.unexpected || boot.rebooted));
  const showNet = Boolean(vpn || net?.last_action || (net && !net.internet));
  if (!showBoot && !showNet) return null;

  return (
    <div className={`health-card ${boot?.unexpected ? "health-warn" : ""}`}>
      {showBoot && boot && (
        <div className="health-boot">
          <div className="health-boot-title">{boot.title}</div>
          {boot.detail && <div className="health-boot-detail">{boot.detail}</div>}
          {boot.repairs && boot.repairs.length > 0 && (
            <div className="health-boot-detail">{t("health.repaired")}: {boot.repairs.join(", ")}</div>
          )}
        </div>
      )}

      {showNet && net && (
        <div className="health-net">
          <div className="health-net-row">
            <span>{t("health.internet")}</span>
            <strong>{net.internet ? t("health.yes") : t("health.no")}</strong>
          </div>
          {vpn && (
            <div className="health-net-row">
              <span>{vpn.name}</span>
              <strong>{vpn.running ? t("health.vpnRunning") : t("health.vpnStopped2")}</strong>
            </div>
          )}
          {net.last_action?.text && <div className="health-boot-detail">{net.last_action.text}</div>}
          {/* Ряд переносится: кнопка за краем экрана — это кнопка, которой нет
              (правило проекта, выведенное на рядах терминала). */}
          <div className="health-actions">
            <button className="btn btn-sm" onClick={recheck} disabled={checking}>
              {checking ? t("health.checking") : t("health.recheck")}
            </button>
            {vpn && vpn.running && (
              <button className="btn btn-sm" onClick={() => toggleVPN("stop")} disabled={busy}>
                {t("health.vpnOff", { name: vpn.name })}
              </button>
            )}
            {vpn && !vpn.running && (
              <button className="btn btn-sm btn-primary" onClick={() => toggleVPN("start")} disabled={busy}>
                {t("health.vpnOn", { name: vpn.name })}
              </button>
            )}
          </div>
        </div>
      )}
    </div>
  );
}
