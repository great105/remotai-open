import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

// Exercise the real view's bootstrap without a DOM or a real device. Child
// components are only JSX descriptors; React hooks are a deterministic host.
const host = vi.hoisted(() => {
  const slots: any[] = [];
  let index = 0;
  let effects: Array<() => void | (() => void)> = [];
  let changed = false;
  const same = (a: unknown[], b: unknown[]) => a?.length === b?.length && a.every((v, i) => Object.is(v, b[i]));
  return {
    slots, realEscape: false, backHandlers: new Set<Function>(), escapeHandlers: [] as Function[], native: false, nativeCallbacks: new Map<string,Function>(), nativeRemove: vi.fn(), polls: [] as Array<() => Promise<void>>, client: null as any, device: "fixture-device", mode: "self_hosted", selectedDevice: "", saveBlob: null as any, upload: vi.fn(),
    reset() { slots.length = 0; index = 0; effects = []; changed = false; this.polls = []; this.escapeHandlers = []; },
    begin() { index = 0; changed = false; this.polls = []; this.escapeHandlers = []; },
    finish() { const pending = effects; effects = []; pending.forEach(effect => effect()); },
    changed: () => changed,
    state(initial: any) {
      const n = index++;
      if (!slots[n]) slots[n] = { value: typeof initial === "function" ? initial() : initial };
      return [slots[n].value, (next: any) => {
        const value = typeof next === "function" ? next(slots[n].value) : next;
        if (!Object.is(value, slots[n].value)) { slots[n].value = value; changed = true; }
      }];
    },
    ref(value: any) { const n = index++; return (slots[n] ||= { current: value }); },
    memo(fn: any, deps: unknown[]) {
      const n = index++;
      if (!slots[n] || !same(slots[n].deps, deps)) slots[n] = { value: fn, deps };
      return slots[n].value;
    },
    effect(fn: any, deps: unknown[]) {
      const n = index++;
      if (!slots[n] || !same(slots[n].deps, deps)) {
        slots[n]?.cleanup?.();
        slots[n] = { deps };
        effects.push(() => { slots[n].cleanup = fn(); });
      }
    },
  };
});
vi.mock("@capacitor/core", () => ({Capacitor:{isNativePlatform:()=>host.native}}));
vi.mock("@capacitor/keyboard", () => ({Keyboard:{addListener:async (name:string,fn:Function)=>{host.nativeCallbacks.set(name,fn);return {remove:host.nativeRemove};}}}));
vi.mock("react", async importOriginal => ({
  ...await importOriginal<typeof import("react")>(),
  useState: (initial: unknown) => host.state(initial),
  useRef: (value: unknown) => host.ref(value),
  useCallback: (fn: unknown, deps: unknown[]) => host.memo(fn, deps),
  useEffect: (fn: unknown, deps: unknown[]) => host.effect(fn, deps),
  useLayoutEffect: (fn: unknown, deps: unknown[]) => host.effect(fn, deps),
  useId: () => "fixture-id",
}));
vi.mock("@tgcontrol/shared", () => ({ FolderNavSheet: () => null, SheetShell: () => null, platform: () => ({ saveBlob: host.saveBlob }), transport: () => ({}), useEscape: (open:boolean,close:()=>void) => { if(host.realEscape) useActualEscape(open,close); if(open) host.escapeHandlers.push(close); }, mapApiError: (e: Error) => e.message }));
vi.mock("react-router-dom", () => ({ useNavigate: () => () => {} }));
vi.mock("../api", () => ({ uploadPtyFile: (...args: any[]) => host.upload(...args) }));
vi.mock("../config", () => ({ getMode: () => host.mode, getSelectedDeviceId: () => host.selectedDevice, getTerminalContextKey: () => host.device, getSelectedDeviceName: () => "Fixture" }));
vi.mock("../hooks/usePolling", () => ({ usePolling: (fn: () => Promise<void>) => { host.polls.push(fn); } }));
vi.mock("./useHermesEvents", () => ({ useHermesEvents: (fn: (signal: AbortSignal) => Promise<void>) => { host.polls.splice(1, 0, () => fn(new AbortController().signal)); } }));
vi.mock("./client", async importOriginal => ({
  ...await importOriginal<typeof import("./client")>(), createHermesClient: () => host.client,
}));
vi.mock("../../../packages/shared/src/platform", () => ({platform:()=>({pushBackHandler:(fn:Function)=>{host.backHandlers.add(fn);return()=>host.backHandlers.delete(fn);}})}));
import { useEscape as useActualEscape } from "../../../packages/shared/src/hooks/useEscape";
import { HermesView } from "../pages/HermesView";

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (error: Error) => void;
  const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}
const snapshot = { session_id: "live-A", stored_session_id: "stored-A", info: { provider: "fixture", model: "fixture-model" }, messages: [{ role: "assistant", text: "Recovered answer" }], _remotai_event_seq: 12 };
let tree: any;
function render() { host.begin(); tree = HermesView(); host.finish(); }
async function settle() {
  for (let i = 0; i < 40; i++) { await Promise.resolve(); if (host.changed()) render(); }
}
function elements(node: any, predicate: (node: any) => boolean): any[] {
  if (Array.isArray(node)) return node.flatMap(child => elements(child, predicate));
  if (!node || typeof node !== "object") return [];
  return [...(predicate(node) ? [node] : []), ...elements(node.props?.children, predicate)];
}
const prop = (name: string) => elements(tree, node => name in (node.props || {}))[0]?.props[name];

beforeEach(() => {
  host.reset(); host.realEscape=false; host.backHandlers.clear(); host.native=false; host.nativeCallbacks.clear(); host.nativeRemove.mockClear(); host.device = "fixture-device"; host.mode = "self_hosted"; host.selectedDevice = ""; host.saveBlob = vi.fn(async () => "saved");
  const values = new Map([["remotai.hermes.draft.v1:fixture-device", JSON.stringify({ sessionId: "stored-A", text: "unsent", cwd: "", provider: "fixture", model: "fixture-model" })]]);
  vi.stubGlobal("window", { innerHeight:844, innerWidth:390, addEventListener:vi.fn(), removeEventListener:vi.fn(), localStorage: { getItem: (key: string) => values.get(key) ?? null, setItem: (key: string, value: string) => values.set(key, value) }, matchMedia: () => ({ matches: false, addEventListener() {}, removeEventListener() {} }) });
  host.client = {
    status: vi.fn(async () => ({ ready: true, installed: true, backend_generation: 1 })),
    eventCursor: vi.fn(async () => ({ latest_seq: 10, events: [] })),
    events: vi.fn(async () => ({ latest_seq: 10, events: [] })),
    waitEvents: vi.fn(async () => ({ latest_seq: 12, events: [] })),
    providers: vi.fn(async () => ({ providers: [] })),
    rpc: vi.fn(async (method: string) => {
      if (method === "session.resume") return snapshot;
      if (method === "model.options") return { providers: [], provider: "fixture", model: "fixture-model" };
      if (method === "setup.runtime_check") return { ok: true, model: "fixture-model" };
      if (method === "session.list") return { sessions: [{ id: "stored-A", title: "Recovered chat" }] };
      if (method === "commands.catalog") return { pairs: [["status", "Status"]], remotai_commands: {version:1,without_arguments:["status"]} };
      return {};
    }),
  };
});
afterEach(() => { host.slots.forEach(slot => slot.cleanup?.()); vi.unstubAllGlobals(); });
async function reopen() { render(); await host.polls[0](); await settle(); }
async function openPendingDecision() {
  const opener = elements(tree,n=>String(n.props?.["aria-label"] || "").startsWith("Проверить запросы Hermes:"))[0];
  if (opener) { opener.props.onClick(); await settle(); }
}

describe("subagent journal view wiring", () => {
  it("publishes a safe last-launch summary in the existing lifecycle row and opens child details", async () => {
    const rpc = host.client.rpc;
    let roster = [{ subagent_id: "child", goal: "Owned task", status: "running", tool_count: 1 }];
    let unavailable = false;
    host.client.rpc = vi.fn(async (method: string, params: unknown) => method === "subagent.list"
      ? unavailable ? Promise.reject(new Error("unavailable")) : { subagents: roster }
      : rpc(method, params));
    await reopen();
    host.client.subagentActivity = vi.fn(async()=>({supported:false}));
    host.client.waitEvents.mockResolvedValue({latest_seq:15,events:[
      {seq:13,frame:{method:"event",params:{session_id:"live-A",type:"subagent.start",payload:{subagent_id:"child",goal:"Owned task",status:"running"}}}},
      {seq:14,frame:{method:"event",params:{session_id:"live-A",type:"subagent.thinking",payload:{subagent_id:"child",text:"PRIVATE_THOUGHT"}}}},
      {seq:15,frame:{method:"event",params:{session_id:"live-A",type:"subagent.tool",payload:{subagent_id:"child",tool_name:"read_file",tool_preview:"PRIVATE_ARGS"}}}},
    ]});
    await host.polls[1](); await settle();
    await host.polls[2](); await settle();
    const compact = elements(tree,n=>!!n.props?.observableActivity)[0];
    expect(compact?.props.observableActivity.detail).toContain("Последний запуск: Чтение файла");
    expect(compact?.props.onDetails).toBeTypeOf("function");
    compact.props.onDetails(); await settle();
    const details = elements(tree,n=>Array.isArray(n.props?.children) && typeof n.props?.selectedId === "string")[0];
    expect(details?.props.selectedId).toBe("child");
    expect(details?.props.children[0].journal).toHaveLength(1);
    expect(JSON.stringify(details.props.children)).not.toContain("PRIVATE");
    const lifecycle = () => elements(tree, n => "observableActivity" in (n.props || {}))[0].props;
    for (const status of ["completed", "failed", "cancelled"]) {
      roster = [{ ...roster[0], status }];
      await host.polls[2](); await settle();
      expect(lifecycle().observableActivity).toBeUndefined();
      expect(lifecycle().subagents[0].status).toBe(status);
      const retained = elements(tree, n => Array.isArray(n.props?.children) && typeof n.props?.selectedId === "string")[0].props.children[0];
      expect(retained.terminalConfirmed).toBeUndefined();
      expect(retained.journal).toHaveLength(1);
    }
    unavailable = true;
    await host.polls[2](); await settle();
    expect(lifecycle().observableActivity).toBeUndefined();
    expect(lifecycle().subagents).toBeNull();
    expect(lifecycle().subagentError).not.toBe("");
    unavailable = false; roster = [{ ...roster[0], status: "running" }];
    await host.polls[2](); await settle();
    expect(lifecycle().observableActivity.title).toBe("Помощник работает");
    roster = [];
    await host.polls[2](); await settle();
    expect(lifecycle().observableActivity).toMatchObject({ unconfirmed: true, title: "Завершение помощника не подтверждено" });
  });
});

