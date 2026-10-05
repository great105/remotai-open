import { useEffect, useRef, useState } from "react";
import type { KeyboardEvent as ReactKeyboardEvent, ReactNode } from "react";
import { setDialogEmitter, type DialogRequest } from "../dialog";
import { useEscape } from "../hooks/useEscape";
import { platform } from "../platform";
import { t } from "../i18n";
import { useLanguage } from "../locale";

const FOCUSABLE_SELECTOR =
  'button:not([disabled]), input:not([disabled]), textarea:not([disabled]), select:not([disabled]), a[href], [tabindex]:not([tabindex="-1"])';

/** Всё, на что внутри контейнера может встать фокус (в порядке DOM). */
export function focusablesIn(root: HTMLElement | null): HTMLElement[] {
  if (!root) return [];
  return [...root.querySelectorAll<HTMLElement>(FOCUSABLE_SELECTOR)];
}

/**
 * Ловушка Tab: фокус ходит по кругу внутри контейнера и не уходит на список под
 * затемнением. Возвращает true, если событие обработано.
 */
export function trapTab(root: HTMLElement | null, e: ReactKeyboardEvent): boolean {
  if (e.key !== "Tab") return false;
  const items = focusablesIn(root);
  if (items.length === 0) return false;
  const index = items.indexOf(document.activeElement as HTMLElement);
  const next = e.shiftKey
    ? (index <= 0 ? items.length - 1 : index - 1)
    : (index < 0 || index === items.length - 1 ? 0 : index + 1);
  e.preventDefault();
  items[next]?.focus();
  return true;
}

/**
 * Single global host that renders imperative dialogs requested via dialog.ts.
 * Mounted once at the app root. Shows one dialog at a time, queueing the rest.
 *
 * Reuses the existing .modal-* shell, so the only extra CSS is .dialog-message /
 * .dialog-sheet / .btn-danger (present in apk; ported into miniapp styles).
 *
 * danger:true требует confirmText с глаголом действия («Удалить», «Выключить») —
 * см. DialogOptionsDanger в dialog.ts. Fallback t("dialog.confirm") ниже остаётся
 * только для вызовов из нетипизированного JS.
 *
 * Клавиатура (окно exe и браузер, где нативный confirm подавлен и это
 * единственный диалог продукта): при открытии фокус ставится внутрь шторки,
 * Tab по ней ходит по кругу и не уходит на список под затемнением, Enter
 * подтверждает безопасное действие, Esc отменяет (useEscape), а после закрытия
 * фокус возвращается на кнопку, из которой диалог вызвали.
 */
