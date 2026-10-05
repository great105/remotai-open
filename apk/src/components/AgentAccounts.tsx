import { Fragment, useCallback, useEffect, useRef, useState } from "react";
import { agentDisplayName, mapApiError } from "@tgcontrol/shared";
import {
  getAgentAccounts, saveAgentAccount, activateAgentAccount, deleteAgentAccount, pinAgentAccount,
} from "../api";
import type { AgentAccount } from "../types";
import { fetchLocalAIUsage, invalidateSelectedAIUsage } from "../aiUsage";
import { useAIUsage, useUsageNow } from "../hooks/useAIUsage";
import { mainUsageWindows, usageAgeLabel, usageIsStale } from "../aiUsagePolicy";
import type { AIUsageSnapshot } from "../aiUsage";
import { haptic, tgConfirm } from "../telegram";
import { IconPin } from "./icons";
import { t } from "../i18n";
import { normalizeAccountProxyForClient } from "../ptyTerm/agentLaunch";

/**
 * Аккаунты нейросетей — общий кусок для шторки терминала и раздела «Агенты».
 *
 * Живёт отдельным модулем ровно потому, что мест ДВА: у человека несколько
 * подписок на одного вендора, и переключаться он хочет и на бегу (в терминале,
 * когда упёрся в лимит), и спокойно (в разделе, где настраивает всё сразу).
 * Две копии этой логики разъехались бы на первой же правке — это ровно та
 * ошибка, из-за которой свои команды когда-то жили двумя несвязанными
 * списками.
 *
 * Что важно помнить про саму механику: аккаунт у CLI-агента — это КАТАЛОГ,
 * задаваемый переменной окружения (её называет реестр на Go, клиент не
 * хардкодит). Поэтому вторая подписка ничего не разлогинивает, а «войти» —
 * это всегда действие человека внутри самого CLI: токен выдаёт вендор.
 */
export interface AgentAccountsState {
  accounts: AgentAccount[];
	/** Версия proxy transport contract компьютера; 0 означает старый агент. */
	proxyContractVersion: number;
  /** Ответ `/api/accounts` получен именно для текущего cwd. */
  accountsReady: boolean;
  /** Ошибка последней загрузки; при ней запуск account-capable CLI закрыт. */
  accountsLoadError: string;
  usage: AIUsageSnapshot | null;
  busy: boolean;
  error: string;
  /** Что стало общим у последнего заведённого аккаунта (скиллы, настройки). */
  lastShared: SharedNote | null;
  /** Папка, с учётом которой посчитан выбор (для закрепления). */
  cwd: string;
  reload: () => void;
  pick: (agentID: string, id: string) => Promise<void>;
  add: (agentID: string, label: string) => Promise<AgentAccount | null>;
  remove: (id: string) => Promise<void>;
  pin: (agentID: string, id: string) => Promise<void>;
  /**
   * Свой прокси у аккаунта (задача владельца от 05.08.2026): рабочая подписка
   * ходит через рабочий канал, личная — напрямую. Системного прокси для этого
   * мало, он один на всю машину. Пустая строка снимает.
   *
   * Возвращает итог ЖИВОЙ проверки самого proxy endpoint: предупреждение,
   * если его не удалось подтвердить, и выходной IP проверочного запроса.
   * Поддержка transport-а CLI допускается сервером отдельно, fail-closed.
   */
  setProxy: (agentID: string, id: string, proxy: string) => Promise<{ ok: boolean; warning: string; exit: string }>;
  clearError: () => void;
}

/** Отчёт «что у нового аккаунта общее с основным» — как его прислал агент. */
export interface SharedNote {
  titles: string[];
  mcpServers: number;
}

/**
 * `enabled=false` — не ходим на агента вовсе (SSH-терминал: каталоги аккаунтов
 * лежат на компьютере, на сервере их нет).
 *
 * `cwd` — папка терминала: с ней сервер учитывает закрепление аккаунта за
 * папкой, и «активный» приезжает уже посчитанным для этого места.
 */
