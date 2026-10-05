import { describe, expect, it } from "vitest";
import { isAgentKind } from "./components/ProcessBadge";

/*
 * `isAgentKind` — РУЧНОЕ зеркало `pty.IsAgentKind` из Go, и от него зависит не
 * косметика: по нему включается ряд быстрых клавиш агента в терминале, счёт
 * агентских сессий на плитках машин и текст уведомлений.
 *
 * Здесь проверяются только свойства, видимые из клиента. Полноту зеркала (что
 * КАЖДЫЙ вид из Go известен и тут) стережёт `internal/pty/agent_kinds_mirror_test.go`:
 * читать Go-исходник отсюда нельзя — этот файл попадает в `tsc` клиента, где
 * node-модулей нет.
 */
describe("isAgentKind", () => {
  it("grok — агент, а не обычный процесс", () => {
    expect(isAgentKind("grok")).toBe(true);
  });

  it("знает агентов, у которых есть свой ряд быстрых клавиш", () => {
    for (const kind of ["claude", "codex", "gemini", "kimi", "grok", "opencode", "aider"]) {
      expect(isAgentKind(kind), `${kind} не считается агентом`).toBe(true);
    }
  });

  it("процессы и оболочка агентами не становятся", () => {
    for (const kind of ["git", "node", "go", "python", "docker", "shell", "other", "", null, undefined]) {
      expect(isAgentKind(kind), `${kind} принят за агента`).toBe(false);
    }
  });
});
