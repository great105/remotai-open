import { getLocale } from "../locale";
import { useState, useEffect, useRef, useCallback } from "react";
import { t } from "../i18n";
import { platform } from "../platform";
import { formatAgoValue } from "../timeAgo";
import type { Session } from "../types";

interface Props {
  session: Session;
  isActive: boolean;
  progress?: string;
  onClick: () => void;
  onSwitch: () => void;
  // Может вернуть Promise<boolean>: false = закрытие не подтверждено
  // (отмена в confirm или ошибка) — тогда свайп-анимация откатывается.
  onClose: () => void | Promise<boolean | void>;
  onClone?: () => void;
  onClear?: () => void;
  onContinue?: () => void;
  onSaveTemplate?: () => void;
}

/**
 * Возраст карточки — общим хелпером (@tgcontrol/shared/timeAgo), тем же, что на
 * главной и в списке терминалов: своя копия печатала латинские «12m / 3h / 2d»
 * в русском интерфейсе, где соседние экраны для той же величины писали «12м».
 * `last_active_at` приходит в СЕКУНДАХ (legacy-сессии агента), formatAgoValue
 * ждёт миллисекунды.
 */
function timeAgo(ts?: number): string {
  if (!ts) return "";
  return formatAgoValue(ts * 1000);
}

