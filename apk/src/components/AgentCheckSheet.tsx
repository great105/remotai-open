import { useCallback, useEffect, useId, useRef, useState } from "react";
import {
  authText, checkHint, freeCheckModel, latencyText, mapApiError, modelText, noteText,
  reasonView, routeSummary, shadowText, SheetShell, sourceText, t, useEscape, whyText,
  type AgentCheckItem, type AgentCheckResult, type AgentConnection,
} from "@tgcontrol/shared";
import { getAgentConnection, runAgentCheck } from "../api";
import { haptic } from "../telegram";
import { IconCheck, IconClose, IconWarning } from "./icons";
import "./agentCheck.css";

/**
 * Шторка «Проверить подключение».
 *
 * Отвечает на два вопроса по порядку. Сверху — ЧТО агент реально использует:
 * куда ходит, чем входит, какой моделью, и откуда каждое значение (переменная
 * окружения, аккаунт, настройки агента). Проигравшие значения показаны тут же,
 * тише: самая дорогая путаница — «я плачу за подписку, а агент тратит ключ».
 * Снизу — одна настоящая проверка с ответом модели, а не «адрес отвечает».
 *
 * Слова и правила — в @tgcontrol/shared (agentCheck.ts), здесь только вёрстка.
 */

export interface AgentCheckAccount {
  id: string;
  title: string;
}

interface Props {
  agentID: string;
  agentName: string;
  /** Аккаунты агента на этом компьютере; пусто — агент их не умеет. */
  accounts?: AgentCheckAccount[];
  /** С каким аккаунтом открыть (обычно активный). */
  initialAccount?: string;
  onClose: () => void;
}

type CheckState =
  | { phase: "idle" }
  | { phase: "running"; startedAt: number; model: string }
  | { phase: "done"; result: AgentCheckResult }
  | { phase: "error"; message: string };

export function AgentCheckSheet({ agentID, agentName, accounts = [], initialAccount = "", onClose }: Props) {
  useEscape(true, onClose);
  const titleID = useId();
  const [account, setAccount] = useState(initialAccount);
  const [conn, setConn] = useState<AgentConnection | null>(null);
  const [loadError, setLoadError] = useState("");
  const [reload, setReload] = useState(0);
  const [check, setCheck] = useState<CheckState>({ phase: "idle" });
  const [now, setNow] = useState(Date.now());
  const abortRef = useRef<AbortController | null>(null);

  useEffect(() => {
    let alive = true;
    setConn(null);
    setLoadError("");
    setCheck({ phase: "idle" });
    getAgentConnection(agentID, account)
      .then((c) => { if (alive) setConn(c); })
      .catch((e) => { if (alive) setLoadError(mapApiError(e)); });
    return () => { alive = false; };
  }, [agentID, account, reload]);

  // Секундомер во время проверки: по подписке она идёт до минуты, и без
  // счёта времени шторка выглядит зависшей.
  useEffect(() => {
    if (check.phase !== "running") return;
    const timer = window.setInterval(() => setNow(Date.now()), 500);
    return () => window.clearInterval(timer);
  }, [check.phase]);

  useEffect(() => () => abortRef.current?.abort(), []);

  // Итог появляется под кнопкой, а на телефоне это ниже края шторки: без
  // прокрутки человек нажал «Проверить» и не увидел ответа (замер 320 px).
  const resultRef = useRef<HTMLDivElement | null>(null);
  useEffect(() => {
    if (check.phase === "done" || check.phase === "error") {
      resultRef.current?.scrollIntoView({ block: "nearest" });
    }
  }, [check.phase]);

  const run = useCallback(async (model: string) => {
    haptic();
    abortRef.current?.abort();
    const ctrl = new AbortController();
    abortRef.current = ctrl;
    const startedAt = Date.now();
    setNow(startedAt);
    setCheck({ phase: "running", startedAt, model });
    try {
      const { result, connection } = await runAgentCheck(agentID, { account, model, signal: ctrl.signal });
      if (ctrl.signal.aborted) return;
      if (connection) setConn(connection);
      setCheck({ phase: "done", result });
    } catch (e) {
      if (ctrl.signal.aborted) return;
      setCheck({ phase: "error", message: mapApiError(e) });
    }
  }, [agentID, account]);

  const running = check.phase === "running";
  const free = conn ? freeCheckModel(conn) : null;
  const canRun = Boolean(conn && conn.check);
  const showAccounts = accounts.length > 1;

  return (
    <SheetShell open onClose={onClose} overlayClassName="modal-overlay" className="modal-sheet ac-sheet" labelledBy={titleID}>
        <div className="ac-head">
          <h2 className="modal-title ac-title" id={titleID}>{t("agentCheck.title", { agent: agentName })}</h2>
          <button className="icon-btn ac-close" onClick={onClose} aria-label={t("modal.close")}>
            <IconClose size={20} />
          </button>
        </div>

        {showAccounts && (
          <div className="ac-accounts" role="radiogroup" aria-label={t("agentCheck.account")}>
            {accounts.map((a) => (
              <button
                key={a.id}
                role="radio"
                aria-checked={a.id === account}
                className={`ac-account${a.id === account ? " is-on" : ""}`}
                disabled={running}
                onClick={() => { haptic(); setAccount(a.id); }}
              >
                {a.title}
              </button>
            ))}
          </div>
        )}

        {!conn && !loadError && <p className="ac-muted" role="status">{t("agentCheck.loading")}</p>}
        {loadError && <div className="ac-load-error">
          <p className="ac-error" role="alert">{t("agentCheck.loadFailed", { error: loadError })}</p>
          <button type="button" className="btn btn-secondary" onClick={() => { haptic(); setReload((value) => value + 1); }}>
            {t("agentCheck.retry")}
          </button>
        </div>}

        {conn && (
          <>
            <p className={`ac-verdict ac-route-${conn.route}`}>{routeSummary(conn)}</p>

            {conn.supported && (
              <section className="ac-uses" aria-labelledby={`${titleID}-uses`}>
                <h3 className="ac-uses-title" id={`${titleID}-uses`}>{t("agentCheck.uses")}</h3>
                <dl className="ac-facts">
                  <Fact label={t("agentCheck.field.endpoint")} value={conn.endpoint.value || "—"} item={conn.endpoint} mono />
                  <Fact
                    label={t("agentCheck.field.auth")}
                    value={authText(conn.auth)}
                    item={conn.auth}
                    extra={conn.login_file === undefined ? "" : t(conn.login_file ? "agentCheck.loginFileYes" : "agentCheck.loginFileNo")}
                  />
                  <Fact label={t("agentCheck.field.model")} value={modelText(conn.model)} item={conn.model} mono={Boolean(conn.model.value)} />
                  {conn.proxy && (
                    <div className="ac-fact">
                      <dt>{t("agentCheck.field.proxy")}</dt>
                      <dd><span className="ac-value ac-mono">{conn.proxy}</span></dd>
                    </div>
                  )}
                </dl>
              </section>
            )}

            {(conn.notes || []).map(noteText).filter(Boolean).map((text) => (
              <p key={text} className="ac-note"><IconWarning size={16} /> <span>{text}</span></p>
            ))}

            <div className="ac-actions">
              <button className="btn btn-primary ac-run" disabled={!canRun || running} onClick={() => void run("")}>
                {check.phase === "running"
                  ? t("agentCheck.checking", { s: Math.max(0, Math.round((now - check.startedAt) / 1000)) })
                  : check.phase === "done" ? t("agentCheck.checkAgain") : t("agentCheck.check")}
              </button>
              {free && (
                <button className="btn btn-secondary ac-run-free" disabled={running} onClick={() => void run(free)}>
                  {t("agentCheck.checkFree")}
                </button>
              )}
              <p className="ac-muted ac-hint">{checkHint(conn)}</p>
            </div>

            <div className="ac-result-slot" aria-live="polite" ref={resultRef}>
              {check.phase === "done" && <CheckResult result={check.result} />}
              {check.phase === "error" && (
                <p className="ac-error" role="alert">{t("agentCheck.checkFailed", { error: check.message })}</p>
              )}
            </div>
          </>
        )}
    </SheetShell>
  );
}

