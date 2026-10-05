import { useState, useEffect, useRef, useCallback } from "react";
import { useParams } from "react-router-dom";
import { getResearch, startResearch, stopResearch, saveResearchConfig, getSession, onWSEvent } from "../api";
import type { ResearchState, ResearchExperiment, ResearchConfig, WSEvent } from "../types";
import { useToast, mapApiError } from "@tgcontrol/shared";
import { t } from "../i18n";
import { useGoBack } from "../navBack";

// ── Presets ────────────────────────────────────────────────────────

interface Preset {
  id: string;
  icon: string;
  /** i18n key for the title. */
  title: string;
  /** Already-localised description (command snippets stay literal). */
  desc: string;
  config: Partial<ResearchConfig>;
}

const presets: Preset[] = [
  {
    id: "go-bench", icon: "🐹", title: "research.preset.goBench.title",
    desc: "go test -bench, ns/op",
    config: {
      eval_command: "go test -bench=. -benchmem -count=1 -run=^$ ./...",
      metric_pattern: "(\\d+)\\s+ns/op",
      metric_name: "ns/op",
      lower_is_better: true,
      invariants_command: "go test ./...",
    },
  },
  {
    id: "go-test", icon: "✅", title: "research.preset.goTest.title",
    desc: t("research.preset.goTest.desc"),
    config: {
      eval_command: "go test -cover ./... 2>&1 | tail -1",
      metric_pattern: "coverage:\\s+([\\d.]+)%",
      metric_name: "coverage %",
      lower_is_better: false,
      invariants_command: "go test ./...",
    },
  },
  {
    id: "node-bundle", icon: "📦", title: "research.preset.jsBundle.title",
    desc: t("research.preset.jsBundle.desc"),
    config: {
      eval_command: "npm run build 2>&1 | grep -oP '\\d+\\.\\d+ kB' | head -1",
      metric_pattern: "([\\d.]+)",
      metric_name: "kB",
      lower_is_better: true,
      invariants_command: "npm test",
    },
  },
  {
    id: "python-bench", icon: "🐍", title: "research.preset.pyBench.title",
    desc: t("research.preset.pyBench.desc"),
    config: {
      eval_command: "python -m pytest --benchmark-only -q",
      metric_pattern: "Mean\\s+([\\d.]+)",
      metric_name: "ms",
      lower_is_better: true,
      invariants_command: "python -m pytest --ignore=benchmarks/",
    },
  },
  {
    id: "custom", icon: "⚙️", title: "research.preset.custom.title",
    desc: t("research.preset.custom.desc"),
    config: {},
  },
];

const defaultConfig: ResearchConfig = {
  eval_command: "",
  metric_pattern: "([\\d.]+)",
  metric_file: "",
  metric_name: "metric",
  lower_is_better: true,
  max_experiments: 10,
  max_wall_clock_minutes: 120,
  max_cost_usd: 5.0,
  invariants_command: "",
  protected_paths: [],
};

// ── Component ─────────────────────────────────────────────────────

