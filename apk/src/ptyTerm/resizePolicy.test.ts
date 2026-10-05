import { describe, expect, it } from "vitest";
import {
  RESIZE_QUIET_MS,
  RESIZE_SETTLE_MS,
  capacityDelivered,
  capacityForServer,
  frameAnswersDeliveredRequest,
  frameAnswersRequest,
  frameFitsCapacity,
  resizeDelayMs,
  sentBaseline,
  shouldFlushResize,
} from "./resizePolicy";

describe("U-FIT: кадр после нашей вместимости не бывает больше неё (пол 20×10)", () => {
  it("меньше или равно вместимости — может быть сеткой PTY", () => {
    expect(frameFitsCapacity(47, 15, { cols: 47, rows: 23 })).toBe(true);
    expect(frameFitsCapacity(47, 23, { cols: 47, rows: 23 })).toBe(true);
  });
  it("шире или выше вместимости — нет: снят до нашего resize или чужой", () => {
    expect(frameFitsCapacity(48, 23, { cols: 47, rows: 23 })).toBe(false);
    expect(frameFitsCapacity(47, 24, { cols: 47, rows: 23 })).toBe(false);
  });
  it("пол сервера: вместимость 103×5 в ландшафте, PTY 103×10 — сетка; 103×11 — нет", () => {
    expect(frameFitsCapacity(103, 10, { cols: 103, rows: 5 })).toBe(true);
    expect(frameFitsCapacity(103, 11, { cols: 103, rows: 5 })).toBe(false);
  });
  it("вместимость или размер кадра неизвестны — нет", () => {
    expect(frameFitsCapacity(47, 15, null)).toBe(false);
    expect(frameFitsCapacity(47, 15, { cols: 0, rows: 0 })).toBe(false);
    expect(frameFitsCapacity(undefined, 15, { cols: 47, rows: 23 })).toBe(false);
  });
});

// Вместимость, сообщённая к моменту запроса, и кадр, который в неё влезает.
const CAP = { cols: 47, rows: 23 };
const FIT = { cols: 47, rows: 15 };

describe("U-FRAME-ANSWER: доказательство сетки — только ответ на НАШ запрос (T-24 не ломается)", () => {
  const ws = { id: "ws" };
  const note = { socket: ws, geomRev: 7, req: 12, delivered: true, capacity: CAP };
  it("координатор: кадр, приписанный запросу в пути, — ответ; кадр без просьбы — нет", () => {
    expect(frameAnswersDeliveredRequest(note, { socket: ws, geomRev: 7, req: undefined, ticket: 12 }, FIT)).toBe(true);
    expect(frameAnswersDeliveredRequest(note, { socket: ws, geomRev: 7, req: undefined, ticket: null }, FIT)).toBe(false);
    expect(frameAnswersDeliveredRequest(note, { socket: ws, geomRev: 7, req: undefined, ticket: 11 }, FIT)).toBe(false);
    expect(frameAnswersRequest({ ...note, req: null }, { socket: ws, geomRev: 7, req: undefined, ticket: 12 })).toBe(false);
  });
  it("без координатора: один ответ на запрос — повтор с той же ревизией уже не доказательство", () => {
    const legacy = { socket: ws, geomRev: 7, req: null, delivered: true, capacity: CAP };
    expect(frameAnswersRequest(legacy, { socket: ws, geomRev: 7, req: undefined })).toBe(true);
    const used = { ...legacy, answered: true };
    expect(frameAnswersRequest(used, { socket: ws, geomRev: 7, req: undefined })).toBe(false);
    expect(frameAnswersDeliveredRequest(used, { socket: ws, geomRev: 7, req: undefined }, FIT)).toBe(false);
  });
  it("ответ на недоставленный запрос — ответ, но не доказательство сетки", () => {
    const early = { ...note, delivered: false };
    expect(frameAnswersRequest(early, { socket: ws, geomRev: 7, req: 12 })).toBe(true);
    expect(frameAnswersDeliveredRequest(early, { socket: ws, geomRev: 7, req: 12 }, FIT)).toBe(false);
  });
  it("функция чистая: проверка не расходует запись — порядок «сначала решение, потом answered»", () => {
    const legacy = { socket: ws, geomRev: 7, req: null, delivered: true, capacity: CAP };
    const frame = { socket: ws, geomRev: 7, req: undefined };
    expect(frameAnswersDeliveredRequest(legacy, frame, FIT)).toBe(true);
    expect(frameAnswersDeliveredRequest(legacy, frame, FIT)).toBe(true);
    expect(legacy).not.toHaveProperty("answered");
  });
});