function Fact({ label, value, item, mono, extra }: {
  label: string;
  value: string;
  item: AgentCheckItem;
  mono?: boolean;
  extra?: string;
}) {
  const why = whyText(item.why);
  return (
    <div className="ac-fact">
      <dt>{label}</dt>
      <dd>
        <span className={`ac-value${mono ? " ac-mono" : ""}`}>{value}</span>
        <span className="ac-src">{t("agentCheck.from", { source: sourceText(item.source) })}</span>
        {why && <span className="ac-why">{why}</span>}
        {extra && <span className="ac-src">{extra}</span>}
        {(item.overridden || []).map((s, i) => (
          <span key={i} className="ac-shadow">{shadowText(s)}</span>
        ))}
      </dd>
    </div>
  );
}

function CheckResult({ result }: { result: AgentCheckResult }) {
  const meta = [result.model, latencyText(result.latency_ms)].filter(Boolean).join(" · ");
  if (result.ok) {
    const empty = result.reason === "empty_reply";
    return (
      <div className="ac-result is-ok">
        <div className="ac-result-head">
          <span className="ac-result-icon"><IconCheck size={18} /></span>
          <b>{empty ? t("agentCheck.okEmpty") : t("agentCheck.ok")}</b>
        </div>
        {!empty && result.reply && <blockquote className="ac-reply">«{result.reply}»</blockquote>}
        {meta && <div className="ac-meta ac-mono">{meta}</div>}
      </div>
    );
  }
  const view = reasonView(result);
  const metaFail = [meta, result.http_status ? t("agentCheck.httpCode", { code: result.http_status }) : ""]
    .filter(Boolean).join(" · ");
  return (
    <div className="ac-result is-fail" role="alert">
      <div className="ac-result-head">
        <span className="ac-result-icon"><IconWarning size={18} /></span>
        <b>{view.title}</b>
      </div>
      <p className="ac-result-hint">{view.hint}</p>
      {metaFail && <div className="ac-meta ac-mono">{metaFail}</div>}
      {result.detail && (
        <details className="ac-detail">
          <summary>{t("agentCheck.detail")}</summary>
          <p className="ac-mono">{result.detail}</p>
        </details>
      )}
    </div>
  );
}
