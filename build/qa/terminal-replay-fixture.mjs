/**
 * Проигрывание записанной трассы терминала на изолированном стенде (ST-01).
 *
 * Вход — файл «Зафиксировать проблему» (remotai-terminal-trace v1) с явно
 * включённой записью вывода (content="bytes"). Здесь только разбор и проверка:
 * какие байты с какими ТОЧНЫМИ границами кадров сокета пришли клиенту, какие
 * служебные сообщения сервера (reset/resumed/screen/exit) стояли между ними и
 * к какому соединению (поколению) это относилось. Модели клиента и xterm тут
 * нет; как отдавать это браузеру, решает mock-agent (serveTerminalReplay).
 *
 * Правила, которые держит модуль:
 * - base64 кадра обязан декодироваться обратимо байт в байт, иначе файл
 *   правленый или битый — отказ, а не «примерно такой же поток»;
 * - служебные сообщения только серверные; ввод и clipboard в записи не бывают
 *   по построению клиента, а если встретились — это чужой файл, отказ;
 * - identical: файл как снят. Любое преобразование при проигрывании (новая
 *   ревизия геометрии у кадра экрана, синтетический маркер, обрезанное
 *   начало) снимает identical — это видно в статусе стенда.
 */

export const REPLAY_FORMAT = "remotai-terminal-trace";
const REPLAY_SESSION_RE = /^qa-replay-(\d+)$/;
const SERVER_CONTROLS = new Set(["reset", "resumed", "screen", "exit"]);

/** Разобрать id тестовой сессии проигрывания; обычные сессии не затрагиваются. */
export function parseReplaySession(id) {
  const m = REPLAY_SESSION_RE.exec(String(id || ""));
  if (!m) return null;
  const run = Number(m[1]);
  return Number.isSafeInteger(run) && run >= 0 ? { run } : null;
}

const finite = (v) => typeof v === "number" && Number.isFinite(v);

function decodeStrictBase64(b64, where) {
  if (typeof b64 !== "string" || b64.length % 4 !== 0 || /[^A-Za-z0-9+/=]/.test(b64)) {
    throw new Error(`${where}: base64 кадра повреждён`);
  }
  const bytes = Buffer.from(b64, "base64");
  if (bytes.toString("base64") !== b64) throw new Error(`${where}: base64 кадра не обратим байт в байт`);
  return bytes;
}

/**
 * Разобрать bundle (строку JSON или объект) в план проигрывания.
 * Бросает на всём, что не является нетронутой записью v1 с байтами.
 *
 * Возвращает { identical, build, content, span, rxChunks, rxBytes, truncated,
 *   connections: [{ gen, marker, items: [{ kind:"rx", seq, t, end, bytes }
 *   | { kind:"ctl", type, seq, t, json }], gaps, firstEnd, lastEnd }] }.
 */