describe("U-SOCKET: «уже отправлено» привязано к сокету (ST-08, I-10)", () => {
  const a = { id: "a" };
  const b = { id: "b" };
  it("тот же сокет — прежнее отправленное", () => {
    expect(sentBaseline({ cols: 48, rows: 30 }, a, a)).toEqual({ cols: 48, rows: 30 });
  });
  it("новый сокет — «ещё ничего»: отправка обязана уйти и уходит быстрым путём", () => {
    const base = sentBaseline({ cols: 48, rows: 30 }, a, b);
    expect(base).toEqual({ cols: 0, rows: 0 });
    expect(shouldFlushResize(base, { cols: 48, rows: 30 }, false)).toBe(true);
    expect(resizeDelayMs(base, { cols: 48, rows: 30 })).toBe(RESIZE_QUIET_MS);
  });
  it("сокета нет — тоже «ничего», а не чужое отправленное", () => {
    expect(sentBaseline({ cols: 48, rows: 30 }, a, null)).toEqual({ cols: 0, rows: 0 });
    expect(sentBaseline({ cols: 48, rows: 30 }, null, null)).toEqual({ cols: 0, rows: 0 });
  });
});

describe("U-FRAME-DELIVERED: кадр отвечает на запрос после доставленной вместимости", () => {
  const ws = { id: "ws" };
  const other = { id: "old" };
  const note = { socket: ws, geomRev: 7, req: 12, delivered: true, capacity: CAP };
  it("сервер с req: только точный номер", () => {
    expect(frameAnswersDeliveredRequest(note, { socket: ws, geomRev: 7, req: 12 }, FIT)).toBe(true);
    expect(frameAnswersDeliveredRequest(note, { socket: ws, geomRev: 7, req: 11 }, FIT)).toBe(false);
  });
  it("старый агент без req: по ревизии сетки", () => {
    const legacy = { ...note, req: null };
    expect(frameAnswersDeliveredRequest(legacy, { socket: ws, geomRev: 7, req: undefined }, FIT)).toBe(true);
    expect(frameAnswersDeliveredRequest(legacy, { socket: ws, geomRev: 6, req: undefined }, FIT)).toBe(false);
    expect(frameAnswersDeliveredRequest(legacy, { socket: ws, geomRev: undefined, req: undefined }, FIT)).toBe(false);
    // Кадр с req при записи без req (номер не наш) — сомнение, прежний путь.
    expect(frameAnswersDeliveredRequest(legacy, { socket: ws, geomRev: 7, req: 3 }, FIT)).toBe(false);
  });
  it("не доставлено, чужой сокет, записи нет — false", () => {
    expect(frameAnswersDeliveredRequest({ ...note, delivered: false }, { socket: ws, geomRev: 7, req: 12 }, FIT)).toBe(false);
    expect(frameAnswersDeliveredRequest(note, { socket: other, geomRev: 7, req: 12 }, FIT)).toBe(false);
    expect(frameAnswersDeliveredRequest(null, { socket: ws, geomRev: 7, req: 12 }, FIT)).toBe(false);
    expect(frameAnswersDeliveredRequest(note, { socket: null, geomRev: 7, req: 12 }, FIT)).toBe(false);
  });
  it("ответ после доставки, но кадр ШИРЕ или ВЫШЕ вместимости — не сетка (снапшот-шов 14.09)", () => {
    expect(frameAnswersDeliveredRequest(note, { socket: ws, geomRev: 7, req: 12 }, { cols: 48, rows: 15 })).toBe(false);
    expect(frameAnswersDeliveredRequest(note, { socket: ws, geomRev: 7, req: 12 }, { cols: 47, rows: 24 })).toBe(false);
    expect(frameAnswersDeliveredRequest(note, { socket: ws, geomRev: 7, req: 12 }, CAP)).toBe(true);
  });
  it("пол сервера 20×10: ландшафт 103×5 → кадр 103×10 — сетка, 103×11 — нет", () => {
    const land = { ...note, capacity: { cols: 103, rows: 5 } };
    expect(frameAnswersDeliveredRequest(land, { socket: ws, geomRev: 7, req: 12 }, { cols: 103, rows: 10 })).toBe(true);
    expect(frameAnswersDeliveredRequest(land, { socket: ws, geomRev: 7, req: 12 }, { cols: 103, rows: 11 })).toBe(false);
  });
  it("вместимость в записи неизвестна или размер кадра не объявлен — false", () => {
    expect(frameAnswersDeliveredRequest({ ...note, capacity: null }, { socket: ws, geomRev: 7, req: 12 }, FIT)).toBe(false);
    expect(frameAnswersDeliveredRequest({ socket: ws, geomRev: 7, req: 12, delivered: true },
      { socket: ws, geomRev: 7, req: 12 }, FIT)).toBe(false);
    expect(frameAnswersDeliveredRequest(note, { socket: ws, geomRev: 7, req: 12 }, { cols: undefined, rows: 15 })).toBe(false);
  });
});

