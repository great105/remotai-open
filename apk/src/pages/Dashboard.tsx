import { useEffect, useState, useCallback, useRef } from "react";
import { useNavigate } from "react-router-dom";
import { useEscape, mapApiError, formatAgoValue } from "@tgcontrol/shared";
import { getSessions, createSession, deleteSession, switchSession, getAgents, getConfig, discoverSessions, importDiscoveredSession, cloneSession, clearSession, sendPrompt, getTemplates, applyTemplate, createTemplate, deleteTemplate, multiSend, listFiles, getQuickPaths, getBookmarks, getRecentFolders, takeScreenshot, onConnectionChange } from "../api";
import type { RecentFolder } from "../api";
import type { SessionTemplate } from "../api";
import { onWSEvent } from "../api";
import { haptic, hapticSuccess, hapticError, tgConfirm } from "../telegram";
import { saveBlob, isShareCancel } from "../saveFile";
import { useToast } from "@tgcontrol/shared";
import { t } from "../i18n";
import type { Session, AgentInfo, DiscoveredSession, QuickPath, Bookmark, FileItem } from "../types";
import { SessionCard } from "@tgcontrol/shared";
import { BottomNav } from "../components/BottomNav";
// Режим подключения решает, показывать ли на главной вход в аккаунт: в
// локальном режиме это единственная дверь к управлению откуда угодно.
import { getMode, isOnPCPanel } from "../config";
import { lastKnownPhonePaired } from "../homeProgress";
import { ReadyCard } from "../components/ReadyCard";
import { HomeActivity } from "../components/HomeActivity";
import { TrialBanner } from "../components/TrialBanner";
import { AgentQuotaChips } from "../components/AgentQuotaChips";
import { useAIUsage } from "../hooks/useAIUsage";
import { DeviceSwitcher } from "../components/DeviceSwitcher";
import { FirstSteps } from "../components/FirstSteps";
import { useHomeProgress, firstStepsGoesOnTop } from "../homeProgress";
import { AppUpdateBanner } from "../components/AppUpdateBanner";
import { InstallPwaBanner } from "../components/InstallPwaBanner";
import { IconGear } from "../components/icons";
import { PresetsTiles } from "@tgcontrol/shared";
import { useFeatures } from "../hooks/useFeatures";
import { useCapabilities } from "../hooks/useCapabilities";
import { HelpSheet } from "../components/HelpSheet";
import { DeviceChip } from "../components/DeviceChip";

function SkeletonCard() {
  return (
    <div className="card skeleton-card" aria-hidden="true">
      <div className="card-top">
        <div className="skeleton-dot" />
        <div className="skeleton-line" style={{ width: "40%" }} />
      </div>
      <div className="skeleton-line" style={{ width: "60%", marginTop: 8 }} />
      <div className="skeleton-line" style={{ width: "50%", marginTop: 6, height: 10 }} />
    </div>
  );
}

/** data:URL скриншота → Blob для saveBlob (шаринг/сохранение на телефон). */
function dataUrlToBlob(dataUrl: string): Blob {
  const [head, b64] = dataUrl.split(",");
  const mime = head.match(/data:(.*?);base64/)?.[1] || "image/png";
  const bin = atob(b64);
  const bytes = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
  return new Blob([bytes], { type: mime });
}

export function Dashboard() {
  return <SimpleHome />;
}

/** Legacy orchestrated sessions stay reachable without replacing the product home. */
export function SessionsDashboard() {
  return <AIDashboard />;
}

/** Home shown when AI features are disabled (default state).
 *
 *  Порядок блоков = порядок вопросов, с которыми открывают приложение:
 *  «жив ли ПК» (ReadyCard + плитки соседних машин) → «что там происходит и что
 *  от меня хотят» (HomeActivity: ждёт ввода / работает / продолжить) →
 *  «запустить» (агент — главным действием, терминал рядом вторым) → «что мне
 *  ещё освоить» (FirstSteps, исчезает после освоения).
 *
 *  Каталога разделов на главной больше НЕТ. Блок «Возможности» (HomeGuide) жил
 *  здесь, пока «Мои компьютеры», SSH, «Система» и «Агенты» не помещались в
 *  навигацию: на телефоне их держала только шторка «Ещё», а в сайдбаре ≥768px
 *  «Агентов» и «Настроек» не было вовсе. Теперь все они стоят и в шторке, и в
 *  сайдбаре (BottomNav: MORE_ITEMS и slots с wideOnly), то есть каталог на
 *  главной был вторым списком тех же мест — а два описания одного экрана
 *  человек читает как два разных экрана.
 *
 *  Обучающая поверхность на главной ОДНА — чеклист «Первые шаги», и он сам
 *  исчезает, когда шаги пройдены. Строка-дверь в гид отсюда тоже убрана: гид
 *  стал таким же разделом навигации («Справка» в сайдбаре, «Как работать с
 *  системой» в шторке), и его строка на главной была четвёртым подряд
 *  предложением поучиться. */
