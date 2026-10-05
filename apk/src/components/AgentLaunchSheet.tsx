import { useCallback, useEffect, useRef, useState } from "react";
import type { KeyboardEvent as ReactKeyboardEvent } from "react";
import { useNavigate } from "react-router-dom";
import {
  useEscape, AGENT_INSTALL_ORDER, setAgentRegistry, nodeInstallCommand, mapApiError, usesNpm,
} from "@tgcontrol/shared";
import { getAgents, rescanAgents } from "../api";
import type { AgentAccount, AgentInfo } from "../types";
import {
  useAgentAccounts, AgentAccountsList,
  accountsOfAgent, activeAccountOfAgent, accountTitle, accountLeftLabel,
} from "./AgentAccounts";
import { haptic } from "../telegram";
import { t } from "../i18n";
import {
  composeLaunch, composeRemoteLaunch, hasDanger, launchAccountForAgent, loadPrefs,
  BLOCKED_ACCOUNTS_MISSING,
  normalizeAccountProxyForClient, savePrefs, toggleFlag, EMPTY_PREFS,
} from "../ptyTerm/agentLaunch";
import type { LaunchPrefs, LaunchAccount, LaunchModel } from "../ptyTerm/agentLaunch";
import { useOpenRouter } from "./OpenRouterCard";
import { shouldOfferOpenRouter } from "../openrouterOffer";

/**
 * Обновление PATH в УЖЕ ЗАПУЩЕННОМ шелле.
 *
 * После `winget install ... NodeJS` в том же терминале снова прилетает «npm не
 * является внутренней или внешней командой»: PATH процессу выдаётся при
 * запуске и сам не обновляется. Строка перечитывает его из реестра прямо в этой
 * сессии — иначе единственный выход был бы «откройте новый терминал», о чём
 * интерфейс не говорил нигде. На POSIX ставится всё в /usr/bin, и мешает разве
 * что кеш путей самого шелла — его сбрасывает `hash -r`.
 */
const PATH_REFRESH_WINDOWS =
  "$env:Path=[Environment]::GetEnvironmentVariable('Path','Machine')+';'+[Environment]::GetEnvironmentVariable('Path','User')";
const PATH_REFRESH_POSIX = "hash -r";

interface Props {
  open: boolean;
  onClose: () => void;
  /** Выполнить команду в текущем терминале (без \r — добавит вызывающий). */
  onRun: (cmd: string) => void;
  /**
   * Запустить именно CLI-агента. Родитель подтверждает переход из живого
   * агента и атомарно связывает metadata с фактически отправленной командой.
   */
  onRunAgent?: (
    cmd: string,
    account: { id: string; label: string; isDefault: boolean } | null,
  ) => Promise<boolean>;
  /** The command was sent; the terminal must still observe the agent process. */
  onLaunchRequested?: (agent: Pick<AgentInfo, "id" | "name">) => void;
  /** SSH terminal: local agent discovery belongs to the bastion PC. */
  remote?: boolean;
  /** Платформа терминала ("windows" | "linux"): выбирает команду Node.js. */
  platform?: string;
  /** Папка терминала: по ней сервер учитывает аккаунт, закреплённый за проектом. */
  cwd?: string;
  /**
   * Аккаунт, ради которого сюда пришли (аудит ИА 02.09.2026, D4): «Войти» у
   * аккаунта в разделе «Агенты» уводит в новый терминал с этой шторкой, и
   * «Запустить» должен уйти под ЭТИМ аккаунтом, а не под активным. Действует,
   * пока человек сам не открыл список аккаунтов — дальше выбирает он.
   */
  accountId?: string;
}

/**
 * «Запустить агента» — отдельная кнопка терминала: выбрал Claude/Codex/… и он
 * поднялся в этом же терминале, в его папке.
 *
 * Раньше это можно было сделать только сниппетом «claude» из ⚡ (и то не было
 * видно, установлен ли агент вообще). Здесь установленные показаны сверху с
 * кнопкой «Запустить», а не найденные — с «Установить»: команда установки
 * уходит в тот же терминал, и после неё агент запускается той же кнопкой.
 *
 * Список берём с агента (`GET /api/agents`, поле `cli` = что набрать), поэтому
 * новый агент в реестре появляется здесь без правок клиента.
 */