describe("Hermes simplified chat navigation", () => {
  it("separates verified files from the collapsed technical tool journal", async () => {
    host.client.controlCapabilities = vi.fn(async () => ({connected:true,generation:1,methods:{}}));
    const base = {session_id:"live-A", stored_session_id:"stored-A", generation:1, run_id:"run-fixture", event_seq:13, tool_id:"tool-fixture", outcome:"completed"};
    host.client.controlSnapshot = vi.fn(async () => ({epoch:1, tasks:[], attention:[], results:[
      {...base, id:"verified", kind:"file", verified:true, path:"report.txt"},
      {...base, id:"unverified", kind:"file", verified:false, path:"not-proven.txt"},
      {...base, id:"tool", kind:"tool", verified:true, tool:"terminal", preview:"fixture stdout"},
    ]}));
    await reopen();
    const files = elements(tree, n => n.props?.["aria-label"] === "Проверенные результаты")[0];
    expect(elements(files, n => n.props?.["data-result-id"])).toHaveLength(1);
    const journal = elements(tree, n => n.type === "details" && n.props?.["aria-label"] === "Журнал инструментов")[0];
    expect(journal).toBeDefined(); expect(journal.props.open).not.toBe(true);
    expect(elements(journal, n => n.props?.["data-result-id"])).toHaveLength(2);
    expect(elements(journal, n => n.type === "button")).toHaveLength(0);
  });
  it("keeps the chat list for navigation instead of repeating current-chat actions", async () => {
    await reopen();
    const sidebar = elements(tree, n => typeof n.props?.onNewChat === "function")[0];
    const body = sidebar.type(sidebar.props);
    const footer = elements(body, n => n.type === "footer")[0];
    expect(elements(footer, n => n.type === "button")).toHaveLength(0);
    expect(elements(body, n => n.props?.["aria-label"] === "Создать новый чат")).toHaveLength(1);
  });
  it("uses the selected conversation as the primary heading", async () => {
    await reopen();
    const header = elements(tree, n => n.type === "header")[0];
    expect(elements(header, n => n.type === "h1")[0].props.children).toBe("Recovered chat");
    expect(elements(header, n => n.type === "h1")[0].props.title).toBe("Recovered chat");
  });
  it("puts verified results before context and leaves diagnostics last", async () => {
    await reopen();
    const work = elements(tree, n => n.props?.className === "hermes-work-view")[0];
    const sections = elements(work, n => n.props?.["aria-label"]);
    const names = sections.map(n => n.props["aria-label"]);
    expect(names.indexOf("Проверенные результаты")).toBeLessThan(names.indexOf("Контекст работы"));
    expect(names.indexOf("Готовность пульта")).toBeGreaterThan(names.indexOf("Фоновые помощники"));
  });
  it("owns viewport height and hides only the keyboard navigation bar", async () => {
    await reopen();
    const root = tree;
    expect(root.props["data-keyboard"]).toBe(false);
    expect(root.props.style["--hermes-viewport-height"]).toBe("844px");
    const listeners = new Map<string,Function>();
    const viewport = {height:444,offsetTop:0,scale:1,addEventListener:(name:string,fn:Function)=>listeners.set(name,fn),removeEventListener:vi.fn()};
    (window as any).visualViewport = viewport;
    (window as any).innerHeight = 844;
    // remount the real view: viewport listeners belong only to this surface.
    host.slots.forEach(slot=>slot.cleanup?.()); host.reset();
    vi.stubGlobal("document", {activeElement:{tagName:"TEXTAREA"},addEventListener:vi.fn(),removeEventListener:vi.fn()});
    await reopen();
    expect(tree.props["data-keyboard"]).toBe(true);
    expect(tree.props.style["--hermes-viewport-height"]).toBe("444px");
    expect(elements(tree,n=>n.props?.active==="hermes")).toHaveLength(0);
    (document as any).activeElement={tagName:"BUTTON"};
    (document.addEventListener as any).mock.calls.find(([name]:[string])=>name==="focusout")[1](); await settle();
    expect(tree.props["data-keyboard"], "opening navigation must not erase an observed overlay keyboard").toBe(true);
    elements(tree,n=>n.props?.["aria-label"]==="Контекст беседы и навигация")[0].props.onClick(); await settle();
    expect(elements(tree,n=>n.props?.className==="hermes-context-navigation")).toHaveLength(1);
  });
  it("preserves resized native height when rotating with the keyboard already open", async () => {
    host.native=true;
    await reopen();
    (window as any).innerHeight=544;
    host.nativeCallbacks.get("keyboardDidShow")!({keyboardHeight:300}); await settle();
    expect(tree.props.style["--hermes-viewport-height"]).toBe("544px");
    (window as any).innerWidth=844; (window as any).innerHeight=190;
    (window.addEventListener as any).mock.calls.find(([name]:[string])=>name==="resize")[1](); await settle();
    expect(tree.props.style["--hermes-viewport-height"]).toBe("190px");
    host.nativeCallbacks.get("keyboardDidShow")!({keyboardHeight:200}); await settle();
    expect(tree.props.style["--hermes-viewport-height"]).toBe("190px");
    (window as any).innerHeight=390;
    host.nativeCallbacks.get("keyboardDidHide")!(); await settle();
    expect(tree.props.style["--hermes-viewport-height"]).toBe("390px");
  });
  it("uses native keyboard events without subtracting resized WebView twice and removes its listeners", async () => {
    host.native=true;
    await reopen();
    host.nativeCallbacks.get("keyboardDidShow")!({keyboardHeight:300}); await settle();
    expect(tree.props.style["--hermes-viewport-height"]).toBe("544px");
    (window as any).innerHeight=444;
    host.nativeCallbacks.get("keyboardDidShow")!({keyboardHeight:400}); await settle();
    expect(tree.props.style["--hermes-viewport-height"]).toBe("444px");
    expect(tree.props["data-keyboard"]).toBe(true);
    (window as any).innerHeight=844;
    host.nativeCallbacks.get("keyboardDidHide")!(); await settle();
    expect(tree.props.style["--hermes-viewport-height"]).toBe("844px");
    expect(tree.props["data-keyboard"]).toBe(false);
    host.slots.forEach(slot=>{slot.cleanup?.();slot.cleanup=undefined;});await settle();
    expect(host.nativeRemove).toHaveBeenCalledTimes(2);
    expect(window.removeEventListener).toHaveBeenCalledWith("resize",expect.any(Function));
  });
  it("gives direct work access and a compact identity context, without wasting work space on input", async () => {
    await reopen();
    const header = elements(tree, n => n.type === "header")[0];
    expect(elements(header, n => n.props?.["aria-label"] === "Ход работы")).toHaveLength(1);
    expect(elements(header, n => n.props?.["aria-label"] === "Контекст беседы и навигация")).toHaveLength(1);
    expect(elements(tree, n => n.props?.className === "hermes-context-bar")).toHaveLength(0);
    expect(elements(tree, n => n.props?.className === "hermes-composer-context")).toHaveLength(0);
    elements(header, n => n.props?.["aria-label"] === "Ход работы")[0].props.onClick(); await settle();
    expect(elements(tree, n => n.type === "form" && n.props?.className === "hermes-composer")).toHaveLength(0);
    elements(tree, n => n.props?.["aria-label"] === "Вернуться в чат")[0].props.onClick(); await settle();
    expect(elements(tree, n => n.type === "textarea")[0].props.value).toBe("unsent");
  });
  it("keeps secondary actions in one context sheet, away from the composer", async () => {
    await reopen();
    const header = elements(tree, n => n.type === "header")[0];
    const menu = elements(header, n => n.props?.["aria-label"] === "Контекст беседы и навигация")[0];
    expect(menu, "header needs a labelled chat menu").toBeDefined();
    expect(elements(header, n => n.props?.["aria-label"] === "Создать новый чат")).toHaveLength(0);
    expect(elements(tree, n => n.props?.className === "hermes-view-tabs")).toHaveLength(0);
    expect(elements(tree, n => n.props?.className === "hermes-response-view")).toHaveLength(0);
    const composer = elements(tree, n => n.props?.className === "hermes-composer")[0];
    expect(elements(composer, n => n.type === "button")).toHaveLength(2);
    menu.props.onClick(); await settle();
    const work = elements(tree, n => n.type === "button" && n.props?.["aria-label"] === "Ход работы")[0];
    expect(work).toBeDefined();
    expect(elements(tree, n => n.type === "button" && n.props?.["aria-label"]?.startsWith("Требует внимания"))).toHaveLength(1);
    work.props.onClick(); await settle();
    expect(elements(tree, n => n.props?.className === "hermes-work-view")[0].props.hidden).toBe(false);
    const workHeader = elements(tree, n => n.type === "header")[0];
    const back = elements(workHeader, n => n.type === "button" && n.props?.["aria-label"] === "Вернуться в чат")[0];
    expect(back, "work return must stay in the fixed header, even when results are scrolled").toBeDefined();
    back.props.onClick(); await settle();
    expect(elements(tree, n => n.props?.className === "hermes-conversation")[0].props.hidden).toBe(false);
  });
});

