import { useRef, useState } from "react";
import type { ButtonHTMLAttributes, PointerEvent as ReactPointerEvent } from "react";
import { haptic, hapticSuccess } from "../telegram";
import { t } from "../i18n";

interface Props extends Omit<ButtonHTMLAttributes<HTMLButtonElement>, "onClick"> {
  onConfirm: () => void;
  holdMs?: number;
}

/** Смещение пальца, после которого жест считается прокруткой, а не удержанием. */
const MOVE_SLOP_PX = 10;

export function HoldButton({ onConfirm, holdMs = 900, children, className = "", disabled, ...rest }: Props) {
  const timerRef = useRef<number | null>(null);
  const startPointRef = useRef<{ x: number; y: number } | null>(null);
  const [holding, setHolding] = useState(false);

  const cancel = () => {
    startPointRef.current = null;
    if (timerRef.current) {
      window.clearTimeout(timerRef.current);
      timerRef.current = null;
    }
    setHolding(false);
  };

  const start = (e: ReactPointerEvent<HTMLButtonElement>) => {
    if (disabled || timerRef.current) return;
    startPointRef.current = { x: e.clientX, y: e.clientY };
    haptic("medium");
    setHolding(true);
    timerRef.current = window.setTimeout(() => {
      timerRef.current = null;
      setHolding(false);
      hapticSuccess();
      onConfirm();
    }, holdMs);
  };

  /**
   * Палец поехал по кнопке — удержание отменяем.
   *
   * Почему без этого кнопка стреляла сама: таймер доходит до конца БЕЗ отрыва
   * пальца, а из-за неявного pointer capture на касании до кнопки не долетает
   * ни `pointerleave`, ни `pointerout`; браузер не шлёт и `pointercancel`,
   * потому что жест ему не отдан. Итог — протаскивание пальца по «Удалить» или
   * по «✖» в списке процессов при попытке пролистать страницу выполняло
   * действие. Теперь смещение больше MOVE_SLOP_PX гасит таймер здесь, а
   * `touch-action: pan-y` в CSS возвращает странице саму прокрутку.
   */
  const move = (e: ReactPointerEvent<HTMLButtonElement>) => {
    const from = startPointRef.current;
    if (!from || !timerRef.current) return;
    if (Math.abs(e.clientX - from.x) > MOVE_SLOP_PX || Math.abs(e.clientY - from.y) > MOVE_SLOP_PX) cancel();
  };

  return (
    <button
      {...rest}
      type={rest.type || "button"}
      className={`${className} hold-button${holding ? " holding" : ""}`}
      disabled={disabled}
      onPointerDown={start}
      onPointerMove={move}
      onPointerUp={cancel}
      onPointerLeave={cancel}
      onPointerCancel={cancel}
      onClick={(e) => e.preventDefault()}
    >
      {holding ? <span className="hold-button-content">{t("generic.hold")}</span> : children}
    </button>
  );
}