export function useAgentAccounts(enabled = true, cwd = ""): AgentAccountsState {
  const [accounts, setAccounts] = useState<AgentAccount[]>([]);
	const [proxyContractVersion, setProxyContractVersion] = useState(0);
  const [accountsLoad, setAccountsLoad] = useState<{
    status: "disabled" | "loading" | "ready" | "error";
    cwd: string;
    error: string;
  }>({ status: enabled ? "loading" : "disabled", cwd, error: "" });
  // Ответ старого cwd не имеет права перезаписать выбор нового терминала.
  const accountsLoadSeq = useRef(0);
  const usage = useAIUsage(enabled).snapshot || null;
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [lastShared, setLastShared] = useState<SharedNote | null>(null);

  const loadAccounts = useCallback(async (surfaceError = false): Promise<boolean> => {
    const seq = ++accountsLoadSeq.current;
    const requestCwd = cwd;
    if (!enabled) {
      setAccounts([]);
      setProxyContractVersion(0);
      setAccountsLoad({ status: "disabled", cwd: requestCwd, error: "" });
      return true;
    }
    setAccountsLoad({ status: "loading", cwd: requestCwd, error: "" });
    try {
      const d = await getAgentAccounts(undefined, requestCwd);
      if (seq !== accountsLoadSeq.current) return false;
      const version = Number.isSafeInteger(d.proxy_contract_version) ? Number(d.proxy_contract_version) : 0;
      setAccounts((d.accounts || []).map((account) => normalizeAccountProxyForClient(account, version)));
      setProxyContractVersion(version);
      setAccountsLoad({ status: "ready", cwd: requestCwd, error: "" });
      return true;
    } catch (e) {
      if (seq !== accountsLoadSeq.current) return false;
      const message = mapApiError(e);
      setAccounts([]);
      setProxyContractVersion(0);
      setAccountsLoad({ status: "error", cwd: requestCwd, error: message });
      if (surfaceError) setError(message);
      return false;
    }
  }, [enabled, cwd]);

  const reload = useCallback(() => {
    void loadAccounts(false);
    if (enabled) void fetchLocalAIUsage();
  }, [loadAccounts, enabled]);

  useEffect(() => { reload(); }, [reload]);

  const run = async <T,>(action: () => Promise<T>): Promise<T | null> => {
    setError("");
    setBusy(true);
    // Любая правка аккаунта/прокси делает старый снимок недоверенным до
    // повторного GET. Заодно отменяем ещё летящий ответ до мутации.
    accountsLoadSeq.current += 1;
    invalidateSelectedAIUsage();
    setAccountsLoad({ status: "loading", cwd, error: "" });
    try {
      const result = await action();
      if (!(await loadAccounts(true))) return null;
      // Лимиты сервер пересобирает после любой правки списка (иначе показывал
      // бы остаток прежнего аккаунта — это ловилось живым прогоном), поэтому
      // снимок перечитываем здесь же.
      invalidateSelectedAIUsage();
      await fetchLocalAIUsage(true);
      return result;
    } catch (e) {
      setError(mapApiError(e));
      // Неудачная мутация обычно ничего не изменила. Всё равно перечитываем:
      // только свежий ответ может снова открыть запуск.
      await loadAccounts(false);
      await fetchLocalAIUsage(true);
      return null;
    } finally {
      setBusy(false);
    }
  };

  const accountsReady = !enabled || (
    accountsLoad.status === "ready" && accountsLoad.cwd === cwd
  );

  return {
    accounts: accountsReady ? accounts : [],
		proxyContractVersion,
    accountsReady,
    accountsLoadError: accountsLoad.cwd === cwd ? accountsLoad.error : "",
    usage,
    busy,
    error,
    lastShared,
    cwd,
    reload,
    clearError: () => setError(""),
    pick: async (agentID, id) => { haptic(); await run(() => activateAgentAccount(agentID, id)); },
    setProxy: async (agentID, id, proxy) => {
      haptic();
      // Ответ агента про проверку прокси возвращаем наверх, а не глотаем:
      // адрес может быть верным, а прокси — ещё не поднятым, и человек об этом
      // узнает только по «агент не отвечает». Выходной IP доказывает путь
      // самого proxy endpoint; transport конкретного CLI проверяет сервер.
      const res = await run(() => saveAgentAccount({ id, agent_id: agentID, proxy }));
			return { ok: !!res, warning: res?.warning || "", exit: res?.proxy_exit || "" };
    },
    add: async (agentID, label) => {
      haptic();
      setLastShared(null);
      return run(async () => {
        const created = await saveAgentAccount({ agent_id: agentID, label });
        const account = created.account;
        // Что стало общим — говорим человеку СРАЗУ и ровно то, что вернул
        // агент: у нового аккаунта свой вход, но чужой пустой агент без
        // скиллов выглядел бы поломкой, а обещание «всё перенесено» было бы
        // неправдой (историю и вход мы не переносим никогда).
        const titles = (created.shared || [])
          .filter((s) => (s.linked || s.copied) && !s.error)
          .map((s) => s.title);
        if (titles.length > 0 || created.mcp_servers) {
          setLastShared({ titles, mcpServers: created.mcp_servers || 0 });
        }
        // НОВЫЙ АККАУНТ АКТИВНЫМ НЕ ДЕЛАЕМ.
        //
        // Раньше делали — «заводят, чтобы им работать». Живой случай владельца
        // показал, чем это кончается: между «завёл» и «вошёл» есть окно, и всё
        // это время активен аккаунт БЕЗ ТОКЕНА. Человек жмёт «Запустить», а
        // Claude встречает его экраном `/login` — при живом рабочем аккаунте
        // рядом. Со стороны это выглядит как поломка агента, а не как «я не
        // довёл настройку до конца».
        //
        // Теперь новый аккаунт просто встаёт в список с пометкой «нужен вход»,
        // а активным становится, когда человек выберет его сам (мы предупредим)
        // или после того, как вход выполнен.
        return account;
      });
    },
    remove: async (id) => { haptic(); await run(() => deleteAgentAccount(id)); },
    // Закрепление за папкой: повторное нажатие снимает его (пустой id).
    pin: async (agentID, id) => {
      if (!cwd) return;
      haptic();
      await run(() => pinAgentAccount(agentID, id, cwd));
    },
  };
}

