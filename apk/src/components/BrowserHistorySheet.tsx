/**
 * История посещений виртуального браузера.
 *
 * «Часто открываю» на стартовом экране отвечает на вопрос «куда я обычно
 * захожу», а история — на другой: «где я видел ТУ страницу вчера». Через
 * удалённый экран это особенно важно: адрес длинной статьи с телефона заново не
 * наберёшь, а закладку человек поставить не успел.
 *
 * История живёт на машине — значит видна с любого пульта, и ищется словами, а
 * не прокруткой пятисот строк.
 */

import { useEffect, useMemo, useRef, useState } from "react";
import type { BrowserHistoryEntry } from "../api";
import { forgetBrowserHistory, getBrowserHistory } from "../api";
import { mapApiError, t, useEscape, useToast } from "@tgcontrol/shared";
import { haptic, tgConfirm } from "../telegram";

export interface BrowserHistorySheetProps {
  onClose: () => void;
  onOpen: (url: string) => void;
}

/** Когда это было: «сегодня», «вчера» или дата — как в телефонных браузерах. */
function dayLabel(at: number): string {
  const date = new Date(at * 1000);
  const today = new Date();
  const same = (a: Date, b: Date) => a.getFullYear() === b.getFullYear()
    && a.getMonth() === b.getMonth() && a.getDate() === b.getDate();
  if (same(date, today)) return t("remote.historyToday");
  const yesterday = new Date(today.getTime() - 86400000);
  if (same(date, yesterday)) return t("remote.historyYesterday");
  return date.toLocaleDateString();
}

function timeLabel(at: number): string {
  return new Date(at * 1000).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
}

export default function BrowserHistorySheet({ onClose, onOpen }: BrowserHistorySheetProps) {
  // Системная «Назад» закрывает лист, а не уводит с экрана (аудит ИА 02.09.2026, P0-6).
  useEscape(true, onClose);
  const [entries, setEntries] = useState<BrowserHistoryEntry[]>([]);
  const [query, setQuery] = useState("");
  const [loading, setLoading] = useState(true);
  const { toastError } = useToast();
  const debounce = useRef<ReturnType<typeof setTimeout> | null>(null);

  const load = (search: string) => {
    setLoading(true);
    getBrowserHistory(search, 200)
      .then((res) => setEntries(res.history || []))
      .catch((e: any) => toastError(mapApiError(e)))
      .finally(() => setLoading(false));
  };

  useEffect(() => { load(""); }, []);

  // Поиск идёт на машине: шлём запрос не на каждую букву, а когда человек
  // остановился — иначе на каждый символ уходил бы запрос через облако.
  useEffect(() => {
    if (debounce.current) clearTimeout(debounce.current);
    debounce.current = setTimeout(() => load(query.trim()), 300);
    return () => { if (debounce.current) clearTimeout(debounce.current); };
  }, [query]);

  // Группировка по дням: без неё двести строк читаются как один поток.
  const groups = useMemo(() => {
    const out: Array<{ day: string; items: BrowserHistoryEntry[] }> = [];
    for (const entry of entries) {
      const day = dayLabel(entry.at);
      const last = out[out.length - 1];
      if (last && last.day === day) last.items.push(entry);
      else out.push({ day, items: [entry] });
    }
    return out;
  }, [entries]);

  const forget = async (url: string) => {
    haptic("light");
    setEntries((list) => list.filter((item) => item.url !== url));
    try { await forgetBrowserHistory(url); } catch { /* уже забыто */ }
  };

  const clearAll = async () => {
    if (!(await tgConfirm(t("remote.historyClearConfirm"), {
      danger: true, confirmText: t("remote.historyClear"),
    }))) return;
    try {
      await forgetBrowserHistory("", true);
      setEntries([]);
    } catch (e: any) {
      toastError(mapApiError(e));
    }
  };

  return (
    <div className="remote-sheet-backdrop" onClick={onClose}>
      <div className="remote-menu-sheet vb-history" onClick={(e) => e.stopPropagation()}>
        <div className="remote-quick-section">{t("remote.historyTitle")}</div>
        <input className="vb-history-search" value={query} placeholder={t("remote.historySearch")}
          autoCapitalize="off" autoCorrect="off" spellCheck={false}
          onChange={(e) => setQuery(e.target.value)} />

        {loading && <div className="vb-fill-empty">…</div>}
        {!loading && entries.length === 0 && (
          <div className="vb-fill-empty">
            {query ? t("remote.historyNothing") : t("remote.historyEmpty")}
          </div>
        )}

        {groups.map((group) => (
          <div key={group.day} className="vb-history-group">
            <div className="vb-history-day">{group.day}</div>
            {group.items.map((entry) => (
              <div key={entry.url + entry.at} className="vb-history-row">
                <button className="vb-history-open" onClick={() => { haptic("light"); onOpen(entry.url); }}>
                  <span className="vb-history-texts">
                    <span className="vb-history-title">{entry.title || entry.host}</span>
                    <span className="vb-history-host">{entry.host} · {timeLabel(entry.at)}</span>
                  </span>
                </button>
                <button className="vb-history-forget" aria-label={t("remote.historyForget")}
                  onClick={() => void forget(entry.url)}>×</button>
              </div>
            ))}
          </div>
        ))}

        {entries.length > 0 && !query && (
          <button className="remote-quick-row remote-menu-danger" onClick={() => void clearAll()}>
            <span className="remote-quick-row-icon">{"🗑"}</span>
            <span className="remote-quick-row-label">{t("remote.historyClear")}</span>
          </button>
        )}
      </div>
    </div>
  );
}