describe("resizeDelayMs", () => {
  it("первый размер уходит быстро — ждать нечего", () => {
    expect(resizeDelayMs({ cols: 0, rows: 0 }, { cols: 48, rows: 30 })).toBe(RESIZE_QUIET_MS);
  });

  // Боевая качель панели браузера: 48x31 → 48x30 → 48x28 → 48x30 за 10 секунд
  // (замер сессии Kimi 11.08.2026). Каждая ступень обязана попасть в долгое
  // окно — тогда высота успевает вернуться и агент не перерисовывает экран.
  it("качель по высоте при той же ширине ждёт долго", () => {
    for (const rows of [30, 28, 29]) {
      expect(resizeDelayMs({ cols: 48, rows: 31 }, { cols: 48, rows })).toBe(RESIZE_SETTLE_MS);
    }
  });

  it("поворот экрана — ширина другая, ждать нечего", () => {
    expect(resizeDelayMs({ cols: 48, rows: 30 }, { cols: 80, rows: 24 })).toBe(RESIZE_QUIET_MS);
  });

  // Клавиатура и шторки меняют высоту на 10–20 строк при той же ширине —
  // боевой лог 13.08.2026: 48x31 → 48x12 → 48x10 → 48x31 за 22 секунды, три
  // полных перерисовки TUI. Прежний порог «качель — это ±3 строки» пропускал
  // такой дрейф быстрым путём; теперь отстаивается любое изменение высоты.
  it("клавиатурный дрейф высоты (десятки строк) — тоже качель", () => {
    expect(resizeDelayMs({ cols: 48, rows: 31 }, { cols: 48, rows: 12 })).toBe(RESIZE_SETTLE_MS);
    expect(resizeDelayMs({ cols: 48, rows: 12 }, { cols: 48, rows: 31 })).toBe(RESIZE_SETTLE_MS);
  });

  it("размер не изменился — задержка не растягивается", () => {
    // Отправлять всё равно нечего (одинаковый размер отсекается на отправке),
    // но и держать таймер лишние секунды незачем.
    expect(resizeDelayMs({ cols: 48, rows: 30 }, { cols: 48, rows: 30 })).toBe(RESIZE_QUIET_MS);
  });
});

describe("delayed resize delivery", () => {
  it("drops a pre-keyboard short measurement and lets the close/refit send the logical size", () => {
    const sent = { cols: 47, rows: 30 };
    expect(shouldFlushResize(sent, { cols: 47, rows: 28 }, true)).toBe(false);
    expect(shouldFlushResize(sent, { cols: 47, rows: 31 }, false)).toBe(true);
  });

  it("does not resend an unchanged or invalid size", () => {
    expect(shouldFlushResize({ cols: 48, rows: 30 }, { cols: 48, rows: 30 }, false)).toBe(false);
    expect(shouldFlushResize({ cols: 48, rows: 30 }, { cols: 1, rows: 30 }, false)).toBe(false);
  });
});

