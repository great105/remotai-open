import { describe, expect, it } from "vitest";
import { screenScrollResponse } from "./scrollResponse";

describe("screenScrollResponse", () => {
  const before = [
    "header", "task one", "task two", "task three", "task four",
    "task five", "task six", "task seven", "status", "spinner -",
  ];

  it("does not accept a spinner or clock changing one or two rows as scroll ack", () => {
    const oneRow = [...before];
    oneRow[9] = "spinner \\";
    expect(screenScrollResponse(before, oneRow).answered).toBe(false);

    const twoRows = [...oneRow];
    twoRows[8] = "status 00:01";
    expect(screenScrollResponse(before, twoRows).answered).toBe(false);
  });

  it("does not mistake repeated rows around one spinner for a shifted screen", () => {
    const repeated = Array.from({ length: 20 }, () => "same status row");
    const after = [...repeated];
    after[19] = "spinner /";
    const response = screenScrollResponse(repeated, after);
    expect(response.shiftedRows).toBe(0); // duplicate rows are not shift evidence
    expect(response.changedRows).toBe(1);
    expect(response.answered).toBe(false);
  });

  it("does not accept a five-row status repaint as a page response", () => {
    const twenty = Array.from({ length: 20 }, (_, i) => `stable row ${i}`);
    const after = [...twenty];
    for (let i = 15; i < 20; i++) after[i] = `busy status ${i}`;
    const response = screenScrollResponse(twenty, after);
    expect(response.changedRows).toBe(5);
    expect(response.answered).toBe(false);
  });

  it("does not derive a fake shift from repeated rows during a status repaint", () => {
    const repeated = Array.from({ length: 20 }, () => "same background row");
    const after = [...repeated];
    for (let i = 15; i < 20; i++) after[i] = `busy status ${i}`;
    const response = screenScrollResponse(repeated, after);
    expect(response.changedRows).toBe(5);
    expect(response.shiftedRows).toBe(0);
    expect(response.answered).toBe(false);
  });

  it("accepts a page-sized row delta", () => {
    const after = before.map((row, i) => `${row} / older ${i}`);
    const response = screenScrollResponse(before, after);
    expect(response.answered).toBe(true);
    expect(response.changedRows).toBe(before.length);
  });

  it("accepts a scroll-like row shift even when most text is reused", () => {
    const after = [...before.slice(3), "older 1", "older 2", "older 3"];
    const response = screenScrollResponse(before, after);
    expect(response.answered).toBe(true);
    expect(response.shiftedRows).toBeGreaterThanOrEqual(7);
  });

  it("separates a row shift from an unrelated full repaint (A03)", () => {
    const rows = Array.from({ length: 24 }, (_, i) => `history ${i}`);
    const unrelated = Array.from({ length: 24 }, (_, i) => `new answer ${i}`);
    const repaint = screenScrollResponse(rows, unrelated);
    expect(repaint).toMatchObject({ answered: true, kind: "repaint", shiftedRows: 0 });
    const shifted = screenScrollResponse(rows, [...rows.slice(8), ...Array.from({ length: 8 }, (_, i) => `older ${i}`)]);
    expect(shifted.kind).toBe("shift");
    const spinner = [...rows];
    spinner[23] = "spinner";
    expect(screenScrollResponse(rows, spinner).kind).toBe("none");
  });

  // A03, «ответ оценивается относительно отправленной команды»: PgUp открывает
  // старший текст, и прежние строки уезжают ВНИЗ. Новый вывод агента в обычном
  // буфере гонит строки ВВЕРХ — без направления он засчитывался бы ответом.
  it("counts only a shift in the direction of the sent intent", () => {
    const rows = Array.from({ length: 24 }, (_, i) => `history ${i}`);
    const newOutput = [...rows.slice(8), ...Array.from({ length: 8 }, (_, i) => `fresh ${i}`)];
    expect(screenScrollResponse(rows, newOutput, "up").kind).not.toBe("shift");
    expect(screenScrollResponse(rows, newOutput, "down").kind).toBe("shift");
    const olderPage = [...Array.from({ length: 8 }, (_, i) => `older ${i}`), ...rows.slice(0, 16)];
    expect(screenScrollResponse(rows, olderPage, "up").kind).toBe("shift");
    expect(screenScrollResponse(rows, olderPage, "down").kind).not.toBe("shift");
    // Без намерения — прежнее поведение: любое направление.
    expect(screenScrollResponse(rows, newOutput).kind).toBe("shift");
  });

  // Лог владельца 23.09 (48×25, Claude Code): PgUp сменила 18 строк переписки,
  // сдвиг 6, подвал из 7 строк (поле ввода, статус) неподвижен. Порог от всего
  // экрана (19) это «молчанием» и считал.
  it("accepts a Claude Code page flip above a fixed footer", () => {
    const transcript = Array.from({ length: 18 }, (_, i) => `reply line ${i}`);
    const footer = ["────", "> ", "────", "5h 92%", "bypass permissions", "", "tips"];
    const before = [...transcript, ...footer];
    const older = Array.from({ length: 18 }, (_, i) => `older line ${i}`);
    const response = screenScrollResponse(before, [...older, ...footer], "up");
    expect(response.changedRows).toBe(18);
    expect(response.answered).toBe(true);
    expect(response.kind).toBe("repaint");
    // Колесо: переписка сдвинулась на 3 строки, подвал стоит.
    const wheel = [...older.slice(0, 3), ...transcript.slice(0, 15), ...footer];
    expect(screenScrollResponse(before, wheel, "up").kind).toBe("shift");
  });

  it("still ignores a spinner and a status line far apart", () => {
    const rows = Array.from({ length: 25 }, (_, i) => `row ${i}`);
    const after = [...rows];
    after[2] = "spinner";
    after[20] = "status 00:02";
    expect(screenScrollResponse(rows, after).answered).toBe(false);
  });

  it("does not treat a mid-size status block repaint as a page", () => {
    const rows = Array.from({ length: 25 }, (_, i) => `row ${i}`);
    const after = [...rows];
    for (let i = 17; i < 25; i++) after[i] = `busy ${i}`;
    expect(screenScrollResponse(rows, after).answered).toBe(false);
  });

  it("does not acknowledge empty or differently sized captures", () => {
    expect(screenScrollResponse([], []).answered).toBe(false);
    expect(screenScrollResponse(before, before.slice(1)).answered).toBe(false);
  });
});