/** Аккаунты одного агента; основной всегда первый — так их отдаёт агент. */
export function accountsOfAgent(state: AgentAccountsState, agentID: string): AgentAccount[] {
  return state.accounts.filter((a) => a.agent_id === agentID);
}

export function activeAccountOfAgent(state: AgentAccountsState, agentID: string): AgentAccount | null {
  return accountsOfAgent(state, agentID).find((a) => a.active) || null;
}

/** Как назвать аккаунт человеку: у основного своего имени нет. */
export function accountTitle(a: AgentAccount): string {
  return a.is_default ? t("agentLaunch.accountDefault") : a.label;
}

/**
 * Вошли ли в этот аккаунт.
 *
 * Отличать «нет данных» от «нет входа» обязательно: у первого агент просто не
 * отдаёт процент (так бывает у части тарифов), а второй означает, что запуск
 * упрётся в `/login`. Пока лимиты не приехали вовсе, ничего не утверждаем —
 * иначе рабочий аккаунт мигал бы предупреждением на каждой загрузке.
 */
export function accountSignedOut(usage: AIUsageSnapshot | null, agentID: string, accountID: string): boolean {
  const provider = (usage?.providers || []).find(
    (p) => p.id === agentID && (p.account_id || "default") === accountID,
  );
  return provider?.status === "signed_out";
}

/**
 * Остаток лимита КОНКРЕТНОГО аккаунта.
 *
 * Проценты у вендоров — «использовано», а человек решает по остатку: на вопрос
 * «чем работать дальше» отвечает большее число. Нет окна — молчим: «0%» был бы
 * прямой неправдой (unknown ≠ 0).
 */
export function accountLeftLabel(
  usage: AIUsageSnapshot | null, agentID: string, accountID: string,
): string {
  const provider = (usage?.providers || []).find(
    (p) => p.id === agentID && (p.account_id || "default") === accountID,
  );
  if (!provider) return "";
  if (provider.status === "signed_out") return t("agentLaunch.accountSignedOut");
  if (provider.status !== "available" && !provider.stale) return "";
  const { five, seven } = mainUsageWindows(provider);
  const window = five || seven;
  if (!window) return "";
  const label = t(five ? "agentLaunch.accountLeft" : "agentLaunch.accountLeftWeek", { percent: Math.max(0, Math.round(100 - window.used_percent)) });
  return usageIsStale(provider, Date.now()) ? `${label} · ${t("usage.staleShort")}` : label;
}

