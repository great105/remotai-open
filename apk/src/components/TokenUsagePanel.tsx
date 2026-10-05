import { useCallback, useState } from "react";
import { getTokenUsage, mapApiError, t } from "@tgcontrol/shared";
import { usePolling } from "../hooks/usePolling";
import { haptic } from "../telegram";

/**
 * Расход токенов агентами этого компьютера (раздел «Агенты»).
 *
 * Цифры считает сам компьютер по файлам сессий Claude Code и Codex
 * (internal/tokenusage) — здесь только показ. Кеш вынесен отдельно: у Claude
 * это 95–99 % всех токенов, и одно большое число без разбивки ничего не
 * объясняет, только пугает.
 */

interface Tokens {
  input: number; output: number; cache_read: number; cache_write: number;
  reasoning: number; calls: number; total: number;
}
interface NamedRow { key: string; provider?: string; label?: string; sessions?: number; tokens: Tokens }
interface SessionRow {
  session: string; provider: string; account: string; project: string; model: string; last: number; tokens: Tokens;
}
export interface TokenUsageReport {
  days: number;
  progress: { running: boolean; complete: boolean; done_bytes: number; all_bytes: number; scanned_at?: number };
  total: Tokens;
  by_day: { day: string; tokens: Tokens }[];
  by_project: NamedRow[];
  by_model: NamedRow[];
  by_account: NamedRow[];
  top_sessions: SessionRow[];
}

const PERIODS = [
  { days: 1, label: "tokens.today" },
  { days: 7, label: "tokens.days7" },
  { days: 30, label: "tokens.days30" },
] as const;

/** 1 339 732 609 → «1,34 млрд»; до тысячи — как есть. */
export function compactTokens(n: number): string {
  const units: [number, string][] = [[1e9, "tokens.unitB"], [1e6, "tokens.unitM"], [1e3, "tokens.unitK"]];
  for (const [base, key] of units) {
    if (Math.abs(n) >= base) {
      const v = n / base;
      const digits = v >= 100 ? 0 : v >= 10 ? 1 : 2;
      return `${v.toFixed(digits).replace(".", ",").replace(/,?0+$/, "")} ${t(key)}`;
    }
  }
  return String(Math.round(n));
}

/** Последние две части пути: «…\Разаработки\TGControl-ALL». */
export function shortPath(p: string): string {
  if (!p) return t("tokens.noProject");
  const parts = p.split(/[\\/]+/).filter(Boolean);
  return parts.length <= 2 ? parts.join("/") : `…/${parts.slice(-2).join("/")}`;
}

function when(unix: number): string {
  if (!unix) return "";
  const d = new Date(unix * 1000);
  const today = new Date();
  const hm = d.toLocaleTimeString("ru-RU", { hour: "2-digit", minute: "2-digit" });
  if (d.toDateString() === today.toDateString()) return hm;
  return `${d.toLocaleDateString("ru-RU", { day: "numeric", month: "short" })}, ${hm}`;
}

function Bars({ rows }: { rows: TokenUsageReport["by_day"] }) {
  const max = Math.max(1, ...rows.map(r => r.tokens.total));
  return (
    <div className="tokens-bars" role="img" aria-label={t("tokens.byDay")}>
      {rows.map(r => (
        <div key={r.day} className="tokens-bar" title={`${r.day}: ${compactTokens(r.tokens.total)}`}>
          <span style={{ height: `${Math.max(r.tokens.total > 0 ? 3 : 0, (r.tokens.total / max) * 100)}%` }} />
        </div>
      ))}
    </div>
  );
}

function List({ title, rows, name, meta }: {
  title: string;
  rows: NamedRow[];
  name: (r: NamedRow) => string;
  meta?: (r: NamedRow) => string;
}) {
  if (rows.length === 0) return null;
  const max = Math.max(1, rows[0]?.tokens.total ?? 1);
  return (
    <div className="tokens-list">
      <h3>{title}</h3>
      {rows.map(r => (
        <div key={r.key} className="tokens-row">
          <div className="tokens-row-head">
            <span className="tokens-row-name" title={r.key}>{name(r)}</span>
            <span className="tokens-row-value">{compactTokens(r.tokens.total)}</span>
          </div>
          <div className="tokens-row-meter"><span style={{ width: `${(r.tokens.total / max) * 100}%` }} /></div>
          {meta && <small>{meta(r)}</small>}
        </div>
      ))}
    </div>
  );
}