describe("Hermes window reopening critical path", () => {
  it.each(["providers", "commands.catalog"])("releases manual-open controls and events while %s stays pending", async blockedLane => {
    await reopen();
    const inventory = deferred<any>();
    const runtime = deferred<any>();
    const resume = deferred<any>();
    const rpc = host.client.rpc;
    if (blockedLane === "providers") host.client.providers = vi.fn(() => inventory.promise);
    host.client.reply = vi.fn(async () => ({}));
    host.client.rpc = vi.fn((method: string, params: any) => method === "session.resume" ? resume.promise
      : method === "setup.runtime_check" ? runtime.promise
      : method === blockedLane ? inventory.promise : rpc(method, params));
    prop("onSelect")("stored-B"); render(); await settle();
    expect(prop("restoring")).toBe(true);
    resume.resolve({ ...snapshot, session_id: "live-B", stored_session_id: "stored-B", _remotai_event_seq: 20,
      messages: [{ role: "assistant", text: "Selected B" }],
      open_requests: [{ id: "approval-B", method: "approval", params: { session_id: "live-B", title: "Allow B?" } }],
    });
    await settle();
    expect(prop("selectedId")).toBe("stored-B");
    expect(elements(tree, node => node.props?.message?.content === "Selected B")).toHaveLength(1);
    expect(prop("restoring")).toBe(false);
    elements(tree, node => node.type === "textarea")[0].props.onChange({ target: { value: "next B task" } });
    render(); await settle();
    expect(elements(tree, node => node.props?.["aria-label"] === "Отправить")[0].props.disabled).toBe(true);
    await openPendingDecision(); const approval = elements(tree, node => node.props?.prompt?.id === "approval-B")[0];
    expect(approval.props.disabled).toBe(false);
    approval.props.onReply({ choice: "once" }); await settle();
    expect(host.client.reply).toHaveBeenCalledWith("approval-B", { choice: "once" });
    expect(elements(tree, node => node.props?.prompt?.id === "approval-B")).toHaveLength(0);
    runtime.resolve({ ok: true, model: "fixture-model" }); await settle();
    expect(elements(tree, node => node.props?.["aria-label"] === "Отправить")[0].props.disabled).toBe(false);
    host.client.waitEvents = vi.fn(async () => ({ latest_seq: 21, events: [{ seq: 21,
      frame: { method: "event", params: { session_id: "live-B", type: "message.delta", payload: { text: "Live B delta" } } },
    }] }));
    await host.polls[1](); await settle();
    expect(host.client.waitEvents).toHaveBeenCalledWith(20, expect.any(AbortSignal));
    expect(elements(tree, node => node.props?.message?.content === "Live B delta")).toHaveLength(1);
    expect(host.client.eventCursor).toHaveBeenCalledOnce();
    expect(rpc.mock.calls.filter(([method]: [string]) => method === "client.capabilities")).toHaveLength(1);
    expect(host.client.rpc.mock.calls.filter(([method]: [string]) => method === "client.capabilities")).toHaveLength(0);
    inventory.resolve(blockedLane === "providers" ? { providers: [] } : { pairs: [] }); await settle();
    expect(elements(tree, node => node.props?.message?.content === "Live B delta")).toHaveLength(1);
  });
  it.each(["startup", "event"])("keeps the latest event history when an older %s list arrives late", async olderRequest => {
    const history = deferred<any>();
    let listCalls = 0;
    const rpc = host.client.rpc;
    host.client.rpc = vi.fn((method: string, params: any) => method === "session.list"
      ? ++listCalls === (olderRequest === "startup" ? 1 : 2) ? history.promise : Promise.resolve({ sessions: [{ id: "stored-A", title: "New title" }] })
      : rpc(method, params));
    await reopen();
    let seq = 12;
    host.client.waitEvents = vi.fn(async () => ({ latest_seq: ++seq, events: [{ seq, frame: { method: "event", params: { type: "session.title", session_id: "live-A" } } }] }));
    await host.polls[1](); await settle();
    if (olderRequest === "event") { await host.polls[1](); await settle(); }
    expect(prop("sessions")).toEqual([{ id: "stored-A", title: "New title" }]);
    history.resolve({ sessions: [{ id: "stored-A", title: "Old title" }] }); await settle();
    expect(prop("sessions")).toEqual([{ id: "stored-A", title: "New title" }]);
    expect(prop("restoring")).toBe(false);
  });
  it.each(["model.options", "setup.runtime_check"])("recovers new chat readiness during a pending %s without repeating bootstrap", async blockedMethod => {
    const oldInventory = deferred<any>();
    const currentRuntime = deferred<any>();
    let blockedCalls = 0;
    const rpc = host.client.rpc;
    host.client.rpc = vi.fn((method: string, params: any) => {
      if (method === blockedMethod && ++blockedCalls === 1) return oldInventory.promise;
      if (method === "setup.runtime_check") return currentRuntime.promise;
      return rpc(method, params);
    });
    await reopen();
    expect(prop("restoring")).toBe(false);
    prop("onNewChat")();
    render(); await settle();
    elements(tree, node => node.type === "textarea")[0].props.onChange({ target: { value: "new task" } });
    render(); await settle();
    oldInventory.resolve(blockedMethod === "model.options"
      ? { providers: [], provider: "stale-provider", model: "stale-model" }
      : { ok: true, model: "stale-model" });
    await settle();
    expect(elements(tree, node => node.props?.["aria-label"] === "Отправить")[0].props.disabled).toBe(true);
    currentRuntime.resolve({ ok: true, model: "fixture-model" }); await settle();
    expect(elements(tree, node => node.props?.["aria-label"] === "Отправить")[0].props.disabled).toBe(false);
    expect(prop("selectedId")).toBe("");
    expect(elements(tree, node => node.props?.message?.content === "Recovered answer")).toHaveLength(0);
    expect(elements(tree, node => node.props?.children === "stale-model")).toHaveLength(0);
    expect(host.client.eventCursor).toHaveBeenCalledOnce();
    expect(host.client.rpc.mock.calls.filter(([method]: [string]) => method === "client.capabilities")).toHaveLength(1);
    expect(host.client.rpc.mock.calls.filter(([method]: [string]) => method === "session.resume")).toHaveLength(1);
    expect(host.client.rpc.mock.calls.filter(([method]: [string]) => method === "model.options")).toHaveLength(2);
    expect(host.client.rpc.mock.calls.filter(([method]: [string]) => method === "model.options")[1][1]).not.toHaveProperty("session_id");
  });
  it("acknowledges an empty unchanged reset after recovery without a resume storm", async () => {
    await reopen();
    host.client.waitEvents = vi.fn(async () => ({ latest_seq: 12, events: [], reset: true }));
    const firstDelay = await host.polls[1](); await settle();
    await host.polls[1](); await settle();
    await host.polls[1](); await settle();
    expect(host.client.rpc.mock.calls.filter(([method]: [string]) => method === "session.resume")).toHaveLength(2);
    expect(firstDelay).toBe(2000);
  });
  it("keeps event delivery independent of a delayed history refresh", async () => {
    await reopen();
    const history = deferred<any>();
    const rpc = host.client.rpc;
    host.client.rpc = vi.fn((method: string, params: any) => method === "session.list" ? history.promise : rpc(method, params));
    host.client.waitEvents = vi.fn(async () => ({ latest_seq: 13, events: [{ seq: 13, frame: { method: "event", params: { type: "sessions.changed", session_id: "live-A" } } }] }));
    let finished = false;
    void host.polls[1]().then(() => { finished = true; });
    await settle();
    expect(finished).toBe(true);
    expect(prop("restoring")).toBe(false);
    history.reject(new Error("history offline")); await settle();
    expect(prop("restoring")).toBe(false);
  });
  it("does not mask an authentication/transport error as an unsupported cursor", async () => {
    host.client.eventCursor = vi.fn(async () => { throw Object.assign(new Error("fixture unauthorized"), { status: 401 }); });
    await reopen();
    expect(host.client.events).not.toHaveBeenCalled();
    expect(host.client.rpc.mock.calls.some(([method]: [string]) => method === "session.resume")).toBe(false);
    expect(prop("restoring")).toBe(true);
  });
  it("does not let a delayed resume replace a newly selected empty chat", async () => {
    const resume = deferred<any>();
    const rpc = host.client.rpc;
    host.client.rpc = vi.fn((method: string, params: any) => method === "session.resume" ? resume.promise : rpc(method, params));
    await reopen();
    prop("onNewChat")();
    render();
    resume.resolve(snapshot); await settle();
    expect(elements(tree, node => node.props?.message?.content === "Recovered answer")).toHaveLength(0);
    expect(prop("selectedId")).toBe("");
    expect(prop("restoring")).toBe(false);
  });
  it("rejects stale inventory results from a previous backend generation", async () => {
    const history = deferred<any>();
    let listCalls = 0;
    const rpc = host.client.rpc;
    host.client.rpc = vi.fn((method: string, params: any) => method === "session.list"
      ? ++listCalls === 1 ? history.promise : Promise.resolve({ sessions: [{ id: "stored-A", title: "Current generation" }] })
      : rpc(method, params));
    await reopen();
    host.client.status = vi.fn(async () => ({ ready: true, installed: true, backend_generation: 2 }));
    await host.polls[0](); await settle();
    expect(prop("sessions")).toEqual([{ id: "stored-A", title: "Current generation" }]);
    history.resolve({ sessions: [{ id: "stored-A", title: "Stale generation" }] }); await settle();
    expect(prop("sessions")).toEqual([{ id: "stored-A", title: "Current generation" }]);
  });
  it("falls back to the legacy baseline only when the cursor request is unsupported", async () => {
    host.client.eventCursor = vi.fn(async () => { throw Object.assign(new Error("unsupported cursor"), { status: 404 }); });
    await reopen();
    expect(host.client.events).toHaveBeenCalledWith(0);
    expect(prop("restoring")).toBe(false);
  });
  it("starts the lightweight cursor baseline alongside capability negotiation", async () => {
    const capabilities = deferred<any>();
    const rpc = host.client.rpc;
    host.client.rpc = vi.fn((method: string, params: any) => method === "client.capabilities" ? capabilities.promise : rpc(method, params));
    await reopen();
    expect(host.client.eventCursor).toHaveBeenCalledOnce();
    expect(host.client.events).not.toHaveBeenCalled();
    expect(host.client.rpc.mock.calls.some(([method]: [string]) => method === "session.resume")).toBe(false);
    capabilities.resolve({ ok: true }); await settle();
    expect(prop("restoring")).toBe(false);
    await host.polls[1](); await settle();
    expect(host.client.waitEvents).toHaveBeenCalledWith(12, expect.any(AbortSignal));
  });
  it("publishes history and checks the model independently of delayed providers and catalog", async () => {
    const providers = deferred<any>();
    const catalog = deferred<any>();
    const runtime = deferred<any>();
    host.client.providers = vi.fn(() => providers.promise);
    const rpc = host.client.rpc;
    host.client.rpc = vi.fn((method: string, params: any) => method === "commands.catalog" ? catalog.promise
      : method === "setup.runtime_check" ? runtime.promise : rpc(method, params));
    await reopen();
    expect(prop("sessions")).toEqual([{ id: "stored-A", title: "Recovered chat" }]);
    expect(host.client.rpc.mock.calls.some(([method]: [string]) => method === "setup.runtime_check")).toBe(true);
    expect(prop("restoring")).toBe(false);
    expect(elements(tree, node => node.props?.["aria-label"] === "Отправить")[0].props.disabled).toBe(true);
    providers.resolve({ providers: [] }); catalog.resolve({ pairs: [] });
    runtime.resolve({ ok: true, model: "fixture-model" }); await settle();
    expect(elements(tree, node => node.props?.["aria-label"] === "Отправить")[0].props.disabled).toBe(false);
  });
  it("enables event recovery while an ancillary command catalog stays pending", async () => {
    const catalog = deferred<any>();
    const rpc = host.client.rpc;
    host.client.rpc = vi.fn((method: string, params: any) => method === "commands.catalog" ? catalog.promise : rpc(method, params));
    await reopen();
    expect(elements(tree, node => node.props?.message?.content === "Recovered answer")).toHaveLength(1);
    expect(prop("restoring")).toBe(false);
    const eventReads = host.client.waitEvents.mock.calls.length;
    await host.polls[1](); await settle();
    expect(host.client.waitEvents.mock.calls.length).toBeGreaterThan(eventReads);
    catalog.resolve({ pairs: [] }); await settle();
  });
  it("opens pending decisions only on intent, closes without replying and fences a captured reply by chat revision", async () => {
    const rpc = host.client.rpc;
    host.client.reply = vi.fn(async()=>({}));
    host.client.rpc = vi.fn((method:string, params:any)=>method==="session.resume" ? Promise.resolve({...snapshot,open_requests:[{id:"permission",method:"approval",params:{session_id:"live-A",command:"exact command",choices:["deny","once","session","always"]}}]}) : rpc(method,params));
    await reopen();
    expect(elements(tree,n=>n.props?.className==="hermes-decision-sheet")).toHaveLength(0);
    const opener=elements(tree,n=>n.props?.["aria-label"]==="Проверить запросы Hermes: 1")[0];
    expect(opener).toBeDefined();
    opener.props.onClick(); await settle();
    const sheet=elements(tree,n=>n.props?.className==="hermes-sheet hermes-decision-sheet")[0];
    expect(sheet).toBeDefined();
    const reply=elements(sheet,n=>n.props?.prompt?.id==="permission")[0].props.onReply;
    sheet.props.onClose(); await settle(); expect(host.client.reply).not.toHaveBeenCalled();
    prop("onNewChat")(); await settle();
    reply({choice:"once"}); await settle(); expect(host.client.reply).not.toHaveBeenCalled();
  });
  it("registers an exclusive decision Escape/platform Back dismissal without replying", async () => {
    host.realEscape=true;
    vi.stubGlobal("document", {activeElement:{tagName:"BUTTON"},addEventListener:vi.fn(),removeEventListener:vi.fn()});
    const rpc=host.client.rpc; host.client.reply=vi.fn(async()=>({}));
    host.client.rpc=vi.fn((method:string,params:any)=>method==="session.resume"?Promise.resolve({...snapshot,open_requests:[{id:"back",method:"approval",params:{choices:["deny","once"]}}]}):rpc(method,params));
    await reopen(); await openPendingDecision();
    expect(host.escapeHandlers).toHaveLength(1);
    expect(host.backHandlers.size).toBe(1);
    expect([...host.backHandlers][0]()).toBe(true); await settle();
    expect(host.backHandlers.size).toBe(0);
    expect(elements(tree,n=>n.props?.className==="hermes-sheet hermes-decision-sheet")).toHaveLength(0);
    expect(host.client.reply).not.toHaveBeenCalled();
    expect(elements(tree,n=>n.props?.className==="hermes-decision-opener")).toHaveLength(1);
  });
  it("requires explicit scope confirmation and acknowledgement for permanent permission", async () => {
    const rpc=host.client.rpc;
    host.client.rpc=vi.fn((method:string,params:any)=>method==="session.resume"?Promise.resolve({...snapshot,open_requests:[{id:"scopes",method:"approval",params:{session_id:"live-A",choices:["deny","once","session","always"]}}]}):rpc(method,params));
    await reopen(); await openPendingDecision();
    const request=elements(tree,n=>n.props?.prompt?.id==="scopes")[0];
    const onReply=vi.fn(); const props={...request.props,onReply};
    host.reset();
    const renderRequest=()=>{host.begin(); const body=request.type(props); host.finish(); return body;};
    let body=renderRequest();
    elements(body,n=>n.type==="button" && n.props.children==="Другие варианты")[0].props.onClick(); body=renderRequest();
    elements(body,n=>n.type==="button" && n.props.children==="На эту беседу")[0].props.onClick(); body=renderRequest();
    expect(onReply).not.toHaveBeenCalled();
    elements(body,n=>n.type==="button" && n.props.children==="Подтвердить: на эту беседу")[0].props.onClick(); expect(onReply).toHaveBeenLastCalledWith({choice:"session"});
    elements(body,n=>n.type==="button" && n.props.children==="Всегда")[0].props.onClick(); body=renderRequest();
    expect(elements(body,n=>n.type==="button" && n.props.children==="Подтвердить: всегда")[0].props.disabled).toBe(true);
    elements(body,n=>n.type==="input" && n.props.type==="checkbox")[0].props.onChange({target:{checked:true}}); body=renderRequest();
    const confirm=elements(body,n=>n.type==="button" && n.props.children==="Подтвердить: всегда")[0]; expect(confirm.props.disabled).toBe(false); confirm.props.onClick();
    expect(onReply).toHaveBeenLastCalledWith({choice:"always"});
  });
  it("renders only approval choices declared by the native request", async () => {
    const rpc = host.client.rpc;
    host.client.rpc = vi.fn((method: string, params: any) => method === "session.resume" ? Promise.resolve({ ...snapshot,
      open_requests: [{ id: "limited", method: "approval", params: { session_id: "live-A", choices: ["deny"], command: "fixture command" } }],
    }) : rpc(method, params));
    await reopen();
    await openPendingDecision(); const request = elements(tree, node => node.props?.prompt?.id === "limited")[0];
    host.reset(); host.begin();
    const rendered = request.type(request.props); host.finish();
    expect(elements(rendered, node => node.type === "button").map(node => node.props["aria-label"] || node.props.children)).toEqual(["Отклонить"]);
  });
  it("shows approval command and native context even when its description differs", async () => {
    const rpc = host.client.rpc;
    host.client.rpc = vi.fn((method: string, params: any) => method === "session.resume" ? Promise.resolve({ ...snapshot,
      open_requests: [{ id: "context", method: "approval", params: { session_id: "live-A", choices: ["once", "deny"], description: "Display summary", command: "fixture command", cwd: "C:/NativeProject", tool: "terminal", reason: "native risk reason" } }],
    }) : rpc(method, params));
    await reopen();
    await openPendingDecision(); const request = elements(tree, node => node.props?.prompt?.id === "context")[0];
    host.reset(); host.begin(); const rendered = request.type(request.props); host.finish();
    const visible = elements(rendered, node => ["pre", "dd"].includes(node.type)).map(node => node.props.children);
    expect(visible).toContain("fixture command");
    expect(visible).toContain("C:/NativeProject");
    expect(visible).toContain("terminal");
    expect(visible).toContain("native risk reason");
  });
  it.each(["generation", "device", "chat"])("discards a pending artifact download after %s changes", async change => {
    const file = deferred<any>();
    host.client.artifact = vi.fn(() => file.promise);
    host.client.controlCapabilities = vi.fn(async () => ({ connected: true, generation: 1 }));
    host.client.controlSnapshot = vi.fn(async () => ({ epoch: 1, tasks: [], attention: [], results: [{ id: "file-A", stored_session_id: "stored-A", kind: "file", verified: true }] }));
    await reopen();
    const button = elements(tree, node => node.type === "button" && node.props.children === "Скачать проверенный файл")[0];
    button.props.onClick(); await settle();
    expect(host.client.artifact).toHaveBeenCalledWith("file-A");
    if (change === "generation") {
      host.client.status = vi.fn(async () => ({ ready: true, installed: true, backend_generation: 2 }));
      await host.polls[0](); await settle();
    } else if (change === "device") host.device = "other-device";
    else { prop("onNewChat")(); render(); await settle(); }
    file.resolve({ name: "report.md", base64: "ZmFrZQ==" }); await settle();
    expect(host.saveBlob).not.toHaveBeenCalled();
  });
  it("does not answer an inbox request from a stale generation callback", async () => {
    const item = { id: "1:request:old", request_id: "old", session_id: "live-A", stored_session_id: "stored-A", generation: 1, kind: "approval", method: "approval", state: "pending", params: { choices: ["once", "deny"] } };
    host.client.controlCapabilities = vi.fn(async () => ({ connected: true, generation: 1 }));
    host.client.controlSnapshot = vi.fn(async () => ({ epoch: 1, tasks: [], results: [], attention: [item] }));
    host.client.controlReply = vi.fn(async () => ({ ok: true }));
    await reopen();
    elements(tree, n => n.props?.["aria-label"] === "Контекст беседы и навигация")[0].props.onClick(); await settle();
    elements(tree, node => node.type === "button" && node.props?.["aria-label"]?.startsWith("Требует внимания:"))[0].props.onClick(); render(); await settle();
    const request = elements(tree, node => node.props?.prompt?.id === "old")[0];
    host.client.status = vi.fn(async () => ({ ready: true, installed: true, backend_generation: 2 }));
    await host.polls[0](); await settle();
    request.props.onReply({ choice: "once" }); await settle();
    expect(host.client.controlReply).not.toHaveBeenCalled();
  });
  it("preserves a new native question when an older inbox snapshot resolves", async () => {
    const inbox = deferred<any>();
    host.client.controlSnapshot = vi.fn(() => inbox.promise);
    host.client.controlCapabilities = vi.fn(async () => ({ connected: true, generation: 1 }));
    await reopen();
    host.client.waitEvents = vi.fn(async () => ({ latest_seq: 13, events: [{ seq: 13, frame: { id: "fresh", method: "clarify", params: { session_id: "live-A", questions: [{ qid: "q", question: "New question" }] } } }] }));
    await host.polls[1](); await settle(); await openPendingDecision();
    expect(elements(tree, node => node.props?.prompt?.id === "fresh")).toHaveLength(1);
    inbox.resolve({ epoch: 1, tasks: [], results: [], attention: [] }); await settle();
    expect(elements(tree, node => node.props?.prompt?.id === "fresh")).toHaveLength(1);
  });
  it("handles a foreign notification link received by the mounted view", async () => {
    (window as any).addEventListener = vi.fn(); (window as any).removeEventListener = vi.fn();
    await reopen();
    expect(window.addEventListener).toHaveBeenCalledWith("hashchange", expect.any(Function));
    (window as any).location = { hash: "#/hermes?device=other-pc&session=other-chat" };
    const listener = (window.addEventListener as any).mock.calls.find(([event]: [string]) => event === "hashchange")[1];
    listener(); render(); await settle();
    expect(elements(tree, node => node.type === "button" && node.props.children === "Открыть компьютер из уведомления")).toHaveLength(1);
    expect(prop("selectedId")).toBe("stored-A");
    expect(host.client.rpc.mock.calls.some(([, params]: [string, any]) => params?.session_id === "other-chat")).toBe(false);
  });
  it("opens a matching notification conversation in the already mounted idle view", async () => {
    host.mode = "cloud"; host.selectedDevice = "pc-A";
    (window as any).addEventListener = vi.fn(); (window as any).removeEventListener = vi.fn();
    await reopen(); const rpc = host.client.rpc;
    host.client.rpc = vi.fn((method: string, params: any) => method === "session.resume" && params.session_id === "linked-B"
      ? Promise.resolve({ ...snapshot, session_id: "live-B", stored_session_id: "linked-B" }) : rpc(method, params));
    (window as any).location = { hash: "#/hermes?device=pc-A&session=linked-B&attention=approval-B" };
    (window.addEventListener as any).mock.calls.find(([event]: [string]) => event === "hashchange")[1](); render(); await settle();
    expect(prop("selectedId")).toBe("linked-B");
    expect(elements(tree, node => node.props?.["aria-label"] === "Входящие Hermes")).toHaveLength(1);
  });
  it("keeps a matching deep link selected while its restore is pending", async () => {
    host.mode = "cloud"; host.selectedDevice = "pc-A";
    (window as any).location = { hash: "#/hermes?device=pc-A&session=linked-B&attention=approval-B" };
    const resume = deferred<any>(); const rpc = host.client.rpc;
    host.client.rpc = vi.fn((method: string, params: any) => method === "session.resume" ? resume.promise.then(value => ({ ...value, stored_session_id: params.session_id })) : rpc(method, params));
    await reopen();
    expect(prop("selectedId")).toBe("linked-B");
    expect(host.client.rpc.mock.calls.filter(([method]: [string]) => method === "session.resume").map(([, params]: [string, any]) => params.session_id)).toEqual(["linked-B"]);
    resume.resolve({ ...snapshot, session_id: "live-B", stored_session_id: "linked-B", messages: [{ role: "assistant", text: "Linked transcript" }] }); await settle();
    expect(elements(tree, node => node.props?.message?.content === "Linked transcript")).toHaveLength(1);
    expect(prop("selectedId")).toBe("linked-B");
  });
  it("does not send a native prompt reply captured before backend restart", async () => {
    const rpc = host.client.rpc;
    host.client.reply = vi.fn(async () => ({ ok: true }));
    host.client.rpc = vi.fn((method: string, params: any) => method === "session.resume" ? Promise.resolve({ ...snapshot,
      open_requests: [{ id: "native-old", method: "approval", params: { session_id: "live-A", choices: ["once", "deny"] } }],
    }) : rpc(method, params));
    await reopen();
    await openPendingDecision(); const request = elements(tree, node => node.props?.prompt?.id === "native-old")[0];
    host.client.status = vi.fn(async () => ({ ready: true, installed: true, backend_generation: 2 }));
    await host.polls[0](); await settle();
    request.props.onReply({ choice: "once" }); await settle();
    expect(host.client.reply).not.toHaveBeenCalled();
  });
  it("rejects a late inbox snapshot after selecting another machine", async () => {
    const inbox = deferred<any>();
    host.client.controlSnapshot = vi.fn(() => inbox.promise);
    host.client.controlCapabilities = vi.fn(async () => ({ connected: true, generation: 1 }));
    await reopen(); host.device = "other-device";
    inbox.resolve({ epoch: 1, attention: [], tasks: [], results: [{ id: "old-device-file", stored_session_id: "stored-A", kind: "file", verified: true }] }); await settle();
    expect(elements(tree, node => node.props?.["data-result-id"] === "old-device-file")).toHaveLength(0);
  });
  it.each([{ smart_denied: true }, { allow_session: false }])("does not offer broader approval scope when native context disallows it: %j", async gates => {
    const rpc = host.client.rpc;
    host.client.rpc = vi.fn((method: string, params: any) => method === "session.resume" ? Promise.resolve({ ...snapshot,
      open_requests: [{ id: "gated", method: "approval", params: { ...gates, session_id: "live-A", choices: ["once", "deny", "session", "always"] } }],
    }) : rpc(method, params));
    await reopen(); await openPendingDecision(); const request = elements(tree, node => node.props?.prompt?.id === "gated")[0];
    host.reset(); host.begin(); const rendered = request.type(request.props); host.finish();
    expect(elements(rendered, node => node.type === "button").map(node => node.props["aria-label"] || node.props.children)).toEqual(["Отклонить", "Разрешить один раз"]);
  });
  it("does not publish a late task admission into a restarted conversation", async () => {
    const admission = deferred<any>();
    host.client.submitTask = vi.fn(() => admission.promise);
    await reopen();
    const form = elements(tree, node => node.props?.className === "hermes-composer")[0];
    const sending = form.props.onSubmit({ preventDefault() {} });
    await settle();
    expect(host.client.submitTask).toHaveBeenCalledOnce();
    host.client.status = vi.fn(async () => ({ ready: true, installed: true, backend_generation: 2 }));
    await host.polls[0](); await settle();
    admission.resolve({ status: "accepted", run_id: "old-run" });
    await sending; await settle();
    expect(elements(tree, node => node.props?.message?.content === "unsent")).toHaveLength(0);
    expect(elements(tree, node => node.type === "textarea")[0].props.value).toBe("unsent");
    host.client.submitTask.mockImplementation(async () => ({ status: "accepted", run_id: "old-run" }));
    const retryForm=elements(tree, node => node.props?.className === "hermes-composer")[0];
    await retryForm.props.onSubmit({ preventDefault() {} }); await settle();
    expect(host.client.submitTask).toHaveBeenCalledTimes(2);
    expect(host.client.submitTask.mock.calls[1][0].client_request_id).toBe(host.client.submitTask.mock.calls[0][0].client_request_id);
  });
  it("preserves queued textarea, durable draft and admission ID after a late acknowledgement", async () => {
    const admission = deferred<any>();
    host.client.submitTask = vi.fn(() => admission.promise);
    host.client.controlCapabilities = vi.fn(async () => ({ connected: true, generation: 1, queue: true }));
    await reopen();
    host.client.waitEvents = vi.fn(async () => ({ latest_seq: 13, events: [{ seq: 13, frame: { method: "event", params: { session_id: "live-A", type: "message.start", payload: {} } } }] }));
    await host.polls[1](); await settle();
    elements(tree, node => node.type === "button" && node.props.children === "Следующая задача")[0].props.onClick(); await settle();
    const original = host.client.submitTask.mock.calls[0][0];
    expect(original).toMatchObject({ text: "unsent", queued: true, session_id: "live-A", stored_session_id: "stored-A", generation: 1 });
    host.client.status = vi.fn(async () => ({ ready: true, installed: true, backend_generation: 2 }));
    await host.polls[0](); await settle();
    admission.resolve({ status: "accepted", run_id: "old-run" }); await settle();
    expect(elements(tree, node => node.type === "textarea")[0].props.value).toBe("unsent");
    expect(JSON.parse(window.localStorage.getItem("remotai.hermes.draft.v1:fixture-device")!).text).toBe("unsent");
    host.client.submitTask.mockImplementation(async () => ({ status: "accepted", run_id: "old-run" }));
    await host.polls[1](); await settle(); // Restored turn exposes the same queue intent.
    await elements(tree, node => node.type === "button" && node.props.children === "Следующая задача")[0].props.onClick(); await settle();
    expect(host.client.submitTask.mock.calls[1][0].client_request_id).toBe(original.client_request_id);
  });
  // Lower-layer compatibility branches only: the protected HTTP handler does
  // not admit arbitrary slash send/skill. Actual HTTP availability is tested below.
  it.each(["send", "skill"])("fences late slash %s admission by the original chat generation", async type => {
    const admission = deferred<any>();
    host.client.submitTask = vi.fn(() => admission.promise);
    await reopen(); const rpc = host.client.rpc;
    host.client.rpc = vi.fn((method: string, params: any) => method === "slash.exec" ? Promise.resolve({ type, message: "expanded fixture" }) : rpc(method, params));
    elements(tree, node => node.type === "textarea")[0].props.onChange({ target: { value: "/status" } }); render(); await settle();
    elements(tree, node => node.props?.className === "hermes-composer")[0].props.onSubmit({ preventDefault() {} }); await settle();
    expect(host.client.submitTask.mock.calls[0][0]).toMatchObject({ text: "expanded fixture", queued: false, generation: 1, session_id: "live-A" });
    host.client.status = vi.fn(async () => ({ ready: true, installed: true, backend_generation: 2 }));
    await host.polls[0](); await settle();
    admission.resolve({ status: "accepted", run_id: "old-run" }); await settle();
    expect(elements(tree, node => node.props?.message?.content === "/status")).toHaveLength(0);
    expect(prop("busy")).toBe(false);
    expect(elements(tree, node => node.type === "textarea")[0].props.value).toBe("/status");
    expect(JSON.parse(window.localStorage.getItem("remotai.hermes.draft.v1:fixture-device")!).text).toBe("/status");
  });
  it.each(["alias", "prefill", "exec", "send", "skill", "error"])("ignores late slash %s resolution before admission", async type => {
    const command = deferred<any>();
    host.client.submitTask = vi.fn(async () => ({ status: "accepted" }));
    await reopen(); const rpc = host.client.rpc;
    host.client.rpc = vi.fn((method: string, params: any) => method === "slash.exec" ? command.promise : rpc(method, params));
    elements(tree, node => node.type === "textarea")[0].props.onChange({ target: { value: "/status" } }); render(); await settle();
    elements(tree, node => node.props?.className === "hermes-composer")[0].props.onSubmit({ preventDefault() {} }); await settle();
    host.client.status = vi.fn(async () => ({ ready: true, installed: true, backend_generation: 2 }));
    await host.polls[0](); await settle();
    if (type === "error") command.reject(new Error("old command error"));
    else command.resolve({ type, target: "next", message: "expanded fixture", display: "old display", output: "old output" });
    await settle();
    expect(host.client.submitTask).not.toHaveBeenCalled();
    expect(host.client.rpc.mock.calls.filter(([method]: [string]) => method === "slash.exec")).toHaveLength(1);
    expect(elements(tree, node => node.type === "textarea")[0].props.value).toBe("/status");
    expect(JSON.parse(window.localStorage.getItem("remotai.hermes.draft.v1:fixture-device")!).text).toBe("/status");
    expect(elements(tree, node => node.props?.message?.content === "old output")).toHaveLength(0);
    expect(elements(tree, node => node.props?.children === "old command error")).toHaveLength(0);
  });
  it("wires durable inbox and distinct steer/queue intents into the real view", async () => {
    host.client.controlSnapshot = vi.fn(async () => ({tasks:[],results:[{id:"result-A",stored_session_id:"stored-A",session_id:"live-A",run_id:"run-A",generation:1,event_seq:10,tool_id:"tool-1",tool:"write_file",verified:true,kind:"file",outcome:"file_verified",path:"report.md"},{id:"result-B",stored_session_id:"stored-B",session_id:"live-B",run_id:"run-B",generation:1,event_seq:11,tool_id:"tool-2",tool:"write_file",verified:true,kind:"file",outcome:"file_verified",path:"wrong.md"}],epoch:1,attention:[{id:"1:request:q",request_id:"q",session_id:"live-A",stored_session_id:"stored-A",generation:1,kind:"question",method:"clarify",state:"pending",params:{questions:[{qid:"q",question:"Fixture question"}]}}]}));
    host.client.controlCapabilities = vi.fn(async () => ({connected:true,generation:1,steer:true,queue:true,interrupt:true,methods:{},model_quota:"unknown"}));
    host.client.controlIntent = vi.fn(async () => ({status:"queued"}));
    host.client.submitTask = vi.fn(async () => ({status:"accepted",run_id:"fixture-run"}));
    await reopen();
    elements(tree, n => n.props?.["aria-label"] === "Контекст беседы и навигация")[0].props.onClick(); await settle();
    const inbox = elements(tree,node=>node.type === "button" && node.props?.["aria-label"]?.startsWith("Требует внимания:"))[0];
    expect(inbox).toBeDefined();
    inbox.props.onClick();render();await settle();
    expect(elements(tree,node=>node.props?.["aria-label"]==="Входящие Hermes")).toHaveLength(1);
    host.client.waitEvents = vi.fn(async () => ({latest_seq:13,events:[{seq:13,frame:{method:"event",params:{session_id:"live-A",type:"message.start",payload:{}}}}]}));
    await host.polls[1]();await settle();
    const steer = elements(tree,node=>node.type==="button"&&node.props?.children==="Уточнить сейчас")[0];
    const queue = elements(tree,node=>node.type==="button"&&node.props?.children==="Следующая задача")[0];
    expect(steer?.props.disabled).toBe(false);expect(queue?.props.disabled).toBe(false);
    await steer.props.onClick();await settle();
    expect(host.client.controlIntent).toHaveBeenCalledWith("steer","live-A","unsent");
    expect(host.client.rpc.mock.calls.some(([method]:[string])=>method==="session.interrupt")).toBe(false);
    elements(tree,node=>node.type==="textarea")[0].props.onChange({target:{value:"next fixture"}});render();await settle();
    const next = elements(tree,node=>node.type==="button"&&node.props?.children==="Следующая задача")[0];
    await next.props.onClick();await settle();
    expect(host.client.submitTask).toHaveBeenCalledWith(expect.objectContaining({session_id:"live-A",stored_session_id:"stored-A",text:"next fixture",queued:true,client_request_id:expect.any(String)}));
    expect(elements(tree,node=>node.props?.["data-result-id"]==="result-A")).toHaveLength(1);
    expect(elements(tree,node=>node.props?.["data-result-id"]==="result-B")).toHaveLength(0);
  });
});

