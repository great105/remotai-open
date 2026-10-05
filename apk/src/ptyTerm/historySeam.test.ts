import { describe, expect, it } from "vitest";
import {
  fillerRows, scrollbackLineCount, shouldReplaceHistory, takeScrollbackLines,
} from "./historySeam";

describe("брать ли присланную историю вместо своей", () => {
  // Живой замер 13.08.2026 на сессиях владельца: у Kimi зеркало копит 500
  // строк, у Claude — 0…49. Claude перерисовывает экран на месте, вверх ничего
  // не уходит, и его прокрутка живёт не в зеркале, а в реплее кольца.
  it("Kimi: зеркало богаче своей истории — меняем", () => {
    expect(shouldReplaceHistory({ serverScrollback: 500, localScrollback: 120, sameWidth: true })).toBe(true);
  });

  it("Claude: зеркало беднее своей истории — НЕ трогаем свою", () => {
    // Ровно жалоба «в Kimi вижу всё, в Claude только часть»: раньше 120 строк
    // реплея менялись на четыре строки зеркала.
    expect(shouldReplaceHistory({ serverScrollback: 4, localScrollback: 120, sameWidth: true })).toBe(false);
  });

  it("своей истории нет — берём любую присланную", () => {
    // Свежая страница с resume: у её xterm пусто, снапшот — единственный источник.
    expect(shouldReplaceHistory({ serverScrollback: 4, localScrollback: 0, sameWidth: true })).toBe(true);
  });

  it("менять не на что — не меняем", () => {
    expect(shouldReplaceHistory({ serverScrollback: 0, localScrollback: 0, sameWidth: true })).toBe(false);
    expect(shouldReplaceHistory({ serverScrollback: -1, localScrollback: 0, sameWidth: true })).toBe(false);
  });

  it("ширина не сошлась — числа несравнимы: меняем только пустую историю", () => {
    // У́же зеркала — длинные строки заворачиваются, и своя история кажется
    // больше, чем есть. Рисковать чужой прокруткой на таком счёте нельзя.
    expect(shouldReplaceHistory({ serverScrollback: 500, localScrollback: 10, sameWidth: false })).toBe(false);
    expect(shouldReplaceHistory({ serverScrollback: 500, localScrollback: 0, sameWidth: false })).toBe(true);
  });

  it("равные — не меняем: размен впустую стоил бы сброса терминала", () => {
    expect(shouldReplaceHistory({ serverScrollback: 30, localScrollback: 30, sameWidth: true })).toBe(false);
  });
});

/**
 * Модель прокрутки терминала: пишем `rows` физических строк в экран высотой
 * `height` и смотрим, сколько ушло в scrollback и где остался курсор.
 * Ровно так считает xterm: пока курсор не в последней строке — он просто
 * опускается, дальше каждый перевод строки сдвигает одну строку в scrollback.
 */
function feed(height: number, physicalRows: number, from = { baseY: 0, cursorY: 0 }) {
  let { baseY, cursorY } = from;
  for (let i = 0; i < physicalRows; i++) {
    if (cursorY < height - 1) cursorY++;
    else baseY++;
  }
  return { baseY, cursorY };
}

describe("шов истории снапшота: scrollback ровно S строк при любой высоте", () => {
  it("разделяет историю на scrollback и видимый экран по числам сервера", () => {
    // hist_lines = 530, screen_rows = 30 → 500 строк scrollback.
    expect(scrollbackLineCount(530, 30)).toBe(500);
  });

  it("старый агент без полей — признаём, что чисел нет", () => {
    expect(scrollbackLineCount(undefined, undefined)).toBe(-1);
    expect(scrollbackLineCount(530, undefined)).toBe(-1);
    expect(scrollbackLineCount(0, 0)).toBe(-1);
    expect(scrollbackLineCount(10, 30)).toBe(-1); // видимых больше, чем всего
  });

  it("берёт первые S строк и завершает последнюю переводом строки", () => {
    const hist = ["a", "b", "c", "d"].join("\r\n");
    const got = takeScrollbackLines(hist, 2);
    expect(got.text).toBe("a\r\nb\r\n");
    expect(got.lines).toBe(2);
  });

  it("строк меньше заявленного — берём сколько есть, а не врём себе", () => {
    const got = takeScrollbackLines("a\r\nb", 5);
    expect(got.lines).toBe(2);
  });

  // Главный инвариант. Матрица из аудита (T-001): снапшот снят в высоте R=30,
  // клиент открыт в R′ — и при каждом R′ в scrollback обязано остаться ровно
  // S строк. Раньше клиент писал S+R строк в свою высоту, и сходилось только
  // при R′ = R: выше — терял самые свежие строки под стиранием кадра, ниже —
  // дублировал верх видимого экрана.
  it.each([
    [30, 500], [60, 500], [28, 500], [8, 500], [80, 500],
    [30, 0], [30, 1], [12, 47],
  ])("высота клиента %i, история %i строк", (clientRows, S) => {
    // 1. Напечатали S строк scrollback-части (каждая с переводом строки).
    const afterHistory = feed(clientRows, S);
    // 2. Дописали расчётное число пустых строк.
    const k = fillerRows(S, afterHistory.baseY, afterHistory.cursorY, clientRows);
    const final = feed(clientRows, k, afterHistory);
    expect(final.baseY).toBe(S);
  });

  // Измерение (а не формула от длины текста) переживает перенос длинных строк:
  // строка шире экрана занимает несколько физических строк, и посчитать их
  // заранее нельзя — Go и xterm.js расходятся в ширине emoji и ZWJ.
  it("перенос длинных строк не ломает счёт: меряем, а не вычисляем", () => {
    const S = 100;
    const clientRows = 24;
    // Каждая десятая строка истории занимает по три физических строки.
    const physical = S + 2 * Math.floor(S / 10);
    const afterHistory = feed(clientRows, physical);
    const k = fillerRows(S, afterHistory.baseY, afterHistory.cursorY, clientRows);
    const final = feed(clientRows, k, afterHistory);
    // В scrollback просят S логических строк; перенос делает их физически
    // больше, и это правильно: столько их и есть на экране такой ширины.
    expect(final.baseY).toBe(S);
  });

  it("в scrollback уже не меньше нужного — не дописываем ничего", () => {
    expect(fillerRows(10, 10, 5, 30)).toBe(0);
    expect(fillerRows(10, 42, 5, 30)).toBe(0);
  });

  it("вырожденная высота не приводит к записи", () => {
    expect(fillerRows(10, 0, 0, 0)).toBe(0);
  });
});
