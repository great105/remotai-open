/**
 * Правила экрана терминала. Главное здесь — не показывать два противоречащих
 * состояния сразу: экран, который одновременно писал «Переподключение…» и
 * «Процесс завершён», заставлял людей перезапускать живой терминал из-за
 * мигнувшей сети.
 */
import { describe, expect, it } from "vitest";
import { answerChoicesOf, outcomeAdviceKey, terminalLinkState, titleNamesAgent, type PtyStateExtra } from "./rules";
import type { PtyState } from "../api";

function state(over: Partial<PtyStateExtra> = {}): PtyStateExtra {
  return { alive: true, ...over } as PtyStateExtra;
}

describe("answerChoicesOf", () => {
  it("подписи пунктов приходят с компьютера и показываются как есть", () => {
    const choices = answerChoicesOf(
      state({ hint_options: ["Yes", "Yes, and don't ask again", "No"] }),
      "choice",
    );
    expect(choices).toEqual([
      { digit: "1", label: "Yes" },
      { digit: "2", label: "Yes, and don't ask again" },
      { digit: "3", label: "No" },
    ]);
  });

  it("длинное меню режется до пяти — больше на телефон не помещается", () => {
    const options = ["a", "b", "c", "d", "e", "f", "g"];
    expect(answerChoicesOf(state({ hint_options: options }), "choice")).toHaveLength(5);
  });

  it("старый агент подписей не прислал — остаются цифры, но кнопки есть", () => {
    expect(answerChoicesOf(state(), "choice").map((c) => c.digit)).toEqual(["1", "2", "3"]);
  });

  it("вопрос не меню (да/нет, ввод текста) — цифры по умолчанию", () => {
    expect(answerChoicesOf(state({ hint_options: ["Yes", "No"] }), "yes_no")
      .map((c) => c.label)).toEqual(["", "", ""]);
    expect(answerChoicesOf(state({ hint_options: ["Yes"] }), "")).toHaveLength(3);
  });
});

describe("terminalLinkState", () => {
  it("пока связь идёт, про мёртвый процесс не говорим — данные протухшие", () => {
    const r = terminalLinkState({ showConnecting: true, reconnecting: false, gaveUp: false, alive: false });
    expect(r).toEqual({ linkPending: true, processDead: false });
  });

  it("переподключение — то же самое", () => {
    const r = terminalLinkState({ showConnecting: false, reconnecting: true, gaveUp: false, alive: false });
    expect(r.linkPending).toBe(true);
    expect(r.processDead).toBe(false);
  });

  it("ждать перестали и процесс мёртв — вот теперь плашка с перезапуском", () => {
    const r = terminalLinkState({ showConnecting: true, reconnecting: true, gaveUp: true, alive: false });
    expect(r).toEqual({ linkPending: false, processDead: true });
  });

  it("живой терминал не объявляется завершённым ни при каких обстоятельствах", () => {
    for (const gaveUp of [false, true]) {
      for (const reconnecting of [false, true]) {
        expect(terminalLinkState({ showConnecting: false, reconnecting, gaveUp, alive: true }).processDead)
          .toBe(false);
      }
    }
  });
});

describe("titleNamesAgent", () => {
  it("заголовок уже назвал агента — бейдж рядом не повторяет имя", () => {
    expect(titleNamesAgent({ agent_kind: "claude" } as PtyState)).toBe(true);
  });

  it("у терминала есть своё имя — заголовок про агента молчит", () => {
    expect(titleNamesAgent({ name: "Сборка", agent_kind: "claude" } as PtyState)).toBe(false);
  });

  it("обычный шелл агентом не считается", () => {
    expect(titleNamesAgent({ agent_kind: "shell" } as PtyState)).toBe(false);
    expect(titleNamesAgent({ agent_kind: "other" } as PtyState)).toBe(false);
    expect(titleNamesAgent({} as PtyState)).toBe(false);
  });
});

describe("outcomeAdviceKey", () => {
  it("одинокое «Killed» объясняется нехваткой памяти", () => {
    expect(outcomeAdviceKey("Killed")).toBe("pty.outcomeOom");
    expect(outcomeAdviceKey("npm error signal SIGKILL")).toBe("pty.outcomeOom");
    expect(outcomeAdviceKey("FATAL ERROR: JavaScript heap out of memory")).toBe("pty.outcomeOom");
  });

  it("обычная ошибка сборки расшифровки не получает — строка говорит сама", () => {
    expect(outcomeAdviceKey("npm ERR! code E404")).toBe("");
    expect(outcomeAdviceKey("")).toBe("");
    expect(outcomeAdviceKey(undefined)).toBe("");
  });
});
