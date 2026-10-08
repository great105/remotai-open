// @ts-ignore — runtime Vitest всегда Node, но browser tsconfig APK не подключает @types/node.
import { execFileSync } from "node:child_process";
import { beforeEach, describe, expect, it } from "vitest";
import {
  composeLaunch, composeRemoteLaunch, EMPTY_PREFS, hasDanger, launchAccountBlockedReason,
  launchAccountForAgent,
  loadPrefs, normalizeAccountProxyForClient, recordedResumeAccount, savePrefs, toggleFlag,
} from "./agentLaunch";

// Минимальный localStorage: модуль обязан работать и без DOM.
beforeEach(() => {
  const store = new Map<string, string>();
  (globalThis as any).localStorage = {
    getItem: (k: string) => store.get(k) ?? null,
    setItem: (k: string, v: string) => { store.set(k, v); },
    removeItem: (k: string) => { store.delete(k); },
  };
});

describe("сборка команды запуска", () => {
  it("без флагов — ровно та же команда, что и раньше", () => {
    expect(composeLaunch("claude")).toBe("claude");
    expect(composeLaunch("claude", EMPTY_PREFS)).toBe("claude");
  });

  it("флаги дописываются в порядке выбора", () => {
    expect(composeLaunch("claude", { flags: ["--dangerously-skip-permissions", "--continue"], extra: "" }))
      .toBe("claude --dangerously-skip-permissions --continue");
  });

  it("своё дописанное идёт ПОСЛЕДНИМ — там же, где человек его набирал", () => {
    expect(composeLaunch("kimi", { flags: ["--yolo"], extra: "--model k2" }))
      .toBe("kimi --yolo --model k2");
  });

  it("повтор флага схлопывается: часть агентов считает его ошибкой разбора", () => {
    expect(composeLaunch("claude", { flags: ["--continue", "--continue"], extra: "" }))
      .toBe("claude --continue");
  });

  it("лишние пробелы и пустые значения не превращаются в аргументы", () => {
    expect(composeLaunch("claude", { flags: ["", "  ", "--verbose"], extra: "   --model   sonnet  " }))
      .toBe("claude --verbose --model sonnet");
  });

  it("без команды агента собирать нечего", () => {
    expect(composeLaunch("", { flags: ["--yolo"], extra: "" })).toBe("");
  });
});

describe("запуск под выбранным аккаунтом", () => {
  const claudeAccount = { envName: "CLAUDE_CONFIG_DIR", envValue: "C:\\Users\\user\\.tgcontrol-accounts\\claude\\acc-1" };

  it("на Windows переменная ставится выражением PowerShell — префикса env там нет", () => {
    const cmd = composeLaunch("claude", EMPTY_PREFS, { account: claudeAccount });
    expect(cmd).toContain("$env:CLAUDE_CONFIG_DIR='C:\\Users\\user\\.tgcontrol-accounts\\claude\\acc-1'");
    expect(cmd).toContain("try {");
    expect(cmd).toContain("claude } finally {");
  });

  it("на POSIX — префиксом env, вместе с окружением флагов", () => {
    const cmd = composeLaunch("claude", { flags: ["--dangerously-skip-permissions"], extra: "" }, {
      posix: true,
      specs: [{ flag: "--dangerously-skip-permissions", env: ["IS_SANDBOX=1"] }],
      account: { envName: "CLAUDE_CONFIG_DIR", envValue: "/root/.remotai-accounts/claude-2" },
    });
    expect(cmd).toContain("env -u CLAUDE_CONFIG_DIR -u HTTP_PROXY");
    expect(cmd).toContain("IS_SANDBOX=1 CLAUDE_CONFIG_DIR='/root/.remotai-accounts/claude-2'");
    expect(cmd.endsWith("claude --dangerously-skip-permissions")).toBe(true);
  });

  it("путь с пробелом остаётся ОДНИМ значением: иначе агент стартует с пустым каталогом", () => {
    expect(composeLaunch("codex", EMPTY_PREFS, { account: { envName: "CODEX_HOME", envValue: "C:\\Users\\Иван Петров\\acc" } }))
      .toContain("$env:CODEX_HOME='C:\\Users\\Иван Петров\\acc'");
    expect(composeLaunch("codex", EMPTY_PREFS, { posix: true, account: { envName: "CODEX_HOME", envValue: "/home/ivan petrov/acc" } }))
      .toContain("CODEX_HOME='/home/ivan petrov/acc' codex");
  });

  it("одинарная кавычка в пути экранируется по правилам своего шелла", () => {
    expect(composeLaunch("kimi", EMPTY_PREFS, { account: { envName: "KIMI_CODE_HOME", envValue: "C:\\d'Artagnan" } }))
      .toContain("$env:KIMI_CODE_HOME='C:\\d''Artagnan'");
    expect(composeLaunch("kimi", EMPTY_PREFS, { posix: true, account: { envName: "KIMI_CODE_HOME", envValue: "/home/d'artagnan" } }))
      .toContain("KIMI_CODE_HOME='/home/d'\\''artagnan' kimi");
  });

  it("основной аккаунт на Windows снимает прежний profile env только на время CLI", () => {
    const windows = composeLaunch("claude", EMPTY_PREFS, { account: { envName: "CLAUDE_CONFIG_DIR", envValue: "" } });
    expect(windows).toContain("Remove-Item -LiteralPath ('Env:'+$__remotaiEnvName)");
    expect(windows).toContain("try { claude } finally {");
    expect(windows).not.toContain("$env:CLAUDE_CONFIG_DIR=");
    expect(composeLaunch("claude", EMPTY_PREFS, { posix: true, account: { envName: "", envValue: "/x" } }))
      .toBe("claude");
  });

  it("на СЕРВЕР аккаунт компьютера не уезжает: такого каталога там нет", () => {
    const cmd = composeRemoteLaunch("claude", EMPTY_PREFS, { account: claudeAccount }, "нет агента", ["claude"]);
    expect(cmd).not.toContain("CLAUDE_CONFIG_DIR");
    expect(cmd).toContain("then claude;");
  });
});

