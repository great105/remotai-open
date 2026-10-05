import { getLocale } from "@tgcontrol/shared";
import { useCallback, useEffect, useRef, useState } from "react";
import { useNavigate, useLocation } from "react-router-dom";
import { useToast, mapApiError } from "@tgcontrol/shared";
import { t } from "../i18n";
import { BottomNav } from "../components/BottomNav";
import { IconChat } from "../components/icons";
import { useGoBack } from "../navBack";
import { haptic, hapticSuccess } from "../telegram";
import {
  getSupportMessages,
  sendSupportMessage,
  type SupportMessage,
} from "../cloud/support";
import { clearSupportUnread, cloudAccountAvailable } from "../supportUnread";
import { usePolling } from "../hooks/usePolling";
import { getConfig } from "../api";
import { readSupportDraft, writeSupportDraft, prefillSupportDraft } from "../supportDraft";
import type { SupportContext } from "../cloud/support";

const POLL_MS = 10_000;

/**
 * Черновик обращения переживает уход с экрана.
 *
 * Обращение в поддержку — самый длинный текст, который человек набирает в
 * приложении, и набирает его обычно после отказа: он уходит посмотреть версию
 * или имя компьютера, возвращается — а поле пустое, потому что экран
 * размонтировался. Тот же приём уже спасает ввод в терминале
 * (`pty.draft.${id}` в PtyTermView).
 *
 * Сам ключ и правила записи живут в `supportDraft.ts`: заготовку обращения
 * кладёт ещё и карточка тарифа («Оставить заявку на Про»).
 */

/**
 * Есть ли физическая клавиатура (окно exe, десктопный браузер).
 *
 * От этого зависит поведение Enter: на телефоне «Enter = отправить» отнимает
 * единственный способ сделать перенос строки — там отправляет только кнопка
 * (N37). Признак тот же, что у CSS `@media (hover: hover) and (pointer: fine)`.
 */
function hasPhysicalKeyboard(): boolean {
  if (typeof window === "undefined" || typeof window.matchMedia !== "function") return false;
  return window.matchMedia("(hover: hover) and (pointer: fine)").matches;
}

function fmtTime(iso: string): string {
  const ts = Date.parse(iso);
  if (Number.isNaN(ts)) return "";
  const d = new Date(ts);
  const today = new Date();
  const sameDay = d.toDateString() === today.toDateString();
  const hm = d.toLocaleTimeString(getLocale(), { hour: "2-digit", minute: "2-digit" });
  return sameDay ? hm : `${d.toLocaleDateString(getLocale(), { day: "numeric", month: "short" })} ${hm}`;
}

/**
 * Своё сообщение, которое ещё не вернулось из переписки.
 *
 * Раньше после отправки поле просто очищалось, а текст появлялся в ленте
 * только если тут же удавался повторный запрос истории. Не удался (на
 * мобильной связи — обычное дело) — и человек видел пустое «Напишите здесь»:
 * он уверен, что обращение не ушло, и пишет то же самое второй раз.
 */
interface PendingMessage {
  /** Локальный ключ отрисовки, к идентификаторам релея отношения не имеет. */
  key: number;
  text: string;
  /** Появляется, когда релей принял сообщение: значит, оно точно доставлено. */
  serverId?: number;
}

/** Чат с поддержкой: свои сообщения справа, ответы поддержки слева.
 *  Опрос каждые 10 с, пока экран виден; после отправки и при новых сообщениях —
 *  автоскролл вниз. Открытие чата гасит счётчик непрочитанных. */
