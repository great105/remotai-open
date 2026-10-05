import { getLocale } from "@tgcontrol/shared";
import { useEffect, useState, useRef, useCallback } from "react";
import { useParams, useNavigate } from "react-router-dom";
import { useEscape, mapApiError } from "@tgcontrol/shared";
import { getSession, sendPrompt, stopSession, updateSessionConfig, getConfig, getPinnedPrompts, addPinnedPrompt, removePinnedPrompt, getClaudeHistory } from "../api";
import { onWSEvent } from "../api";
import { haptic, hapticError, hapticSuccess } from "../telegram";
import { useToast } from "@tgcontrol/shared";
import { t } from "../i18n";
import type { ChatMessage, SessionDetail, AppConfig, OrchestratorStep } from "../types";

// Форматирование таймера активности агента (п. «работает 4:32 · активность 12с назад»).
const fmtElapsed = (ms: number) => {
  const s = Math.max(0, Math.floor(ms / 1000));
  return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, "0")}`;
};
const fmtAgo = (ms: number, justNow: string) => {
  const s = Math.max(0, Math.floor(ms / 1000));
  if (s < 3) return justNow;
  if (s < 60) return t("ui.ptytermview.mee89ed3363", { p0: (s) });
  return t("ui.sessionview.mb273defe4d", { p0: (Math.floor(s / 60)), p1: (s % 60) });
};

export function SessionView() {
  const { name } = useParams<{ name: string }>();
  const navigate = useNavigate();
  const decodedName = decodeURIComponent(name || "");
  const { toastSuccess, toastError } = useToast();

  const [session, setSession] = useState<SessionDetail | null>(null);
  const [messages, setMessages] = useState<ChatMessage[]>([]);
  const [input, setInput] = useState("");
  const [busy, setBusy] = useState(false);
  const [stopping, setStopping] = useState(false);
  const [busySince, setBusySince] = useState<number | null>(null);
  const [lastActivity, setLastActivity] = useState<number | null>(null);
  const [, setActivityTick] = useState(0);
  const [progress, setProgress] = useState<string | null>(null);
  const [progressLog, setProgressLog] = useState<string[]>([]);
  const [orchSteps, setOrchSteps] = useState<OrchestratorStep[]>([]);
  const [showSteps, setShowSteps] = useState(false);
  const [showConfig, setShowConfig] = useState(false);
  const [appConfig, setAppConfig] = useState<AppConfig | null>(null);
  const [savingConfig, setSavingConfig] = useState(false);

  const [pinnedPrompts, setPinnedPrompts] = useState<string[]>([]);
  const [showPinInput, setShowPinInput] = useState(false);
  const [pinText, setPinText] = useState("");
  const [claudeHistory, setClaudeHistory] = useState<ChatMessage[]>([]);
  const [showHistory, setShowHistory] = useState(false);

  const messagesEndRef = useRef<HTMLDivElement>(null);
  const textareaRef = useRef<HTMLTextAreaElement>(null);

  useEscape(showConfig, () => setShowConfig(false));
  useEscape(showHistory, () => setShowHistory(false));

  const scrollBottom = () => {
    messagesEndRef.current?.scrollIntoView({ behavior: "smooth" });
  };

  const load = useCallback(async () => {
    if (!decodedName) return;
    try {
      const data = await getSession(decodedName);
      setSession(data);
      setMessages(data.messages);
      setBusy(data.is_busy);
      if (data.is_busy) {
        // Реальный старт рана неизвестен после перезахода — считаем от загрузки.
        setBusySince((v) => v ?? Date.now());
        setLastActivity(Date.now());
      }
    } catch {
      navigate("/");
    }
  }, [decodedName, navigate]);

  useEffect(() => {
    load();
    if (decodedName) {
      getPinnedPrompts(decodedName).then(d => setPinnedPrompts(d.prompts)).catch(() => {});
    }
  }, [load, decodedName]);

  useEffect(scrollBottom, [messages]);

  // Тикаем раз в секунду, пока агент работает, — для таймера активности.
  useEffect(() => {
    if (!busy) return;
    const id = setInterval(() => setActivityTick((v) => v + 1), 1000);
    return () => clearInterval(id);
  }, [busy]);

  useEffect(() => {
    const unsub = onWSEvent((ev) => {
      if (ev.type === "message" && ev.session === decodedName) {
        // Skip user messages — already added optimistically in handleSend
        if (ev.message.role === "user") return;
        setMessages((prev) => [...prev, ev.message]);
        if (ev.message.role === "agent") {
          setBusy(false);
          setProgress(null);
          setProgressLog([]);
        }
      } else if (ev.type === "status" && ev.session === decodedName) {
        setBusy(ev.is_busy);
        if (!ev.is_busy) { setProgress(null); setProgressLog([]); setBusySince(null); setLastActivity(null); }
        else { setBusySince((v) => v ?? Date.now()); setLastActivity(Date.now()); }
      } else if (ev.type === "progress" && ev.session === decodedName) {
        setLastActivity(Date.now());
        setProgress(ev.text);
        setProgressLog((prev) => {
          if (prev.length > 0 && prev[prev.length - 1] === ev.text) return prev;
          return [...prev, ev.text];
        });
      } else if (ev.type === "orchestrator_step" && ev.session === decodedName) {
        setLastActivity(Date.now());
        setOrchSteps((prev) => [...prev, ev.step]);
      }
    });
    return unsub;
  }, [decodedName]);

  const handleSend = async () => {
    const text = input.trim();
    if (!text || busy) return;
    setInput("");
    setBusy(true);
    setBusySince(Date.now());
    setLastActivity(Date.now());
    setProgressLog([]);
    setOrchSteps([]);
    haptic();

    // Optimistic add
    const userMsg: ChatMessage = { role: "user", text, timestamp: Date.now() / 1000 };
    setMessages((prev) => [...prev, userMsg]);

    try {
      await sendPrompt(decodedName, text);
    } catch (e: any) {
      hapticError();
      setBusy(false);
      setMessages((prev) => [
        ...prev,
        { role: "agent", text: `${t("error.label")}: ${e.message}`, timestamp: Date.now() / 1000, is_error: true },
      ]);
    }

    // Auto-resize textarea
    if (textareaRef.current) {
      textareaRef.current.style.height = "38px";
    }
  };

  const handleStop = async () => {
    setStopping(true);
    try {
      await stopSession(decodedName);
    } catch { /* ignore */ }
    setStopping(false);
  };

  const handleCopyMessage = async (text: string) => {
    try {
      await navigator.clipboard.writeText(text);
      hapticSuccess();
      toastSuccess(t("settings.copied"));
    } catch {
      hapticError();
    }
  };

  const handleKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === "Enter" && !e.shiftKey) {
      e.preventDefault();
      handleSend();
    }
  };

  const autoResize = (el: HTMLTextAreaElement) => {
    el.style.height = "38px";
    el.style.height = Math.min(el.scrollHeight, 120) + "px";
  };

  const formatTime = (ts: number) => {
    const d = new Date(ts * 1000);
    return d.toLocaleTimeString(getLocale(), { hour: "2-digit", minute: "2-digit" });
  };

  const openConfig = async () => {
    haptic();
    if (!appConfig) {
      try {
        const cfg = await getConfig();
        setAppConfig(cfg);
      } catch { /* ignore */ }
    }
    setShowConfig(true);
  };

  const updateCfg = async (field: string, value: string) => {
    if (!session) return;
    haptic();
    setSavingConfig(true);
    try {
      await updateSessionConfig(decodedName, { [field]: value });
      setSession((prev) => prev ? {
        ...prev,
        agent_config: { ...prev.agent_config, [field]: value },
      } : prev);
      hapticSuccess();
    } catch (e: any) {
      hapticError();
      toastError(mapApiError(e));
    }
    setSavingConfig(false);
  };

  const getEffective = (field: string): string => {
    if (session?.agent_config?.[field]) return session.agent_config[field];
    if (appConfig) return (appConfig as any)[field] || "";
    return "";
  };

  return (
    <div className="page session-page">
      <div className="page-header">
        <button className="back-btn" onClick={() => navigate("/")} aria-label={t("generic.back")}>
          {"\u2190"}
        </button>
        <h1>{decodedName}</h1>
        <button className="header-action" onClick={() => setShowPinInput(!showPinInput)}
          aria-label="Pin prompt" title="Pin prompt">
          {"\uD83D\uDCCC"}
        </button>
        <button className="header-action" onClick={async () => {
          try {
            const data = await getClaudeHistory(decodedName);
            setClaudeHistory(data.messages as ChatMessage[]);
            setShowHistory(true);
          } catch { /* ignore */ }
        }} aria-label="Claude history" title="Claude history">
          {"\uD83D\uDD04"}
        </button>
        {session?.agent_type === "researcher" && (
          <button className="header-action" onClick={() => navigate(`/research/${name}`)} title="Research">
            {"\uD83D\uDD2C"}
          </button>
        )}
        <button className="header-action" onClick={openConfig} aria-label="Session settings">
          {"\u2699\uFE0F"}
        </button>
      </div>

      {session && (
        <div className="session-info">
          <div className="session-info-row">
            <span className="badge">
              {session.agent_icon || "\uD83E\uDD16"} {session.agent_name || session.agent_type}
            </span>
            <span style={{ flex: 1, overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>
              {session.cwd}
            </span>
          </div>
        </div>
      )}

      <div className="chat-messages">
        {messages.length === 0 && !busy && (
          <div className="empty">
            <div className="empty-icon">{"\uD83D\uDCAC"}</div>
            <div className="empty-text">{t("chat.sendFirst")}</div>
          </div>
        )}

        {messages.map((msg, i) => (
          <div key={i} className={`chat-bubble ${msg.role} ${msg.is_error ? "error" : ""}`}>
            {msg.role === "agent" && msg.tools && msg.tools.length > 0 && (
              <div className="bubble-tools">
                {msg.tools.slice(0, 8).map((t, j) => (
                  <span key={j} className="tool-chip">{t}</span>
                ))}
                {msg.tools.length > 8 && (
                  <span className="tool-chip">+{msg.tools.length - 8}</span>
                )}
              </div>
            )}
            <div>{msg.text}</div>
            <div className="bubble-time">
              {formatTime(msg.timestamp)}
              {msg.cost_usd != null && (
                <span className="bubble-cost"> &middot; ${msg.cost_usd.toFixed(4)}</span>
              )}
              <button
                onClick={() => handleCopyMessage(msg.text)}
                aria-label={t("chat.copyMessage")}
                title={t("chat.copyMessage")}
                style={{ background: "none", border: "none", cursor: "pointer", opacity: 0.6, fontSize: 12, padding: 0, marginLeft: 6, color: "inherit" }}
              >
                {"\uD83D\uDCCB"}
              </button>
            </div>
          </div>
        ))}

        {busy && (
          <div className="thinking-bar">
            <div className="thinking-dots">
              <span /><span /><span />
            </div>
            <span className="thinking-text">{progress || t("chat.thinking")}</span>
            {busySince !== null && (
              <span className="thinking-timer">
                {fmtElapsed(Date.now() - busySince)}
                {lastActivity !== null && ` · ${fmtAgo(Date.now() - lastActivity, t("chat.justNow"))}`}
              </span>
            )}
          </div>
        )}

        {/* Orchestrator detailed steps */}
        {orchSteps.length > 0 && (
          <div className="orch-panel">
            <button className="orch-toggle" onClick={() => setShowSteps(!showSteps)}>
              {showSteps ? "\u25BC" : "\u25B6"} {t("generic.steps")} ({orchSteps.length})
              {orchSteps.filter(s => s.type === "api_call").length > 0 && (
                <span className="orch-tokens">
                  {orchSteps.filter(s => s.type === "api_call").reduce((a, s) => a + (s.tokens_in || 0) + (s.tokens_out || 0), 0).toLocaleString(getLocale())} tok
                </span>
              )}
            </button>
            {showSteps && (
              <div className="orch-steps">
                {orchSteps.map((step, i) => (
                  <div key={i} className={`orch-step orch-step-${step.type}`}>
                    <div className="orch-step-header">
                      <span className="orch-step-icon">
                        {step.type === "thinking" ? "\uD83D\uDCAD" :
                         step.type === "tool_call" ? "\uD83D\uDD27" :
                         step.type === "tool_result" ? "\uD83D\uDCE4" :
                         step.type === "api_call" ? "\uD83C\uDF10" :
                         "\u26A0\uFE0F"}
                      </span>
                      <span className="orch-step-type">
                        {step.type === "api_call" ? `iter ${step.iteration}` :
                         step.type === "tool_call" ? step.tool :
                         step.type === "tool_result" ? `${step.tool} result` :
                         step.type}
                      </span>
                      {step.duration_ms != null && step.duration_ms > 0 && (
                        <span className="orch-step-dur">{(step.duration_ms / 1000).toFixed(1)}s</span>
                      )}
                      {step.type === "api_call" && step.tokens_in != null && (
                        <span className="orch-step-tok">{step.tokens_in}+{step.tokens_out} tok</span>
                      )}
                    </div>
                    {step.input && <div className="orch-step-text">{step.input}</div>}
                    {step.output && <div className="orch-step-output">{step.output}</div>}
                    {step.error && <div className="orch-step-error">{step.error}</div>}
                  </div>
                ))}
              </div>
            )}
          </div>
        )}

        {/* Simple progress log fallback (non-orchestrator agents) */}
        {busy && progressLog.length > 1 && orchSteps.length === 0 && (
          <div className="activity-log">
            {progressLog.map((line, i) => (
              <div key={i} className={`activity-line${i === progressLog.length - 1 ? " active" : ""}`}>
                {line}
              </div>
            ))}
          </div>
        )}

        <div ref={messagesEndRef} />
      </div>

      {busy && (
        <button className="stop-btn" onClick={handleStop} disabled={stopping}>
          {stopping ? "\u23F3" : t("chat.stop")}
        </button>
      )}

      {/* Pin prompt input */}
      {showPinInput && (
        <div className="pinned-bar" style={{ gap: 6, padding: "6px 12px" }}>
          <input
            className="modal-input"
            style={{ margin: 0, flex: 1, fontSize: 13, padding: "6px 10px" }}
            placeholder={t("chat.pinPrompt")}
            value={pinText}
            onChange={(e) => setPinText(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter" && pinText.trim()) {
                addPinnedPrompt(decodedName, pinText.trim()).then(() => {
                  setPinnedPrompts((prev) => [...prev, pinText.trim()]);
                  setPinText("");
                  setShowPinInput(false);
                });
              } else if (e.key === "Escape") {
                setShowPinInput(false);
                setPinText("");
              }
            }}
            autoFocus
          />
          <button className="btn btn-primary btn-sm" disabled={!pinText.trim()} onClick={() => {
            if (!pinText.trim()) return;
            addPinnedPrompt(decodedName, pinText.trim()).then(() => {
              setPinnedPrompts((prev) => [...prev, pinText.trim()]);
              setPinText("");
              setShowPinInput(false);
            });
          }}>{t("chat.pin")}</button>
          <button className="btn btn-secondary btn-sm" onClick={() => { setShowPinInput(false); setPinText(""); }}>
            {"\u2715"}
          </button>
        </div>
      )}

      {/* Pinned prompts */}
      {pinnedPrompts.length > 0 && (
        <div className="pinned-bar">
          {pinnedPrompts.map((p, i) => (
            <button key={i} className="pinned-chip" onClick={() => {
              setInput(p);
              haptic();
            }}>
              {p.length > 25 ? p.slice(0, 22) + "..." : p}
              <span className="pinned-x" onClick={(e) => {
                e.stopPropagation();
                removePinnedPrompt(decodedName, p).then(() =>
                  setPinnedPrompts(prev => prev.filter(x => x !== p))
                );
              }}>{"\u2715"}</span>
            </button>
          ))}
        </div>
      )}

      <div className="chat-input-bar">
        <textarea
          ref={textareaRef}
          value={input}
          onChange={(e) => { setInput(e.target.value); autoResize(e.target); }}
          onKeyDown={handleKeyDown}
          placeholder={t("chat.placeholder")}
          rows={1}
        />
        <button
          className="send-btn"
          onClick={handleSend}
          disabled={busy || !input.trim()}
        >
          {"\u2191"}
        </button>
      </div>

      {/* Session config modal */}
      {showConfig && session && appConfig && (
        <div className="modal-overlay" onClick={(e) => { if (e.target === e.currentTarget) setShowConfig(false); }}>
          <div className="modal-sheet">
            <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center", marginBottom: 12 }}>
              <div className="modal-title" style={{ margin: 0 }}>
                {session.agent_icon} {session.agent_name} — {t("generic.settings")}
              </div>
              {savingConfig && <span style={{ fontSize: 12, color: "var(--tg-hint)" }}>{t("config.saving")}</span>}
            </div>

            {/* Claude settings */}
            {session.agent_type === "claude" && (
              <>
                <div className="setting-label">{t("config.model")}</div>
                <div className="setting-options" style={{ marginBottom: 12 }}>
                  {appConfig.options.claude_models.map((m) => (
                    <button key={m}
                      className={`setting-chip ${getEffective("claude_model") === m ? "active" : ""}`}
                      onClick={() => updateCfg("claude_model", m)}>
                      {m}
                    </button>
                  ))}
                </div>

                <div className="setting-label">{t("config.permissionMode")}</div>
                <div className="setting-options" style={{ marginBottom: 12 }}>
                  {appConfig.options.claude_permission_modes.map((m) => (
                    <button key={m}
                      className={`setting-chip ${getEffective("claude_permission_mode") === m ? "active" : ""}`}
                      onClick={() => updateCfg("claude_permission_mode", m)}>
                      {m}
                    </button>
                  ))}
                </div>
              </>
            )}

            {/* Codex settings */}
            {session.agent_type === "codex" && (
              <>
                <div className="setting-label">{t("config.model")}</div>
                <div className="setting-options" style={{ marginBottom: 12 }}>
                  {appConfig.options.codex_models.map((m) => (
                    <button key={m}
                      className={`setting-chip ${getEffective("codex_model") === m ? "active" : ""}`}
                      onClick={() => updateCfg("codex_model", m)}>
                      {m}
                    </button>
                  ))}
                </div>

                <div className="setting-label">{t("config.reasoning")}</div>
                <div className="setting-options" style={{ marginBottom: 12 }}>
                  {appConfig.options.codex_reasoning.map((r) => (
                    <button key={r}
                      className={`setting-chip ${getEffective("codex_reasoning") === r ? "active" : ""}`}
                      onClick={() => updateCfg("codex_reasoning", r)}>
                      {r}
                    </button>
                  ))}
                </div>

                <div className="setting-label">{t("config.approvalMode")}</div>
                <div className="setting-options" style={{ marginBottom: 12 }}>
                  {appConfig.options.codex_approval_modes.map((m) => (
                    <button key={m}
                      className={`setting-chip ${getEffective("codex_approval_mode") === m ? "active" : ""}`}
                      onClick={() => updateCfg("codex_approval_mode", m)}>
                      {m}
                    </button>
                  ))}
                </div>
              </>
            )}

            {/* Orchestrator / Researcher model selector */}
            {(session.agent_type === "orchestrator" || session.agent_type === "researcher") && (
              <>
                <div className="setting-label">{t("config.modelOpenRouter")}</div>
                <div className="setting-options" style={{ marginBottom: 12, flexWrap: "wrap" }}>
                  {(appConfig.options.orchestrator_models || []).map((m: any) => {
                    const mid = typeof m === "string" ? m : m.id;
                    const label = typeof m === "string" ? m : `${m.name}`;
                    const price = typeof m === "object" ? ` $${m.input_m}/${m.output_m}` : "";
                    return (
                      <button key={mid}
                        className={`setting-chip ${getEffective("orchestrator_model") === mid ? "active" : ""}`}
                        onClick={() => updateCfg("orchestrator_model", mid)}
                        style={{ fontSize: 11 }}>
                        {label}<span style={{ opacity: 0.5, marginLeft: 4 }}>{price}</span>
                      </button>
                    );
                  })}
                </div>
                <div style={{ fontSize: 11, color: "var(--tg-hint)", marginBottom: 8 }}>
                  {t("config.priceHint")}
                </div>
              </>
            )}

            {/* Generic agent info */}
            {!["claude", "codex", "orchestrator", "researcher"].includes(session.agent_type) && (
              <div style={{ fontSize: 13, color: "var(--tg-hint)", padding: "8px 0" }}>
                {t("chat.defaultSettings")}
              </div>
            )}

            <div className="session-config-info">
              <div className="session-config-label">{t("config.workDir")}</div>
              <div className="session-config-value">{session.cwd}</div>
            </div>

            <div className="modal-actions">
              <button className="btn btn-secondary" onClick={() => setShowConfig(false)} style={{ flex: 1 }}>
                {t("modal.close")}
              </button>
            </div>
          </div>
        </div>
      )}

      {/* Claude History Modal */}
      {showHistory && (
        <div className="modal-overlay" onClick={(e) => { if (e.target === e.currentTarget) setShowHistory(false); }}>
          <div className="modal-sheet" style={{ maxHeight: "80vh", overflow: "auto" }}>
            <div className="modal-title">{"\uD83D\uDD04"} {t("chat.history")}</div>
            {claudeHistory.length === 0 ? (
              <div style={{ padding: 16, textAlign: "center", color: "var(--tg-hint)" }}>
                {t("chat.noHistory")}
              </div>
            ) : (
              <div className="claude-history-list">
                {claudeHistory.map((m, i) => (
                  <div key={i} className="claude-history-item">
                    <div className="claude-history-text">{m.text}</div>
                    <div className="claude-history-time">
                      {new Date(m.timestamp * 1000).toLocaleString(getLocale())}
                      {(m as any).source && <span className="claude-history-src">{(m as any).source}</span>}
                    </div>
                  </div>
                ))}
              </div>
            )}
            <div className="modal-actions">
              <button className="btn btn-secondary" onClick={() => setShowHistory(false)}>{t("modal.close")}</button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