function SimpleHome() {
  const navigate = useNavigate();
  const { ai } = useFeatures();
  // Тост — единственный ответ на нажатие кнопки запуска у выключенного ПК
  // (см. комментарий у .home-new-terminal ниже).
  const { toast } = useToast();
  // «ПК не в сети» узнаём из двух источников: кадр agent_status живого канала
  // (облако — мгновенно, той же правдой живут баннер связи и точка в чипе) и
  // провал health в карточке готовности (LAN и окно exe, где причина обрыва не
  // называется). Признак гасит запуск терминала, плитки и шаги: с выключенным
  // компьютером они вели в пустую шторку выбора папки.
  const [linkOffline, setLinkOffline] = useState(false);
  const [healthOffline, setHealthOffline] = useState(false);
  const pcOffline = linkOffline || healthOffline;
  /**
   * Чеклист «Первые шаги» встаёт НАВЕРХ, пока человек только начинает.
   *
   * Замысел порядка блоков (см. шапку файла) — «жив ли ПК → что происходит →
   * запустить → что ещё освоить», и для того, кто уже работает, он верный:
   * чеклист внизу не мешает. Но открывшему приложение ВПЕРВЫЕ он и есть первый
   * вопрос, а стоял на 745-й строке пикселей телефона 390×844 — за нижней
   * панелью. Обучение, которого не видно, обучением не является.
   *
   * Порог — «сделан максимум один шаг»: два из трёх означают, что человек уже
   * освоился сам и подсказка ему только занимает первый экран. Скрытый вручную
   * чеклист наверх не всплывает: `FirstSteps` уважает «Скрыть» и вернёт null.
   */
  const { done: stepsDone, dismissed: stepsDismissed } = useHomeProgress();
  const firstStepsOnTop = firstStepsGoesOnTop(stepsDone, stepsDismissed);
  // Объяснение локального режима свёрнуто по умолчанию: на первом экране оно
  // было четвёртой обучающей поверхностью подряд. Состояние в памяти экрана, а
  // не в localStorage: это разовое любопытство, а не настройка.
  const [signinOpen, setSigninOpen] = useState(false);
  useEffect(() => onConnectionChange((state) => {
    if (state.reason === "pc_offline") { setLinkOffline(true); return; }
    if (state.connected) setLinkOffline(false);
  }), []);
  return (
    <div className="page">
      <div className="page-header">
        <div className="page-header-context"><h1>{t("dash.title")}</h1></div>
        <button className="header-action" onClick={() => navigate("/settings")} aria-label={t("settings.title")}>
          <IconGear size={20} />
        </button>
      </div>
      <div className="page-content home-page-content">
        <AppUpdateBanner />
        {/* Живёт над рабочей зоной рядом с плашкой обновления; на iPhone они
            одновременно не появляются — APK там не бывает. */}
        <InstallPwaBanner />
        <div className="home-workspace">
          <section className="home-machine">
            <ReadyCard
              onOpenSettings={() => navigate("/settings")}
              onOpenPower={() => navigate("/system?tab=power")}
              onOfflineChange={setHealthOffline}
            />
            <DeviceSwitcher />
          </section>
          <section className="home-work">
            {/* Кончившаяся проба — единственное, что важнее первых шагов: без
                неё удалённый доступ уже не работает, и ход к оплате обязан быть
                под пальцем, а не под нижней панелью (probe-trial-over-cta). */}
            <TrialBanner place="top" />
            {firstStepsOnTop && <FirstSteps pcOffline={pcOffline} />}
            <HomeActivity />
            {/* Действия стоят перед загружаемыми пресетами, чтобы не уехать из-под пальца. */}
            <button className={pcOffline ? "home-new-agent home-offline-gate" : "home-new-agent"}
              aria-disabled={pcOffline}
              onClick={() => {
                if (pcOffline) { hapticError(); toast(t("home.offlineDisabled"), "info"); return; }
                haptic(); navigate("/pty?new=1&agent=1");
              }}>
              {t("home.newAgent")}
            </button>
            <button className={pcOffline ? "home-new-terminal home-offline-gate" : "home-new-terminal"}
              aria-disabled={pcOffline}
              onClick={() => {
                if (pcOffline) { hapticError(); toast(t("home.offlineDisabled"), "info"); return; }
                haptic(); navigate("/pty?new=1");
              }}>
              {"+"} {t("home.newTerminal")}
            </button>
            <div className={pcOffline ? "home-offline-gate" : undefined} inert={pcOffline}>
              <PresetsTiles showTitle showSessionPresets={ai} />
            </div>
          </section>
          <section className="home-secondary">
            <TrialBanner />
            {getMode() !== "cloud" && !(isOnPCPanel() && lastKnownPhonePaired() === true) && (
              <div className="home-signin-card">
                <button className="home-signin-toggle" aria-expanded={signinOpen}
                  onClick={() => { haptic(); setSigninOpen(!signinOpen); }}>
                  <b>{t("home.signin.title")}</b>
                  <span className="home-signin-more">{t("home.signin.more")}</span>
                  <span className="home-guide-chevron" aria-hidden>{"›"}</span>
                </button>
                {signinOpen && <p className="home-signin-desc">{t("home.signin.desc")}</p>}
                <button className="btn btn-primary" onClick={() => { haptic(); navigate("/cloud-login"); }}>
                  {t("home.signin.btn")}
                </button>
              </div>
            )}
            {!firstStepsOnTop && <FirstSteps pcOffline={pcOffline} />}
            {isOnPCPanel() && <p className="home-close-hint">{t("home.closeWindowHint")}</p>}
          </section>
        </div>
      </div>
      <BottomNav active="home" />
    </div>
  );
}