export function SupportView() {
  const navigate = useNavigate();
  const location = useLocation();
  const { toastError } = useToast();
  const [messages, setMessages] = useState<SupportMessage[] | null>(null);
  const [loadFailed, setLoadFailed] = useState(false);
  const [draft, setDraft] = useState(() => {
    const query = new URLSearchParams(location.search);
    if (query.get("topic") !== "installation") return readSupportDraft();
    const os = query.get("os");
    const system = os === "macos" ? "macOS" : os === "linux" ? "Linux" : os === "windows" ? "Windows" : t("ui.supportview.m9c9d3a3cb0");
    return prefillSupportDraft(t("ui.supportview.mc44993b9a9", { p0: (system) }));
  });
  const [sending, setSending] = useState(false);
  const [pending, setPending] = useState<PendingMessage[]>([]);
  const pendingKeyRef = useRef(0);
  const listRef = useRef<HTMLDivElement>(null);
  const inputRef = useRef<HTMLTextAreaElement>(null);
  const stickToBottomRef = useRef(true);
  const contextRef = useRef<SupportContext>({});
  // Чат живёт на релее: без облачного аккаунта здесь нечего показывать и некуда
  // писать. Раньше экран в LAN-режиме открывался как обычно и выдавал «Не
  // удалось загрузить переписку» при активном поле ввода (V14).
  const [hasAccount] = useState(cloudAccountAvailable);
  const [enterSends] = useState(hasPhysicalKeyboard);

  useEffect(() => {
    getConfig().then((config) => {
      contextRef.current = {
        agent_version: config.version,
        platform: config.platform,
        hostname: config.hostname,
      };
    }).catch(() => {});
  }, []);

  /**
   * Поле ввода растёт под текст (N37).
   *
   * На телефоне Enter снова переносит строку, но при `rows={1}` без роста
   * написанное выше сразу уезжает из видимости: человек пишет обращение в
   * поддержку вслепую. Потолок задан в CSS (`.support-input { max-height }`) —
   * дальше поле скроллится внутри себя.
   *
   * Вызывается эффектом ниже (после объявления scrollToBottom).
   */
  const autoGrowInput = useCallback(() => {
    const el = inputRef.current;
    if (!el) return;
    el.style.height = "auto";
    el.style.height = `${el.scrollHeight}px`;
  }, []);

  const scrollToBottom = useCallback((smooth: boolean) => {
    const el = listRef.current;
    if (el) el.scrollTo({ top: el.scrollHeight, behavior: smooth ? "smooth" : "auto" });
  }, []);

  // Сброс после отправки (draft = "") тоже возвращает поле к одной строке.
  // Растущее поле откусывает высоту у переписки — если человек был у низа,
  // держим последнее сообщение на виду (историю при чтении не дёргаем).
  useEffect(() => {
    autoGrowInput();
    if (stickToBottomRef.current) scrollToBottom(false);
  }, [draft, autoGrowInput, scrollToBottom]);

  // Черновик пишем на каждое изменение и стираем, когда поле опустело
  // (в том числе после успешной отправки — там setDraft("")).
  useEffect(() => {
    writeSupportDraft(draft);
  }, [draft]);

  // Возвращает признак успеха: отправка обязана знать, удалось ли перечитать
  // переписку, — иначе отказ на этом шаге проходил вообще без следа.
  const reload = useCallback(async (initial = false): Promise<boolean> => {
    try {
      const r = await getSupportMessages();
      setLoadFailed(false);
      // Релей на этом же запросе помечает переписку прочитанной
      // (MarkSupportReadUser в handleSupportList) — гасим общий счётчик, чтобы
      // бейдж настроек и точка в навигации не висели до следующего такта.
      clearSupportUnread();
      setMessages((prev) => {
        const next = r.messages || [];
        // Скроллим вниз при первой загрузке или когда пришли новые сообщения.
        if (initial || (prev && next.length > prev.length)) {
          stickToBottomRef.current = true;
        }
        return next;
      });
      return true;
    } catch {
      if (initial) {
        setMessages([]);
        setLoadFailed(true);
      }
      return false;
    }
  }, []);

  // Опрос переписки — через общий usePolling: голый setInterval продолжал
  // стучаться раз в 10 с и в свёрнутом приложении (в WebView2 и Telegram
  // таймеры не троттлятся). Первый вызов помечаем initial — он же рисует
  // скелет/скроллит вниз; возврат из фона обновляет чат сразу, без ожидания
  // такта. Колбэк-стрелка здесь безопасна: usePolling держит его в ref и в
  // deps таймера не кладёт (грабля 2.28.1).
  const startedRef = useRef(false);
  usePolling(() => {
    const initial = !startedRef.current;
    startedRef.current = true;
    return reload(initial);
  }, POLL_MS, { enabled: hasAccount });

  // Автоскролл: вниз только если пользователь уже у низа (не дёргать при чтении истории).
  useEffect(() => {
    if (stickToBottomRef.current) scrollToBottom(false);
  }, [messages, pending, scrollToBottom]);

  // Сообщение вернулось из переписки — локальная копия больше не нужна, иначе
  // текст на секунду двоился бы.
  useEffect(() => {
    if (!messages) return;
    setPending((prev) => {
      if (prev.length === 0) return prev;
      const ids = new Set(messages.map((m) => m.id));
      const next = prev.filter((p) => !(p.serverId && ids.has(p.serverId)));
      return next.length === prev.length ? prev : next;
    });
  }, [messages]);

  const onScroll = () => {
    const el = listRef.current;
    if (!el) return;
    stickToBottomRef.current = el.scrollHeight - el.scrollTop - el.clientHeight < 60;
  };

  const submit = async () => {
    const text = draft.trim();
    if (!text || sending) return;
    setSending(true);
    haptic();
    // Своя копия сообщения появляется в ленте сразу и живёт там, пока то же
    // самое не придёт с релея: экран после отправки не должен пустеть ни на
    // секунду, иначе поддержка получает дубли одного обращения.
    const key = ++pendingKeyRef.current;
    stickToBottomRef.current = true;
    setPending((prev) => [...prev, { key, text }]);
    try {
      const sent = await sendSupportMessage(text, contextRef.current);
      setDraft("");
      hapticSuccess();
      setPending((prev) => prev.map((p) => (p.key === key ? { ...p, serverId: sent.id } : p)));
      // Обращение уже у поддержки; не перечиталась только лента — так и пишем,
      // иначе человек прочитает отказ как «не отправилось».
      if (!(await reload())) toastError(t("support.refreshFailed"));
      scrollToBottom(true);
    } catch (e) {
      // Отправка не удалась — копию убираем, текст остаётся в поле ввода.
      setPending((prev) => prev.filter((p) => p.key !== key));
      toastError(mapApiError(e));
    } finally {
      setSending(false);
    }
  };

  /**
   * Экранная «←» ведёт туда же, куда системная «Назад».
   *
   * Стрелка жёстко уносила в «Настройки», а системная кнопка — на главную:
   * человек не мог предсказать, куда его вернёт, и после пары попыток начинал
   * ходить по вкладкам заново. Теперь обе кнопки зовут одно правило из
   * navBack.ts (шаг назад по истории, «Настройки» — запасной вариант для входа
   * по прямой ссылке из уведомления или бота). Считать шаги через
   * `window.history.length` больше нельзя: в APK и Telegram маршруты живут в
   * MemoryRouter, и длина истории WebView к нашим экранам отношения не имеет.
   */
  const stepBack = useGoBack();
  const goBack = () => {
    haptic();
    stepBack();
  };

  // Подтверждение приёма — как у Telegram-бота (sendSupportAccepted): пока
  // поддержка не ответила, приложение молчало, и человек на всякий случай
  // дублировал то же обращение в Telegram.
  const hasOwnMessage = pending.length > 0 || !!messages?.some((m) => m.sender === "user");
  const hasReply = !!messages?.some((m) => m.sender === "admin");
  const showAccepted = hasOwnMessage && !hasReply;

  return (
    <div className="page support-page">
      <div className="page-header">
        {/* Стрелка нарисована символом: без имени скринридер читает её как
            «кнопка» и уйти с экрана вслепую нельзя. */}
        <button className="back-btn" aria-label={t("generic.back")} onClick={goBack}>
          {"←"}
        </button>
        <h1>{t("settings.help.supportChat")}</h1>
      </div>

      <div className="support-list" ref={listRef} onScroll={onScroll}>
        {!hasAccount ? (
          // Честное «нужен вход», а не «не удалось загрузить переписку»:
          // облачного аккаунта нет, писать некуда и читать нечего (V14).
          <div className="support-empty">
            <div className="support-empty-glyph" aria-hidden>🔑</div>
            <p><b>{t("support.needAccountTitle")}</b></p>
            <p>{t("support.needAccount")}</p>
            <button className="btn btn-primary" onClick={() => { haptic(); navigate(`/cloud-login?next=${encodeURIComponent("/support" + location.search)}`); }}>
              {t("support.signIn")}
            </button>
          </div>
        ) : messages === null ? (
          <div className="loading-center"><div className="spinner" /></div>
        ) : loadFailed && messages.length === 0 && pending.length === 0 ? (
          <div className="support-empty">
            <div className="support-empty-glyph" aria-hidden>⚠️</div>
            <p>{t("support.loadError")}</p>
            <button className="btn btn-secondary" onClick={() => void reload(true)}>
              {t("support.retry")}
            </button>
          </div>
        ) : messages.length === 0 && pending.length === 0 ? (
          <div className="support-empty">
            <div className="support-empty-glyph" aria-hidden><IconChat size={40} /></div>
            <p>{t("support.empty")}</p>
          </div>
        ) : (
          <>
            {messages.map((m) => (
              <div key={m.id} className={`support-msg ${m.sender === "user" ? "mine" : "theirs"}`}>
                <div className="support-bubble">{m.text}</div>
                <div className="support-time">{fmtTime(m.created_at)}</div>
              </div>
            ))}
            {/* Отправленное, но ещё не вернувшееся из переписки: вместо времени
                под пузырём стоит состояние — «отправляется» / «отправлено». */}
            {pending.map((p) => (
              <div key={`pending-${p.key}`} className="support-msg mine">
                <div className="support-bubble">{p.text}</div>
                <div className="support-time pending">
                  {p.serverId ? t("support.stateSent") : t("support.stateSending")}
                </div>
              </div>
            ))}
            {showAccepted && <div className="support-system">{t("support.accepted")}</div>}
          </>
        )}
      </div>

      {hasAccount && (
        <div className="support-input-bar">
          <textarea
            className="support-input"
            ref={inputRef}
            value={draft}
            onChange={(e) => setDraft(e.target.value)}
            placeholder={t("support.placeholder")}
            rows={1}
            // На телефоне Enter — это перенос строки (единственный доступный), а
            // отправляет кнопка: «Enter = отправить» оставлено там, где есть
            // физическая клавиатура (N37).
            enterKeyHint={enterSends ? "send" : "enter"}
            onKeyDown={(e) => {
              if (enterSends && e.key === "Enter" && !e.shiftKey) {
                e.preventDefault();
                void submit();
              }
            }}
          />
          <button
            className="support-send"
            disabled={!draft.trim() || sending}
            onClick={() => void submit()}
            aria-label={t("support.send")}
          >
            ➤
          </button>
        </div>
      )}
      <BottomNav active="support" />
    </div>
  );
}