export function SessionCard({ session, isActive, progress, onClick, onSwitch, onClose, onClone, onClear, onContinue, onSaveTemplate }: Props) {
  const s = session;
  const [showContext, setShowContext] = useState(false);
  const [, setTick] = useState(0);

  // Swipe state
  const cardRef = useRef<HTMLDivElement>(null);
  const touchRef = useRef<{ startX: number; startY: number; dx: number; swiping: boolean; startTime: number }>({
    startX: 0, startY: 0, dx: 0, swiping: false, startTime: 0,
  });
  // Long-press state
  const longPressTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  // Время последнего touchend — чтобы погасить «ghost click», который браузер
  // синтезирует после тач-тапа (иначе на мобиле навигация сработала бы дважды).
  const lastTouchRef = useRef(0);

  // Live time-ago: re-render every 30s
  useEffect(() => {
    const interval = setInterval(() => setTick((t) => t + 1), 30000);
    return () => clearInterval(interval);
  }, []);

  const shortPath = (p: string) => {
    const parts = p.replace(/\\/g, "/").split("/");
    if (parts.length <= 3) return p;
    return "~/" + parts.slice(-2).join("/");
  };

  const statusClass = s.is_busy
    ? "busy"
    : s.status === "dead"
    ? "dead"
    : s.status === "not-ready"
    ? "notready"
    : "idle";

  const statusLabel = s.is_busy
    ? t("card.statusRunning")
    : s.status === "dead"
    ? t("card.statusError")
    : s.status === "not-ready"
    ? t("card.statusNotReady")
    : t("card.statusActive");

  // Touch handlers for swipe-to-close and long-press
  const onTouchStart = useCallback((e: React.TouchEvent) => {
    const touch = e.touches[0];
    touchRef.current = { startX: touch.clientX, startY: touch.clientY, dx: 0, swiping: false, startTime: Date.now() };
    // Start long-press timer
    longPressTimer.current = setTimeout(() => {
      platform().haptic("medium");
      setShowContext(true);
      touchRef.current.swiping = false; // cancel swipe
    }, 500);
  }, []);

  const onTouchMove = useCallback((e: React.TouchEvent) => {
    const touch = e.touches[0];
    const dx = touch.clientX - touchRef.current.startX;
    const dy = touch.clientY - touchRef.current.startY;

    // If vertical movement is dominant, cancel swipe & long-press
    if (Math.abs(dy) > 10 && !touchRef.current.swiping) {
      if (longPressTimer.current) clearTimeout(longPressTimer.current);
      return;
    }

    // If horizontal movement exceeds threshold, enter swipe mode
    if (Math.abs(dx) > 10) {
      if (longPressTimer.current) clearTimeout(longPressTimer.current);
      touchRef.current.swiping = true;
    }

    if (touchRef.current.swiping && cardRef.current) {
      // Only allow swipe left (negative dx)
      const clampedDx = Math.min(0, dx);
      touchRef.current.dx = clampedDx;
      cardRef.current.style.transform = `translateX(${clampedDx}px)`;
    }
  }, []);

  const onTouchEnd = useCallback(() => {
    if (longPressTimer.current) clearTimeout(longPressTimer.current);
    lastTouchRef.current = Date.now();

    const { dx, swiping, startTime } = touchRef.current;

    if (swiping && cardRef.current) {
      if (dx < -120) {
        // Swipe far enough — close
        cardRef.current.style.transform = `translateX(-100%)`;
        cardRef.current.style.opacity = "0";
        cardRef.current.style.transition = "transform 0.2s, opacity 0.2s";
        setTimeout(() => {
          const res = onClose();
          // Отмена в confirm (onClose вернул Promise<false>) — возвращаем
          // карточку на место, иначе она остаётся невидимой.
          if (res && typeof res.then === "function") {
            res.then((confirmed) => {
              if (confirmed !== false || !cardRef.current) return;
              cardRef.current.style.transform = "";
              cardRef.current.style.opacity = "";
              setTimeout(() => {
                if (cardRef.current) cardRef.current.style.transition = "";
              }, 200);
            });
          }
        }, 200);
      } else {
        // Snap back
        cardRef.current.style.transition = "transform 0.2s";
        cardRef.current.style.transform = "";
        setTimeout(() => {
          if (cardRef.current) cardRef.current.style.transition = "";
        }, 200);
      }
      return;
    }

    // If not swiping and short tap — navigate
    const elapsed = Date.now() - startTime;
    if (elapsed < 400 && Math.abs(dx) < 5 && !showContext) {
      onClick();
    }
  }, [onClick, onClose, showContext]);

  // Десктоп (окно exe = WebView2, обычный браузер): touch-события не приходят,
  // поэтому карточка была «мёртвой» на клик. Вешаем навигацию на mouse-click,
  // гася ghost-click после тач-тапа (там onTouchEnd уже навигировал).
  const onMouseClick = useCallback(() => {
    if (Date.now() - lastTouchRef.current < 600) return; // это ghost после тача
    if (showContext) return;
    onClick();
  }, [onClick, showContext]);

  // Правый клик на десктопе открывает то же контекст-меню, что и долгий тап.
  const onContextMenu = useCallback((e: React.MouseEvent) => {
    e.preventDefault();
    platform().haptic("medium");
    setShowContext(true);
  }, []);

  const handleContextAction = (action: () => void) => {
    setShowContext(false);
    action();
  };

  return (
    <>
      <div
        ref={cardRef}
        className="card card-swipeable"
        onTouchStart={onTouchStart}
        onTouchMove={onTouchMove}
        onTouchEnd={onTouchEnd}
        onClick={onMouseClick}
        onContextMenu={onContextMenu}
      >
        {/* Swipe delete indicator behind card */}
        <div className="card-swipe-bg">
          <span>{t("card.close")}</span>
        </div>

        <div className="card-content">
          <div className="card-top">
            <div className={`card-status ${statusClass}`} aria-label={statusLabel} title={statusLabel} />
            <span className="card-name">{s.name}</span>
            {isActive && <span className="card-active-badge">{t("card.active")}</span>}
            {s.mode === "oneshot" && <span className="card-mode-badge">oneshot</span>}
            {!!s.topic_id && <span className="card-topic-badge">{"\uD83D\uDD17"}</span>}
          </div>
          <div className="card-meta">
            {s.agent_icon || "\uD83E\uDD16"} {s.agent_name || s.agent_type}
            {s.is_busy && !progress && <span className="card-running-dot"> {t("card.running")}</span>}
            {s.status === "dead" && !s.is_busy && ` \u00B7 ${t("card.error")}`}
            {s.permission_mode && (
              <span className="card-perm">{"\uD83D\uDD12"} {s.permission_mode}</span>
            )}
          </div>
          {progress && (
            <div className="card-progress">
              <div className="card-progress-dots"><span /><span /><span /></div>
              <span className="card-progress-text">{progress}</span>
            </div>
          )}
          <div className="card-path">
            {shortPath(s.cwd)}
            {s.last_active_at ? (
              <span className="card-age" title={new Date(s.last_active_at * 1000).toLocaleString(getLocale())}>
                {timeAgo(s.last_active_at)}
              </span>
            ) : null}
            {s.ttl_minutes ? (
              <span className="card-ttl">{"\u23F1"} {t("card.ttlMinutes", { n: s.ttl_minutes })}</span>
            ) : null}
          </div>
        </div>
      </div>

      {/* Context menu (long-press) */}
      {showContext && (
        <div className="card-context-overlay" onClick={() => setShowContext(false)}>
          <div className="card-context-menu" onClick={(e) => e.stopPropagation()}>
            <div className="card-context-title">{s.name}</div>
            {!isActive && (
              <button className="card-context-item" onClick={() => handleContextAction(onSwitch)}>
                {"\u21C4"} {t("card.switch")}
              </button>
            )}
            {onContinue && (
              <button className="card-context-item" onClick={() => handleContextAction(onContinue)}>
                {"\u25B6"} {t("card.continue")}
              </button>
            )}
            {onClear && (
              <button className="card-context-item" onClick={() => handleContextAction(onClear)}>
                {"\uD83D\uDDD1"} {t("card.clear")}
              </button>
            )}
            {onClone && (
              <button className="card-context-item" onClick={() => handleContextAction(onClone)}>
                {"\uD83D\uDCCB"} {t("card.clone")}
              </button>
            )}
            {onSaveTemplate && (
              <button className="card-context-item" onClick={() => handleContextAction(onSaveTemplate)}>
                {"\uD83D\uDCBE"} {t("session.saveAsTemplate")}
              </button>
            )}
            <button className="card-context-item card-context-danger" onClick={() => handleContextAction(onClose)}>
              {"\u2715"} {t("card.close")}
            </button>
          </div>
        </div>
      )}
    </>
  );
}