describe("хуки агента (launch_args)", () => {
  const claudeArgs = ["--settings", "C:\\Users\\Иван Петров\\AppData\\Local\\Remotai\\agent-hooks\\claude-hooks.json"];
  const codexWin = ["-c", "notify=['C:/Users/user/AppData/Local/Programs/Remotai/remotai.exe','hook','codex']"];

  it("Windows: флаг как есть, путь с пробелом — одним значением в кавычках PowerShell", () => {
    expect(composeLaunch("claude", EMPTY_PREFS, { launchArgs: claudeArgs }))
      .toBe("claude --settings 'C:\\Users\\Иван Петров\\AppData\\Local\\Remotai\\agent-hooks\\claude-hooks.json'");
  });

  it("Codex на Windows: апострофы TOML удваиваются по правилам PowerShell", () => {
    expect(composeLaunch("codex", EMPTY_PREFS, { launchArgs: codexWin }))
      .toBe("codex -c 'notify=[''C:/Users/user/AppData/Local/Programs/Remotai/remotai.exe'',''hook'',''codex'']'");
  });

  it("POSIX: значение в одинарных кавычках шелла", () => {
    expect(composeLaunch("codex", EMPTY_PREFS, { posix: true, launchArgs: ["-c", 'notify=["/usr/local/bin/remotai","hook","codex"]'] }))
      .toBe(`codex -c 'notify=["/usr/local/bin/remotai","hook","codex"]'`);
  });

  it("хуки идут после флагов, но до ручной добавки", () => {
    expect(composeLaunch("claude", { flags: ["--continue"], extra: "--model sonnet" }, { launchArgs: ["--settings", "/h/c.json"] }))
      .toBe("claude --continue --settings /h/c.json --model sonnet");
  });

  it("«продолжить» тоже получает хуки", () => {
    expect(composeLaunch("claude --continue", EMPTY_PREFS, { launchArgs: ["--settings", "/h/c.json"] }))
      .toBe("claude --continue --settings /h/c.json");
  });

  it("на СЕРВЕР хуки не уезжают: файла настроек и remotai.exe там нет", () => {
    const cmd = composeRemoteLaunch("claude", EMPTY_PREFS, { launchArgs: claudeArgs }, "нет");
    expect(cmd).not.toContain("--settings");
    expect(cmd).not.toContain("claude-hooks.json");
  });

  it("старый агент без launch_args — команда как раньше", () => {
    expect(composeLaunch("claude", EMPTY_PREFS, { launchArgs: undefined })).toBe("claude");
    expect(composeLaunch("claude", EMPTY_PREFS, { launchArgs: [] })).toBe("claude");
  });
});

describe("предупреждение об опасных флагах", () => {
  const danger = ["--dangerously-skip-permissions", "--yolo"];

  it("видит опасный флаг среди выбранных", () => {
    expect(hasDanger({ flags: ["--continue", "--yolo"], extra: "" }, danger)).toBe(true);
  });

  it("безобидный набор опасным не считает", () => {
    expect(hasDanger({ flags: ["--continue"], extra: "" }, danger)).toBe(false);
  });

  it("опасное, дописанное РУКАМИ, предупреждением не считается — человек написал это сам", () => {
    expect(hasDanger({ flags: [], extra: "--yolo" }, danger)).toBe(false);
  });
});

describe("память выбора", () => {
  it("выбор переживает закрытие шторки и хранится ПО АГЕНТУ", () => {
    savePrefs("claude", { flags: ["--dangerously-skip-permissions"], extra: "--model opus" });
    savePrefs("kimi", { flags: ["--auto"], extra: "" });
    expect(loadPrefs("claude")).toEqual({ flags: ["--dangerously-skip-permissions"], extra: "--model opus" });
    expect(loadPrefs("kimi")).toEqual({ flags: ["--auto"], extra: "" });
    expect(loadPrefs("codex")).toEqual({ flags: [], extra: "" });
  });

  it("пустой выбор запись удаляет — хранилище не копит мусор", () => {
    savePrefs("claude", { flags: ["--verbose"], extra: "" });
    savePrefs("claude", { flags: [], extra: "  " });
    expect(loadPrefs("claude")).toEqual({ flags: [], extra: "" });
    expect(localStorage.getItem("pty.agentLaunch.v1")).toBe("{}");
  });

  it("мусор в хранилище не роняет шторку", () => {
    localStorage.setItem("pty.agentLaunch.v1", "не json");
    expect(loadPrefs("claude")).toEqual({ flags: [], extra: "" });
  });

  it("чужие типы внутри записи отфильтровываются", () => {
    localStorage.setItem("pty.agentLaunch.v1", JSON.stringify({ claude: { flags: ["--ok", 5, null], extra: 7 } }));
    expect(loadPrefs("claude")).toEqual({ flags: ["--ok"], extra: "" });
  });
});