export function DialogHost() {
  useLanguage();
  const [queue, setQueue] = useState<DialogRequest[]>([]);
  const [value, setValue] = useState("");
  const cur = queue[0] ?? null;
  const sheetRef = useRef<HTMLDivElement | null>(null);
  const confirmRef = useRef<HTMLButtonElement | null>(null);
  const cancelRef = useRef<HTMLButtonElement | null>(null);

  useEffect(() => {
    setDialogEmitter((req) => setQueue((q) => [...q, req]));
    return () => setDialogEmitter(null);
  }, []);

  // Начальное значение поля ставим ВО ВРЕМЯ РЕНДЕРА, а не эффектом.
  //
  // Эффектом это опаздывало на кадр: диалог успевал показать значение
  // ПРЕДЫДУЩЕГО prompt (замер `probe-pty-rename.mjs`: поле переименования
  // открывалось с «Работа» — именем группы из прошлого диалога — и только через
  // ~500 мс становилось своим). Хуже мигания то, что опоздавший setValue
  // затирал уже набранное: человек на телефоне жмёт «Переименовать» и сразу
  // печатает — первые символы исчезали.
  //
  // Это штатный приём React «adjusting state during render»: сравнение с
  // прошлым id, setState того же компонента, перерисовка до отрисовки кадра.
  const [seededFor, setSeededFor] = useState<number | null>(null);
  if (cur && cur.kind === "prompt" && seededFor !== cur.id) {
    setSeededFor(cur.id);
    setValue(cur.defaultValue ?? "");
  }

  const finish = (result: boolean | string | null | void) => {
    if (!cur) return;
    platform().haptic();
    cur.resolve(result);
    setQueue((q) => q.slice(1));
  };

  const onCancel = () => {
    if (!cur) return;
    finish(cur.kind === "alert" ? undefined : cur.kind === "prompt" ? null : false);
  };

  const onConfirm = () => {
    if (!cur) return;
    finish(cur.kind === "alert" ? undefined : cur.kind === "prompt" ? value : true);
  };

  useEscape(!!cur, onCancel);

  // Начальный фокус и его возврат. У опасного действия фокус стоит на «Отмена»:
  // Enter по инерции не должен удалять файл или выключать компьютер.
  const dialogId = cur?.id;
  const dialogKind = cur?.kind;
  const dialogDanger = cur?.danger;
  useEffect(() => {
    if (!dialogId) return;
    const opener = typeof document !== "undefined" ? document.activeElement as HTMLElement | null : null;
    // prompt сам ставит фокус в поле (autoFocus), остальным его ставим тут.
    if (dialogKind !== "prompt") {
      const target = dialogDanger ? cancelRef.current : confirmRef.current;
      target?.focus();
    }
    return () => {
      if (opener && typeof opener.focus === "function" && document.contains(opener)) opener.focus();
    };
  }, [dialogId, dialogKind, dialogDanger]);

  const onSheetKeyDown = (e: ReactKeyboardEvent<HTMLDivElement>) => {
    if (trapTab(sheetRef.current, e)) return;
    if (e.key === "Tab") return;
    if (e.key !== "Enter" || e.shiftKey || e.ctrlKey || e.metaKey || e.altKey) return;
    const tag = (e.target as HTMLElement | null)?.tagName;
    // В поле ввода Enter уже обработан (однострочный prompt) или означает
    // перевод строки; на кнопке его отработает сам браузер.
    if (tag === "INPUT" || tag === "TEXTAREA" || tag === "BUTTON") return;
    // Опасное действие подтверждается только явным нажатием на красную кнопку.
    if (cur?.danger) return;
    e.preventDefault();
    onConfirm();
  };

  if (!cur) return null;

  const confirmLabel = cur.confirmText ?? (cur.kind === "alert" ? t("dialog.ok") : t("dialog.confirm"));
  const cancelLabel = cur.cancelText ?? t("dialog.cancel");

  return (
    // Второй класс, а не замена: обычные модалки продукта тоже .modal-overlay,
    // им слой поднимать нечего. Диалог подтверждения — единственный канал
    // «точно удалить?» в окне exe (нативные confirm там подавлены), поэтому он
    // обязан лежать выше шторок: .snippets-backdrop сидит на 9999 и накрывала
    // бы вопрос об удалении аккаунта, вызванный из шторки запуска агента.
    <div className="modal-overlay dialog-overlay" onClick={onCancel}>
      <div
        ref={sheetRef}
        className="modal-sheet dialog-sheet"
        role={cur.kind === "alert" ? "alertdialog" : "dialog"}
        aria-modal="true"
        onClick={(e) => e.stopPropagation()}
        onKeyDown={onSheetKeyDown}
      >
        {cur.title && <div className="modal-title">{cur.title}</div>}
        <div className="dialog-message">{cur.message}</div>

        {cur.kind === "prompt" && (
          cur.multiline ? (
            <textarea
              key={cur.id}
              className="modal-input"
              rows={3}
              autoFocus
              value={value}
              placeholder={cur.placeholder}
              onChange={(e) => setValue(e.target.value)}
            />
          ) : (
            <input
              key={cur.id}
              className="modal-input"
              autoFocus
              value={value}
              placeholder={cur.placeholder}
              onChange={(e) => setValue(e.target.value)}
              onKeyDown={(e) => { if (e.key === "Enter") onConfirm(); }}
            />
          )
        )}

        <div className="modal-actions">
          {cur.kind !== "alert" && (
            <button ref={cancelRef} className="btn btn-secondary" onClick={onCancel}>{cancelLabel}</button>
          )}
          <button
            ref={confirmRef}
            className={`btn ${cur.danger ? "btn-danger" : "btn-primary"}`}
            onClick={onConfirm}
          >
            {confirmLabel}
          </button>
        </div>
      </div>
    </div>
  );
}

