/**
 * Кнопка «Привязать этот сервер» появляется ровно тогда, когда в терминале
 * действительно ждут подтверждения привязки — и не появляется ни секундой
 * дольше.
 */
import { describe, expect, it } from "vitest";
import { pairCodeInOutput } from "./pairOffer";

const PAIR_OUTPUT = `
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
  Remotai — привязка сервера
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

  Сервер:   ams-1-vm-7o4h  (device b9d6f138c0cd)
  Код:      4E3A-HJED
  Годен до: 20:33:05

  Подтвердите привязку одним из способов:
   1) Прямо здесь: в Remotai нажмите «Привязать этот сервер»
   2) Откройте в Telegram:  https://t.me/Autocode1_bot?start=pair_4E3A-HJED
   3) Отсканируйте QR приложением Remotai на телефоне:

  Ожидаю подтверждения (Ctrl+C — отмена)…
`;

describe("pairCodeInOutput", () => {
  it("находит код в выводе `remotai pair`", () => {
    expect(pairCodeInOutput(PAIR_OUTPUT)).toBe("4E3A-HJED");
  });

  it("без заголовка команды кодом ничего не считается", () => {
    // Похожий набор символов в чужом выводе не должен звать привязывать сервер.
    expect(pairCodeInOutput("git log --oneline\nA1B2-C3D4 fix: что-то\n")).toBe("");
    expect(pairCodeInOutput("Код: 4E3A-HJED\n")).toBe("");
  });

  it("после отмены (Ctrl+C) предлагать нечего", () => {
    expect(pairCodeInOutput(PAIR_OUTPUT + "\n.....^C\nremotai: отменено\n")).toBe("");
  });

  it("после успешной привязки предложение исчезает", () => {
    expect(pairCodeInOutput(PAIR_OUTPUT + "\n✓ Сервер привязан к аккаунту\n")).toBe("");
  });

  it("новая попытка после отменённой снова даёт код", () => {
    const again = PAIR_OUTPUT + "\nremotai: отменено\n" + PAIR_OUTPUT.replace("4E3A-HJED", "77KP-9QW2");
    expect(pairCodeInOutput(again)).toBe("77KP-9QW2");
  });

  it("пустой вывод не ломает правило", () => {
    expect(pairCodeInOutput("")).toBe("");
  });
});