describe("Hermes chat attachments", () => {
 it.each(["accepted", "removed"])("releases staged refs when files are %s", async action => {
  host.upload=vi.fn(async()=>({path:"C:/upload/cache.txt"}));
  host.client.submitTask=vi.fn().mockRejectedValueOnce(new Error("lost receipt")).mockResolvedValue({status:"accepted"});
  const rpc=host.client.rpc;host.client.rpc=vi.fn((m:string,p:any)=>m==="file.attach"?Promise.resolve({attached:true,ref_text:"@file:cached.txt"}):rpc(m,p));
  await reopen();
  elements(tree,n=>n.type==="input"&&n.props?.type==="file")[0].props.onChange({target:{files:[new File(["a"],"cache.txt")],value:""}});await settle();
  const submit=()=>elements(tree,n=>n.props?.className==="hermes-composer")[0].props.onSubmit({preventDefault(){}});
  submit();await settle();
  const cache=host.slots.map(slot=>slot.current).find(value=>value instanceof Map && [...value.values()].includes("@file:cached.txt")) as Map<string,string>;
  expect(cache?.size).toBe(1); // Ambiguous admission retains retry refs.
  if(action==="accepted")submit();
  else elements(tree,n=>n.props?.["aria-label"]==="Удалить cache.txt")[0].props.onClick();
  await settle();
  expect(elements(tree,n=>n.props?.["aria-label"]==="Удалить cache.txt")).toHaveLength(0);
  expect(cache.size).toBe(0);
 });
 it.each([false, true])("restores unsaved draft files after opening an existing chat (pending=%s)", async pending => {
  await reopen();
  prop("onNewChat")(); render(); await settle();
  elements(tree,n=>n.type==="textarea")[0].props.onChange({target:{value:"unsaved draft text"}}); render(); await settle();
  const upload=deferred<any>();host.upload=vi.fn(()=>pending?upload.promise:Promise.resolve({path:"C:/upload/unsaved.txt"}));
  elements(tree,n=>n.type==="input"&&n.props?.type==="file")[0].props.onChange({target:{files:[new File(["a"],"unsaved.txt")],value:""}});await settle();
  expect(elements(tree,n=>n.props?.["aria-label"]==="Удалить unsaved.txt")).toHaveLength(1);
  prop("onSelect")("stored-A"); render(); await settle();
  expect(elements(tree,n=>n.props?.["aria-label"]==="Удалить unsaved.txt")).toHaveLength(0);
  upload.resolve({path:"C:/upload/unsaved.txt"}); await settle();
  prop("onNewChat")(); render(); await settle();
  expect(elements(tree,n=>n.type==="textarea")[0].props.value).toBe("unsaved draft text");
  expect(elements(tree,n=>n.props?.["aria-label"]==="Удалить unsaved.txt")).toHaveLength(1);
  expect(elements(tree,n=>n.props?.["aria-label"]==="Отправить")[0].props.disabled).toBe(false);
  expect(host.upload).toHaveBeenCalledOnce();
 });
 it("preserves the attachment admission fingerprint after backend restart and session resume", async () => {
  host.upload=vi.fn(async()=>({path:"C:/upload/durable.txt"}));host.client.submitTask=vi.fn().mockRejectedValueOnce(new Error("lost receipt")).mockResolvedValue({status:"accepted"});
  let stagedCount=0;const rpc=host.client.rpc;
  host.client.rpc=vi.fn((m:string,p:any)=>m==="file.attach"?Promise.resolve({attached:true,ref_text:`@file:staged-${++stagedCount}.txt`}):m==="session.resume"?Promise.resolve({...snapshot,session_id:stagedCount?"live-A-new":"live-A"}):rpc(m,p));await reopen();
  elements(tree,n=>n.type==="input"&&n.props?.type==="file")[0].props.onChange({target:{files:[new File(["a"],"durable.txt")],value:""}});await settle();
  const submit=()=>elements(tree,n=>n.props?.className==="hermes-composer")[0].props.onSubmit({preventDefault(){}});submit();await settle();
  host.client.status=vi.fn(async()=>({ready:true,installed:true,backend_generation:2}));await host.polls[0]();await settle();submit();await settle();
  expect(host.client.submitTask).toHaveBeenCalledTimes(2);
  expect(host.client.submitTask.mock.calls[1][0].client_request_id).toBe(host.client.submitTask.mock.calls[0][0].client_request_id);
  expect(stagedCount).toBe(1);
 });
 it("preserves upload errors and retries without losing the draft", async () => {
  host.upload=vi.fn().mockRejectedValueOnce(new Error("upload fixture failure")).mockResolvedValue({path:"C:/upload/retry.txt"});await reopen();
  elements(tree,n=>n.type==="input"&&n.props?.type==="file")[0].props.onChange({target:{files:[new File(["retry"],"retry.txt")],value:""}});await settle();
  expect(elements(tree,n=>n.props?.children==="upload fixture failure")).toHaveLength(1);
  expect(elements(tree,n=>n.props?.["aria-label"]==="Отправить")[0].props.disabled).toBe(true);
  elements(tree,n=>n.type==="button"&&n.props?.children==="Повторить")[0].props.onClick();await settle();
  expect(host.upload).toHaveBeenCalledTimes(2);expect(elements(tree,n=>n.type==="textarea")[0].props.value).toBe("unsent");
  expect(elements(tree,n=>n.props?.["aria-label"]==="Отправить")[0].props.disabled).toBe(false);
 });
 it("preserves files on ambiguous admission and retries with the identical request ID and refs", async () => {
  host.upload=vi.fn(async()=>({path:"C:/upload/retry.txt"}));host.client.submitTask=vi.fn().mockRejectedValueOnce(new Error("lost receipt")).mockResolvedValue({status:"accepted"});
  const rpc=host.client.rpc;host.client.rpc=vi.fn((m:string,p:any)=>m==="file.attach"?Promise.resolve({attached:true,ref_text:"@file:confirmed.txt"}):rpc(m,p));await reopen();
  elements(tree,n=>n.type==="input"&&n.props?.type==="file")[0].props.onChange({target:{files:[new File(["retry"],"retry.txt")],value:""}});await settle();
  const submit=()=>elements(tree,n=>n.props?.className==="hermes-composer")[0].props.onSubmit({preventDefault(){}});
  submit();await settle();expect(elements(tree,n=>n.props?.["aria-label"]==="Удалить retry.txt")).toHaveLength(1);
  expect(elements(tree,n=>n.type==="textarea")[0].props.value).toBe("unsent");submit();await settle();
  expect(host.client.submitTask.mock.calls[0][0]).toEqual(host.client.submitTask.mock.calls[1][0]);
  expect(host.client.rpc.mock.calls.filter(([m]:any)=>m==="file.attach")).toHaveLength(1);
 });
 it.each(["device", "generation", "chat"])("fences pending staging after %s changes before the next file or admission", async change => {
  const stage=deferred<any>();host.upload=vi.fn(async(file:File)=>({path:`C:/upload/${file.name}`}));host.client.submitTask=vi.fn();
  const rpc=host.client.rpc;host.client.rpc=vi.fn((m:string,p:any)=>m==="file.attach"?stage.promise:rpc(m,p));await reopen();
  elements(tree,n=>n.type==="input"&&n.props?.type==="file")[0].props.onChange({target:{files:[new File(["a"],"first.txt"),new File(["b"],"second.txt")],value:""}});await settle();
  elements(tree,n=>n.props?.className==="hermes-composer")[0].props.onSubmit({preventDefault(){}});await settle();
  if(change==="device")host.device="other-device";
  if(change==="generation"){host.client.status=vi.fn(async()=>({ready:true,installed:true,backend_generation:2}));await host.polls[0]();await settle();}
  if(change==="chat"){prop("onNewChat")();await settle();}
  stage.resolve({attached:true,ref_text:"@file:stale.txt"});await settle();
  expect(host.client.rpc.mock.calls.filter(([m]:any)=>m==="file.attach")).toHaveLength(1);expect(host.client.submitTask).not.toHaveBeenCalled();
 });
 it("aborts a removed pending file and ignores its late result", async () => {
  const upload=deferred<any>();host.upload=vi.fn(()=>upload.promise);await reopen();
  elements(tree,n=>n.type==="input"&&n.props?.type==="file")[0].props.onChange({target:{files:[new File(["a"],"remove.txt")],value:""}});await settle();
  elements(tree,n=>n.props?.["aria-label"]==="Удалить remove.txt")[0].props.onClick();await settle();
  expect(host.upload.mock.calls[0][2].signal.aborted).toBe(true);upload.resolve({path:"C:/upload/remove.txt"});await settle();
  expect(elements(tree,n=>n.props?.["aria-label"]==="Удалить remove.txt")).toHaveLength(0);
 });
 it("does not silently omit attachments from steer or next-task controls", async () => {
  host.upload=vi.fn(async()=>({path:"C:/upload/file.txt"}));
  host.client.controlCapabilities=vi.fn(async()=>({steer:true,queue:true,methods:{}}));await reopen();
  host.client.waitEvents=vi.fn(async()=>({latest_seq:13,events:[{seq:13,frame:{method:"event",params:{session_id:"live-A",type:"message.start",payload:{}}}}]}));await host.polls[1]();await settle();
  elements(tree,n=>n.type==="input"&&n.props?.type==="file")[0].props.onChange({target:{files:[new File(["data"],"file.txt")],value:""}});await settle();
  expect(elements(tree,n=>n.type==="button"&&n.props?.children==="Уточнить сейчас")[0].props.disabled).toBe(true);
  expect(elements(tree,n=>n.type==="button"&&n.props?.children==="Следующая задача")[0].props.disabled).toBe(true);
 });
 it("shows attached filenames in the accepted file-only user message", async () => {
  host.upload=vi.fn(async()=>({path:"C:/upload/report.txt"}));host.client.submitTask=vi.fn(async()=>({status:"accepted"}));
  const rpc=host.client.rpc;host.client.rpc=vi.fn((m:string,p:any)=>m==="file.attach"?Promise.resolve({attached:true,ref_text:"@file:report.txt"}):rpc(m,p));await reopen();
  elements(tree,n=>n.type==="textarea")[0].props.onChange({target:{value:""}});render();await settle();
  elements(tree,n=>n.type==="input"&&n.props?.type==="file")[0].props.onChange({target:{files:[new File(["data"],"report.txt")],value:""}});await settle();
  elements(tree,n=>n.props?.className==="hermes-composer")[0].props.onSubmit({preventDefault(){}});await settle();
  expect(elements(tree,n=>n.props?.message?.role==="user").slice(-1)[0]?.props.message.content).toContain("report.txt");
 });
 it("pastes clipboard images as files while leaving ordinary text paste alone", async () => {
  host.upload=vi.fn(async()=>({path:"C:/upload/clipboard.png"}));await reopen();
  const field=elements(tree,n=>n.type==="textarea")[0];
  expect(field.props.onPaste).toBeTypeOf("function");
  const preventDefault=vi.fn();
  field.props.onPaste({preventDefault,clipboardData:{files:[new File(["image"],"clipboard.png",{type:"image/png"})]}});await settle();
  expect(host.upload).toHaveBeenCalledOnce();expect(preventDefault).toHaveBeenCalledOnce();
  field.props.onPaste({preventDefault,clipboardData:{files:[]}});
  expect(preventDefault).toHaveBeenCalledOnce();
 });
 it("accepts dropped files without pasting their paths into task text", async () => {
  host.upload=vi.fn(async()=>({path:"C:/upload/drop.pdf"}));await reopen();
  const form=elements(tree,n=>n.props?.className==="hermes-composer")[0];
  expect(form.props.onDrop, "composer must accept file drops").toBeTypeOf("function");
  const preventDefault=vi.fn();form.props.onDrop({preventDefault,dataTransfer:{files:[new File(["pdf"],"drop.pdf")]}});await settle();
  expect(preventDefault).toHaveBeenCalled();expect(host.upload).toHaveBeenCalledOnce();
  expect(elements(tree,n=>n.type==="textarea")[0].props.value).toBe("unsent");
 });
 it("keeps a late upload in its original chat instead of moving it to the newly opened chat", async () => {
  const upload=deferred<any>();host.upload=vi.fn(()=>upload.promise);
  await reopen();
  elements(tree,n=>n.type==="input"&&n.props?.type==="file")[0].props.onChange({target:{files:[new File(["a"],"original.txt")],value:""}});await settle();
  const rpc=host.client.rpc;host.client.rpc=vi.fn((m:string,p:any)=>m==="session.resume"?Promise.resolve({...snapshot,session_id:`live-${p.session_id}`,stored_session_id:p.session_id}):rpc(m,p));
  prop("onSelect")("stored-B");await settle();upload.resolve({path:"C:/upload/original.txt"});await settle();
  expect(elements(tree,n=>n.props?.["aria-label"]==="Удалить original.txt")).toHaveLength(0);
  prop("onSelect")("stored-A");await settle();
  expect(elements(tree,n=>n.props?.["aria-label"]==="Удалить original.txt")).toHaveLength(1);
 });
 it("chooses multiple arbitrary files and admits server-returned refs with a file-only task", async () => {
  host.upload = vi.fn(async (file: File, progress: (n: number) => void) => { progress(50); return {path: `C:/upload/${file.name}`}; });
  host.client.submitTask = vi.fn(async () => ({status:"accepted"}));
  const rpc=host.client.rpc;
  host.client.rpc=vi.fn((m:string,p:any)=> m==="file.attach" ? Promise.resolve({attached:true,path:p.path,ref_text:`@file:\`${p.path}\``}) : rpc(m,p));
  await reopen();
  elements(tree,n=>n.type==="textarea")[0].props.onChange({target:{value:""}});render();await settle();
  const picker=elements(tree,n=>n.type==="input"&&n.props?.type==="file")[0];
  expect(picker, "chat must have a real file picker").toBeDefined();
  expect(picker.props.multiple).toBe(true);expect(picker.props.accept).toBeUndefined();
  picker.props.onChange({target:{files:[new File(["one"],"one.txt"),new File(["two"],"two.zip")],value:"selected"}});await settle();
  expect(host.upload).toHaveBeenCalledTimes(2);
  expect(elements(tree,n=>n.props?.["aria-label"]==="Удалить one.txt")).toHaveLength(1);
  expect(elements(tree,n=>n.props?.["aria-label"]==="Отправить")[0].props.disabled).toBe(false);
  elements(tree,n=>n.props?.className==="hermes-composer")[0].props.onSubmit({preventDefault(){}});await settle();
  expect(host.client.rpc.mock.calls.filter(([m]:any)=>m==="file.attach").map(([,p]:any)=>p)).toEqual([
   {profile:"default",session_id:"live-A",path:"C:/upload/one.txt",name:"one.txt"},
   {profile:"default",session_id:"live-A",path:"C:/upload/two.zip",name:"two.zip"},
  ]);
  expect(host.client.submitTask).toHaveBeenCalledWith(expect.objectContaining({text:"@file:`C:/upload/one.txt`\n@file:`C:/upload/two.zip`",session_id:"live-A",stored_session_id:"stored-A",client_request_id:expect.any(String)}));
  expect(elements(tree,n=>n.props?.["aria-label"]==="Удалить one.txt")).toHaveLength(0);
 });
});

