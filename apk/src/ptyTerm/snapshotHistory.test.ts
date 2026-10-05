import { describe, expect, it } from "vitest";
import {
  SNAPSHOT_HISTORY_INIT, noteSyncMarker, takeSnapshotHistory,
} from "./snapshotHistory";

describe("применение истории из кадра экрана (SOTA-снапшот)", () => {
  it("кадр без маркера (возврат из фона) — историю не применяем", () => {
    // Терминал не пуст: применить историю значит стереть живой вывод.
    const d = takeSnapshotHistory(SNAPSHOT_HISTORY_INIT, true);
    expect(d.apply).toBe(false);
    expect(d.next).toBe("none");
  });

  it("reset + кадр с историей — применяем ровно один раз", () => {
    let s = noteSyncMarker("reset");
    const first = takeSnapshotHistory(s, true);
    expect(first.apply).toBe(true);
    s = first.next;
    // Второй кадр того же соединения (повторный запрос) истории не получает.
    const second = takeSnapshotHistory(s, true);
    expect(second.apply).toBe(false);
    expect(second.next).toBe("done");
  });

  it("resumed на ЖИВОЙ странице — своя история валидна, чужую игнорируем", () => {
    const s = noteSyncMarker("resumed", false);
    const d = takeSnapshotHistory(s, true);
    expect(d.apply).toBe(false);
    expect(d.next).toBe("none");
  });

  // С 2.55.19 позиция потока переживает страницу (resumeStore в localStorage):
  // СВЕЖАЯ страница приходит с resume= и получает resumed — а истории у её
  // xterm нет, она умерла вместе с прошлой страницей. Боевой скриншот
  // 13.08.2026 («вот тут не скролит»): экран собран кадром, scrollback пуст,
  // снапшот с историей отброшен. Девственный терминал применяет и на resumed.
  it("resumed на СВЕЖЕЙ странице (терминал ещё не печатал) — историю применяем", () => {
    const s = noteSyncMarker("resumed", true);
    const d = takeSnapshotHistory(s, true);
    expect(d.apply).toBe(true);
    expect(d.next).toBe("done");
  });

  it("resumed после reset снимает готовность", () => {
    noteSyncMarker("reset");
    const s = noteSyncMarker("resumed");
    expect(takeSnapshotHistory(s, true).apply).toBe(false);
  });

  it("кадр БЕЗ истории готовность не тратит: следующий кадр с историей применяется", () => {
    let s = noteSyncMarker("reset");
    const empty = takeSnapshotHistory(s, false);
    expect(empty.apply).toBe(false);
    expect(empty.next).toBe("ready"); // зеркало было неживо — шанс сохраняется
    s = empty.next;
    expect(takeSnapshotHistory(s, true).apply).toBe(true);
  });

  it("повторный reset в том же соединении (новая эпоха) снова разрешает историю", () => {
    let s = noteSyncMarker("reset");
    s = takeSnapshotHistory(s, true).next; // применили — done
    expect(takeSnapshotHistory(s, true).apply).toBe(false);
    s = noteSyncMarker("reset"); // сервер пересоздал сессию, терминал снова пуст
    expect(takeSnapshotHistory(s, true).apply).toBe(true);
  });
});