export function parseReplayBundle(input) {
  const bundle = typeof input === "string" ? JSON.parse(input) : input;
  if (!bundle || typeof bundle !== "object") throw new Error("не JSON-объект");
  if (bundle.format !== REPLAY_FORMAT || bundle.v !== 1) {
    throw new Error(`не трасса терминала v1 (format=${bundle.format} v=${bundle.v})`);
  }
  if (bundle.content !== "bytes" || !bundle.recording) {
    throw new Error("в трассе нет записи вывода: для проигрывания нужна явно включённая запись");
  }
  const rec = bundle.recording;
  if (rec.v !== 1 || !Array.isArray(rec.chunks) || rec.chunks.length === 0) {
    throw new Error("запись вывода не v1 или пуста");
  }
  // seq → поколение соединения по событиям трассы (rx слиты: seq записи — seq
  // слитого события). Вытесненные из кольца события наследуют предыдущее.
  const genBySeq = new Map();
  for (const ev of Array.isArray(bundle.events) ? bundle.events : []) {
    if (ev && finite(ev.seq) && ev.ctx && finite(ev.ctx.gen)) genBySeq.set(ev.seq, ev.ctx.gen);
  }
  const items = [];
  let lastSeq = -Infinity;
  let lastT = -Infinity;
  for (let i = 0; i < rec.chunks.length; i++) {
    const c = rec.chunks[i];
    const where = `chunk ${i}`;
    if (!c || typeof c !== "object" || !finite(c.seq) || !finite(c.t)) throw new Error(`${where}: нет seq/t`);
    // Порядок записи — порядок приёма: seq не убывает (rx одной слитой записи
    // делят seq), время монотонно.
    if (c.seq < lastSeq || c.t < lastT) throw new Error(`${where}: порядок записи нарушен`);
    lastSeq = c.seq;
    lastT = c.t;
    if (c.k === "rx") {
      if (!finite(c.end)) throw new Error(`${where}: у кадра нет границы end`);
      items.push({ kind: "rx", seq: c.seq, t: c.t, end: c.end, bytes: decodeStrictBase64(c.b64, where) });
    } else if (c.k === "ctl") {
      let msg;
      try { msg = JSON.parse(c.json); } catch { throw new Error(`${where}: служебное сообщение не JSON`); }
      const type = msg && typeof msg === "object" ? String(msg.t) : "";
      if (!SERVER_CONTROLS.has(type)) throw new Error(`${where}: чужое служебное сообщение «${type}» — не запись сервера`);
      items.push({ kind: "ctl", type, seq: c.seq, t: c.t, json: c.json, msg });
    } else {
      throw new Error(`${where}: неизвестный вид ${c.k}`);
    }
  }

  // Раскладка по соединениям: по поколению, если оно известно; иначе каждый
  // reset/resumed открывает новое.
  const connections = [];
  let current = null;
  let gen = null;
  const haveGen = items.some((it) => genBySeq.has(it.seq));
  for (const it of items) {
    const known = genBySeq.get(it.seq);
    const nextGen = known ?? gen;
    const startsBySync = !haveGen && it.kind === "ctl" && (it.type === "reset" || it.type === "resumed");
    if (!current || (haveGen && nextGen !== gen) || (startsBySync && current.items.length > 0)) {
      current = { gen: nextGen ?? connections.length + 1, marker: null, items: [], gaps: 0, firstEnd: null, lastEnd: null };
      connections.push(current);
    }
    gen = nextGen;
    current.items.push(it);
  }
  let rxChunks = 0;
  let rxBytes = 0;
  for (const conn of connections) {
    const first = conn.items[0];
    if (first.kind === "ctl" && (first.type === "reset" || first.type === "resumed")) conn.marker = first.msg;
    // Непрерывность принятого потока: end кадра = end прежнего + длина. Маркер
    // задаёт базу заново. Разрыв не прячется: он считается и видно в статусе.
    let expected = conn.marker && finite(conn.marker.offset) ? conn.marker.offset : null;
    for (const it of conn.items) {
      if (it.kind === "ctl") {
        if ((it.type === "reset" || it.type === "resumed") && finite(it.msg.offset)) expected = it.msg.offset;
        continue;
      }
      rxChunks++;
      rxBytes += it.bytes.length;
      if (conn.firstEnd === null) conn.firstEnd = it.end;
      if (expected !== null && it.end - it.bytes.length !== expected) conn.gaps++;
      expected = it.end;
      conn.lastEnd = it.end;
    }
  }
  const truncated = !!rec.truncatedBefore;
  const identical = bundle.identical === true && !truncated;
  return {
    identical,
    build: bundle.build && typeof bundle.build === "object" ? bundle.build : {},
    content: bundle.content,
    span: items.length ? items[items.length - 1].t - items[0].t : 0,
    rxChunks,
    rxBytes,
    truncated,
    connections,
  };
}

/**
 * Ответ на просьбу клиента о кадре экрана записанным кадром. Ревизия
 * геометрии (geom_rev) локальна для клиента: записанная относится к прошлому
 * открытию. Поэтому кадр перештамповывается ревизией ТЕКУЩЕЙ просьбы (и её
 * req, если клиент его прислал) — это уже не байтовая копия, и вызывающий
 * обязан снять identical.
 */
export function restampScreen(json, request) {
  const msg = JSON.parse(json);
  const recordedRev = msg.geom_rev;
  if (request && request.geom_rev !== undefined) msg.geom_rev = request.geom_rev;
  else delete msg.geom_rev;
  if (request && request.req !== undefined) msg.req = request.req;
  else delete msg.req;
  return { json: JSON.stringify(msg), changed: msg.geom_rev !== recordedRev };
}
