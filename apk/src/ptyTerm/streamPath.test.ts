import { describe, expect, it } from "vitest";
import { STREAM_QUERY_ALLOWED, agentSupportsResumeInPath, localStreamPath, resumeParam } from "./streamPath";

// ЖИВОЙ ОТКАЗ 12.08.2026, стоил телефону всех терминалов на семь минут.
// Клиент начал просить кадр экрана флагом `?screen=1`. Релей проверяет
// параметры пути по ЗАКРЫТОМУ белому списку и на незнакомый ответил
// «400 invalid stream path» за 2 мс: соединение умирало на релее, до
// компьютера не доходило вовсе (35 отказов подряд, в журнале агента — ни одной
// попытки). На localhost этого не видно: там релея нет.
//
// Поэтому список сверяется С ИСХОДНИКОМ РЕЛЕЯ, а не переписан по памяти:
// разъедутся — упадёт здесь, а не у человека в руках.
describe("параметры пути стрима и белый список релея", () => {
  it("параметр вне списка не уезжает молча, а валит сборку пути", () => {
    expect(() => localStreamPath("/ws/pty/a1", { screen: "1" } as never)).toThrow(/screen/);
    expect(() => localStreamPath("/ws/pty/a1", { anything: "x" } as never)).toThrow();
  });

  it("список ровно тот, что пропускает релей (сверку с его исходником делает check-stream-query)", () => {
    expect([...STREAM_QUERY_ALLOWED]).toEqual(["resume"]);
  });
});

describe("localStreamPath", () => {
  it("без параметров возвращает путь как есть", () => {
    expect(localStreamPath("/ws/pty/a1")).toBe("/ws/pty/a1");
    expect(localStreamPath("/ws/pty/a1", {})).toBe("/ws/pty/a1");
  });

  it("добавляет resume к чистому пути", () => {
    expect(localStreamPath("/ws/pty/a1", { resume: "6f1a:918273" }))
      .toBe("/ws/pty/a1?resume=6f1a%3A918273");
  });

  it("пропускает пустые значения — первый коннект просить нечего", () => {
    expect(localStreamPath("/ws/pty/a1", { resume: undefined })).toBe("/ws/pty/a1");
    expect(localStreamPath("/ws/pty/a1", { resume: "" })).toBe("/ws/pty/a1");
    expect(localStreamPath("/ws/pty/a1", { resume: null })).toBe("/ws/pty/a1");
  });

  it("уважает уже имеющийся запрос", () => {
    expect(localStreamPath("/ws/pty/a1?x=1", { resume: "e:2" }))
      .toBe("/ws/pty/a1?x=1&resume=e%3A2");
  });

  it("экранирует значения — двоеточие в resume не должно ломать разбор", () => {
    const path = localStreamPath("/ws/pty/a1", { resume: "6f1a2b3c:918273" });
    // Именно этот путь целиком уезжает в параметр path= облачного адреса,
    // поэтому он обязан разбираться обратно без потерь.
    const parsed = new URL("ws://host" + path);
    expect(parsed.searchParams.get("resume")).toBe("6f1a2b3c:918273");
  });
});

describe("resumeParam", () => {
  it("склеивает эпоху и смещение", () => {
    expect(resumeParam("6f1a", 918273)).toBe("6f1a:918273");
  });

  it("без эпохи не просит ничего", () => {
    expect(resumeParam("", 100)).toBeUndefined();
  });
});

describe("agentSupportsResumeInPath", () => {
  it("умеют версии с 2.49.11", () => {
    expect(agentSupportsResumeInPath("2.49.11")).toBe(true);
    expect(agentSupportsResumeInPath("2.49.12")).toBe(true);
    expect(agentSupportsResumeInPath("2.50.0")).toBe(true);
    expect(agentSupportsResumeInPath("3.0.0")).toBe(true);
    expect(agentSupportsResumeInPath("v2.49.11")).toBe(true);
  });

  it("не умеют версии до 2.49.11 — им путь с запросом ломал авторизацию", () => {
    expect(agentSupportsResumeInPath("2.49.10")).toBe(false);
    expect(agentSupportsResumeInPath("2.49.9")).toBe(false);
    expect(agentSupportsResumeInPath("2.48.0")).toBe(false);
    expect(agentSupportsResumeInPath("1.9.9")).toBe(false);
  });

  it("непонятная версия считается неумеющей: цена ошибки — неоткрывшийся терминал", () => {
    expect(agentSupportsResumeInPath("")).toBe(false);
    expect(agentSupportsResumeInPath(undefined)).toBe(false);
    expect(agentSupportsResumeInPath(null)).toBe(false);
    expect(agentSupportsResumeInPath("dev")).toBe(false);
  });
});