describe("переключение флага", () => {
  it("включает и выключает, не трогая остальное", () => {
    let p = { flags: [] as string[], extra: "--model x" };
    p = toggleFlag(p, "--yolo");
    expect(p).toEqual({ flags: ["--yolo"], extra: "--model x" });
    p = toggleFlag(p, "--continue");
    expect(p.flags).toEqual(["--yolo", "--continue"]);
    p = toggleFlag(p, "--yolo");
    expect(p).toEqual({ flags: ["--continue"], extra: "--model x" });
  });
});

// ── На сервере команда пишется иначе ────────────────────────────────────────
// Всё ниже — не догадки, а замер на живом Linux под root (04.08.2026).
describe("сервер против своего ПК", () => {
  const CLAUDE = [
    { flag: "--dangerously-skip-permissions", danger: true, env: ["IS_SANDBOX=1"] },
    { flag: "--continue" },
  ];
  const GEMINI = [
    { flag: "--yolo", danger: true, with: ["--skip-trust"] },
    { flag: "--skip-trust" },
  ];

  it("под POSIX опасный флаг claude едет с IS_SANDBOX — иначе под root он ОТКАЗЫВАЕТСЯ запускаться", () => {
    expect(composeLaunch("claude", { flags: ["--dangerously-skip-permissions"], extra: "" },
      { posix: true, specs: CLAUDE }))
      .toBe("env IS_SANDBOX=1 claude --dangerously-skip-permissions");
  });

  it("на своём Windows-ПК префикса НЕТ: в PowerShell такого синтаксиса не существует", () => {
    expect(composeLaunch("claude", { flags: ["--dangerously-skip-permissions"], extra: "" },
      { posix: false, specs: CLAUDE }))
      .toBe("claude --dangerously-skip-permissions");
  });

  it("окружение не дублируется, даже если его просят два флага", () => {
    const specs = [
      { flag: "--a", env: ["IS_SANDBOX=1"] },
      { flag: "--b", env: ["IS_SANDBOX=1"] },
    ];
    expect(composeLaunch("x", { flags: ["--a", "--b"], extra: "" }, { posix: true, specs }))
      .toBe("env IS_SANDBOX=1 x --a --b");
  });

  it("спутник добавляется сам: без --skip-trust gemini МОЛЧА возвращает подтверждения", () => {
    expect(composeLaunch("gemini", { flags: ["--yolo"], extra: "" }, { posix: true, specs: GEMINI }))
      .toBe("gemini --yolo --skip-trust");
  });

  it("спутник, выбранный руками, не задваивается", () => {
    expect(composeLaunch("gemini", { flags: ["--yolo", "--skip-trust"], extra: "" }, { posix: true, specs: GEMINI }))
      .toBe("gemini --yolo --skip-trust");
  });

  it("флаг не для этой площадки в команду не попадает", () => {
    const specs = [{ flag: "--posix-only", only: "posix" }, { flag: "--win-only", only: "windows" }];
    expect(composeLaunch("x", { flags: ["--posix-only", "--win-only"], extra: "" }, { posix: true, specs }))
      .toBe("x --posix-only");
    expect(composeLaunch("x", { flags: ["--posix-only", "--win-only"], extra: "" }, { posix: false, specs }))
      .toBe("x --win-only");
  });

  it("без описаний флагов ведёт себя как раньше — старый агент не ломает запуск", () => {
    expect(composeLaunch("claude", { flags: ["--dangerously-skip-permissions"], extra: "" }, { posix: true }))
      .toBe("claude --dangerously-skip-permissions");
  });
});

// ── Запуск на СЕРВЕРЕ: строка целиком ───────────────────────────────────────
describe("команда запуска на сервере", () => {
  const CLAUDE = [{ flag: "--dangerously-skip-permissions", danger: true, env: ["IS_SANDBOX=1"] }];

  it("НЕТ exec: он замещал единственный шелл SSH-сессии, и выход из агента ронял сессию", () => {
    const cmd = composeRemoteLaunch("claude", EMPTY_PREFS, { specs: CLAUDE });
    expect(cmd).not.toContain("exec ");
  });

  it("НЕТ конструкции && … || …: без exec она печатала бы «не установлен» на любой ошибке агента", () => {
    const cmd = composeRemoteLaunch("claude", EMPTY_PREFS, { specs: CLAUDE });
    expect(cmd).not.toContain("||");
    expect(cmd).toContain("if command -v claude");
    expect(cmd).toContain("else echo");
    expect(cmd).toMatch(/fi$/);
  });

  it("серверная строка НЕСЁТ окружение — это и есть то, чего не хватало под root", () => {
    const cmd = composeRemoteLaunch("claude", { flags: ["--dangerously-skip-permissions"], extra: "" }, { specs: CLAUDE });
    expect(cmd).toBe(
      "if command -v claude >/dev/null 2>&1; then env IS_SANDBOX=1 claude --dangerously-skip-permissions; "
      + 'else echo "REMOTAI_AGENT_MISSING Агент claude не установлен на сервере"; fi',
    );
  });

  it("сервер считается POSIX всегда, даже если сам Remotai на Windows", () => {
    const cmd = composeRemoteLaunch("claude", { flags: ["--dangerously-skip-permissions"], extra: "" },
      { posix: false, specs: CLAUDE });
    expect(cmd).toContain("env IS_SANDBOX=1");
  });

  it("кавычки в сообщении не рвут строку", () => {
    const cmd = composeRemoteLaunch("claude", EMPTY_PREFS, {}, 'нет "такого" агента');
    expect(cmd).toContain("else echo \"REMOTAI_AGENT_MISSING нет 'такого' агента\"");
  });

  it("без команды агента собирать нечего", () => {
    expect(composeRemoteLaunch("", EMPTY_PREFS)).toBe("");
  });
});