export function TokenUsagePanel() {
  const [days, setDays] = useState<number>(7);
  const [report, setReport] = useState<TokenUsageReport | null>(null);
  const [error, setError] = useState<string>("");

  const load = useCallback(async () => {
    try {
      const r = await getTokenUsage<TokenUsageReport>(days);
      setReport(r);
      setError("");
    } catch (e) {
      const status = (e as { status?: number })?.status;
      setError(status === 404 ? t("tokens.needsUpdate") : mapApiError(e));
    }
  }, [days]);
  // Идёт первый подсчёт — показываем прогресс чаще; дальше раз в минуту.
  usePolling(load, report?.progress.running ? 5000 : 60000);

  const pick = (d: number) => {
    if (d === days) return;
    haptic();
    setDays(d);
    setReport(null);
  };

  const r = report && report.days === days ? report : null;
  const scanning = r && r.progress.running && !r.progress.complete;
  const pct = r && r.progress.all_bytes > 0 ? Math.floor((r.progress.done_bytes / r.progress.all_bytes) * 100) : 0;
  const io = r ? r.total.input + r.total.output + r.total.cache_write : 0;

  return (
    <div className="tokens-panel">
      <div className="tokens-periods" role="tablist">
        {PERIODS.map(p => (
          <button
            key={p.days}
            role="tab"
            aria-selected={p.days === days}
            className={`tokens-period${p.days === days ? " active" : ""}`}
            onClick={() => pick(p.days)}
          >{t(p.label)}</button>
        ))}
      </div>

      {error && !r && <p className="tokens-note tokens-error">{error}</p>}
      {!r && !error && <p className="tokens-note">…</p>}

      {r && (
        <>
          {scanning && (
            <p className="tokens-note">{t("tokens.scanning", { pct })}<br />{t("tokens.scanningFirst")}</p>
          )}
          <div className="tokens-total">
            <small>{t("tokens.total")}</small>
            <strong>{compactTokens(r.total.total)}</strong>
            {r.total.total > 0 && (
              <span>{t("tokens.split", { io: compactTokens(io), cache: compactTokens(r.total.cache_read) })}</span>
            )}
            {r.total.calls > 0 && <small>{t("tokens.calls", { n: r.total.calls, count: r.total.calls.toLocaleString("ru-RU") })}</small>}
          </div>
          {r.total.total === 0 && !scanning && <p className="tokens-note">{t("tokens.empty")}</p>}
          {r.total.total > 0 && (
            <>
              {r.by_day.length > 1 && <Bars rows={r.by_day} />}
              <List
                title={t("tokens.byAccount")}
                rows={r.by_account}
                name={row => `${row.provider === "codex" ? "Codex" : "Claude"} · ${row.label || row.key.split("|")[1]}`}
              />
              <List title={t("tokens.byModel")} rows={r.by_model} name={row => row.key} />
              <List
                title={t("tokens.byProject")}
                rows={r.by_project}
                name={row => shortPath(row.key)}
                meta={row => t("tokens.sessions", { n: row.sessions ?? 0 })}
              />
              {r.top_sessions.length > 0 && (
                <List
                  title={t("tokens.topSessions")}
                  rows={r.top_sessions.map(s => ({ key: `${s.provider}|${s.session}`, tokens: s.tokens, label: s.project, provider: s.model }))}
                  name={row => shortPath(row.label ?? "")}
                  meta={row => {
                    const s = r.top_sessions.find(x => `${x.provider}|${x.session}` === row.key);
                    return s ? `${s.model} · ${when(s.last)}` : "";
                  }}
                />
              )}
              <p className="tokens-note">{t("tokens.cacheHint")}</p>
            </>
          )}
          <p className="tokens-note">{t("tokens.source")}</p>
        </>
      )}
    </div>
  );
}