export function ResearchView() {
  const { name } = useParams<{ name: string }>();
  const decodedName = decodeURIComponent(name || "");
  const { toastError } = useToast();
  // «←» и системная «Назад» ведут в одно место (navBack.ts): шаг по истории, а
  // по прямой ссылке — карточка сессии, которой принадлежит исследование.
  const goBack = useGoBack();

  const [state, setState] = useState<ResearchState | null>(null);
  const [config, setConfig] = useState<ResearchConfig>({ ...defaultConfig });
  const [task, setTask] = useState("");
  const [progress, setProgress] = useState("");
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [step, setStep] = useState(0); // 0=presets, 1=metric, 2=safety, 3=budget
  const [showSetup, setShowSetup] = useState(false);
  const [expandedExp, setExpandedExp] = useState<number | null>(null);
  const [protectedInput, setProtectedInput] = useState("");
  const [showAdvanced, setShowAdvanced] = useState(false);
  const [cwd, setCwd] = useState("");
  const chartRef = useRef<HTMLCanvasElement>(null);

  useEffect(() => {
    if (!decodedName) return;
    Promise.all([getResearch(decodedName), getSession(decodedName)])
      .then(([res, sess]) => {
        setState(res);
        setCwd(sess.cwd);
        if (res.config?.eval_command) {
          setConfig(res.config);
        }
      })
      .catch(() => {})
      .finally(() => setLoading(false));
  }, [decodedName]);

  useEffect(() => {
    return onWSEvent((ev: WSEvent) => {
      if (ev.type !== "research" || ev.session !== decodedName) return;
      if (ev.action === "started") setState(s => s ? { ...s, running: true, experiments: [] } : s);
      if (ev.action === "progress") setProgress(ev.text || "");
      if (ev.action === "experiment" && ev.experiment)
        setState(s => s ? { ...s, experiments: [...s.experiments, ev.experiment!] } : s);
      if (ev.action === "finished")
        setState(s => s ? { ...s, running: false, summary: ev.summary, improvement: ev.improvement || "",
          branch: (ev as any).branch || s?.branch, experiments: ev.experiments || s.experiments } : s);
    });
  }, [decodedName]);

  useEffect(() => {
    const canvas = chartRef.current;
    if (!canvas || !state?.experiments?.length) return;
    const render = () => drawChart(canvas, state.experiments, state.baseline, config.lower_is_better);
    render();
    // drawChart берёт размер из getBoundingClientRect, поэтому при резайзе окна
    // (особенно окна exe 600↔1920px) надо перерисовать — иначе битмап остаётся
    // фиксированным и график мылится/обрезается.
    const ro = new ResizeObserver(render);
    ro.observe(canvas);
    return () => ro.disconnect();
  }, [state?.experiments, state?.baseline, config.lower_is_better]);

  const handleSaveConfig = useCallback(async () => {
    setSaving(true);
    try { await saveResearchConfig(decodedName, config); } catch { /* */ }
    setSaving(false);
    setShowSetup(false);
    setStep(0);
  }, [decodedName, config]);

  const handleStart = useCallback(async () => {
    if (!config.eval_command) { setShowSetup(true); return; }
    try {
      await startResearch(decodedName, task || t("research.defaultTask"));
      setState(s => s ? { ...s, running: true, experiments: [] } : s);
      setTask("");
    } catch (e: any) { toastError(mapApiError(e)); }
  }, [decodedName, task, config.eval_command]);

  const handleStop = useCallback(async () => {
    try { await stopResearch(decodedName); } catch { /* */ }
  }, [decodedName]);

  const applyPreset = (p: Preset) => {
    if (p.id === "custom") {
      setStep(1);
      return;
    }
    setConfig(c => ({ ...c, ...p.config }));
    setStep(2); // skip to safety
  };

  const addProtectedPath = () => {
    if (!protectedInput.trim()) return;
    setConfig(c => ({ ...c, protected_paths: [...(c.protected_paths || []), protectedInput.trim()] }));
    setProtectedInput("");
  };

  const isConfigured = !!config.eval_command;
  const experiments = state?.experiments || [];
  const running = state?.running || false;

  if (loading) return <div className="page research-page"><div className="r-loading">{t("generic.loading")}</div></div>;

  // ── Setup wizard ──
  if (showSetup) return (
    <div className="page research-page">
      <div className="page-header">
        <button className="back-btn" aria-label={t("generic.back")} onClick={() => { if (step > 0) setStep(s => s - 1); else setShowSetup(false); }}>{"←"}</button>
        <h1>{t("research.setupTitle")}</h1>
        <div className="r-steps">
          {[t("research.step.template"), t("research.step.metric"), t("research.step.safety"), t("research.step.budget")].map((s, i) => (
            <div key={s} className={`r-step ${i === step ? "active" : i < step ? "done" : ""}`}
              onClick={() => i <= step && setStep(i)}>
              <div className="r-step-dot">{i < step ? "✓" : i + 1}</div>
              <div className="r-step-label">{s}</div>
            </div>
          ))}
        </div>
      </div>
      <div className="page-content">

        {/* Step 0: Presets */}
        {step === 0 && (
          <div className="r-section">
            <div className="r-section-title">{t("research.chooseTemplate")}</div>
            <div className="r-section-hint">{t("research.chooseTemplateHint")}</div>
            <div className="r-presets">
              {presets.map(p => (
                <button key={p.id} className="r-preset-card" onClick={() => applyPreset(p)}>
                  <div className="r-preset-icon">{p.icon}</div>
                  <div className="r-preset-title">{t(p.title)}</div>
                  <div className="r-preset-desc">{p.desc}</div>
                </button>
              ))}
            </div>
          </div>
        )}

        {/* Step 1: Metric config */}
        {step === 1 && (
          <div className="r-section">
            <div className="r-section-title">{t("research.whatToMeasure")}</div>
            <div className="r-section-hint">{t("research.whatToMeasureHint")}</div>

            <div className="r-field">
              <label className="r-field-label">{t("research.evalCommand")}</label>
              <div className="r-field-hint">{t("research.evalCommandHint")}</div>
              <input className="r-input" value={config.eval_command}
                onChange={e => setConfig({ ...config, eval_command: e.target.value })}
                placeholder="go test -bench=. -benchmem ./..." />
            </div>

            <div className="r-field">
              <label className="r-field-label">{t("research.howExtract")}</label>
              <div className="r-field-hint">{t("research.howExtractHint")}</div>
              <input className="r-input" value={config.metric_pattern}
                onChange={e => setConfig({ ...config, metric_pattern: e.target.value })}
                placeholder="(\d+)\s*ns/op" />
            </div>

            <div className="r-row">
              <div className="r-field" style={{ flex: 1 }}>
                <label className="r-field-label">{t("research.metricLabel")}</label>
                <input className="r-input" value={config.metric_name}
                  onChange={e => setConfig({ ...config, metric_name: e.target.value })}
                  placeholder="ns/op" />
              </div>
              <div className="r-field" style={{ flex: 1 }}>
                <label className="r-field-label">{t("research.goal")}</label>
                <div className="r-toggle-group">
                  <button className={`r-toggle ${config.lower_is_better ? "active" : ""}`}
                    onClick={() => setConfig({ ...config, lower_is_better: true })}>
                    <span className="r-toggle-icon">{"⬇"}</span> {t("research.minimize")}
                  </button>
                  <button className={`r-toggle ${!config.lower_is_better ? "active" : ""}`}
                    onClick={() => setConfig({ ...config, lower_is_better: false })}>
                    <span className="r-toggle-icon">{"⬆"}</span> {t("research.maximize")}
                  </button>
                </div>
              </div>
            </div>

            {/* Advanced */}
            <button className="r-advanced-toggle" onClick={() => setShowAdvanced(!showAdvanced)}>
              {showAdvanced ? "▼" : "▶"} {t("research.advanced")}
            </button>
            {showAdvanced && (
              <div className="r-advanced">
                <div className="r-field">
                  <label className="r-field-label">{t("research.metricFile")}</label>
                  <div className="r-field-hint">{t("research.metricFileHint")}</div>
                  <input className="r-input" value={config.metric_file || ""}
                    onChange={e => setConfig({ ...config, metric_file: e.target.value })}
                    placeholder="result.txt" />
                </div>
              </div>
            )}

            <button className="r-next-btn" onClick={() => setStep(2)} disabled={!config.eval_command}>
              {t("research.nextSafety")} {"→"}
            </button>
          </div>
        )}

        {/* Step 2: Safety */}
        {step === 2 && (
          <div className="r-section">
            <div className="r-section-title">{t("research.safetyTitle")}</div>
            <div className="r-section-hint">{t("research.safetyHint")}</div>

            <div className="r-field">
              <label className="r-field-label">{t("research.invariants")}</label>
              <div className="r-field-hint">{t("research.invariantsHint")}</div>
              <input className="r-input" value={config.invariants_command}
                onChange={e => setConfig({ ...config, invariants_command: e.target.value })}
                placeholder="go test ./..." />
              {!config.invariants_command && (
                <div className="r-field-warn">{t("research.invariantsWarn")}</div>
              )}
            </div>

            <div className="r-field">
              <label className="r-field-label">{t("research.protectedFiles")}</label>
              <div className="r-field-hint">{t("research.protectedHint")}</div>
              <div className="r-row" style={{ gap: 4 }}>
                <input className="r-input" style={{ flex: 1 }} value={protectedInput}
                  onChange={e => setProtectedInput(e.target.value)}
                  onKeyDown={e => e.key === "Enter" && addProtectedPath()}
                  placeholder={t("research.protectedPlaceholder")} />
                <button className="r-add-btn" onClick={addProtectedPath} aria-label={t("generic.add")}>+</button>
              </div>
              {(config.protected_paths || []).length > 0 && (
                <div className="r-tags">
                  {config.protected_paths.map((p, i) => (
                    <span key={i} className="r-tag">
                      {p}
                      <button aria-label={t("generic.remove")} onClick={() => setConfig(c => ({
                        ...c, protected_paths: c.protected_paths.filter((_, j) => j !== i)
                      }))}>{"×"}</button>
                    </span>
                  ))}
                </div>
              )}
            </div>

            <div className="r-info-box">
              <div className="r-info-icon">{"🔒"}</div>
              <div className="r-info-text">{t("research.branchInfo")}</div>
            </div>

            <button className="r-next-btn" onClick={() => setStep(3)}>
              {t("research.nextBudget")} {"→"}
            </button>
          </div>
        )}

        {/* Step 3: Budget + Save */}
        {step === 3 && (
          <div className="r-section">
            <div className="r-section-title">{t("research.budgetTitle")}</div>
            <div className="r-section-hint">{t("research.budgetHint")}</div>

            <div className="r-budget-grid">
              <div className="r-budget-card">
                <div className="r-budget-icon">{"🧪"}</div>
                <div className="r-budget-label">{t("research.budgetExperiments")}</div>
                <input className="r-input r-budget-input" type="number" value={config.max_experiments}
                  onChange={e => setConfig({ ...config, max_experiments: parseInt(e.target.value) || 10 })} />
              </div>
              <div className="r-budget-card">
                <div className="r-budget-icon">{"⏱"}</div>
                <div className="r-budget-label">{t("research.budgetMinutes")}</div>
                <input className="r-input r-budget-input" type="number" value={config.max_wall_clock_minutes}
                  onChange={e => setConfig({ ...config, max_wall_clock_minutes: parseInt(e.target.value) || 120 })} />
              </div>
              <div className="r-budget-card">
                <div className="r-budget-icon">{"💰"}</div>
                <div className="r-budget-label">{t("research.budgetUsd")}</div>
                <input className="r-input r-budget-input" type="number" step="0.5" value={config.max_cost_usd}
                  onChange={e => setConfig({ ...config, max_cost_usd: parseFloat(e.target.value) || 5 })} />
              </div>
            </div>

            {/* Summary */}
            <div className="r-summary-box">
              <div className="r-summary-title">{t("research.summaryTitle")}</div>
              <div className="r-summary-row"><span>{t("research.summaryEval")}</span><code>{config.eval_command || t("research.notSet")}</code></div>
              <div className="r-summary-row"><span>{t("research.summaryMetric")}</span><code>{config.metric_name} ({config.lower_is_better ? t("research.minimizeShort") : t("research.maximizeShort")})</code></div>
              <div className="r-summary-row"><span>{t("research.summaryInvariants")}</span><code>{config.invariants_command || t("research.none")}</code></div>
              <div className="r-summary-row"><span>{t("research.summaryProtected")}</span><code>{(config.protected_paths||[]).join(", ") || t("research.none")}</code></div>
              <div className="r-summary-row"><span>{t("research.summaryBudget")}</span><code>{t("research.budgetSummary", { exp: config.max_experiments, min: config.max_wall_clock_minutes, cost: config.max_cost_usd })}</code></div>
            </div>

            <button className="r-save-btn" onClick={handleSaveConfig} disabled={saving || !config.eval_command}>
              {saving ? t("research.saving") : t("research.saveReady")}
            </button>
          </div>
        )}
      </div>
    </div>
  );

  // ── Main view ──
  return (
    <div className="page research-page">
      <div className="page-header">
        <button className="back-btn" aria-label={t("generic.back")} onClick={goBack}>{"←"}</button>
        <h1>{t("research.title")}</h1>
        {isConfigured && <button className="header-action" onClick={() => { setStep(1); setShowSetup(true); }} title={t("research.editConfig")}>{"⚙"}</button>}
      </div>

      <div className="page-content">
        {/* Chart */}
        {experiments.length > 0 && (
          <div className="r-chart-wrap">
            <canvas ref={chartRef} className="r-chart" />
          </div>
        )}

        {/* Result banner */}
        {state?.improvement && !running && (
          <div className="r-result-banner">
            <div className="r-result-label">{t("research.result")}</div>
            <div className="r-result-value">{state.improvement}</div>
            {state.branch && <div className="r-result-branch">{t("research.branch")} <code>{state.branch}</code></div>}
          </div>
        )}

        {/* Progress */}
        {running && (
          <div className="r-progress-bar">
            <div className="r-progress-pulse" />
            <span className="r-progress-text">{progress || t("research.starting")}</span>
            <button className="r-stop-btn" onClick={handleStop}>{t("research.stop")}</button>
          </div>
        )}

        {/* Experiments */}
        {experiments.length > 0 && (
          <div className="r-experiments">
            <div className="r-exp-header">{t("research.experimentsCount", { n: experiments.length })}</div>
            {experiments.map(exp => (
              <div key={exp.id} className={`r-exp ${exp.kept ? "kept" : "reverted"}`}
                onClick={() => setExpandedExp(expandedExp === exp.id ? null : exp.id)}>
                <div className="r-exp-row">
                  <span className="r-exp-num">#{exp.id}</span>
                  <span className="r-exp-status">{exp.kept ? "✅" : "❌"}{!exp.invariants_passed ? " ⚠️" : ""}</span>
                  <span className="r-exp-desc">{exp.description}</span>
                  <span className="r-exp-delta">{exp.delta}</span>
                </div>
                {expandedExp === exp.id && (
                  <div className="r-exp-details">
                    <div className="r-exp-detail-row">{t("research.summaryMetric")} <b>{exp.metric.toPrecision(6)} {config.metric_name}</b></div>
                    <div className="r-exp-detail-row">{t("research.expInvariants")} {exp.invariants_passed ? t("research.passed") : t("research.failed")}</div>
                    {exp.files_changed && exp.files_changed.length > 0 && (
                      <div className="r-exp-detail-row">{t("research.expFiles")} {exp.files_changed.join(", ")}</div>
                    )}
                    {exp.diff && <pre className="r-diff">{exp.diff}</pre>}
                  </div>
                )}
              </div>
            ))}
          </div>
        )}

        {/* Empty / not configured */}
        {!isConfigured && !running && experiments.length === 0 && (
          <div className="r-empty">
            <div className="r-empty-flow">
              <div className="r-flow-step"><div className="r-flow-icon">{"📊"}</div><div>{t("research.flowMeasure")}<br/><small>{t("research.flowBaseline")}</small></div></div>
              <div className="r-flow-arrow">{"→"}</div>
              <div className="r-flow-step"><div className="r-flow-icon">{"🧪"}</div><div>{t("research.flowExperiment")}<br/><small>{t("research.flowChangeCode")}</small></div></div>
              <div className="r-flow-arrow">{"→"}</div>
              <div className="r-flow-step"><div className="r-flow-icon">{"✅"}</div><div>{t("research.flowVerify")}<br/><small>{t("research.flowTestsPass")}</small></div></div>
              <div className="r-flow-arrow">{"→"}</div>
              <div className="r-flow-step"><div className="r-flow-icon">{"📈"}</div><div>{t("research.flowBetter")}<br/><small>{t("research.flowKeepRevert")}</small></div></div>
            </div>
            <div className="r-empty-title">{t("research.emptyTitle")}</div>
            <div className="r-empty-desc">
              {t("research.emptyDesc")}
            </div>
            <button className="r-setup-btn" onClick={() => setShowSetup(true)}>
              {t("research.setupBtn")}
            </button>
          </div>
        )}

        {/* Configured, ready to start */}
        {isConfigured && !running && (
          <div className="r-start-section">
            <div className="r-config-summary">
              <span>{config.metric_name}</span>
              <span className="r-config-sep">{"·"}</span>
              <span>{config.lower_is_better ? t("research.minimizeShort") : t("research.maximizeShort")}</span>
              <span className="r-config-sep">{"·"}</span>
              <span>{config.max_experiments} {t("research.expShort")}</span>
              <span className="r-config-sep">{"·"}</span>
              <span>${config.max_cost_usd} {t("research.maxShort")}</span>
            </div>
            <textarea className="r-task-input" value={task}
              onChange={e => setTask(e.target.value)} rows={2}
              placeholder={t("research.taskPlaceholder")} />
            <button className="r-go-btn" onClick={handleStart}>
              {t("research.startBtn")}
            </button>
          </div>
        )}

        {cwd && <div className="r-cwd">{cwd}</div>}
      </div>
    </div>
  );
}