// ── Сервер: имя бинаря и метка отказа ───────────────────────────────────────
describe("сервер: все имена бинаря и метка отказа", () => {
  it("проверяются ВСЕ известные имена, а не одно", () => {
    const cmd = composeRemoteLaunch("github-copilot-cli", EMPTY_PREFS, {}, "нет копайлота",
      ["github-copilot-cli", "copilot"]);
    expect(cmd).toContain("if command -v github-copilot-cli");
    expect(cmd).toContain("elif command -v copilot");
    // Каждая ветка запускает СВОЁ имя, а не имя с компьютера.
    expect(cmd).toContain("then copilot");
  });

  it("повтор имени не плодит лишнюю ветку", () => {
    const cmd = composeRemoteLaunch("claude", EMPTY_PREFS, {}, "", ["claude", "claude"]);
    expect(cmd.match(/command -v claude/g)?.length).toBe(1);
  });

  it("метка отказа обязана быть: без неё детектор читает имя агента в сообщении как «агент работает»", () => {
    const cmd = composeRemoteLaunch("claude", EMPTY_PREFS);
    expect(cmd).toContain("REMOTAI_AGENT_MISSING");
    // И метка стоит ТОЛЬКО в ветке отказа, а не в запуске.
    expect(cmd.indexOf("REMOTAI_AGENT_MISSING")).toBeGreaterThan(cmd.indexOf("then "));
  });
});

describe("модель OpenRouter в команде запуска", () => {
  const model = { flag: "-m", value: "openrouter/cohere/north-mini-code:free" };

  it("модель идёт сразу после имени агента — её видно, а не ищут среди флагов", () => {
    expect(composeLaunch("opencode", EMPTY_PREFS, { model }))
      .toBe("opencode -m openrouter/cohere/north-mini-code:free");
  });

  it("модель стоит перед флагами и своей добавкой", () => {
    expect(composeLaunch("opencode", { flags: ["--auto"], extra: "--thinking" }, { model }))
      .toBe("opencode -m openrouter/cohere/north-mini-code:free --auto --thinking");
  });

  it("агент, у которого флаг модели не проверен, модель НЕ получает", () => {
    // Пустой model_flag в реестре значит «мы не знаем, чем этот CLI принимает
    // модель». Подставленный наугад флаг уронил бы разбор аргументов, и человек
    // с телефона увидел бы только «агент не запустился».
    expect(composeLaunch("kimi", EMPTY_PREFS, { model: { flag: "", value: "openrouter/x" } }))
      .toBe("kimi");
  });

  it("вписанное руками сильнее настройки: второй флаг модели не появляется", () => {
    expect(composeLaunch("opencode", { flags: [], extra: "-m openrouter/openai/gpt-oss-20b:free" }, { model }))
      .toBe("opencode -m openrouter/openai/gpt-oss-20b:free");
    expect(composeLaunch("opencode", { flags: [], extra: "--model openrouter/openai/gpt-oss-20b:free" }, { model }))
      .toBe("opencode --model openrouter/openai/gpt-oss-20b:free");
  });

  it("на СЕРВЕР модель не едет: ключ живёт в окружении этого компьютера", () => {
    const cmd = composeRemoteLaunch("opencode", EMPTY_PREFS, { model }, "", ["opencode"]);
    expect(cmd).not.toContain("-m openrouter/");
    expect(cmd).toContain("then opencode");
  });

  it("модель и аккаунт уживаются: окружение аккаунта остаётся на месте", () => {
    const cmd = composeLaunch("opencode", EMPTY_PREFS, {
      model,
      account: { envName: "OPENCODE_CONFIG", envValue: "C:\\Users\\Иван Петров\\профиль" },
    });
    expect(cmd).toContain("$env:OPENCODE_CONFIG='C:\\Users\\Иван Петров\\профиль'");
    expect(cmd).toContain("opencode -m openrouter/cohere/north-mini-code:free } finally {");
  });
});

/**
 * Свой прокси у аккаунта (задача владельца от 05.08.2026).
 *
 * Список proxy env приходит с компьютера после проверки transport-а агента —
 * клиент его не сочиняет и не обещает универсальный Node-флаг.
 */
