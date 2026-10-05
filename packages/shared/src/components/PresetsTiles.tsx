import { useEffect, useState } from "react";
import { useNavigate, useLocation } from "react-router-dom";
import { getPresets, launchPreset, createSession } from "../api-endpoints";
import type { Preset } from "../types";
import { platform } from "../platform";
import { useToast } from "./Toast";
import { t } from "../i18n";
import { mapApiError } from "../api-core";

// Разделы, уже доступные в нижней навигации — url-пресеты на них дублируют
// меню и только шумят на главной.
const NAV_URLS = new Set(["/remote", "/files", "/pty", "/system"]);

interface Props {
  /**
   * Показывать session-пресеты (Claude/Codex…). apk передаёт флаг AI-режима:
   * без него роуты сессий закрыты RequireAI и тап вёл бы в никуда — курс
   * продукта «только терминал». miniapp оставляет дефолт (true).
   */
  showSessionPresets?: boolean;
  /** Заголовок «Быстрый запуск» над плитками (нужен на главной apk). */
  showTitle?: boolean;
  /**
   * ПК не в сети — запускать нечего. Раньше плитки гасил только внешний
   * inert-контейнер: в WebView2 и старых Android-вебвью атрибут не
   * поддерживается, тап проходил насквозь и уводил в пустую шторку выбора
   * папки. Гасим сам <button>, чтобы отказ не зависел от возможностей движка.
   */
  disabled?: boolean;
}

/**
 * Quick-launch tiles. Лучше "ещё одной кнопки" — это плитки конкретных
 * сессий/терминалов/экранов, которые пользователь часто использует.
 *
 * Builtin presets ставятся серверной стороной, чтобы у новичка не было пустого
 * экрана. User-defined пишутся в ~/.tgcontrol-presets.json.
 */
export function PresetsTiles({ showSessionPresets = true, showTitle = false, disabled = false }: Props = {}) {
  const navigate = useNavigate();
  // Откуда человек запустил плитку. Плитки стоят и на главной, и в списке
  // терминалов, а терминал открывается через `/pty?shell=…` — и до 05.09.2026
  // «Назад» из него всегда приводила в список терминалов, даже если человек
  // нажал плитку на ГЛАВНОЙ и в списке ни разу не был (единственная строка
  // «системная „назад“ ведёт не туда» в обходе карты).
  const location = useLocation();
  const { toastError, toastSuccess } = useToast();
  const [items, setItems] = useState<Preset[]>([]);
  const [loading, setLoading] = useState(true);
  const [running, setRunning] = useState<string | null>(null);

  useEffect(() => {
    getPresets()
      .then((r) => setItems(r.presets))
      .catch(() => {/* silent — Dashboard works without presets */})
      .finally(() => setLoading(false));
  }, []);

  const dispatch = async (p: Preset) => {
    // Клавиатура и синтетические клики в обход disabled — второй рубеж.
    if (disabled) return;
    platform().haptic();
    setRunning(p.id);
    try {
      // Bump server-side stats (best-effort).
      launchPreset(p.id).catch(() => {});
      switch (p.kind) {
        case "url":
          if (p.url) navigate(p.url);
          break;
        case "session":
          if (p.agent) {
            const name = `${p.agent}-${Date.now().toString(36)}`;
            await createSession(name, p.agent, p.cwd || "");
            navigate(`/session/${encodeURIComponent(name)}`);
            toastSuccess(t("ui.presetstiles.mbe5292dd43", { p0: (p.agent) }));
          }
          break;
        case "pty":
          // /pty has its own create flow; deep-link with query params.
          const q = new URLSearchParams();
          if (p.shell) q.set("shell", p.shell);
          if (p.cwd) q.set("cwd", p.cwd);
          // Дом для «Назад» называет тот, кто открыл: список терминалов
          // пробросит его дальше в сам терминал (PtyListView.openTerminal).
          if (location.pathname && location.pathname !== "/pty") q.set("from", location.pathname);
          navigate(`/pty?${q.toString()}`);
          break;
      }
    } catch (e: any) {
      toastError(mapApiError(e));
    } finally {
      setRunning(null);
    }
  };

  const visible = items.filter(
    (p) =>
      (showSessionPresets || p.kind !== "session") &&
      !(p.kind === "url" && NAV_URLS.has(p.url || "")),
  );

  if (loading || visible.length === 0) return null;
  return (
    <>
    {showTitle && <div className="home-guide-title">{t("pty.quickLaunch")}</div>}
    <div className="presets-tiles">
      {visible.map((p) => (
        <button
          key={p.id}
          className={`preset-tile ${running === p.id ? "preset-tile-busy" : ""} ${p.pinned ? "preset-tile-pinned" : ""}`}
          onClick={() => dispatch(p)}
          disabled={disabled || running === p.id}
        >
          <span className="preset-tile-icon">{p.icon || "▶"}</span>
          <span className="preset-tile-title">{p.title}</span>
          {p.cwd && <span className="preset-tile-cwd">{shortPath(p.cwd)}</span>}
        </button>
      ))}
    </div>
    </>
  );
}

function shortPath(p: string): string {
  // Show last 2 segments: "...\TGControl-go" rather than full C:\Users\...
  const parts = p.split(/[\\/]/).filter(Boolean);
  if (parts.length <= 2) return p;
  return ".../" + parts.slice(-2).join("/");
}
