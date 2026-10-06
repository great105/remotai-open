import { formatAgoValue, t } from "@tgcontrol/shared";
import type { PtySessionInfo } from "@tgcontrol/shared";

export type PtyStatusIcon = "working" | "sleep" | "stalled" | "waiting" | "ready" | "error" | "link" | null;
export interface PtyStatusInfo { cls: string; icon: PtyStatusIcon; text: string; title?: string }

/** One status rule for a selected computer and the combined terminal list. */
export function ptyListStatus(s: PtySessionInfo, nowMs: number, rememberedAgent = false): PtyStatusInfo {
  const timeAgo = (ms: number) => t("pty.timeAgo", { value: formatAgoValue(ms, nowMs) });

    const at = s.status_at || s.last_active || s.created;
    // Спящий агент (ptyTerm/agentSleep.ts): в терминале шелл, и без этой ветки
    // карточка говорила бы «свободен», хотя беседа ждёт пробуждения.
    if (s.alive && s.sleep) {
      return { cls: "idle", icon: "sleep", text: t("pty.statusSleeping", { value: timeAgo(s.sleep.at) }) };
    }
    const st = s.status || (s.alive ? "idle" : "dead");
    switch (st) {
      case "working": {
        // «Работает» само по себе не отвечает на главный вопрос списка: агент
        // думает или подвис? Возраст последнего вывода отличает одно от
        // другого — данные уже приходят в ответе, их просто не показывали.
        const since = s.last_active || s.status_at || s.created;
        return since
          ? { cls: "working", icon: "working", text: t("pty.statusWorkingSince", { value: timeAgo(since) }) }
          : { cls: "working", icon: "working", text: t("pty.statusWorking") };
      }
      case "stalled":
        // Подвис ≠ ждёт ответа. Янтарь остаётся (это ненормально), но иконка
        // своя и без пульса: пульсирующий ⏳ означает «агент задал вопрос»,
        // и подвисший терминал был от него неотличим. Возраст терминала из
        // текста убран — важна только длительность тишины.
        return {
          cls: "stalled",
          icon: "stalled",
          // Величина «сырая», без «назад»: подставляя timeAgo, фраза выходила
          // «работает, но вывода нет 12м назад» — человек спотыкался ровно на
          // том статусе, ради которого и открыл список.
          text: t("pty.statusStalledFor", {
            value: formatAgoValue(s.last_active || s.status_at || s.created, nowMs),
          }),
        };
      case "waiting":
        return { cls: "waiting", icon: "waiting", text: t("pty.statusWaiting"), title: s.hint };
      case "ready": {
        // Агент жив, но вопроса нет — «освободился»: без янтарной тревоги и
        // пульса, иначе каждый закончивший агент выглядит как требующий ответа.
        //
        // СО ВРЕМЕНЕМ. «Свободен» был единственным статусом без него: агент,
        // закончивший пять минут назад сорокаминутную работу, выглядел ровно
        // так же, как простаивающий месяц, — а это и есть первый вопрос, с
        // которым сюда возвращаются (аудит путей 29.08.2026).
        const done = s.status_at || s.last_active || 0;
        return {
          cls: "ready",
          icon: "ready",
          text: done ? t("pty.statusReadySince", { value: timeAgo(done) }) : t("pty.statusReady"),
        };
      }
      case "error":
        // title — та же строка ошибки, что и на карточке: на десктопе она
        // читается целиком по наведению, если не влезла в одну строку.
        return { cls: "error", icon: "error", text: t("pty.statusError", { value: timeAgo(at) }), title: s.hint };
      case "dead":
        // Процесс терминала ЖИВ, оборвалась только связь с ним: «завершён» —
        // неправда, и работа внутри продолжается прямо сейчас.
        if (s.host_alive) {
          return { cls: "dead", icon: "link", text: t("pty.linkLost"), title: t("pty.linkLostHint") };
        }
        if (s.died_at) {
          const left = Math.max(0, Math.ceil((s.died_at + 5 * 60_000 - nowMs) / 60_000));
          return {
            cls: "dead",
            icon: null,
            text: left > 0 ? t("pty.deadRetained", { n: left }) : t("pty.deadExpiring"),
            title: s.hint,
          };
        }
        return { cls: "dead", icon: null, text: t("pty.dead"), title: s.hint };
      default: {
        // idle. «Готово» у шелла, в котором ничего не запускали, читается как
        // «задача выполнена» — человек идёт искать результат, которого нет.
        // Итог работы бывает только там, где работал агент; голый шелл просто
        // свободен (то же слово, что и у освободившегося агента).
        const hadAgent = rememberedAgent || !!(s.agent_kind && s.agent_kind !== "shell");
        return hadAgent
          ? { cls: "idle", icon: "ready", text: t("pty.statusDone", { value: timeAgo(at) }) }
          : { cls: "idle", icon: null, text: t("pty.statusFree", { value: timeAgo(at) }) };
      }
    }
  }