describe("прокси аккаунта в команде запуска", () => {
  const proxyEnv = [
    { name: "CLAUDE_CONFIG_DIR", value: "C:\\acc" },
    { name: "HTTPS_PROXY", value: "http://10.0.0.1:8080" },
    { name: "HTTP_PROXY", value: "http://10.0.0.1:8080" },
    { name: "ALL_PROXY", value: "http://10.0.0.1:8080" },
    { name: "NO_PROXY", value: "localhost,127.0.0.1,::1" },
    { name: "https_proxy", value: "http://10.0.0.1:8080" },
    { name: "http_proxy", value: "http://10.0.0.1:8080" },
    { name: "all_proxy", value: "http://10.0.0.1:8080" },
    { name: "no_proxy", value: "localhost, 127.0.0.1, ::1" },
  ];
  const windowsContext = {
    agentID: "claude",
    accountEnvName: "CLAUDE_CONFIG_DIR",
    proxyContractVersion: 1,
    account: {
      agentID: "claude",
      envName: "CLAUDE_CONFIG_DIR",
      envValue: "C:\\acc",
      env: proxyEnv,
      proxyContractVersion: 1,
      proxyConfigured: true,
    },
  };

  it("в PowerShell safe-переменные применяются только внутри временного scope", () => {
    const cmd = composeLaunch("claude", EMPTY_PREFS, windowsContext);
    expect(cmd).toContain("$env:CLAUDE_CONFIG_DIR='C:\\acc'");
    expect(cmd).toContain("$env:HTTPS_PROXY='http://10.0.0.1:8080'");
    expect(cmd).not.toContain("$env:NODE_USE_ENV_PROXY=");
    expect(cmd).toContain("try {");
    expect(cmd).toContain("claude } finally {");
  });

  it("в POSIX все переменные идут одним env", () => {
    const cmd = composeLaunch("claude", { flags: ["--dangerously-skip-permissions"], extra: "" }, {
      posix: true,
      agentID: "claude",
      accountEnvName: "CLAUDE_CONFIG_DIR",
      proxyContractVersion: 1,
      account: {
        agentID: "claude", envName: "CLAUDE_CONFIG_DIR", envValue: "/root/acc",
        env: proxyEnv.map((pair) => pair.name === "CLAUDE_CONFIG_DIR"
          ? { ...pair, value: "/root/acc" }
          : pair),
        proxyContractVersion: 1, proxyConfigured: true,
      },
    });
    expect(cmd.startsWith("env ")).toBe(true);
    expect(cmd).toContain("HTTPS_PROXY='http://10.0.0.1:8080'");
    expect(cmd).toContain("https_proxy='http://10.0.0.1:8080'");
    expect(cmd).toContain("NO_PROXY='localhost,127.0.0.1,::1'");
    expect(cmd).toContain("no_proxy='localhost,127.0.0.1,::1'");
    expect(cmd).toContain("-u NODE_USE_ENV_PROXY");
    expect(cmd).not.toContain("NODE_USE_ENV_PROXY='");
  });

  it("дубли не плодятся: каталог приехал и списком, и старой парой", () => {
    const cmd = composeLaunch("claude", EMPTY_PREFS, windowsContext);
    expect(cmd.match(/\$env:CLAUDE_CONFIG_DIR=/g)).toHaveLength(1);
  });

  it("старый агент без proxy/env сохраняет обычную пару профиля", () => {
    // Пульт обновляется отдельно от компьютера: на старом агенте поля `env`
    // нет вовсе; proxy contract для этой одной пары не нужен.
    const cmd = composeLaunch("claude", EMPTY_PREFS, {
      account: { envName: "CLAUDE_CONFIG_DIR", envValue: "C:\\acc" },
    });
    expect(cmd).toContain("$env:CLAUDE_CONFIG_DIR='C:\\acc'");
    expect(cmd).toContain("claude } finally {");
  });

  it("основной/direct запуск очищает прежние profile/proxy и ничего не подставляет", () => {
    const cmd = composeLaunch("claude", EMPTY_PREFS, {
      agentID: "claude",
      accountEnvName: "CLAUDE_CONFIG_DIR",
      proxyContractVersion: 1,
    });
    expect(cmd).toContain("'CLAUDE_CONFIG_DIR','HTTP_PROXY','HTTPS_PROXY','ALL_PROXY','NO_PROXY'");
    expect(cmd).toContain("try { claude } finally {");
    expect(cmd).not.toContain("$env:CLAUDE_CONFIG_DIR=");
    expect(cmd).not.toContain("$env:HTTPS_PROXY=");
  });
});