function AIDashboard() {
  const navigate = useNavigate();
  const { toastSuccess, toastError } = useToast();
  const [sessions, setSessions] = useState<Session[]>([]);
  const [active, setActive] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [showNew, setShowNew] = useState(false);
  const [newName, setNewName] = useState("");
  const [newAgent, setNewAgent] = useState("claude");
  const [newCwd, setNewCwd] = useState("");
  const [agents, setAgents] = useState<AgentInfo[]>([]);
  useAIUsage();
  // Discover
  const [showDiscover, setShowDiscover] = useState(false);
  const [discovered, setDiscovered] = useState<DiscoveredSession[]>([]);
  const [discovering, setDiscovering] = useState(false);
  const [importing, setImporting] = useState<string | null>(null);
  // Templates
  const [templates, setTemplates] = useState<SessionTemplate[]>([]);
  // Multi-send
  const [showMulti, setShowMulti] = useState(false);
  const [multiTargets, setMultiTargets] = useState<string[]>([]);
  const [multiPrompt, setMultiPrompt] = useState("");
  // Live progress per session (orchestrator etc)
  const [progressMap, setProgressMap] = useState<Record<string, string>>({});
  // Folder browser in create modal
  const [showFolderBrowser, setShowFolderBrowser] = useState(false);
  const [folderPath, setFolderPath] = useState("");
  const [folderItems, setFolderItems] = useState<FileItem[]>([]);
  const [folderLoading, setFolderLoading] = useState(false);
  const [quickPaths, setQuickPaths] = useState<QuickPath[]>([]);
  const [bookmarks, setBookmarks] = useState<Bookmark[]>([]);
  // Search
  const [search, setSearch] = useState("");
  // Screenshot
  const [screenshotLoading, setScreenshotLoading] = useState(false);
  const [screenshotData, setScreenshotData] = useState<string | null>(null);
  // Server capabilities — hide Remote shortcut on a headless server (no display).
  const { remoteDesktop } = useCapabilities();
  // FAB
  const [fabOpen, setFabOpen] = useState(false);
  const [showHelp, setShowHelp] = useState(false);

  const [recentFolders, setRecentFolders] = useState<RecentFolder[]>([]);

  // Folder browser helpers
  const openFolderBrowser = async () => {
    setShowFolderBrowser(true);
    if (quickPaths.length === 0) {
      getQuickPaths().then((d) => setQuickPaths(d.paths)).catch(() => {});
      getBookmarks().then((d) => setBookmarks(d.bookmarks)).catch(() => {});
      getRecentFolders().then((d) => setRecentFolders(d.folders || [])).catch(() => {});
    }
  };

  const browseTo = async (path: string) => {
    setFolderLoading(true);
    try {
      const data = await listFiles(path);
      setFolderPath(data.path);
      setFolderItems(data.items.filter((f) => f.is_dir));
    } catch { /* ignore */ }
    setFolderLoading(false);
  };

  const selectFolder = (path: string) => {
    setNewCwd(path);
    setShowFolderBrowser(false);
    haptic();
  };

  // Auto-generate session name
  const suggestName = (agent: string) => {
    const now = new Date();
    const hm = `${String(now.getHours()).padStart(2, "0")}${String(now.getMinutes()).padStart(2, "0")}`;
    return `${agent}-${hm}`;
  };
  // Pull-to-refresh
  const [refreshing, setRefreshing] = useState(false);
  const pullRef = useRef<{ startY: number; pulling: boolean }>({ startY: 0, pulling: false });
  const pullIndicatorRef = useRef<HTMLDivElement>(null);
  const contentRef = useRef<HTMLDivElement>(null);

  const refresh = useCallback(async () => {
    try {
      const data = await getSessions();
      setSessions(data.sessions);
      setActive(data.active);
    } catch { /* ignore */ }
    setLoading(false);
  }, []);

  useEffect(() => {
    refresh();
    getAgents().then((data) => setAgents(data.agents)).catch(() => {});
    getTemplates().then((d) => setTemplates(d.templates)).catch(() => {});
    getConfig().then((cfg) => {
      if (cfg.default_agent) setNewAgent(cfg.default_agent);
      if (cfg.default_cwd) setNewCwd(cfg.default_cwd);
    }).catch(() => {});
    const unsub = onWSEvent((ev) => {
      if (ev.type === "sessions_updated" || ev.type === "status") refresh();
      if (ev.type === "progress") {
        setProgressMap((prev) => ({ ...prev, [ev.session]: ev.text }));
      }
      // Clear progress when session finishes
      if (ev.type === "status" && !ev.is_busy) {
        setProgressMap((prev) => { const n = { ...prev }; delete n[ev.session]; return n; });
      }
    });
    return unsub;
  }, [refresh]);

  // Pull-to-refresh handlers
  const onTouchStart = (e: React.TouchEvent) => {
    const el = contentRef.current;
    if (!el || el.scrollTop > 5) return;
    pullRef.current = { startY: e.touches[0].clientY, pulling: true };
  };
  const onTouchMove = (e: React.TouchEvent) => {
    const { startY, pulling } = pullRef.current;
    if (!pulling) return;
    const dy = Math.max(0, e.touches[0].clientY - startY);
    const clamped = Math.min(dy * 0.5, 60);
    if (pullIndicatorRef.current) {
      pullIndicatorRef.current.style.height = `${clamped}px`;
      pullIndicatorRef.current.style.opacity = `${Math.min(clamped / 40, 1)}`;
    }
  };
  const onTouchEnd = async () => {
    const { pulling } = pullRef.current;
    if (!pulling) return;
    pullRef.current.pulling = false;
    const h = parseFloat(pullIndicatorRef.current?.style.height || "0");
    if (h >= 40 && !refreshing) {
      setRefreshing(true);
      haptic();
      await refresh();
      setRefreshing(false);
    }
    if (pullIndicatorRef.current) {
      pullIndicatorRef.current.style.height = "0px";
      pullIndicatorRef.current.style.opacity = "0";
    }
  };

  const handleScreenshot = async () => {
    setScreenshotLoading(true);
    haptic();
    try {
      const data = await takeScreenshot();
      setScreenshotData(`data:${data.mime};base64,${data.data}`);
      toastSuccess(t("toast.screenshotCaptured"));
    } catch (e: any) {
      toastError(mapApiError(e));
    }
    setScreenshotLoading(false);
  };

  const handleShareScreenshot = async () => {
    if (!screenshotData) return;
    haptic();
    try {
      await saveBlob(dataUrlToBlob(screenshotData), `screenshot-${Date.now()}.png`);
      hapticSuccess();
    } catch (e: any) {
      if (!isShareCancel(e)) toastError(mapApiError(e));
    }
  };

  const handleCreate = async () => {
    if (!newName.trim()) return;
    try {
      await createSession(newName.trim(), newAgent, newCwd || ".");
      hapticSuccess();
      toastSuccess(t("toast.sessionCreated", { name: newName.trim() }));
      setShowNew(false);
      setNewName("");
      refresh();
    } catch (e: any) {
      toastError(mapApiError(e));
    }
  };

  const handleDelete = async (name: string): Promise<boolean> => {
    if (!(await tgConfirm(t("confirm.closeSession", { name }), { danger: true, confirmText: t("confirm.btn.closeSession") }))) return false;
    try {
      await deleteSession(name);
      haptic("medium");
      toastSuccess(t("toast.sessionClosed"));
      refresh();
      return true;
    } catch (e: any) {
      toastError(mapApiError(e));
      return false;
    }
  };

  const handleSwitch = async (name: string) => {
    try {
      await switchSession(name);
      haptic();
      refresh();
    } catch { /* ignore */ }
  };

  const handleDiscover = async () => {
    setShowDiscover(true);
    setDiscovering(true);
    try {
      const data = await discoverSessions();
      setDiscovered(data.sessions);
      hapticSuccess();
    } catch (e: any) {
      // Сетевая ошибка — не «пусто»: показываем причину, иначе пользователь
      // видит ложное «сессий не найдено».
      hapticError();
      toastError(mapApiError(e));
    }
    setDiscovering(false);
  };

  const handleImport = async (ds: DiscoveredSession) => {
    setImporting(ds.session_id);
    try {
      await importDiscoveredSession(ds);
      hapticSuccess();
      toastSuccess(t("toast.imported", { name: ds.name }));
      setDiscovered((prev) =>
        prev.map((d) => (d.session_id === ds.session_id ? { ...d, imported: true } : d))
      );
      refresh();
    } catch (e: any) {
      toastError(mapApiError(e));
    }
    setImporting(null);
  };

  const handleImportAll = async () => {
    const toImport = discovered.filter((d) => !d.imported);
    if (toImport.length === 0) return;
    const okIds = new Set<string>();
    let failed = 0;
    for (const ds of toImport) {
      try {
        await importDiscoveredSession(ds);
        okIds.add(ds.session_id);
      } catch { failed++; }
    }
    setDiscovered((prev) => prev.map((d) => okIds.has(d.session_id) ? { ...d, imported: true } : d));
    if (failed === 0) {
      hapticSuccess();
      toastSuccess(t("toast.importedAll", { n: okIds.size }));
    } else {
      hapticError();
      toastError(t("toast.importedPartial", { ok: okIds.size, failed }));
    }
    refresh();
  };

  const shortPath = (p: string) => {
    const parts = p.replace(/\\/g, "/").split("/");
    if (parts.length <= 3) return p;
    return ".../" + parts.slice(-2).join("/");
  };

  // Возраст обнаруженной сессии — общим хелпером (@tgcontrol/shared): своя
  // копия писала латинские «12m / 3h / 2d» там, где остальное приложение для
  // той же величины говорит «12м / 3ч / 2д».
  const timeAgo = (ms: number) => formatAgoValue(ms);

  const handleClone = async (name: string) => {
    try {
      await cloneSession(name);
      hapticSuccess();
      toastSuccess(t("toast.cloned", { name }));
      refresh();
    } catch (e: any) { toastError(mapApiError(e)); }
  };

  const handleClear = async (name: string) => {
    if (!(await tgConfirm(t("confirm.clearHistory", { name }), { danger: true, confirmText: t("confirm.btn.clearHistory") }))) return;
    try {
      await clearSession(name);
      hapticSuccess();
      toastSuccess(t("toast.historyClosed"));
      refresh();
    } catch (e: any) { toastError(mapApiError(e)); }
  };

  const handleContinue = async (name: string) => {
    try {
      await sendPrompt(name, "continue");
      haptic();
    } catch (e: any) { toastError(mapApiError(e)); }
  };

  const handleApplyTemplate = async (tmpl: SessionTemplate) => {
    try {
      await applyTemplate(tmpl.name);
      hapticSuccess();
      toastSuccess(t("toast.templateApplied", { name: tmpl.name }));
      refresh();
    } catch (e: any) { toastError(mapApiError(e)); }
  };

  const handleSaveTemplate = async (s: Session) => {
    try {
      await createTemplate({
        name: s.name,
        agent_type: s.agent_type,
        cwd: s.cwd,
        permission_mode: s.permission_mode,
        mode: s.mode || "persistent",
      });
      hapticSuccess();
      toastSuccess(t("toast.savedAsTemplate", { name: s.name }));
      const d = await getTemplates();
      setTemplates(d.templates);
    } catch (e: any) {
      toastError(mapApiError(e));
    }
  };

  const handleMultiSend = async () => {
    if (!multiPrompt.trim() || multiTargets.length === 0) return;
    try {
      const res = await multiSend(multiTargets, multiPrompt.trim());
      hapticSuccess();
      toastSuccess(t("toast.sentToSessions", { n: multiTargets.length }));
      setShowMulti(false);
      setMultiPrompt("");
      setMultiTargets([]);
      refresh();
    } catch (e: any) { toastError(mapApiError(e)); }
  };

  useEscape(showNew, () => setShowNew(false));
  useEscape(showDiscover, () => setShowDiscover(false));
  useEscape(showMulti, () => setShowMulti(false));

  // Filtered sessions
  const filteredSessions = search.trim()
    ? sessions.filter((s) => {
        const q = search.toLowerCase();
        return s.name.toLowerCase().includes(q) ||
               s.agent_type.toLowerCase().includes(q) ||
               (s.agent_name || "").toLowerCase().includes(q) ||
               s.cwd.toLowerCase().includes(q);
      })
    : sessions;

  // Count stats
  const aliveCount = sessions.filter((s) => s.status === "alive" || !s.status).length;
  const busyCount = sessions.filter((s) => s.is_busy).length;
  const deadCount = sessions.filter((s) => s.status === "dead").length;

  return (
    <div className="page">
      <div className="page-header">
        <div className="page-header-context">
          <h1>{t("dash.title")}</h1>
          <DeviceChip />
        </div>
        <button className="header-action" onClick={() => setShowHelp(true)} aria-label={t("help.title")}>
          {"?"}
        </button>
        <button className="header-action" onClick={() => navigate("/settings")} aria-label={t("settings.title")}>
          {"\u2699\uFE0F"}
        </button>
      </div>

      <div
        className="page-content"
        ref={contentRef}
        onTouchStart={onTouchStart}
        onTouchMove={onTouchMove}
        onTouchEnd={onTouchEnd}
      >
        {/* Pull-to-refresh indicator */}
        <div className="pull-indicator" ref={pullIndicatorRef}>
          <div className={`pull-spinner ${refreshing ? "active" : ""}`} />
        </div>

        {/* Quick Actions */}
        <div className="quick-actions">
          {/* Подпись — тем же ключом, что вкладка и строка гида: слово «Серверы»
              означало в продукте две разные вещи (SSH-хосты и собственные
              машины), и переименовать его в одном месте было нельзя, пока здесь
              стояла жёстко зашитая строка. */}
          <button className="quick-action-btn" onClick={() => { haptic(); navigate("/ssh"); }}>
            <span className="quick-action-icon">{"🗄️"}</span>
            {t("nav.ssh")}
          </button>
          {/* Remote + Screenshot \u0442\u0440\u0435\u0431\u0443\u044E\u0442 \u0434\u0438\u0441\u043F\u043B\u0435\u0439 \u2014 \u043D\u0430 headless-\u0441\u0435\u0440\u0432\u0435\u0440\u0435 \u0441\u043A\u0440\u044B\u0442\u044B. */}
          {remoteDesktop && (
            <button className="quick-action-btn primary" onClick={() => { haptic(); navigate("/remote"); }}>
              <span className="quick-action-icon">{"\uD83D\uDDA5\uFE0F"}</span>
              {t("quick.remote")}
            </button>
          )}
          <button className="quick-action-btn" onClick={() => { haptic(); navigate("/pty"); }}>
            <span className="quick-action-icon">{"\uD83D\uDCBB"}</span>
            {t("quick.terminal")}
          </button>
          {remoteDesktop && (
            <button className="quick-action-btn" onClick={handleScreenshot} disabled={screenshotLoading}>
              <span className="quick-action-icon">{screenshotLoading ? "\u23F3" : "\uD83D\uDCF7"}</span>
              {screenshotLoading ? t("quick.capturing") : t("quick.screenshot")}
            </button>
          )}
          <button className="quick-action-btn" onClick={() => { haptic(); navigate("/files"); }}>
            <span className="quick-action-icon">{"\uD83D\uDCC1"}</span>
            {t("quick.files")}
          </button>
        </div>

        {/* Тот же ряд в гид, что на главной: экран /sessions живёт отдельно,
            и без дубля дверь отсюда была бы невидима. */}
        <div style={{ padding: "0 16px", marginBottom: 8 }}>
          <button className="home-guide-row" onClick={() => { haptic(); navigate("/guide"); }}>
            <span className="home-guide-icon" aria-hidden>{"❓"}</span>
            <span className="home-guide-text">
              <span className="home-guide-row-title">{t("home.guide.howto")}</span>
              <span className="home-guide-row-desc">{t("home.guide.howtoDesc")}</span>
            </span>
            <span className="home-guide-chevron">{"›"}</span>
          </button>
        </div>

        {/* Screenshot preview */}
        {screenshotData && (
          <div style={{ padding: "0 16px", marginBottom: 8 }}>
            <img src={screenshotData} alt="Screenshot" style={{ width: "100%", borderRadius: 8 }} />
            <div style={{ display: "flex", gap: 8, marginTop: 4 }}>
              <button className="btn btn-secondary btn-block btn-sm" onClick={() => setScreenshotData(null)}>
                {t("modal.close")}
              </button>
              <button className="btn btn-primary btn-block btn-sm" onClick={handleShareScreenshot}>
                {"\uD83D\uDCE4"} {t("screenshot.share")}
              </button>
            </div>
          </div>
        )}

        {/* Stats bar */}
        {sessions.length > 0 && (
          <div className="dash-stats">
            <span className="dash-stat">
              <span className="dash-dot alive" /> {aliveCount} {t("dash.alive")}
            </span>
            {busyCount > 0 && (
              <span className="dash-stat">
                <span className="dash-dot busy" /> {busyCount} {t("dash.running")}
              </span>
            )}
            {deadCount > 0 && (
              <span className="dash-stat">
                <span className="dash-dot dead" /> {deadCount} {t("dash.error")}
              </span>
            )}
          </div>
        )}

        {/* Templates */}
        {templates.length > 0 && (
          <div className="dash-templates">
            <div className="dash-section-title">{t("dash.templates")}</div>
            <div className="dash-template-list">
              {templates.map((tmpl) => (
                <button key={tmpl.name} className="dash-template-chip"
                  onClick={() => handleApplyTemplate(tmpl)}
                  onContextMenu={async (e) => {
                    e.preventDefault();
                    if (await tgConfirm(`${t("files.delete")} "${tmpl.name}"?`, { danger: true, confirmText: t("confirm.btn.delete") })) {
                      await deleteTemplate(tmpl.name);
                      haptic("medium");
                      const d = await getTemplates();
                      setTemplates(d.templates);
                    }
                  }}>
                  {tmpl.agent_type === "claude" ? "\uD83D\uDFE0" : tmpl.agent_type === "codex" ? "\uD83D\uDFE2" : tmpl.agent_type === "orchestrator" ? "\uD83C\uDFAF" : "\uD83E\uDD16"} {tmpl.name}
                </button>
              ))}
            </div>
          </div>
        )}

        {/* Search — show if 3+ sessions */}
        {sessions.length >= 3 && (
          <div className="dash-search">
            <input
              type="text"
              className="dash-search-input"
              placeholder={t("dash.search")}
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              aria-label={t("dash.search")}
            />
            {search && (
              <button className="dash-search-clear" onClick={() => setSearch("")} aria-label={t("dash.clearSearch")}>
                {"\u2715"}
              </button>
            )}
          </div>
        )}

        {loading ? (
          <div className="skeleton-list">
            <SkeletonCard />
            <SkeletonCard />
            <SkeletonCard />
          </div>
        ) : filteredSessions.length === 0 && search ? (
          <div className="empty">
            <div className="empty-icon">{"\uD83D\uDD0D"}</div>
            <div className="empty-text">
              {t("dash.noMatch")} "{search}"
            </div>
            <button className="btn btn-secondary btn-sm" onClick={() => setSearch("")} style={{ marginTop: 12 }}>
              {t("dash.clearSearch")}
            </button>
          </div>
        ) : sessions.length === 0 ? (
          <div className="empty">
            <div className="empty-icon">{"\uD83E\uDD16"}</div>
            <div className="empty-title">{t("empty.welcome")}</div>
            <div className="empty-desc">{t("empty.welcomeDesc")}</div>
            <div className="empty-actions">
              <button className="btn btn-primary" onClick={() => { haptic(); setNewName(suggestName(newAgent)); setShowNew(true); }}>
                {t("empty.createSession")}
              </button>
            </div>
            <div className="empty-hints">
              <div className="empty-hint">{"\uD83D\uDDA5\uFE0F"} {t("empty.hintRemote")}</div>
              <div className="empty-hint">{"\uD83D\uDCBB"} {t("empty.hintTerminal")}</div>
              <div className="empty-hint">{"\uD83D\uDCC1"} {t("empty.hintFiles")}</div>
            </div>
          </div>
        ) : (
          <div className="session-grid">
            {filteredSessions.map((s) => (
              <SessionCard
                key={s.name}
                session={s}
                isActive={s.name === active}
                progress={progressMap[s.name]}
                onClick={() => navigate(`/session/${encodeURIComponent(s.name)}`)}
                onSwitch={() => handleSwitch(s.name)}
                onClose={() => handleDelete(s.name)}
                onClone={() => handleClone(s.name)}
                onClear={() => handleClear(s.name)}
                onContinue={() => handleContinue(s.name)}
                onSaveTemplate={() => handleSaveTemplate(s)}
              />
            ))}
          </div>
        )}
      </div>

      {/* FAB — floating action button */}
      {sessions.length > 0 && (
        <>
          {fabOpen && <div className="fab-backdrop" onClick={() => setFabOpen(false)} />}
          <div className={`fab-container${fabOpen ? " open" : ""}`}>
            {fabOpen && (
              <div className="fab-menu">
                <button className="fab-menu-item" onClick={() => { setFabOpen(false); haptic(); setNewName(suggestName(newAgent)); setShowNew(true); }}>
                  {"\u2795"} {t("fab.newSession")}
                </button>
                <button className="fab-menu-item" onClick={() => { setFabOpen(false); handleDiscover(); }}>
                  {"\uD83D\uDD0D"} {t("fab.discover")}
                </button>
                {sessions.length >= 2 && (
                  <button className="fab-menu-item" onClick={() => { setFabOpen(false); setShowMulti(true); setMultiTargets([]); }}>
                    {"\uD83D\uDCE4"} {t("fab.multiSend")}
                  </button>
                )}
              </div>
            )}
            <button className="fab-btn" onClick={() => { haptic(); setFabOpen(!fabOpen); }}>
              <span className={`fab-icon${fabOpen ? " rotated" : ""}`}>{"\u002B"}</span>
            </button>
          </div>
        </>
      )}

      {/* New Session Modal */}
      {showNew && (
        <div className="modal-overlay" onClick={(e) => { if (e.target === e.currentTarget) setShowNew(false); }}>
          <div className="modal-sheet">
            <div className="modal-title">{t("modal.newSession")}</div>

            <input
              className="modal-input"
              placeholder={t("modal.sessionName")}
              value={newName}
              onChange={(e) => setNewName(e.target.value)}
              onKeyDown={(e) => { if (e.key === "Enter") handleCreate(); }}
              autoFocus
            />

            <div className="setting-label">{t("modal.agent")}</div>
            <div className="agent-grid">
              {agents.map((a) => (
                <button
                  key={a.id}
                  className={`agent-chip ${newAgent === a.id ? "active" : ""}`}
                  onClick={() => { setNewAgent(a.id); setNewName(suggestName(a.id)); }}
                  title={a.description}
                >
                  <span className="agent-chip-icon">{a.icon}</span>
                  <span className="agent-chip-name">{a.name}</span>
                  <AgentQuotaChips quota={a.quota} agentName={a.name} agentID={a.id} />
                </button>
              ))}
            </div>

            <div className="setting-label">{t("modal.workDir")}</div>
            <div style={{ display: "flex", gap: 6, marginBottom: 12 }}>
              <input
                className="modal-input"
                style={{ margin: 0, flex: 1 }}
                placeholder={t("modal.defaultCurrent")}
                value={newCwd}
                onChange={(e) => setNewCwd(e.target.value)}
              />
              <button className="btn btn-secondary" style={{ flexShrink: 0, padding: "8px 12px" }} onClick={openFolderBrowser}>
                {"\uD83D\uDCC2"}
              </button>
            </div>

            {/* Inline folder browser */}
            {showFolderBrowser && (
              <div className="folder-browser">
                {/* Quick access */}
                {!folderPath && (
                  <div className="folder-quick">
                    {quickPaths.map((qp) => (
                      <button key={`${qp.name}:${qp.path}`} className="folder-quick-btn" onClick={() => browseTo(qp.path)}>
                        {"\uD83D\uDCC1"} {qp.name}
                      </button>
                    ))}
                    {bookmarks.map((b) => (
                      <button key={b.path} className="folder-quick-btn" onClick={() => browseTo(b.path)}>
                        {"\uD83D\uDCCC"} {b.name}
                      </button>
                    ))}
                    {recentFolders.length > 0 && recentFolders.map((rf) => (
                      <button key={rf.path} className="folder-quick-btn" onClick={() => browseTo(rf.path)}>
                        {"\uD83D\uDD52"} {rf.name}
                      </button>
                    ))}
                  </div>
                )}

                {/* Browsing a directory */}
                {folderPath && (
                  <>
                    <div className="folder-nav">
                      <button className="folder-nav-btn" onClick={() => {
                        const parent = folderPath.replace(/[\\/][^\\/]+$/, "") || folderPath;
                        if (parent !== folderPath) browseTo(parent);
                        else { setFolderPath(""); setFolderItems([]); }
                      }}>{"\u2190"}</button>
                      <span className="folder-nav-path">{folderPath}</span>
                      <button className="btn btn-primary btn-sm" onClick={() => selectFolder(folderPath)}>
                        {t("generic.select")}
                      </button>
                    </div>

                    {folderLoading ? (
                      <div style={{ padding: 16, textAlign: "center" }}><div className="spinner spinner-sm" style={{ margin: "0 auto" }} /></div>
                    ) : folderItems.length === 0 ? (
                      <div style={{ padding: 12, textAlign: "center", fontSize: 13, color: "var(--tg-hint)" }}>
                        {t("modal.noSubfolders")}
                      </div>
                    ) : (
                      <div className="folder-list">
                        {folderItems.map((f) => (
                          <button key={f.path} className="folder-item" onClick={() => browseTo(f.path)}>
                            {"\uD83D\uDCC1"} {f.name}
                          </button>
                        ))}
                      </div>
                    )}
                  </>
                )}
              </div>
            )}

            <div className="modal-actions">
              <button className="btn btn-secondary" onClick={() => setShowNew(false)}>
                {t("modal.cancel")}
              </button>
              <button className="btn btn-primary" onClick={handleCreate} disabled={!newName.trim()}>
                {t("modal.create")}
              </button>
            </div>
          </div>
        </div>
      )}

      {/* Discover Modal */}
      {showDiscover && (
        <div className="modal-overlay" onClick={(e) => { if (e.target === e.currentTarget) setShowDiscover(false); }}>
          <div className="modal-sheet" style={{ maxHeight: "80vh", overflow: "auto" }}>
            <div className="modal-title">{"\uD83D\uDD0D"} {t("discover.title")}</div>

            {discovering ? (
              <div className="loading-center" style={{ padding: 32 }}>
                <div className="spinner" />
              </div>
            ) : discovered.length === 0 ? (
              <div style={{ padding: "24px 0", textAlign: "center", color: "var(--tg-hint)" }}>
                {t("discover.noSessions")}
              </div>
            ) : (
              <>
                <div className="discover-list">
                  {discovered.map((ds) => (
                    <div key={ds.session_id} className="discover-item">
                      <div className="discover-top">
                        <span className={`discover-dot ${ds.is_alive ? "alive" : "dead"}`} />
                        <span className="discover-name">{ds.name}</span>
                        <span className="discover-agent">{ds.agent_type}</span>
                        {ds.imported && <span className="discover-imported">{"\u2705"}</span>}
                      </div>
                      <div className="discover-meta">
                        PID:{ds.pid} {"\u00B7"} {timeAgo(ds.started_at)} {"\u00B7"} {shortPath(ds.cwd)}
                      </div>
                      {!ds.imported && (
                        <button
                          className="btn btn-primary btn-sm"
                          style={{ marginTop: 6 }}
                          onClick={() => handleImport(ds)}
                          disabled={importing === ds.session_id}
                        >
                          {importing === ds.session_id ? "\u23F3" : "\uD83D\uDCE5"} {t("discover.import")}
                        </button>
                      )}
                    </div>
                  ))}
                </div>

                {discovered.some((d) => !d.imported) && (
                  <button
                    className="btn btn-primary btn-block"
                    style={{ marginTop: 12 }}
                    onClick={handleImportAll}
                  >
                    {"\uD83D\uDCE5"} {t("discover.importAll")} ({discovered.filter((d) => !d.imported).length})
                  </button>
                )}
              </>
            )}

            <div className="modal-actions">
              <button className="btn btn-secondary" onClick={() => setShowDiscover(false)}>
                {t("modal.close")}
              </button>
              <button className="btn btn-secondary" onClick={handleDiscover}>
                {"\u21BB"} {t("discover.refresh")}
              </button>
            </div>
          </div>
        </div>
      )}

      {/* Multi-Send Modal */}
      {showMulti && (
        <div className="modal-overlay" onClick={(e) => { if (e.target === e.currentTarget) setShowMulti(false); }}>
          <div className="modal-sheet">
            <div className="modal-title">{"\uD83D\uDCE4"} {t("multi.title")}</div>
            <div className="setting-label">{t("multi.selectSessions")}</div>
            <div className="multi-targets">
              {sessions.filter(s => !s.is_busy).map((s) => (
                <button
                  key={s.name}
                  className={`setting-chip ${multiTargets.includes(s.name) ? "active" : ""}`}
                  onClick={() => setMultiTargets(prev =>
                    prev.includes(s.name) ? prev.filter(n => n !== s.name) : [...prev, s.name]
                  )}
                >
                  {s.agent_icon} {s.name}
                </button>
              ))}
            </div>
            <textarea
              className="modal-input"
              placeholder={t("multi.prompt")}
              value={multiPrompt}
              onChange={(e) => setMultiPrompt(e.target.value)}
              rows={3}
              style={{ resize: "vertical", minHeight: 60 }}
              autoFocus
            />
            <div className="modal-actions">
              <button className="btn btn-secondary" onClick={() => setShowMulti(false)}>{t("modal.cancel")}</button>
              <button
                className="btn btn-primary"
                onClick={handleMultiSend}
                disabled={!multiPrompt.trim() || multiTargets.length === 0}
              >
                {t("multi.send")} ({multiTargets.length})
              </button>
            </div>
          </div>
        </div>
      )}

      <HelpSheet
        open={showHelp}
        onClose={() => setShowHelp(false)}
        title={t("dash.helpTitle")}
        guide="terminal"
        items={[
          { icon: "\uD83D\uDC46", title: t("dash.helpOpenTitle"), text: t("dash.helpOpenText") },
          { icon: "\u23F1", title: t("dash.helpLongTitle"), text: t("dash.helpLongText") },
          { icon: "\u2B05", title: t("dash.helpSwipeTitle"), text: t("dash.helpSwipeText") },
          { icon: "\u2795", title: t("dash.helpFabTitle"), text: t("dash.helpFabText") },
        ]}
      />

      <BottomNav active="sessions" />
    </div>
  );
}