// ── Chart ──────────────────────────────────────────────────────────

function drawChart(canvas: HTMLCanvasElement, experiments: ResearchExperiment[], baseline: number, lowerIsBetter: boolean) {
  const ctx = canvas.getContext("2d");
  if (!ctx) return;
  const dpr = window.devicePixelRatio || 1;
  const rect = canvas.getBoundingClientRect();
  canvas.width = rect.width * dpr;
  canvas.height = rect.height * dpr;
  ctx.scale(dpr, dpr);
  const W = rect.width, H = rect.height;
  ctx.clearRect(0, 0, W, H);

  const points = [
    { value: baseline, kept: true, label: t("research.chartBase") },
    ...experiments.map(e => ({ value: e.metric, kept: e.kept, label: `#${e.id}` })),
  ];
  if (points.length < 2) return;

  const values = points.map(p => p.value);
  let minV = Math.min(...values), maxV = Math.max(...values);
  const range = maxV - minV || 1;
  minV -= range * 0.15; maxV += range * 0.15;

  const padL = 52, padR = 12, padT = 8, padB = 22;
  const cW = W - padL - padR, cH = H - padT - padB;
  const x = (i: number) => padL + (i / (points.length - 1)) * cW;
  const y = (v: number) => padT + (1 - (v - minV) / (maxV - minV)) * cH;

  // Grid
  ctx.strokeStyle = "rgba(255,255,255,0.06)"; ctx.lineWidth = 1;
  for (let i = 0; i <= 3; i++) {
    const yy = padT + (i / 3) * cH;
    ctx.beginPath(); ctx.moveTo(padL, yy); ctx.lineTo(W - padR, yy); ctx.stroke();
    ctx.fillStyle = "rgba(255,255,255,0.3)"; ctx.font = "9px -apple-system, sans-serif"; ctx.textAlign = "right";
    ctx.fillText((maxV - (i / 3) * (maxV - minV)).toPrecision(4), padL - 6, yy + 3);
  }

  // Baseline
  ctx.strokeStyle = "rgba(255,255,255,0.15)"; ctx.setLineDash([4, 4]);
  ctx.beginPath(); ctx.moveTo(padL, y(baseline)); ctx.lineTo(W - padR, y(baseline)); ctx.stroke();
  ctx.setLineDash([]);

  // Area under line
  ctx.beginPath();
  points.forEach((p, i) => { i === 0 ? ctx.moveTo(x(i), y(p.value)) : ctx.lineTo(x(i), y(p.value)); });
  ctx.lineTo(x(points.length - 1), padT + cH);
  ctx.lineTo(x(0), padT + cH);
  ctx.closePath();
  const grad = ctx.createLinearGradient(0, padT, 0, padT + cH);
  grad.addColorStop(0, "rgba(88, 166, 255, 0.15)");
  grad.addColorStop(1, "rgba(88, 166, 255, 0)");
  ctx.fillStyle = grad;
  ctx.fill();

  // Line
  ctx.strokeStyle = "#58a6ff"; ctx.lineWidth = 2;
  ctx.beginPath();
  points.forEach((p, i) => { i === 0 ? ctx.moveTo(x(i), y(p.value)) : ctx.lineTo(x(i), y(p.value)); });
  ctx.stroke();

  // Points
  points.forEach((p, i) => {
    const color = i === 0 ? "#8b949e" : p.kept ? "#3fb950" : "#f85149";
    // Glow
    ctx.beginPath(); ctx.arc(x(i), y(p.value), 8, 0, Math.PI * 2);
    ctx.fillStyle = color.replace(")", ", 0.2)").replace("rgb", "rgba"); ctx.fill();
    // Dot
    ctx.beginPath(); ctx.arc(x(i), y(p.value), 3.5, 0, Math.PI * 2);
    ctx.fillStyle = color; ctx.fill();
    // Label
    ctx.fillStyle = "rgba(255,255,255,0.4)"; ctx.font = "8px -apple-system, sans-serif"; ctx.textAlign = "center";
    ctx.fillText(p.label, x(i), H - 4);
  });
}