/**
 * Общая обёртка шторки продукта (bottom sheet / оверлей со списком).
 *
 * Зачем: шторки продукта были обычными `div`-оверлеями — без `role="dialog"`,
 * без `aria-modal` и без ловушки фокуса. Экранный диктор не объявлял, что
 * открылось окно, а Tab в окне exe уходил на список ПОД затемнением: человек
 * «печатал» в невидимый экран. Здесь то же поведение, что у DialogHost:
 *
 *  • `role="dialog"` + `aria-modal="true"` + имя (`label`/`labelledBy`);
 *  • Tab и Shift+Tab ходят по кругу внутри шторки;
 *  • при открытии фокус встаёт внутрь (если его туда не поставил autoFocus),
 *    при закрытии возвращается на кнопку, из которой шторку вызвали.
 *
 * Esc/«Назад» НЕ перехватываем: у шторок свои слои (контекст-меню → модалка →
 * сама шторка), и порядок закрытия знает только вызывающий — он вешает
 * `useEscape` сам.
 *
 * `extra` — узлы внутри затемнения, но ВНЕ самой шторки (вложенные модалки и
 * контекст-меню, которым нужен свой слой поверх).
 */
export function SheetShell({
  open, onClose, overlayClassName, className, label, labelledBy,
  role = "dialog", closeOnBackdrop = true, children, extra, onKeyDown,
}: {
  open: boolean;
  onClose: () => void;
  /** Класс затемнения (.folder-sheet-overlay, .snippets-backdrop, …). */
  overlayClassName: string;
  /** Класс самой шторки (.folder-sheet, .snippets-sheet, …). */
  className: string;
  /** Имя окна для диктора, если видимого заголовка в разметке нет. */
  label?: string;
  /** id видимого заголовка внутри шторки — предпочтительнее label. */
  labelledBy?: string;
  role?: "dialog" | "alertdialog";
  /** false — промах мимо шторки не закрывает её (идёт долгая операция). */
  closeOnBackdrop?: boolean;
  children: ReactNode;
  /** Слои поверх шторки: вложенная модалка, контекст-меню. */
  extra?: ReactNode;
  onKeyDown?: (e: ReactKeyboardEvent<HTMLDivElement>) => void;
}) {
  const sheetRef = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    if (!open) return;
    const opener = typeof document !== "undefined"
      ? document.activeElement as HTMLElement | null
      : null;
    // autoFocus внутри разметки срабатывает раньше эффекта — уважаем его выбор.
    const root = sheetRef.current;
    if (root && !root.contains(document.activeElement)) {
      (focusablesIn(root)[0] ?? root).focus();
    }
    return () => {
      if (opener && typeof opener.focus === "function" && document.contains(opener)) opener.focus();
    };
  }, [open]);

  if (!open) return null;

  return (
    <div
      className={overlayClassName}
      onClick={(e) => { if (e.target === e.currentTarget && closeOnBackdrop) onClose(); }}
    >
      <div
        ref={sheetRef}
        className={className}
        role={role}
        aria-modal="true"
        aria-label={labelledBy ? undefined : label}
        aria-labelledby={labelledBy}
        tabIndex={-1}
        onClick={(e) => e.stopPropagation()}
        onKeyDown={(e) => { if (!trapTab(sheetRef.current, e)) onKeyDown?.(e); }}
      >
        {children}
      </div>
      {extra}
    </div>
  );
}