describe("HTTP command availability", () => {
 it.each([false,true])("typed unsupported commands never execute against an old or new server: %s",async marker=>{
  const rpc=host.client.rpc;host.client.rpc=vi.fn((m:string,p:any)=>m==="commands.catalog"?Promise.resolve({pairs:[["status","Status"]],...(marker?{remotai_commands:{version:1,without_arguments:["status"]}}:{})}):rpc(m,p));host.client.submitTask=vi.fn();await reopen();
  elements(tree,n=>n.type==="textarea")[0].props.onChange({target:{value:marker?"/skills fixture":"/status"}});render();await settle();
  await elements(tree,n=>n.props?.className==="hermes-composer")[0].props.onSubmit({preventDefault(){}});await settle();
  expect(host.client.rpc.mock.calls.filter(([m]:any)=>m==="slash.exec")).toHaveLength(0);expect(host.client.submitTask).not.toHaveBeenCalled();
  expect(elements(tree,n=>n.type==="textarea")[0].props.value).toBe(marker?"/skills fixture":"/status");
 });
 it.each([false,true])("disables unsupported commands and context with capability marker=%s",async marker=>{
  const httpCatalog=process.env.HERMES_CATALOG_FIXTURE ? JSON.parse((await import("node:fs")).readFileSync(process.env.HERMES_CATALOG_FIXTURE,"utf8")) : {pairs:[["status","Status"],["skills","Skills"],["context","Context"],["plugins","Plugins"]],remotai_commands:{version:1,without_arguments:["status"]}};
  if(!marker)delete httpCatalog.remotai_commands;
  const rpc=host.client.rpc;host.client.rpc=vi.fn((m:string,p:any)=>m==="commands.catalog"?Promise.resolve(httpCatalog):rpc(m,p));
  await reopen();
  prop("onCommands")();render();await settle();
  const list=elements(tree,n=>n.props?.className==="hermes-command-list")[0];
  const buttons=elements(list,n=>n.type==="button");
  for(const name of ["skills","context","plugins"]){const button=buttons.find(n=>elements(n,c=>c.type==="b")[0]?.props.children.join("") === "/"+name);expect(button?.props.disabled).toBe(true);}
  const status=buttons.find(n=>elements(n,c=>c.type==="b")[0]?.props.children.join("") === "/status");expect(status?.props.disabled).toBe(!marker);
  prop("onSettings")();render();await settle();expect(prop("contextAvailable")).toBe(false);
 });
});