describe("клиентский proxy contract: fail closed", () => {
  const account = (extra: Record<string, unknown> = {}) => ({
    id: "acc-1",
    agent_id: "claude",
    label: "рабочий",
    env_name: "CLAUDE_CONFIG_DIR",
    env_value: "C:\\acc",
    env: [{ name: "CLAUDE_CONFIG_DIR", value: "C:\\acc" }],
    ...extra,
  });
  const fullProxyEnv = (proxy: string, profile = "C:\\acc") => [
    { name: "CLAUDE_CONFIG_DIR", value: profile },
    ...["HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"]
      .map((name) => ({ name, value: proxy })),
    { name: "NO_PROXY", value: "localhost,127.0.0.1,::1" },
    { name: "no_proxy", value: "localhost,127.0.0.1,::1" },
  ];

  it("новый клиент не доверяет proxy env старого agent без contract", () => {
    const oldServer = account({
      proxy: "http://proxy.corp:8080",
      env: [
        { name: "CLAUDE_CONFIG_DIR", value: "C:\\acc" },
        { name: "HTTPS_PROXY", value: "http://proxy.corp:8080" },
      ],
    });
    expect(launchAccountBlockedReason(oldServer, undefined)).toContain("обновите Remotai");
    const normalized = normalizeAccountProxyForClient(oldServer, undefined);
    expect(normalized.proxy).toBeUndefined();
    expect(normalized.env).toEqual([{ name: "CLAUDE_CONFIG_DIR", value: "C:\\acc" }]);
    expect(normalized.proxy_blocked).toBe(true);
  });

  it("сырой legacy proxy без env тоже блокируется и удаляется", () => {
    const oldServer = account({
      proxy: "http://worker@example.com:secret@legacy.proxy:8080",
      env: undefined,
    });
    expect(launchAccountBlockedReason(oldServer, 0)).not.toBe("");
    const normalized = normalizeAccountProxyForClient(oldServer, 0);
    expect(normalized.proxy).toBeUndefined();
    expect(normalized.env).toEqual([{ name: "CLAUDE_CONFIG_DIR", value: "C:\\acc" }]);
    expect(JSON.stringify(normalized)).not.toContain("worker@example.com");
    expect(JSON.stringify(normalized)).not.toContain("secret");
  });

  it("warning-only ответ промежуточного server тоже блокируется и локализуется", () => {
    const warned = account({
      proxy: "http://proxy.corp:8080",
      proxy_warning: "upstream secret diagnostic",
      env: [
        { name: "CLAUDE_CONFIG_DIR", value: "C:\\acc" },
        { name: "HTTPS_PROXY", value: "http://proxy.corp:8080" },
      ],
    });
    const normalized = normalizeAccountProxyForClient(warned, 1);
    expect(normalized.proxy_blocked).toBe(true);
    expect(normalized.proxy).toBeUndefined();
    expect(normalized.env).toEqual([{ name: "CLAUDE_CONFIG_DIR", value: "C:\\acc" }]);
    expect(normalized.proxy_warning).not.toContain("upstream secret");
    expect(launchAccountBlockedReason(normalized, 1)).not.toBe("");
  });

  it("SOCKS отсекается даже при contract=1", () => {
    const socks = account({
      proxy: "socks5://10.0.0.1:1080",
      env: [
        { name: "CLAUDE_CONFIG_DIR", value: "C:\\acc" },
        { name: "HTTPS_PROXY", value: "socks5://10.0.0.1:1080" },
        { name: "ALL_PROXY", value: "socks5://10.0.0.1:1080" },
      ],
    });
    expect(launchAccountBlockedReason(socks, 1)).toContain("HTTP/HTTPS");
    expect(JSON.stringify(normalizeAccountProxyForClient(socks, 1))).not.toContain("socks5://");
  });

  it("Gemini от старого agent тоже не получает proxy без contract", () => {
    const gemini = account({
      agent_id: "gemini",
      env_name: "GEMINI_CLI_HOME",
      env_value: "C:\\gemini-acc",
      env: [
        { name: "GEMINI_CLI_HOME", value: "C:\\gemini-acc" },
        { name: "HTTP_PROXY", value: "http://proxy.corp:8080" },
      ],
    });
    expect(launchAccountBlockedReason(gemini, 0)).not.toBe("");
    expect(normalizeAccountProxyForClient(gemini, 0).env)
      .toEqual([{ name: "GEMINI_CLI_HOME", value: "C:\\gemini-acc" }]);
  });

  it("userinfo не остаётся ни в proxy, ни в одной переменной", () => {
    const withUserinfo = account({
      proxy: "http://mail@example.com:secret@proxy.corp:8080",
      env: [
        { name: "CLAUDE_CONFIG_DIR", value: "C:\\acc" },
        { name: "HTTP_PROXY", value: "http://mail@example.com:secret@proxy.corp:8080" },
        { name: "HTTPS_PROXY", value: "http://mail@example.com:secret@proxy.corp:8080" },
      ],
    });
    const normalized = normalizeAccountProxyForClient(withUserinfo, 1);
    const serialized = JSON.stringify(normalized);
    expect(serialized).not.toContain("mail@example.com");
    expect(serialized).not.toContain("secret");
    expect(normalized.env).toEqual([{ name: "CLAUDE_CONFIG_DIR", value: "C:\\acc" }]);
    expect(normalized.proxy_warning).not.toContain("proxy.corp");
  });

  it("path/query/fragment proxy блокируются и секрет не остаётся в state", () => {
    for (const proxy of [
      "http://proxy.corp:8080/private",
      "http://proxy.corp:8080?token=SECRET",
      "http://proxy.corp:8080#SECRET",
    ]) {
      const unsafe = account({
        proxy,
        env: [
          { name: "CLAUDE_CONFIG_DIR", value: "C:\\acc" },
          { name: "HTTPS_PROXY", value: proxy },
        ],
      });
      const normalized = normalizeAccountProxyForClient(unsafe, 1);
      expect(normalized.proxy_blocked).toBe(true);
      expect(normalized.proxy).toBeUndefined();
      expect(normalized.env).toEqual([{ name: "CLAUDE_CONFIG_DIR", value: "C:\\acc" }]);
      expect(JSON.stringify(normalized)).not.toContain("SECRET");
      expect(JSON.stringify(normalized)).not.toContain("/private");
    }
  });

  it("account-capable launch ждёт verified список и не подменяет удалённый recorded account активным", () => {
    const agent = { id: "claude", account_env: "CLAUDE_CONFIG_DIR" };
    const active = account({ active: true });
    const loading = launchAccountForAgent(agent, {
      accounts: [active], proxyContractVersion: 1, accountsReady: false,
    });
    expect(loading?.blockedReason).toContain("ещё не проверены");
    expect(composeLaunch("claude --continue", EMPTY_PREFS, {
      agentID: agent.id,
      accountEnvName: agent.account_env,
      proxyContractVersion: 1,
      account: loading,
      blockedReason: loading?.blockedReason,
    })).toBe("");

    const missingRecorded = launchAccountForAgent(agent, {
      accounts: [active], proxyContractVersion: 1, accountsReady: true,
    }, null);
    expect(missingRecorded?.blockedReason).toContain("не вернул основной аккаунт");

    const selected = launchAccountForAgent(agent, {
      accounts: [active], proxyContractVersion: 1, accountsReady: true,
    }, active);
    expect(selected?.blockedReason).toBe("");
    expect(selected?.envValue).toBe("C:\\acc");
  });

  it("Resume выбирает записанный профиль, а пустой id означает default", () => {
    const secondary = account({ id: "acc-1", active: true });
    const primary = account({ id: "default", is_default: true, active: false, env_value: "" });
    const accounts = [secondary, primary];
    expect(recordedResumeAccount(accounts, "claude", "acc-1")?.id).toBe("acc-1");
    expect(recordedResumeAccount(accounts, "claude", "")?.id).toBe("default");
    expect(recordedResumeAccount(accounts, "claude", "deleted")).toBeNull();
  });

  it("одна невалидная переменная снимает всю proxy-группу", () => {
    const mixed = account({
      proxy: "http://proxy.corp:8080",
      env: [
        { name: "CLAUDE_CONFIG_DIR", value: "C:\\acc" },
        { name: "HTTP_PROXY", value: "http://proxy.corp:8080" },
        { name: "HTTPS_PROXY", value: "ftp://proxy.corp:21" },
        { name: "NO_PROXY", value: "localhost" },
      ],
    });
    const normalized = normalizeAccountProxyForClient(mixed, 1);
    expect(normalized.env).toEqual([{ name: "CLAUDE_CONFIG_DIR", value: "C:\\acc" }]);
    expect(normalized.proxy).toBeUndefined();
    expect(JSON.stringify(normalized)).not.toContain("HTTP_PROXY");
  });

  it("NO_PROXY допускает только точный loopback allowlist", () => {
    for (const noProxy of ["*", ".openai.com", "localhost,127.0.0.1,::1,.anthropic.com"]) {
      const unsafe = account({
        proxy: "http://proxy.corp:8080",
        env: [
          { name: "CLAUDE_CONFIG_DIR", value: "C:\\acc" },
          { name: "HTTP_PROXY", value: "http://proxy.corp:8080" },
          { name: "NO_PROXY", value: noProxy },
        ],
      });
      const normalized = normalizeAccountProxyForClient(unsafe, 1);
      expect(normalized.proxy_blocked).toBe(true);
      expect(normalized.env).toEqual([{ name: "CLAUDE_CONFIG_DIR", value: "C:\\acc" }]);
      expect(JSON.stringify(normalized)).not.toContain("HTTP_PROXY");
    }
  });

  it("разные upper/lower proxy значения блокируют всю группу", () => {
    const conflicting = account({
      proxy: "http://selected.proxy:8080",
      env: [
        { name: "CLAUDE_CONFIG_DIR", value: "C:\\acc" },
        { name: "HTTP_PROXY", value: "http://selected.proxy:8080" },
        { name: "http_proxy", value: "http://bypass.proxy:8080" },
      ],
    });
    const normalized = normalizeAccountProxyForClient(conflicting, 1);
    expect(normalized.proxy_blocked).toBe(true);
    expect(normalized.env).toEqual([{ name: "CLAUDE_CONFIG_DIR", value: "C:\\acc" }]);
    expect(composeLaunch("claude", EMPTY_PREFS, {
      agentID: "claude",
      accountEnvName: "CLAUDE_CONFIG_DIR",
      proxyContractVersion: 1,
      account: {
        agentID: "claude",
        envName: "CLAUDE_CONFIG_DIR",
        envValue: "C:\\acc",
        env: conflicting.env,
        proxyConfigured: true,
      },
    })).toBe("");
  });

  it("partial или не совпадающий с account.proxy набор блокируется целиком", () => {
    for (const unsafeEnv of [
      [
        { name: "CLAUDE_CONFIG_DIR", value: "C:\\acc" },
        { name: "HTTPS_PROXY", value: "http://selected.proxy:8080" },
      ],
      fullProxyEnv("http://other.proxy:8080"),
    ]) {
      const normalized = normalizeAccountProxyForClient(account({
        proxy: "http://selected.proxy:8080",
        env: unsafeEnv,
      }), 1);
      expect(normalized.proxy_blocked).toBe(true);
      expect(normalized.env).toEqual([{ name: "CLAUDE_CONFIG_DIR", value: "C:\\acc" }]);
      expect(JSON.stringify(normalized)).not.toContain("selected.proxy");
      expect(JSON.stringify(normalized)).not.toContain("other.proxy");
    }
  });

  it("POSIX всегда снимает inherited proxy scope до полного проверенного набора", () => {
    const cmd = composeLaunch("claude", EMPTY_PREFS, {
      posix: true,
      agentID: "claude",
      accountEnvName: "CLAUDE_CONFIG_DIR",
      proxyContractVersion: 1,
      account: {
        agentID: "claude",
        envName: "CLAUDE_CONFIG_DIR",
        envValue: "/tmp/acc",
        env: fullProxyEnv("http://selected.proxy:8080", "/tmp/acc"),
        proxyConfigured: true,
      },
    });
    expect(cmd).toContain("-u HTTP_PROXY -u HTTPS_PROXY -u ALL_PROXY");
    expect(cmd).toContain("-u http_proxy -u https_proxy -u all_proxy");
    expect(cmd).toContain("-u NO_PROXY -u no_proxy");
    expect(cmd).toContain("HTTPS_PROXY='http://selected.proxy:8080'");
    expect(cmd).toContain("https_proxy='http://selected.proxy:8080'");
  });

  it("composeLaunch не возвращает CLI при blocked reason или невалидной proxy-группе", () => {
    expect(composeLaunch("claude", EMPTY_PREFS, {
      agentID: "claude",
      accountEnvName: "CLAUDE_CONFIG_DIR",
      blockedReason: "blocked",
    })).toBe("");
    expect(composeLaunch("claude", EMPTY_PREFS, {
      agentID: "claude",
      accountEnvName: "CLAUDE_CONFIG_DIR",
      proxyContractVersion: 1,
      account: {
        agentID: "claude",
        envName: "CLAUDE_CONFIG_DIR",
        envValue: "C:\\acc",
        env: [
          { name: "CLAUDE_CONFIG_DIR", value: "C:\\acc" },
          { name: "HTTP_PROXY", value: "http://safe.proxy:8080" },
          { name: "HTTPS_PROXY", value: "http://user:pass@unsafe.proxy:8080" },
        ],
        proxyConfigured: true,
      },
    })).toBe("");
  });

  it("Windows proxied → default очищает скоуп и восстанавливает точное прежнее состояние", () => {
    const proxied = composeLaunch("claude", EMPTY_PREFS, {
      agentID: "claude",
      accountEnvName: "CLAUDE_CONFIG_DIR",
      proxyContractVersion: 1,
      account: {
        agentID: "claude",
        envName: "CLAUDE_CONFIG_DIR",
        envValue: "C:\\acc",
        proxyConfigured: true,
        env: fullProxyEnv("http://proxy.corp:8080"),
      },
    });
    const direct = composeLaunch("claude", EMPTY_PREFS, {
      agentID: "claude",
      accountEnvName: "CLAUDE_CONFIG_DIR",
      proxyContractVersion: 1,
    });

    expect(proxied.indexOf("Remove-Item -LiteralPath")).toBeLessThan(proxied.indexOf("$env:HTTPS_PROXY="));
    expect(proxied).toContain("@{ Exists=(Test-Path");
    expect(proxied).toContain("finally { foreach");
    expect(proxied).toContain("[Environment]::SetEnvironmentVariable");
    expect(direct).toContain("'CLAUDE_CONFIG_DIR','HTTP_PROXY','HTTPS_PROXY','ALL_PROXY','NO_PROXY'");
    expect(direct).toContain("try { claude } finally {");
    expect(direct).not.toContain("proxy.corp");
    expect(direct).not.toContain("$env:HTTPS_PROXY=");
  });

  it("реальный powershell.exe видит selected env только внутри CLI и восстанавливает originals/absence", () => {
    if ((globalThis as any).process?.platform !== "win32") return;
    const probe = "Write-Output ('INSIDE=' + [string]$env:CLAUDE_CONFIG_DIR + '|' + [string]$env:HTTPS_PROXY + '|' + [string](Test-Path Env:ALL_PROXY))";
    const scoped = composeLaunch(probe, EMPTY_PREFS, {
      agentID: "claude",
      accountEnvName: "CLAUDE_CONFIG_DIR",
      proxyContractVersion: 1,
      account: {
        agentID: "claude",
        envName: "CLAUDE_CONFIG_DIR",
        envValue: "selected-profile",
        proxyConfigured: true,
        env: fullProxyEnv("http://selected.proxy:8080", "selected-profile"),
      },
    });
    const script = [
      "$env:CLAUDE_CONFIG_DIR='original-profile'",
      "$env:HTTPS_PROXY='http://original.proxy:8080'",
      "Remove-Item Env:ALL_PROXY -ErrorAction SilentlyContinue",
      scoped,
      "Write-Output ('AFTER=' + [string]$env:CLAUDE_CONFIG_DIR + '|' + [string]$env:HTTPS_PROXY + '|' + [string](Test-Path Env:ALL_PROXY))",
    ].join("; ");
    const output = execFileSync("powershell.exe", ["-NoProfile", "-Command", script], { encoding: "utf8", windowsHide: true, timeout: 60_000 });
    expect(output).toContain("INSIDE=selected-profile|http://selected.proxy:8080|True");
    expect(output).toContain("AFTER=original-profile|http://original.proxy:8080|False");
  }, 65_000); // Cold Windows CI startup exceeded 15s; this checks env restoration, not launch speed.
});
