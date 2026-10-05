import { describe, expect, it } from "vitest";
import { isTerminalAutoReply } from "./autoReply";

describe("автоответы терминала не считаются вводом человека", () => {
  it("отчёт о фокусе, позиция курсора, атрибуты устройства, режимы, статус", () => {
    for (const s of [
      "\x1b[I", "\x1b[O",
      "\x1b[24;19R", "\x1b[1;1R",
      "\x1b[?1;2c", "\x1b[?62;22c", "\x1b[>0;276;0c",
      "\x1b[?1004;1$y", "\x1b[?2004;2$y",
      "\x1b[0n", "\x1b[3n",
      "\x1b]11;rgb:0000/0000/0000\x07", "\x1b]10;rgb:ffff/ffff/ffff\x1b\\",
    ]) {
      expect(isTerminalAutoReply(s), JSON.stringify(s)).toBe(true);
    }
  });

  it("пачка ответов в одном вызове — тоже автоответ", () => {
    expect(isTerminalAutoReply("\x1b[I\x1b[24;1R")).toBe(true);
  });

  it("клавиши человека — ввод: буквы, Enter, стрелки, Esc, Ctrl+C, Tab, вставка", () => {
    for (const s of [
      "a", "Какая модель", "\r", "\n", "\x03", "\t", "\x1b",
      "\x1b[A", "\x1b[B", "\x1b[C", "\x1b[D", // стрелки
      "\x1b[5~", "\x1b[6~", // PgUp/PgDn
      "\x1b[H", "\x1b[F", "\x1bOH", // Home/End
      "\x1b[200~hello\x1b[201~", // bracketed paste
      "\x1b[3~", // Delete
      "\x1b[I\x1bq", // отчёт о фокусе, склеенный с чем-то чужим
    ]) {
      expect(isTerminalAutoReply(s), JSON.stringify(s)).toBe(false);
    }
  });

  it("пустая строка — не ввод и не ответ", () => {
    expect(isTerminalAutoReply("")).toBe(false);
  });
});