describe("final receipt reconciliation", () => {
 it.each(["session.history", "control.snapshot"])("completed receipt releases admission and events while %s is delayed",async lane=>{
  const delayed=deferred<any>(); const rpc=host.client.rpc;
  host.client.rpc=vi.fn((m:string,p:any)=>m==="session.history" ? lane===m ? delayed.promise : Promise.resolve({messages:[{role:"assistant",text:"terminal history"}]}) : rpc(m,p));
  host.client.submitTask=vi.fn(async()=>({status:"completed",run_id:"old"}));
  await reopen();
  host.client.controlSnapshot=vi.fn(()=>lane==="control.snapshot" ? delayed.promise : Promise.resolve({tasks:[],attention:[],results:[],epoch:1}));
  const sending=elements(tree,n=>n.props?.className==="hermes-composer")[0].props.onSubmit({preventDefault(){}});
  await settle();
  elements(tree,n=>n.type==="textarea")[0].props.onChange({target:{value:"new draft"}});render();await settle();
  const sendReleased=elements(tree,n=>typeof n.props?.onNewChat === "function")[0].props.disabled;
  host.client.waitEvents=vi.fn(async()=>({latest_seq:13,events:[{seq:13,frame:{method:"event",params:{session_id:"live-A",type:"message.delta",payload:{text:"new live answer"}}}}]}));
  await host.polls[1]();await settle();
  const requests=host.client.waitEvents.mock.calls.length;
  delayed.resolve(lane==="session.history" ? {messages:[{role:"assistant",text:"terminal history"}]} : {tasks:[],attention:[],results:[],epoch:1});
  await sending;await settle();
  expect(requests).toBe(1);
  expect(sendReleased).toBe(false);
  expect(elements(tree,n=>n.props?.message?.content==="new live answer")).toHaveLength(1);
  expect(elements(tree,n=>n.props?.message?.content==="terminal history")).toHaveLength(0);
  expect(elements(tree,n=>n.type==="textarea")[0].props.value).toBe("new draft");
 });
 it.each(["admission", "chat", "device", "generation", "control"])("late receipt reconciliation cannot overwrite newer %s ownership",async change=>{
  const delayed=deferred<any>();const rpc=host.client.rpc;
  host.client.rpc=vi.fn((m:string,p:any)=>m==="session.history"?delayed.promise:rpc(m,p));
  host.client.submitTask=vi.fn(async()=>({status:"completed",run_id:"old"}));
  await reopen();
  host.client.controlSnapshot=vi.fn(async()=>({tasks:change==="control" ? [{session_id:"live-A",generation:1,state:"running",run_id:"old-active"}] : [],attention:[],results:[],epoch:1}));
  await elements(tree,n=>n.props?.className==="hermes-composer")[0].props.onSubmit({preventDefault(){}});await settle();
  if(change==="admission"){
   host.client.submitTask=vi.fn(async()=>({status:"accepted",run_id:"new"}));
   elements(tree,n=>n.type==="textarea")[0].props.onChange({target:{value:"next task"}});render();await settle();
   await elements(tree,n=>n.props?.className==="hermes-composer")[0].props.onSubmit({preventDefault(){}});await settle();
  }else if(change==="chat"){
   prop("onSelect")("stored-B");render();await settle();
  }else if(change==="device")host.device="different-device";
  else if(change==="generation"){
   host.client.status=vi.fn(async()=>({ready:true,installed:true,backend_generation:2}));await host.polls[0]();await settle();
  }else{
   host.client.controlSnapshot=vi.fn(async()=>({tasks:[],attention:[],results:[],epoch:1}));
   await host.polls[3]();await settle();
  }
  delayed.resolve({messages:[{role:"assistant",text:"stale terminal history"}]});await settle();
  if(change==="control")expect(elements(tree,n=>n.props&&"hasAnswer" in n.props&&"stopAction" in n.props)[0].props.busy).toBe(false);
  if(change!=="control")expect(elements(tree,n=>n.props?.message?.content==="stale terminal history")).toHaveLength(0);
  if(change==="admission"){
   expect(elements(tree,n=>n.props?.message?.content==="next task")).toHaveLength(1);
   expect(elements(tree,n=>n.props&&"hasAnswer" in n.props&&"stopAction" in n.props)[0].props.busy).toBe(true);
  }
 });
 it("completed queue receipt clears stale busy when no actual active task remains",async()=>{
  host.client.submitTask=vi.fn(async()=>({status:"completed",run_id:"old"}));
  host.client.controlCapabilities=vi.fn(async()=>({connected:true,generation:1,queue:true}));
  host.client.controlSnapshot=vi.fn(async()=>({tasks:[],attention:[],results:[],epoch:1}));
  await reopen();
  host.client.waitEvents=vi.fn(async()=>({latest_seq:13,events:[{seq:13,frame:{method:"event",params:{session_id:"live-A",type:"message.start",payload:{}}}}]}));
  await host.polls[1]();await settle();
  await elements(tree,n=>n.type==="button"&&n.props.children==="Следующая задача")[0].props.onClick();await settle();
  expect(elements(tree,n=>n.props&&"hasAnswer" in n.props&&"stopAction" in n.props)[0].props.busy).toBe(false);
 });
 it("keeps another actual active task busy while reconciling a terminal queue receipt",async()=>{
  host.client.submitTask=vi.fn(async()=>({status:"completed",run_id:"old"}));host.client.controlCapabilities=vi.fn(async()=>({connected:true,generation:1,queue:true}));host.client.controlSnapshot=vi.fn(async()=>({tasks:[{session_id:"live-A",generation:1,state:"running",run_id:"other"}],attention:[],results:[],epoch:1}));await reopen();
  host.client.waitEvents=vi.fn(async()=>({latest_seq:13,events:[{seq:13,frame:{method:"event",params:{session_id:"live-A",type:"message.start",payload:{}}}}]}));await host.polls[1]();await settle();
  await elements(tree,n=>n.type==="button"&&n.props.children==="Следующая задача")[0].props.onClick();await settle();
  expect(elements(tree,n=>n.props&&"hasAnswer" in n.props&&"stopAction" in n.props)[0].props.busy).toBe(true);
  expect(elements(tree,n=>n.props?.message?.role==="user")).toHaveLength(0);
 });
 it("unknown HTTP 409 preserves the exact retry identity",async()=>{
  host.device="unknown-409";window.localStorage.setItem(`remotai.hermes.draft.v1:${host.device}`,JSON.stringify({sessionId:"stored-A",text:"unsent",provider:"fixture",model:"fixture-model"}));
  host.client.submitTask=vi.fn(async()=>{throw Object.assign(Error("uncertain"),{status:409,code:"hermes_control_failed"})});await reopen();
  for(let i=0;i<2;i++){await elements(tree,n=>n.props?.className==="hermes-composer")[0].props.onSubmit({preventDefault(){}});await settle();}
  expect(host.client.submitTask).toHaveBeenCalledTimes(2);expect(host.client.submitTask.mock.calls[1][0].client_request_id).toBe(host.client.submitTask.mock.calls[0][0].client_request_id);
 });
 it("late completed receipts preserve a restarted chat and its draft",async()=>{
  const receipt=deferred<any>();host.client.submitTask=vi.fn(()=>receipt.promise);await reopen();
  const sending=elements(tree,n=>n.props?.className==="hermes-composer")[0].props.onSubmit({preventDefault(){}});await settle();host.client.status=vi.fn(async()=>({ready:true,installed:true,backend_generation:2}));await host.polls[0]();await settle();
  receipt.resolve({status:"completed",run_id:"old"});await sending;await settle();expect(elements(tree,n=>n.type==="textarea")[0].props.value).toBe("unsent");expect(JSON.parse(window.localStorage.getItem("remotai.hermes.draft.v1:fixture-device")!).text).toBe("unsent");expect(elements(tree,n=>n.props?.message?.role==="user")).toHaveLength(0);
 });
 it("a failed history refresh after completion does not make the accepted draft replayable",async()=>{
  host.client.submitTask=vi.fn(async()=>({status:"completed",run_id:"done"}));const rpc=host.client.rpc;host.client.rpc=vi.fn((m:string,p:any)=>m==="session.history"?Promise.reject(Error("history offline")):rpc(m,p));await reopen();
  await elements(tree,n=>n.props?.className==="hermes-composer")[0].props.onSubmit({preventDefault(){}});await settle();
  expect(elements(tree,n=>n.type==="textarea")[0].props.value).toBe("");
  expect(JSON.parse(window.localStorage.getItem("remotai.hermes.draft.v1:fixture-device")!).text).toBe("");
  expect(elements(tree,n=>n.props?.message?.content==="Recovered answer")).toHaveLength(1);
 });
 it.each(["ordinary", "command", "prepared"])("completed %s retry restores history without duplicate row or busy",async route=>{
  host.device=`completed-${route}`;
  window.localStorage.setItem(`remotai.hermes.draft.v1:${host.device}`,JSON.stringify({sessionId:"stored-A",text:"unsent",provider:"fixture",model:"fixture-model"}));
  host.client.submitTask=vi.fn(async()=>({status:"completed",run_id:"old-run"}));
  const rpc=host.client.rpc;
  host.client.rpc=vi.fn((m:string,p:any)=>m==="session.history"?Promise.resolve({messages:[{role:"user",text:"unsent"},{role:"assistant",text:"Final retry answer"}]})
   :m==="slash.exec"?Promise.resolve(route==="prepared"?{type:"prefill",message:"expanded",display:"Prepared task"}:{type:"send",message:"expanded"}):rpc(m,p));
  await reopen();
  const submit=()=>elements(tree,n=>n.props?.className==="hermes-composer")[0].props.onSubmit({preventDefault(){}});
  if(route!=="ordinary"){elements(tree,n=>n.type==="textarea")[0].props.onChange({target:{value:"/status"}});render();await settle();}
  await submit();await settle();if(route==="prepared"){await submit();await settle();}
  const execution=elements(tree,n=>n.props&&"hasAnswer" in n.props&&"stopAction" in n.props)[0];
  expect(execution.props.busy).toBe(false);
  expect(host.client.rpc.mock.calls.some(([m]:any)=>m==="session.history")).toBe(true);
  expect(elements(tree,n=>n.props?.message?.role==="user")).toHaveLength(1);
  expect(elements(tree,n=>n.props?.message?.content==="Final retry answer")).toHaveLength(1);
 });
 it("releases only typed HTTP preadmission refusals after 128 distinct queue drafts",async()=>{
  host.device="typed-refusals";window.localStorage.setItem(`remotai.hermes.draft.v1:${host.device}`,JSON.stringify({sessionId:"stored-A",text:"unsent",provider:"fixture",model:"fixture-model"}));
  // This shape is produced by TestDefinitiveAdmissionHTTPErrorContract, not a
  // generic 409: unknown failures may already have crossed durable admission.
  const body=process.env.HERMES_REFUSAL_FIXTURE ? JSON.parse((await import("node:fs")).readFileSync(process.env.HERMES_REFUSAL_FIXTURE,"utf8")) : {code:"hermes_admission_rejected",error:"fixture queue full"};
  const {createHttpClient}=await import("../../../packages/shared/src/api-core");
  const actual=await vi.importActual<typeof import("./client")>("./client");
  const fetcher=vi.fn(async()=>new Response(JSON.stringify(body),{status:409}));vi.stubGlobal("fetch",fetcher);
  host.client.submitTask=vi.fn(actual.createHermesClient(()=>createHttpClient({baseUrl:"http://fixture.invalid",getHeaders:()=>({"X-API-Token":"fixture"})})).submitTask);
  host.client.controlCapabilities=vi.fn(async()=>({connected:true,generation:1,queue:true}));await reopen();
  host.client.waitEvents=vi.fn(async()=>({latest_seq:13,events:[{seq:13,frame:{method:"event",params:{session_id:"live-A",type:"message.start",payload:{}}}}]}));await host.polls[1]();await settle();
  for(let i=0;i<128;i++){elements(tree,n=>n.type==="textarea")[0].props.onChange({target:{value:`refused-${i}`}});render();await settle();await elements(tree,n=>n.type==="button"&&n.props.children==="Следующая задача")[0].props.onClick();await settle();}
  host.client.submitTask=vi.fn(async()=>({status:"accepted",run_id:"next"}));elements(tree,n=>n.type==="textarea")[0].props.onChange({target:{value:"legitimate task"}});render();await settle();await elements(tree,n=>n.type==="button"&&n.props.children==="Следующая задача")[0].props.onClick();await settle();
  expect(host.client.submitTask).toHaveBeenCalledTimes(1);
 });
});