interface ListProps {
  agentID: string;
  state: AgentAccountsState;
  /** «Войти»: в терминале — запуск здесь же, в разделе — переход в терминал. */
  onSignIn: (account: AgentAccount) => void;
  /** Показывать пояснение под списком (в разделе оно ни к чему — там есть свой текст). */
  hint?: boolean;
  /**
   * Подпись кнопки входа. В шторке терминала «Войти» запускает вход прямо
   * здесь; в разделе «Агенты» та же кнопка открывает НОВЫЙ терминал — и слово
   * «Войти» там обещало не то, что происходило (аудит ИА 02.09.2026, P1-17).
   */
  signInLabel?: string;
}

/**
 * Список аккаунтов агента: выбрать, войти, забыть, завести новый.
 *
 * Имя нового аккаунта спрашиваем СВОИМ полем, а не `prompt`: в окне exe
 * нативные диалоги подавлены (WebView2), и кнопка там просто не работала бы.
 */
export function AgentAccountsList({ agentID, state, onSignIn, hint = true, signInLabel }: ListProps) {
  const [newOpen, setNewOpen] = useState(false);
  const [optionsFor, setOptionsFor] = useState("");
  const [name, setName] = useState("");
  // Прокси правится у одного аккаунта за раз: id открытого поля и его текст.
  const [proxyFor, setProxyFor] = useState("");
  const [proxyText, setProxyText] = useState("");
  const [proxyWarn, setProxyWarn] = useState("");
  const [proxyExit, setProxyExit] = useState("");
  const list = accountsOfAgent(state, agentID);
  const now = useUsageNow();

  // Удаление аккаунта необратимо и стоит дорого: сервер сносит его каталог
  // целиком (api_accounts.go, os.RemoveAll) — вместе с входом и перепиской
  // агента. Спрашиваем ровно так же, как продукт уже спрашивает про удаление
  // SSH-сервера и устройства (UX-аудит 2026-08-23, NIELSEN-6).
  const forgetAccount = async (acc: AgentAccount) => {
    const ok = await tgConfirm(
      t("agentLaunch.accountForgetConfirm", { name: accountTitle(acc), agent: agentDisplayName(agentID) }),
      { danger: true, confirmText: t("confirm.btn.forgetAccount") },
    );
    if (!ok) return;
    await state.remove(acc.id);
  };

  const saveProxy = async (id: string) => {
		const { ok, warning, exit } = await state.setProxy(agentID, id, proxyText.trim());
    setProxyWarn(warning);
    setProxyExit(exit);
    // Поле закрываем, только когда показывать НЕЧЕГО: иначе предупреждение о
    // молчащем прокси исчезало вместе с полем — боевой случай 14.08.2026,
    // человек сохранил мёртвый socks5://127.0.0.1:1080 и узнал об отказе по
    // «Unable to connect» у агента, а не от нас. И выходной IP — тоже повод
    // оставить поле открытым: это подтверждение доступности proxy endpoint.
		if (ok && !warning && !exit) setProxyFor("");
  };

  const create = async () => {
    const label = name.trim();
    if (!label) return;
    const created = await state.add(agentID, label);
    if (created) {
      setNewOpen(false);
      setName("");
    }
  };

  return (
    <div className="agent-accounts">
      {list.map((acc) => {
        const left = accountLeftLabel(state.usage, agentID, acc.id);
        const usageProvider = state.usage?.providers.find(p => p.id === agentID && (p.account_id || "default") === acc.id);
        // В аккаунт не входили — выбирать его бессмысленно: запуск встретит
        // человека экраном `/login`. Кнопку не гасим (человек вправе выбрать
        // и войти), но говорим прямо и предлагаем «Войти» первым действием.
        const needsLogin = accountSignedOut(state.usage, agentID, acc.id);
        return (
          <Fragment key={acc.id}>
          <div className={`agent-account${acc.active ? " on" : ""}${needsLogin ? " needs-login" : ""}`}>
            <button
              className="agent-account-pick"
			  disabled={state.busy || acc.active || !!acc.proxy_blocked}
			  onClick={() => { if (!acc.proxy_blocked) void state.pick(agentID, acc.id); }}
              aria-pressed={acc.active}
            >
              <span className="agent-flag-box" aria-hidden>{acc.active ? "✓" : ""}</span>
              <span className="agent-account-body">
                <span className="agent-account-title">{accountTitle(acc)}</span>
                <span className="agent-account-hint" title={usageProvider ? usageAgeLabel(usageProvider, now) : undefined}>
                  {acc.pinned && <b className="agent-account-pinned">{t("agentLaunch.accountPinnedHere")} · </b>}
                  {needsLogin
                    ? <b className="agent-account-needslogin">{t("agentLaunch.accountNeedsLogin")}</b>
                    : left || (acc.is_default ? t("agentLaunch.accountDefaultHint") : t("agentLaunch.accountNoData"))}
                  {/* Через какой канал ходит подписка — видно сразу: иначе
                      «почему рабочий агент не отвечает» превращается в
                      расследование. */}
									{acc.proxy && !acc.proxy_warning && <em className="agent-account-proxy">{t("agentLaunch.accountViaProxy", { proxy: acc.proxy })}</em>}
                </span>
              </span>
            </button>
            {needsLogin && (
              <button className="btn btn-sm btn-primary" disabled={state.busy || !!acc.proxy_blocked}
                onClick={() => { if (!acc.proxy_blocked) onSignIn(acc); }}>
                {signInLabel ?? t("agentLaunch.accountSignIn")}
              </button>
            )}
            <button className="btn btn-secondary btn-sm agent-account-settings"
              aria-label={t("agentLaunch.accountSettings", { name: accountTitle(acc) })}
              aria-expanded={optionsFor === acc.id}
              onClick={() => { haptic(); setOptionsFor(optionsFor === acc.id ? "" : acc.id); setProxyFor(""); }}>
              {t("agentLaunch.accountSettingsLabel")}
            </button>
            {optionsFor === acc.id && <div className="agent-account-options">
            {/* «📌» — закрепить аккаунт за папкой терминала: рабочий код
                всегда открывается рабочей подпиской, и решать это каждый раз
                руками не нужно. Есть только там, где известна папка. */}
            {state.cwd && (
              <button
                className={`agent-account-pin${acc.pinned ? " on" : ""}`}
                disabled={state.busy}
                onClick={() => void state.pin(agentID, acc.pinned ? "" : acc.id)}
                aria-pressed={!!acc.pinned}
                title={acc.pinned ? t("agentLaunch.accountUnpin") : t("agentLaunch.accountPin")}
                aria-label={acc.pinned ? t("agentLaunch.accountUnpin") : t("agentLaunch.accountPin")}
              >
                <IconPin size={16} />
              </button>
            )}
            {/* У аккаунта без входа «Войти» — главное действие: без него
                выбирать его нечего. У остальных это редкий случай (перелогин),
                поэтому кнопка остаётся вторичной. */}
            {!needsLogin && <button
              className={`btn btn-sm ${needsLogin ? "btn-primary" : "btn-secondary"}`}
				disabled={state.busy || !!acc.proxy_blocked}
				onClick={() => { if (!acc.proxy_blocked) onSignIn(acc); }}
            >
              {signInLabel ?? t("agentLaunch.accountSignIn")}
            </button>}
            {/* «🌐» — свой прокси у этой подписки. Есть и у основного
                аккаунта: у большинства людей он единственный, и без этого
                функция им недоступна вовсе. */}
            <button
				className={`agent-account-proxy-btn${acc.proxy || acc.proxy_legacy ? " on" : ""}`}
              disabled={state.busy}
              onClick={() => {
                haptic();
                state.clearError();
                setProxyWarn(acc.proxy_warning || "");
                setProxyExit("");
                setProxyFor(proxyFor === acc.id ? "" : acc.id);
								setProxyText(acc.proxy || "");
              }}
              aria-expanded={proxyFor === acc.id}
              title={t("agentLaunch.accountProxy")}
              aria-label={t("agentLaunch.accountProxy")}
            >
              {t("agentLaunch.accountProxy")}
            </button>
            {!acc.is_default && (
              <button
                className="agent-account-del"
                disabled={state.busy}
                onClick={() => void forgetAccount(acc)}
                title={t("agentLaunch.accountForget")}
                aria-label={t("agentLaunch.accountForget")}
              >
                {t("agentLaunch.accountForget")}
              </button>
            )}
            </div>}
            {/* Предупреждение остаётся ЧАСТЬЮ строки аккаунта (иначе оно
                перестаёт к ней относиться — и для человека, и для замера
                probe-account-proxy, который ищет его внутри блока), но занимает
                всю ширину: ряд переносит его на свою строку. В колонке между
                именем и кнопками ему доставалось около ста пикселей, и текст
                рвался по два слова в столбик — терпимо на кегле 11px, нечитаемо
                после общей шкалы кеглей. */}
            {acc.proxy_warning && (
              <div className="agent-account-proxy-warn">{acc.proxy_warning}</div>
            )}
          </div>
          {/* Поле стоит ОТДЕЛЬНОЙ строкой под аккаунтом, а не внутри его ряда:
              адрес прокси длинный, и попытка уместить его в ряд с кнопками
              переносила сами кнопки — ряд разваливался (видно на первом же
              снимке стенда). */}
          {proxyFor === acc.id && (
              <div className="agent-account-proxy-edit">
                <input
                  autoFocus
                  value={proxyText}
                  onChange={(e) => setProxyText(e.target.value)}
                  onKeyDown={(e) => { if (e.key === "Enter") { e.preventDefault(); void saveProxy(acc.id); } }}
                  placeholder={t("agentLaunch.accountProxyPlaceholder")}
                  aria-label={t("agentLaunch.accountProxy")}
                  autoCapitalize="off"
                  autoCorrect="off"
                  spellCheck={false}
                />
                <div className="agent-account-proxy-actions">
                  <button className="btn btn-secondary btn-sm" onClick={() => { setProxyFor(""); setProxyWarn(""); setProxyExit(""); }}>
                    {t("generic.cancel")}
                  </button>
                  <button className="btn btn-primary btn-sm" disabled={state.busy} onClick={() => void saveProxy(acc.id)}>
                    {t("generic.save")}
                  </button>
                </div>
                <div className="agent-account-proxy-hint">{t("agentLaunch.accountProxyHint")}</div>
                {proxyExit && <div className="agent-account-proxy-ok">{t("agentLaunch.accountProxyExit", { ip: proxyExit })}</div>}
                {proxyWarn && <div className="agent-account-proxy-warn">{proxyWarn}</div>}
              </div>
          )}
          </Fragment>
        );
      })}

      {newOpen ? (
        <div className="agent-account-new">
          <input
            className="agent-flag-extra"
            autoFocus
            value={name}
            placeholder={t("agentLaunch.accountNamePlaceholder")}
            onChange={(e) => setName(e.target.value)}
            onKeyDown={(e) => { if (e.key === "Enter") void create(); }}
            aria-label={t("agentLaunch.accountNamePlaceholder")}
          />
          <button className="btn btn-sm btn-primary" disabled={state.busy || !name.trim()} onClick={() => void create()}>
            {t("agentLaunch.accountCreate")}
          </button>
          <button className="btn btn-sm btn-secondary" onClick={() => { setNewOpen(false); setName(""); }}>
            {t("modal.cancel")}
          </button>
        </div>
      ) : (
        <button
          className="agent-account-add"
          onClick={() => { haptic(); setNewOpen(true); setName(""); state.clearError(); }}
        >
          + {t("agentLaunch.accountAdd")}
        </button>
      )}
      {/* Что стало общим у нового аккаунта. Говорим сразу и по делу: у второй
          подписки свой вход и своя переписка, но скиллы и настройки — те же.
          Без этой строки человек заводит аккаунт и обнаруживает агента без
          единого скилла — формально верно, а читается как поломка. */}
      {state.lastShared && (
        <div className="agent-account-shared">
          {t("agentLaunch.accountShared", { list: state.lastShared.titles.join(", ") })}
          {state.lastShared.mcpServers > 0 && ` ${t("agentLaunch.accountSharedMcp", { count: state.lastShared.mcpServers })}`}
          {/* Следующий шаг называем сразу: аккаунт заведён, но пока в него не
              вошли, он ничего не делает — а раньше он ещё и молча становился
              активным, и запуск агента упирался в /login. */}
          <b> {t("agentLaunch.accountCreatedNext")}</b>
        </div>
      )}
      {state.error && <div className="agent-launch-danger-note">{state.error}</div>}
      {hint && <div className="agent-account-note">{t("agentLaunch.accountNote")}</div>}
    </div>
  );
}
