import { useId, useState } from "react";
import { agentDisplayName, formatAgoValue, mapApiError, t, useToast, type PtyLostSession } from "@tgcontrol/shared";
import { closePtySession, restorePtySession } from "../api";
import { haptic, hapticSuccess, tgConfirm } from "../telegram";

/**
 * Терминалы, прерванные перезагрузкой компьютера.
 *
 * Персистентный терминал переживает перезапуск Remotai (мини-tmux), но не
 * выключение системы: процесс шелла умирает вместе с ней. Раньше запись о таком
 * терминале просто удалялась — человек включал компьютер, открывал список и
 * видел пустоту: ни имени, ни папки, ни следа ночной работы агента. При этом
 * продукт обещает обратное («работа идёт, пока тебя нет»), и автообновление
 * специально откладывает перезапуск, лишь бы не потерять сессии.
 *
 * Блок НЕ притворяется, что процесс жив: он говорит, что случилось, и предлагает
 * ровно одно действие — открыть терминал заново там же, с последними строками
 * прошлой работы. Ни одна команда сама не повторяется: что запускать, решает
 * человек (агент мог остановиться на половине задачи).
 */
export function LostTerminals({
  lost,
  compact = false,
  onOpen,
  onChanged,
}: {
  lost: PtyLostSession[];
  /** Keep normal sessions visible when there is also work to restore. */
  compact?: boolean;
  /** Терминал поднят заново — открыть его (id тот же, что был). */
  onOpen: (id: string) => void;
  /** Список изменился (восстановили или убрали) — обновить экран. */
  onChanged: () => void;
}) {
  const { toastError, toastSuccess } = useToast();
  const [busyId, setBusyId] = useState("");
  const [expanded, setExpanded] = useState(false);
  const contentId = useId();
  const showCards = !compact || expanded;

  if (!lost.length) return null;

  // Имя карточки нужно и в заголовке, и в вопросе об удалении: считаем его в
  // одном месте, иначе диалог назовёт терминал не так, как он подписан.
  const cardName = (item: PtyLostSession) =>
    item.name || item.cwd.split(/[\\/]/).pop() || t("pty.title");

  const restore = async (item: PtyLostSession) => {
    if (busyId) return;
    haptic();
    setBusyId(item.id);
    try {
      // Размер терминала выставит сам экран после открытия; здесь важен только
      // факт «подними процесс», поэтому просим разумный минимум.
      await restorePtySession(item.id, 80, 24);
      hapticSuccess();
      toastSuccess(t("pty.lost.restored"));
      onChanged();
      onOpen(item.id);
    } catch (e) {
      toastError(mapApiError(e));
    } finally {
      setBusyId("");
    }
  };

  const forget = async (item: PtyLostSession) => {
    if (busyId) return;
    haptic();
    // Единственное необратимое действие в списке: сервер удаляет и запись, и
    // сохранённый хвост вывода (ForgetLost в internal/pty/restore.go), то есть
    // ночную работу агента вернуть будет нечем. Мёртвый терминал в списке ниже
    // спрашивают, а этот — нет; выравниваем (UX-аудит 2026-08-23, NIELSEN-6).
    // Спрашиваем ДО setBusyId: иначе отказ оставит карточку с заблокированными
    // кнопками — до `finally` эта ветка не доходит.
    const ask = item.has_scrollback ? "pty.lost.forgetConfirm" : "pty.lost.forgetConfirmNoTail";
    const ok = await tgConfirm(t(ask, { name: cardName(item) }), {
      danger: true,
      confirmText: t("confirm.btn.closeDead"),
    });
    if (!ok) return;
    setBusyId(item.id);
    try {
      // Та же дверь, что и у закрытия живого терминала: агент понимает DELETE и
      // для карточки, ждущей восстановления.
      await closePtySession(item.id);
      toastSuccess(t("pty.lost.forgot"));
      onChanged();
    } catch (e) {
      toastError(mapApiError(e));
    } finally {
      setBusyId("");
    }
  };

  return (
    <section className="pty-lost" aria-label={t("pty.lost.title")}>
      {compact && (
        <button className="pty-lost-summary" aria-expanded={showCards} aria-controls={contentId}
          onClick={() => { haptic(); setExpanded(!expanded); }}>
          <span>{t("pty.lost.summary", { n: lost.length })}</span>
          <span>{t(showCards ? "generic.hide" : "pty.lost.showRecovery")}</span>
        </button>
      )}
      {showCards && <div id={contentId}>
      <div className="pty-lost-head">
        <div className="pty-lost-title">{t("pty.lost.title")}</div>
        <div className="pty-lost-desc">{t("pty.lost.desc")}</div>
      </div>
      <div className="cards-grid">
        {lost.map((item) => (
          <div key={item.id} className="card pty-card pty-lost-card">
            <div className="pty-card-top">
              <span className="pty-card-name">{cardName(item)}</span>
            </div>
            <div className="pty-card-path" title={item.cwd}>{item.cwd}</div>
            <div className="pty-lost-facts">
              {item.agent && (
                <span className="pty-lost-fact">
                  {t("pty.lost.agentWas", { agent: agentDisplayName(item.agent) })}
                </span>
              )}
              {item.lost_at > 0 && (
                <span className="pty-lost-fact">
                  {t("pty.lost.when", { value: formatAgoValue(item.lost_at) })}
                </span>
              )}
              {/* Обещаем «последние строки» только когда они есть. */}
              {item.has_scrollback && <span className="pty-lost-fact">{t("pty.lost.hasTail")}</span>}
              {item.cwd_missing && (
                <span className="pty-lost-fact pty-lost-warn">{t("pty.lost.cwdMissing")}</span>
              )}
            </div>
            <div className="pty-lost-actions">
              <button
                className="btn btn-primary btn-sm"
                disabled={busyId === item.id}
                onClick={(e) => { e.stopPropagation(); void restore(item); }}
              >
                {busyId === item.id ? t("pty.lost.restoring") : t("pty.lost.restore")}
              </button>
              <button
                className="btn btn-secondary btn-sm"
                disabled={busyId === item.id}
                onClick={(e) => { e.stopPropagation(); void forget(item); }}
              >
                {t("pty.lost.forget")}
              </button>
            </div>
          </div>
        ))}
      </div>
      </div>}
    </section>
  );
}
