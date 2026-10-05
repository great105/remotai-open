import { afterEach, expect, it, vi } from "vitest";
import {
  foregroundTask, SILENCE_RECHECK_SLACK_MS, SILENCE_THAW_GRACE_MS, silenceRecheckDelay, silenceVerdict, thawedAt,
} from "./ForegroundTask";

afterEach(() => vi.useRealTimers());

it("rechecks silence right when the thaw grace ends, not on the next watchdog tick (T-33 dead after background)", () => {
  const limit = 12_500;
  const back = 1_000_000;
  // Возврат из фона: перепроверка ровно по истечении льготы (плюс запас), и
  // в этот момент приговор уже выносится — мёртвый сокет закрыт за ~3 с, а не
  // на следующем 5-секундном тике (замер ревьюера: 6,6–6,9 с).
  const delay = silenceRecheckDelay(back, back, limit)!;
  expect(delay).toBe(SILENCE_THAW_GRACE_MS + SILENCE_RECHECK_SLACK_MS);
  expect(silenceVerdict(back + delay, back - 20_000, limit, back)).toBe(true);
  // Живой сокет: накопившиеся hb доехали внутри льготы — перепроверка не рвёт.
  expect(silenceVerdict(back + delay, back + 400, limit, back)).toBe(false);
  // Вызов позже возврата — остаток льготы; льгота уже прошла — сразу (запас).
  expect(silenceRecheckDelay(back + 1_000, back, limit)).toBe(SILENCE_THAW_GRACE_MS - 1_000 + SILENCE_RECHECK_SLACK_MS);
  expect(silenceRecheckDelay(back + 60_000, back, limit)).toBe(SILENCE_RECHECK_SLACK_MS);
  // Сторож выключен или страница не возвращалась — таймера нет.
  expect(silenceRecheckDelay(back, back, 0)).toBeNull();
  expect(silenceRecheckDelay(back, 0, limit)).toBeNull();
  expect(silenceRecheckDelay(NaN, back, limit)).toBeNull();
  expect(SILENCE_RECHECK_SLACK_MS).toBeLessThan(1000);
});

it("detects a frozen visible page by a late watchdog tick, never a hidden one (T-33 thaw)", () => {
  // Обычный тик и небольшое опоздание — не разморозка.
  expect(thawedAt(10_000, 15_000, 5_000, true)).toBeNull();
  expect(thawedAt(10_000, 20_000, 5_000, true)).toBeNull();
  // Тик опоздал больше чем на два периода — страница стояла.
  expect(thawedAt(10_000, 20_001, 5_000, true)).toBe(20_001);
  // Скрытая страница: таймеры троттлятся, а сообщения идут — не судим.
  expect(thawedAt(10_000, 80_000, 5_000, false)).toBeNull();
  // Первый тик соединения и порченые периоды.
  expect(thawedAt(0, 80_000, 5_000, true)).toBeNull();
  expect(thawedAt(10_000, 80_000, 0, true)).toBeNull();
  expect(thawedAt(NaN, 80_000, 5_000, true)).toBeNull();
  // Вместе со сторожем: после разморозки живой сокет не закрывается сразу.
  const thaw = thawedAt(10_000, 70_000, 5_000, true)!;
  expect(silenceVerdict(70_000, 9_000, 12_500, thaw)).toBe(false);
  expect(silenceVerdict(70_000 + SILENCE_THAW_GRACE_MS, 9_000, 12_500, thaw)).toBe(true);
});

it("does not kill a live socket right after thaw, but still detects real silence (T-33)", () => {
  const limit = 50_000;
  // Сервер не объявил hb — сторож выключен при любой тишине.
  expect(silenceVerdict(1e9, 0, 0, 0, SILENCE_THAW_GRACE_MS)).toBe(false);
  // Страница не уходила в фон: тишина дольше лимита — мёртв.
  expect(silenceVerdict(60_001, 10_000, limit, 0, SILENCE_THAW_GRACE_MS)).toBe(true);
  expect(silenceVerdict(60_000, 10_000, limit, 0, SILENCE_THAW_GRACE_MS)).toBe(false);
  // Разморозка: lastMsg взят до заморозки 5 минут назад, но страница видима
  // только 1 с — накопившиеся hb ещё едут, закрывать нельзя.
  const back = 400_000;
  expect(silenceVerdict(back + 1_000, 100_000, limit, back, SILENCE_THAW_GRACE_MS)).toBe(false);
  expect(silenceVerdict(back + 2_999, 100_000, limit, back, SILENCE_THAW_GRACE_MS)).toBe(false);
  // Льгота истекла, а сообщений так и нет — мёртв.
  expect(silenceVerdict(back + 3_000, 100_000, limit, back, SILENCE_THAW_GRACE_MS)).toBe(true);
  expect(SILENCE_THAW_GRACE_MS).toBe(3000);
});
it("removes hidden timers, refreshes on return, never overlaps and stops pending work", async () => {
  vi.useFakeTimers();
  let listener = () => {};
  const visibility = { hidden: false,
    addEventListener: (_: string, callback: () => void) => { listener = callback; },
    removeEventListener: vi.fn(),
  };
  let finish = () => {};
  const task = vi.fn(() => new Promise<void>(resolve => { finish = resolve; }));
  const stop = foregroundTask(task, 100, visibility);
  await vi.advanceTimersByTimeAsync(1000);
  expect(task).toHaveBeenCalledTimes(1);
  visibility.hidden = true; listener(); finish();
  await vi.advanceTimersByTimeAsync(1000);
  expect(vi.getTimerCount()).toBe(0);
  visibility.hidden = false; listener();
  expect(task).toHaveBeenCalledTimes(2);
  finish(); await vi.advanceTimersByTimeAsync(0);
  expect(vi.getTimerCount()).toBe(1);
  stop(); await vi.advanceTimersByTimeAsync(1000);
  expect(task).toHaveBeenCalledTimes(2);
  expect(vi.getTimerCount()).toBe(0);
  expect(visibility.removeEventListener).toHaveBeenCalled();
});
