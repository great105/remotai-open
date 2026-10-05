/**
 * Клавиатура удалённого управления. Оба правила здесь родились из живых
 * жалоб: сочетание в кириллической раскладке уходило на компьютер пустым
 * нажатием, а Ctrl+C внутри строки ввода улетал на ПК вместо копирования.
 */
import { describe, expect, it } from "vitest";
import { isNonAsciiChar, layoutToLatin, localEditCodes, localEditKeys } from "./keyboard";

describe("раскладка ЙЦУКЕН → QWERTY", () => {
  it("буква сопоставляется по ФИЗИЧЕСКОЙ клавише, а не по звучанию", () => {
    expect(layoutToLatin["с"]).toBe("c"); // Ctrl+С = Ctrl+C
    expect(layoutToLatin["ф"]).toBe("a"); // Ctrl+Ф = Ctrl+A
    expect(layoutToLatin["в"]).toBe("d");
    expect(layoutToLatin["я"]).toBe("z");
  });

  it("покрыт весь ряд без пропусков — пропущенная клавиша молча теряется", () => {
    const rows = ["йцукенгшщзхъ", "фывапролджэ", "ячсмитьбюё"];
    for (const row of rows) {
      for (const ch of row) {
        expect(layoutToLatin[ch], `нет соответствия для «${ch}»`).toBeTruthy();
      }
    }
  });

  it("одна физическая клавиша — один латинский символ, дублей нет", () => {
    const values = Object.values(layoutToLatin);
    expect(new Set(values).size).toBe(values.length);
  });
});

describe("isNonAsciiChar", () => {
  it("кириллицу надо печатать, а не «нажимать»", () => {
    expect(isNonAsciiChar("ж")).toBe(true);
    expect(isNonAsciiChar("Ж")).toBe(true);
  });

  it("латиница и цифры нажимаются как клавиши", () => {
    expect(isNonAsciiChar("a")).toBe(false);
    expect(isNonAsciiChar("7")).toBe(false);
  });

  it("именованные клавиши символом не считаются", () => {
    expect(isNonAsciiChar("Enter")).toBe(false);
    expect(isNonAsciiChar("ArrowLeft")).toBe(false);
    expect(isNonAsciiChar("")).toBe(false);
  });

  it("эмодзи — одна кодовая точка из двух единиц — тоже печатается", () => {
    expect(isNonAsciiChar("😀")).toBe(true);
  });
});

describe("правка текста остаётся на телефоне", () => {
  it("Ctrl+A/C/V/X/Z не форвардятся на компьютер", () => {
    for (const code of ["KeyA", "KeyC", "KeyV", "KeyX", "KeyZ"]) {
      expect(localEditCodes.has(code)).toBe(true);
    }
    // Фолбэк по key — для клавиатур без code.
    for (const key of ["a", "c", "v", "x", "z"]) {
      expect(localEditKeys.has(key)).toBe(true);
    }
  });

  it("остальные сочетания уходят на ПК", () => {
    expect(localEditCodes.has("KeyS")).toBe(false);
    expect(localEditKeys.has("s")).toBe(false);
  });
});