// U-CAPACITY (ST-08, T-32, I-10). Кадр на запрос после доставленной
// вместимости — это сетка PTY (сервер читает resize и screen по порядку одной
// горутиной). Всё, что делает доставку сомнительной, обязано давать false:
// тогда клиент остаётся на прежнем осторожном resync.
describe("U-CAPACITY: вместимость доставлена в этот сокет", () => {
  const socket = { id: "ws-2" };
  const base = {
    sent: { cols: 48, rows: 40 }, report: { cols: 48, rows: 40 }, measuredWithoutKeyboard: true,
    resizePending: false, sentOnSocket: socket, socket,
  };

  it("отправлено в этот сокет, равно отчёту, дебаунса нет — доставлено", () => {
    expect(capacityDelivered(base)).toBe(true);
  });

  it("отправлено в прежний сокет — новый Viewer на сервере размера не знает", () => {
    expect(capacityDelivered({ ...base, sentOnSocket: { id: "ws-1" } })).toBe(false);
    expect(capacityDelivered({ ...base, sentOnSocket: null })).toBe(false);
    expect(capacityDelivered({ ...base, socket: null, sentOnSocket: null })).toBe(false);
  });

  it("взведён дебаунс — новая вместимость ещё в пути", () => {
    expect(capacityDelivered({ ...base, resizePending: true })).toBe(false);
  });

  it("отчёт изменился после отправки — сервер знает старое", () => {
    expect(capacityDelivered({ ...base, report: { cols: 48, rows: 38 } })).toBe(false);
    expect(capacityDelivered({ ...base, report: { cols: 80, rows: 40 } })).toBe(false);
  });

  it("строки измерены под клавиатурой — это не вместимость (I-09)", () => {
    expect(capacityDelivered({ ...base, measuredWithoutKeyboard: false })).toBe(false);
  });

  it("ещё ничего не отправлено или отчёт пуст", () => {
    expect(capacityDelivered({ ...base, sent: null })).toBe(false);
    expect(capacityDelivered({ ...base, sent: { cols: 0, rows: 0 }, report: { cols: 0, rows: 0 } })).toBe(false);
  });
});

describe("U-CAPACITY: что сообщать серверу как вместимость (I-10)", () => {
  it("измерено без клавиатуры — отправляем измеренное, даже если сейчас клавиатура", () => {
    expect(capacityForServer({
      report: { cols: 48, rows: 30 }, measuredWithoutKeyboard: true, termCols: 48, termRows: 20,
    })).toEqual({ cols: 48, rows: 30 });
  });

  it("строки измерены под клавиатурой — null, а не логическая высота", () => {
    // Дыра (а) карты ST-08: терминал открыт уже с клавиатурой, отчёт пуст, и
    // fitLocal писал в него логическую высоту кадра — чужой минимум уходил
    // серверу как наша вместимость.
    expect(capacityForServer({
      report: { cols: 48, rows: 30 }, measuredWithoutKeyboard: false, termCols: 48, termRows: 30,
    })).toBeNull();
  });

  it("до первого замера — null, НЕ сетка терминала", () => {
    // Дыра (б): sizeForServer при пустом отчёте отдавал term.cols/rows, а это
    // может быть принятая (adopt) сетка другого зрителя.
    expect(capacityForServer({
      report: { cols: 0, rows: 0 }, measuredWithoutKeyboard: true, termCols: 48, termRows: 20,
    })).toBeNull();
    expect(capacityForServer({
      report: { cols: 48, rows: 1 }, measuredWithoutKeyboard: true, termCols: 48, termRows: 20,
    })).toBeNull();
  });

  it("принятая сетка никогда не просачивается в ответ", () => {
    const res = capacityForServer({
      report: { cols: 120, rows: 40 }, measuredWithoutKeyboard: true, termCols: 48, termRows: 20,
    });
    expect(res).toEqual({ cols: 120, rows: 40 });
  });
});