export function AgentLaunchSheet({ open, onClose, onRun, onRunAgent, onLaunchRequested, remote = false, platform = "", cwd = "", accountId = "" }: Props) {
  const navigate = useNavigate();
  const [agents, setAgents] = useState<AgentInfo[] | null>(null);
  // Отказ запроса хранится отдельно от данных: раньше ошибка превращалась в
  // пустой массив, шторка писала «Не удалось получить список агентов» и
  // становилась тупиком — перезагрузка была возможна только её закрытием.
  const [loadError, setLoadError] = useState("");
  // Что уже отправлено в терминал. Флаги переживают закрытие шторки (компонент
  // остаётся смонтированным), и человек, вернувшийся сюда после установки,
  // видит не тот же самый «Шаг 2», а следующий шаг: обновить PATH и проверить
  // снова. Раньше шторка закрывалась сразу после отправки команды и о том, что
  // делать, когда npm домотает, не говорила ничего.
  const [nodeSent, setNodeSent] = useState(false);
  const [installSent, setInstallSent] = useState("");
  const [rechecking, setRechecking] = useState(false);
  const [launchingAccount, setLaunchingAccount] = useState(false);
  // Какой агент раскрыл свои флаги (одновременно — один: шторка на телефоне и
  // так узкая, а два раскрытых списка сразу читаются как каша).
  const [flagsOpen, setFlagsOpen] = useState("");
  // Выбор флагов по агенту. Держим в состоянии, чтобы экран перерисовывался, но
  // источником правды остаётся localStorage — выбор переживает и закрытие
  // шторки, и уход со страницы (см. ptyTerm/agentLaunch.ts).
  const [prefs, setPrefs] = useState<Record<string, LaunchPrefs>>({});
  const prefsOf = (id: string): LaunchPrefs => prefs[id] ?? loadPrefs(id);
  // Аккаунты нейросетей этой машины и остаток лимита у каждого — общий модуль
  // с разделом «Агенты» (components/AgentAccounts.tsx): переключаться человек
  // хочет и на бегу отсюда, и спокойно в разделе, а две копии этой логики
  // разъехались бы на первой правке.
  //
  // На SSH-терминале аккаунтов нет по построению: каталоги лежат на
  // компьютере, а команда уходит в шелл сервера.
  const accountsState = useAgentAccounts(!remote, cwd);
  // API-ответ не попадает ни в UI, ни в composeLaunch без клиентской
  // проверки. Это важно для пары «новый APK + старый agent»: там
  // нет proxy_contract_version, и сырой SOCKS/userinfo нельзя даже показывать.
  const clientAccountsState = {
    ...accountsState,
    accounts: accountsState.accounts.map((account) => (
      normalizeAccountProxyForClient(account, accountsState.proxyContractVersion)
    )),
  };
  // Какой агент раскрыл список своих аккаунтов (одновременно — один: шторка на
  // телефоне узкая, два раскрытых списка читаются как каша).
  const [accountsOpen, setAccountsOpen] = useState("");
  // Аккаунт из адреса (см. Props.accountId): состояние, а не проп напрямую,
  // потому что снимается, как только человек раскрыл список аккаунтов сам.
  const [pinnedAccountId, setPinnedAccountId] = useState(accountId);
  // POSIX — это Linux/mac-агент ИЛИ терминал SSH-сервера. Второе важнее: сам
  // Remotai может стоять на Windows, а команда уедет в bash сервера.
  const posixShell = remote || platform !== "windows";
  // Модель OpenRouter, выбранная на ЭТОМ компьютере. На SSH-сервер она не едет
  // (там свой ключ и свой Remotai), поэтому и спрашивать её незачем.
  const openrouter = useOpenRouter(!remote);
  /**
   * Чем задать модель этому агенту.
   *
   * Флаг называет РЕЕСТР НА GO (`model_flag`, сверен с живым `--help`), а не
   * клиент: агент, у которого флаг не проверен, модель не получит вовсе —
   * несуществующий флаг роняет разбор аргументов, и человек с телефона видит
   * только «агент не запустился».
   */
  const modelOf = (a: AgentInfo): LaunchModel | undefined => {
    const value = openrouter.status?.model || "";
    return a.model_flag && value ? { flag: a.model_flag, value } : undefined;
  };
  const launchCtx = (agent: AgentInfo, account?: LaunchAccount, model?: LaunchModel) => ({
    posix: posixShell,
    specs: agent.launch_flags || [],
    agentID: agent.id,
    accountEnvName: remote ? "" : (agent.account_env || ""),
    proxyContractVersion: remote ? undefined : accountsState.proxyContractVersion,
    blockedReason: account?.blockedReason,
    account,
    model,
    // Хуки агента — пути этого компьютера; на SSH-сервер не едут.
    launchArgs: remote ? undefined : agent.launch_args,
  });

  /** Аккаунт запуска с явным agent/contract и без сырого proxy. */
  const launchAccountOf = (agent: AgentInfo, selected?: AgentAccount | null): LaunchAccount | undefined => (
    launchAccountForAgent(agent, clientAccountsState, selected)
  );
  const setPrefsOf = (id: string, next: LaunchPrefs) => {
    setPrefs((p) => ({ ...p, [id]: next }));
    savePrefs(id, next);
  };
  useEscape(open, onClose);
  const sheetRef = useRef<HTMLDivElement | null>(null);

  /**
   * `rescan` — не косметика: список агентов на ПК считается один раз при старте
   * (`agents.DetectOnce`), и без принудительного пересканирования только что
   * поставленный claude остаётся «не установлен» до перезапуска Remotai.
   */
  const load = useCallback((rescan = false) => {
    setAgents(null);
    setLoadError("");
    (rescan ? rescanAgents() : getAgents())
      .then((d) => {
        setAgentRegistry(d.agents || []);
        setAgents(d.agents || []);
      })
      .catch((e) => {
        setLoadError(mapApiError(e));
        setAgents([]);
      })
      .finally(() => setRechecking(false));
  }, []);

  useEffect(() => {
    if (!open) return;
    load();
    // Список аккаунтов мог измениться в разделе «Агенты», пока шторка была
    // закрыта, — перечитываем при каждом открытии.
    accountsState.reload();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, load]);

  // Фокус внутрь шторки при открытии и возврат его на кнопку-источник: без
  // этого Tab уходил в терминал под затемнением (в окне exe это ловится сразу).
  useEffect(() => {
    if (!open) return;
    const opener = document.activeElement as HTMLElement | null;
    sheetRef.current?.focus();
    return () => {
      if (opener && typeof opener.focus === "function" && document.contains(opener)) opener.focus();
    };
  }, [open]);

  if (!open) return null;

  // Только CLI-агенты: shell/orchestrator/researcher — встроенные, у них нет
  // команды запуска в терминале (сервер отдаёт cli: "").
  const order = new Map<string, number>(AGENT_INSTALL_ORDER.map((id, i) => [id, i]));
  const cliAgents = (agents || [])
    .filter((a) => !!a.cli)
    .sort((a, b) => (order.get(a.id) ?? 999) - (order.get(b.id) ?? 999));
  const installed = remote ? cliAgents : cliAgents.filter((a) => a.detected);
  // На СЕРВЕР агента поставить было нечем: обе двери закрыты одновременно —
  // здесь `missing` был пуст по построению, а в ⚡ список агентов для SSH даже
  // не запрашивался. Node.js на сервер поставить можно, а то, ради чего его
  // ставят, — нельзя.
  //
  // Признак «установлен» на сервере мы честно не знаем (детект — это ПК),
  // поэтому на SSH показываем ВСЕХ, у кого есть команда установки. Команда —
  // POSIX-вариант: `install` резолвится по ОС бастиона, и с Windows-ПК в bash
  // сервера уехал бы `winget install …`.
  const serverInstall = (a: AgentInfo) => a.install_posix || a.install || "";
  /**
   * Запуск заблокирован тем, что компьютер не отдал аккаунты. Это состояние
   * гасит кнопки у ВСЕХ агентов, которые аккаунты умеют, — значит объяснение и
   * ход должны стоять один раз наверху, а не сноской в каждой строке.
   */
  const accountsBlocked = !remote
    && installed.some((a) => !!a.account_env)
    && (!!accountsState.accountsLoadError
      || (accountsState.accountsReady && accountsState.accounts.length === 0));
  /**
   * Второе лицо того же тупика: аккаунты компьютер ПРИСЛАЛ, но ни один не
   * помечен активным — тогда `launchAccountOf` гасит запуск у каждого агента
   * с причиной BLOCKED_ACCOUNTS_MISSING, а объяснение сверху не показывалось:
   * условие выше требовало ПУСТОГО списка. Замер J03 05.09.2026 упирался
   * ровно сюда — четыре агента в списке, все кнопки серые, и ни слова о том,
   * что делать. Ход тот же, что и выше, плюс дверь в «Агенты»: активный
   * аккаунт выбирают там.
   */
  const noActiveAccount = !remote
    && !accountsBlocked
    && accountsState.accountsReady
    && installed.some((a) => !!a.account_env)
    && installed.filter((a) => !!a.account_env).every(
      (a) => launchAccountOf(a)?.blockedReason === BLOCKED_ACCOUNTS_MISSING);
  // Предлагать ли ключ OpenRouter прямо здесь (см. openrouterOffer.ts). На SSH
  // не предлагаем: ключ живёт в окружении агентов ЭТОГО компьютера, на сервер
  // он не едет.
  const offerOpenRouter = !remote && shouldOfferOpenRouter({
    configured: Boolean(openrouter.status?.configured),
    installedAgents: installed.length,
    usage: accountsState.usage,
  });
  const missing = remote
    ? cliAgents.filter((a) => !!serverInstall(a))
    : cliAgents.filter((a) => !a.detected && !!a.install);
  // Почти все агенты реестра ставятся через npm, а npm — это Node.js. На чистой
  // машине «Установить» отвечал английским «npm не распознан», и о Node.js
  // интерфейс не говорил нигде. Поэтому установка показана как два шага, а
  // команда первого шага выбирается под ОС терминала (winget на Windows).
  const npmInstalls = missing.some((a) => usesNpm(remote ? serverInstall(a) : (a.install || "")));
  const nodeCmd = nodeInstallCommand(platform);
  const pathRefreshCmd = platform === "windows" ? PATH_REFRESH_WINDOWS : PATH_REFRESH_POSIX;
  // Блок «что дальше» живёт ровно до тех пор, пока установка не удалась: как
  // только поставленный агент виден, подсказка исчезает сама и не превращается
  // в вечную плашку у тех, кто всё поставил ещё вчера.
  const installPending = !!installSent
    && cliAgents.find((a) => a.id === installSent)?.detected !== true;
  const showNextStep = (nodeSent && missing.length > 0) || (!remote && installPending);

  const run = (cmd: string) => {
    haptic();
    onRun(cmd);
    onClose();
  };

  /** Запуск CLI с тем аккаунтом, который реально был выбран этой кнопкой. */
  const runAgent = async (agent: AgentInfo, cmd: string, usedAccount?: AgentAccount | null) => {
    // composeLaunch возвращает пустую строку при fail-closed отказе.
    // Даже если DOM-кнопку вызвали программно, в терминал ничего не уйдёт.
    if (!cmd || launchingAccount) return;
    setLaunchingAccount(true);
    try {
      // SignIn может запускать НЕ активный аккаунт. Metadata обязана описывать
      // фактический процесс, иначе следующий Resume откроет чужой профиль.
      const actual = usedAccount === undefined
        ? activeAccountOfAgent(clientAccountsState, agent.id)
        : usedAccount;
      const selected = actual ? {
        id: actual.is_default ? "" : actual.id,
        label: actual.is_default ? "" : actual.label,
        isDefault: Boolean(actual.is_default),
      } : null;
      const sent = onRunAgent
        ? await onRunAgent(cmd, selected)
        : (onRun(cmd), true);
      if (sent) {
        haptic();
        onLaunchRequested?.(agent);
        onClose();
      }
    } catch (e) {
      // Команду при ошибке metadata/сокета не шлём: иначе разговор запустится
      // под B, а Resume будет доверять записанному раньше A.
      setLoadError(mapApiError(e));
    } finally {
      setLaunchingAccount(false);
    }
  };

  /**
   * Установка отличается от запуска: команда уходит в терминал (и шторка
   * закрывается — иначе не видно её вывод), но шаг ЗАПОМИНАЕТСЯ. Вернувшись,
   * человек получает «что дальше», а не тот же самый шаг с той же кнопкой.
   */
  const runInstall = (cmd: string, what: "node" | string) => {
    if (what === "node") setNodeSent(true);
    else setInstallSent(what);
    run(cmd);
  };

  // PATH обновляется мгновенно и без вывода, поэтому шторку не закрываем: сразу
  // после команды пересканируем список — обычно агент тут же становится
  // «установленным», и его можно запустить, не выходя отсюда.
  const refreshPath = () => {
    haptic();
    onRun(pathRefreshCmd);
    setRechecking(true);
    window.setTimeout(() => load(true), 900);
  };

  const recheck = () => {
    haptic();
    setRechecking(true);
    load(true);
  };

  /** Ловушка фокуса шторки — то же правило, что в DialogHost. */
  const onSheetKeyDown = (e: ReactKeyboardEvent<HTMLDivElement>) => {
    if (e.key !== "Tab") return;
    const items = [...e.currentTarget.querySelectorAll<HTMLElement>(
      'button:not([disabled]), input:not([disabled]), textarea:not([disabled]), select:not([disabled]), a[href], [tabindex]:not([tabindex="-1"])',
    )];
    if (items.length === 0) return;
    const index = items.indexOf(document.activeElement as HTMLElement);
    const next = e.shiftKey
      ? (index <= 0 ? items.length - 1 : index - 1)
      : (index < 0 || index === items.length - 1 ? 0 : index + 1);
    e.preventDefault();
    items[next]?.focus();
  };

  const row = (a: AgentInfo, action: "run" | "install") => {
    const flags = a.launch_flags || [];
    const prefs = prefsOf(a.id);
    const open = flagsOpen === a.id;
    // Аккаунт из адреса (D4) побеждает активный, пока человек не открыл список
    // сам; чужому агенту он не достанется — ищем только среди своих.
    const activeAccount = (pinnedAccountId
      ? accountsOfAgent(clientAccountsState, a.id).find((acc) => acc.id === pinnedAccountId)
      : null) || activeAccountOfAgent(clientAccountsState, a.id);
    const launchAccount = remote ? undefined : launchAccountOf(a, activeAccount);
    const context = launchCtx(a, launchAccount, modelOf(a));
    // Команда со всеми выбранными флагами — её и покажем человеку целиком.
    // Площадка важна: на сервере команда пишется ИНАЧЕ (env-префикс, флаги-
    // спутники) — см. composeLaunch. SSH-терминал — всегда POSIX-шелл сервера,
    // даже если сам Remotai стоит на Windows.
    const cmd = composeLaunch(a.cli || "", prefs, context);
    // Превью обязано совпадать с тем, что реально уйдёт в шелл: пока они
    // расходились, превью показывало `env IS_SANDBOX=1 …`, а кнопка слала
    // команду без него — и на сервере под root агент не стартовал вовсе.
    const runCmd = remote
      ? composeRemoteLaunch(a.cli || "", prefs, launchCtx(a), t("agentLaunch.notOnServer", { name: a.cli || "" }), a.cli_names)
      : cmd;
    const blockedReason = !remote && action === "run"
      ? (launchAccount?.blockedReason || (!cmd ? t("ui.agentlaunchsheet.m458f285310") : ""))
      : "";
    const resumeCmd = !remote && a.resume_cli
      ? composeLaunch(a.resume_cli, EMPTY_PREFS, launchCtx(a, launchAccount))
      : "";
    const dangerPicked = hasDanger(prefs, flags.filter((f) => f.danger).map((f) => f.flag));
    const picked = (prefs.flags || []).length + (prefs.extra.trim() ? 1 : 0);
    // Аккаунты — только у своей машины и только у агентов, которые их умеют
    // (реестр на Go называет переменную; клиент её не хардкодит). У SSH-сервера
    // свой аккаунт — заводить его надо там же.
    const agentAccounts = !remote && a.account_env ? accountsOfAgent(clientAccountsState, a.id) : [];
    const accountsShown = accountsOpen === a.id;
    // `accounts-open` включает перенос — без него раскрытый список ложится в тот
    // же ряд, что имя агента, и уезжает за правый край (поймано замером
    // shot-agent-accounts.mjs: 9 кнопок за краем).
    return (
      <div key={a.id} data-agent-id={a.id} className={`agent-launch-row${blockedReason ? " launch-blocked" : ""}${action === "install" ? " missing" : ""}${open ? " flags-open" : ""}${accountsShown ? " accounts-open" : ""}${agentAccounts.length > 0 && action === "run" ? " has-account" : ""}`}>
        <span className="agent-launch-icon">{a.icon}</span>
        <span className="agent-launch-body">
          <span className="agent-launch-name">{a.name}</span>
          <span className="agent-launch-meta">
            {remote && action === "run"
              ? t("ui.agentlaunchsheet.m75c5a47abd")
              : action === "run"
                ? blockedReason === BLOCKED_ACCOUNTS_MISSING ? t("agentLaunch.chooseAccountHint")
                  : blockedReason || t("agentLaunch.readyToRun")
                : t("agentLaunch.notInstalled")}
          </span>
        </span>
        <span className="agent-launch-actions">
          {/* Параметры и точная команда доступны у любого установленного агента. */}
          {action === "run" && (
            <button
              className={`btn btn-sm btn-secondary agent-launch-flags-btn${picked ? " on" : ""}${dangerPicked ? " danger" : ""}`}
              onClick={() => { haptic(); setFlagsOpen(open ? "" : a.id); }}
              aria-expanded={open}
              title={t("agentLaunch.flagsTitle")}
              aria-label={t("agentLaunch.settingsFor", { name: a.name })}
            >
              {picked ? `⚙ ${picked}` : "⚙"}
            </button>
          )}
          {action === "run" && !remote && a.supports_resume && a.resume_cli && (
            <button
              className="btn btn-sm btn-secondary"
              disabled={launchingAccount || !!blockedReason || !resumeCmd}
              title={t("agentLaunch.resumeHint")}
              onClick={() => { if (resumeCmd) void runAgent(a, resumeCmd, activeAccount); }}
            >
              {t("agentLaunch.resume")}
            </button>
          )}
          <button
            className={`btn btn-sm ${action === "run" ? "btn-primary" : "btn-secondary"}`}
            disabled={action === "run" && (launchingAccount || !!blockedReason || !runCmd)}
            onClick={() => {
              if (action === "run") {
                if (runCmd) void runAgent(a, runCmd, activeAccount);
              } else {
                runInstall(remote ? serverInstall(a) : a.install!, a.id);
              }
            }}
          >
            {action === "run" ? t("agentLaunch.run") : t("agentLaunch.install")}
          </button>
        </span>

        {blockedReason && (
          <div className="agent-launch-recovery">
            {!accountsState.accountsReady || accountsState.accountsLoadError ? (
              <button className="btn btn-secondary btn-sm" disabled={accountsState.busy}
                onClick={() => { haptic(); accountsState.reload(); }}>
                {t("agentLaunch.retry")}
              </button>
            ) : (
              <>
                <button className="btn btn-secondary btn-sm" onClick={() => {
                  haptic();
                  if (blockedReason === BLOCKED_ACCOUNTS_MISSING && agentAccounts.length > 0) {
                    setAccountsOpen(a.id); setPinnedAccountId("");
                  } else {
                    onClose(); navigate("/agents", { state: { focus: "accounts", agentID: a.id } });
                  }
                }}>
                  {t(blockedReason === BLOCKED_ACCOUNTS_MISSING ? "agentLaunch.chooseAccount" : "agentLaunch.fixAccount")}
                </button>
                <button className="btn btn-secondary btn-sm" disabled={accountsState.busy}
                  onClick={() => { haptic(); accountsState.reload(); }}>
                  {t("agentLaunch.retry")}
                </button>
              </>
            )}
          </div>
        )}

        {/* Аккаунт — на виду, а не за ⚙: человек держит вторую подписку именно
            для того, чтобы переключаться, когда упёрся в лимит. Тут же и
            остаток — иначе выбирать пришлось бы наугад. Строка идёт ОТДЕЛЬНЫМ
            рядом: в общем ряду с кнопками имя аккаунта ужималось до «..». */}
        {action === "run" && agentAccounts.length > 0 && (
          <button
            className={`agent-account-line${accountsShown ? " open" : ""}`}
            onClick={() => { haptic(); setAccountsOpen(accountsShown ? "" : a.id); setPinnedAccountId(""); accountsState.clearError(); }}
            aria-expanded={accountsShown}
          >
            <span className="agent-account-who">
              {"\u{1F464}"} {activeAccount ? accountTitle(activeAccount) : t("agentLaunch.accountDefault")}
            </span>
            {activeAccount && accountLeftLabel(accountsState.usage, a.id, activeAccount.id) && (
              <span className="agent-account-left">
                {accountLeftLabel(accountsState.usage, a.id, activeAccount.id)}
              </span>
            )}
            <span className="agent-account-caret" aria-hidden>{accountsShown ? "▴" : "▾"}</span>
          </button>
        )}

        {accountsShown && (
          <AgentAccountsList
            agentID={a.id}
            state={clientAccountsState}
            onSignIn={(acc) => {
              const signInAccount = launchAccountOf(a, acc);
              if (signInAccount?.blockedReason) return;
              const signInCmd = composeLaunch(a.cli || "", EMPTY_PREFS, launchCtx(a, signInAccount));
              if (signInCmd) void runAgent(a, signInCmd, acc);
            }}
          />
        )}

        {open && (
          <div className="agent-launch-flags">
            {a.supports_resume && <p className="agent-resume-hint">{t("agentLaunch.resumeHint")}</p>}
            {flags.map((f) => {
              const on = (prefs.flags || []).includes(f.flag);
              return (
                <button
                  key={f.flag}
                  className={`agent-flag${on ? " on" : ""}${f.danger ? " danger" : ""}`}
                  onClick={() => { haptic(); setPrefsOf(a.id, toggleFlag(prefs, f.flag)); }}
                  aria-pressed={on}
                >
                  <span className="agent-flag-box" aria-hidden>{on ? "✓" : ""}</span>
                  <span className="agent-flag-body">
                    <span className="agent-flag-title">{ownedText(f.title)}</span>
                    {f.hint && <span className="agent-flag-hint">{ownedText(f.hint)}</span>}
                  </span>
                  <code className="agent-flag-code">{f.flag}</code>
                </button>
              );
            })}
            <input
              className="agent-flag-extra"
              value={prefs.extra}
              placeholder={t("agentLaunch.extraPlaceholder")}
              onChange={(e) => setPrefsOf(a.id, { ...prefs, extra: e.target.value })}
              aria-label={t("agentLaunch.extraPlaceholder")}
            />
            {/* Опасное называем опасным ОДИН раз и словами, а не иконкой на
                каждой строке: человек уже согласился, повторять нотацию —
                мешать работать. */}
            {dangerPicked && <div className="agent-launch-danger-note">{t("agentLaunch.dangerNote")}</div>}
            <div className="agent-launch-preview"><code>{runCmd}</code></div>
          </div>
        )}
      </div>
    );
  };

  return (
    <div className="snippets-backdrop" onClick={onClose}>
      <div
        ref={sheetRef}
        className="snippets-sheet"
        role="dialog"
        aria-modal="true"
        aria-labelledby="agent-launch-title"
        tabIndex={-1}
        onClick={(e) => e.stopPropagation()}
        onKeyDown={onSheetKeyDown}
      >
        <div className="snippets-head">
          {/* Аудит ИА 02.09.2026, P1-17: одно имя у двери и у шторки за ней —
              «Запустить AI-агента», как на кнопке в списке терминалов. */}
          <h3 id="agent-launch-title">{"🤖"} {t("pty.launchAgent")}</h3>
          <button onClick={onClose} className="snippets-close" aria-label={t("modal.close")}>×</button>
        </div>

        {/* «Что дальше» — единственный шаг, которого не хватало на чистой
            машине: команда установки ушла, человек вернулся, а npm в этом же
            шелле по-прежнему «не распознан». Здесь ему и говорят почему, и
            дают обе кнопки: обновить PATH и проверить снова. */}
        {showNextStep && (
          <div className="agent-launch-next">
            <div className="agent-launch-next-title">{t("agentLaunch.afterInstallTitle")}</div>
            <div className="agent-launch-next-text">
              {nodeSent ? t("agentLaunch.afterNodeText") : t("agentLaunch.afterInstallText")}
            </div>
            <div className="agent-launch-next-actions">
              {nodeSent && (
                <button className="btn btn-sm btn-secondary" disabled={rechecking} onClick={refreshPath}>
                  {t("agentLaunch.refreshPath")}
                </button>
              )}
              <button className="btn btn-sm btn-primary" disabled={rechecking} onClick={recheck}>
                {rechecking ? t("agentLaunch.rechecking") : t("agentLaunch.recheck")}
              </button>
            </div>
          </div>
        )}

        {/* Тупик, у которого не было ни хода, ни объяснения: если компьютер не
            вернул аккаунты, гасли ВСЕ кнопки запуска, а на строках стояло
            «Запуск заблокирован: компьютер не вернул основной аккаунт агента».
            Из этой фразы не следует ни причина, ни действие — а причин ровно
            две, и обе поправимые (аудит онбординга 30.08.2026). */}
        {accountsBlocked && (
          <div className="agent-launch-next">
            <div className="agent-launch-next-title">{t("agentLaunch.accountsBlockedTitle")}</div>
            <div className="agent-launch-next-text">{t("agentLaunch.accountsBlockedText")}</div>
            <div className="agent-launch-next-actions">
              <button
                className="btn btn-sm btn-primary"
                disabled={accountsState.busy}
                onClick={() => { haptic(); accountsState.reload(); }}
              >
                {t("agentLaunch.retry")}
              </button>
            </div>
          </div>
        )}

        {/* Аккаунты пришли, но активного нет: запуск гаснет у всех агентов, а
            причина стояла только сноской в серой строке. Ход здесь двойной —
            перечитать (компьютер мог не успеть) и уйти туда, где активный
            аккаунт выбирают руками. */}
        {noActiveAccount && (
          <div className="agent-launch-next">
            <div className="agent-launch-next-title">{t("agentLaunch.noActiveAccountTitle")}</div>
            <div className="agent-launch-next-text">{t("agentLaunch.noActiveAccountText")}</div>
            <div className="agent-launch-next-actions">
              <button
                className="btn btn-sm btn-primary"
                onClick={() => { haptic(); onClose(); navigate("/agents"); }}
              >
                {t("agentLaunch.openAgents")}
              </button>
              <button
                className="btn btn-sm btn-secondary"
                disabled={accountsState.busy}
                onClick={() => { haptic(); accountsState.reload(); }}
              >
                {t("agentLaunch.retry")}
              </button>
            </div>
          </div>
        )}

        <div className="snippets-list">
          {agents === null ? (
            <div className="agent-launch-empty"><span className="spinner spinner-sm" /></div>
          ) : loadError ? (
            <div className="agent-launch-empty">
              <div>{loadError}</div>
              <button className="btn btn-sm btn-secondary" onClick={() => { haptic(); load(); }}>
                {t("agentLaunch.retry")}
              </button>
            </div>
          ) : cliAgents.length === 0 ? (
            /* Пусто — это НЕ ошибка связи. Здесь стояло «Не удалось получить
               список агентов»: запрос прошёл, компьютер честно ответил пустым
               списком, а человек читал про сбой и не получал ни одного хода
               (замер J03, ветка 1a, 05.09.2026). Причина у пустого реестра
               одна и поправимая — компьютеру нечего перечислять, обычно из-за
               старой версии Remotai на нём. */
            <div className="agent-launch-empty">
              <div>{t("agentLaunch.emptyRegistry")}</div>
              <button className="btn btn-sm btn-secondary" onClick={() => { haptic(); load(); }}>
                {t("agentLaunch.retry")}
              </button>
            </div>
          ) : (
            <>
              {installed.length > 0 && installed.map((a) => row(a, "run"))}
              {installed.length === 0 && (
                <div className="agent-launch-empty">{t("agentLaunch.emptyInstalled")}</div>
              )}
              {/* Ход для человека БЕЗ подписки — на главном пути, а не в
                  свёрнутой секции соседнего экрана. Плацдарм продукта — это
                  как раз он: до этой строки в шторке запуска слова
                  «OpenRouter» не было вовсе (аудит онбординга 30.08.2026). */}
              {offerOpenRouter && (
                <button
                  type="button"
                  className="agent-launch-or"
                  onClick={() => { haptic(); onClose(); navigate("/agents", { state: { focus: "openrouter" } }); }}
                >
                  <span className="agent-launch-or-title">{t("agentLaunch.noSubscription")}</span>
                  <span className="agent-launch-or-note">{t("agentLaunch.noSubscriptionNote")}</span>
                </button>
              )}
              {missing.length > 0 && (
                <>
                  {npmInstalls && (
                    <>
                      <div className="snippets-group">{t("agentLaunch.nodeStep")}</div>
                      <div className="agent-launch-row agent-launch-prereq">
                        <span className="agent-launch-icon">{"📦"}</span>
                        <span className="agent-launch-body">
                          <span className="agent-launch-name">Node.js LTS</span>
                          <span className="agent-launch-meta">{t("agentLaunch.nodeHint")}</span>
                        </span>
                        <span className="agent-launch-actions">
                          <button className="btn btn-sm btn-secondary" onClick={() => runInstall(nodeCmd, "node")}>
                            {t("agentLaunch.nodeInstall")}
                          </button>
                        </span>
                      </div>
                      {/* Предупреждаем ДО того, как человек упрётся: подсказка
                          шага 1 обещает, что «npm не распознан» лечится этим
                          шагом, но в уже запущенном шелле PATH останется
                          старым — и та же ошибка придёт снова. */}
                      <div className="agent-launch-note">{t("agentLaunch.nodePathWarn")}</div>
                    </>
                  )}
                  <div className="snippets-group">
                    {npmInstalls ? t("agentLaunch.installStep") : t("agentLaunch.installGroup")}
                  </div>
                  {missing.map((a) => row(a, "install"))}
                </>
              )}
            </>
          )}
        </div>

        <div className="agent-launch-hint">
          {remote
            ? t("ui.agentlaunchsheet.mfef107edc9")
            : t("agentLaunch.hint")}
        </div>
      </div>
    </div>
  );
}
import { ownedText } from "@tgcontrol/shared";
