import { useEffect, type RefObject } from "react";

/**
 * Горизонтальный ряд, который слушается КОЛЕСА МЫШИ.
 *
 * Жалоба владельца 01.09.2026 про ленту «Быстрый запуск» в окне на ПК: «на
 * компе не скроллится избранное». Пальцем ряд листается, а мышью — нет, и это
 * не дефект конкретной ленты: браузер вертикальным колесом горизонтальный
 * контейнер не двигает вовсе, а полосу прокрутки ленты дизайн прячет
 * (`scrollbar-width: none`). То есть на компьютере половина закреплённых папок
 * недостижима, и взяться за неё нечем.
 *
 * Правила, каждое из которых оплачено отдельной поломкой в чужих реализациях
 * этого приёма:
 *
 *  • `passive: false` обязателен. React вешает `onWheel` пассивно, и
 *    `preventDefault()` в нём молча игнорируется: лента едет вбок И страница
 *    вниз одновременно.
 *  • На краю ленты событие НЕ перехватываем. Иначе колесо «залипает»: человек
 *    крутит над лентой, лента уже докручена, а список терминалов под ней не
 *    двигается — выглядит как зависшая страница.
 *  • Горизонтальные жесты (трекпад, мышь с боковым колесом) не трогаем: они и
 *    так работают, а вмешательство удваивает шаг.
 */
export function useHorizontalWheel(ref: RefObject<HTMLElement | null>, deps: unknown[] = []) {
  useEffect(() => {
    const el = ref.current;
    // ⚠ Лента появляется ПОЗЖЕ первого рендера: список папок приезжает с ПК, а
    // до него `ref.current` пуст. Эффект с пустыми зависимостями отрабатывал
    // ровно один раз — на пустоте — и колесо не работало никогда (замер ловил
    // «0 → 0 px»). Поэтому вызывающий передаёт сюда то, от чего лента зависит.
    if (!el) return;

    const onWheel = (e: WheelEvent) => {
      if (Math.abs(e.deltaX) > Math.abs(e.deltaY)) return; // уже горизонтальный жест
      if (e.deltaY === 0) return;
      const max = el.scrollWidth - el.clientWidth;
      if (max <= 1) return; // прокручивать нечего — ряд влез целиком
      const atStart = el.scrollLeft <= 0;
      const atEnd = el.scrollLeft >= max - 1;
      if ((e.deltaY < 0 && atStart) || (e.deltaY > 0 && atEnd)) return; // край — отдаём странице
      e.preventDefault();
      el.scrollLeft = Math.max(0, Math.min(max, el.scrollLeft + e.deltaY));
    };

    el.addEventListener("wheel", onWheel, { passive: false });
    return () => el.removeEventListener("wheel", onWheel);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [ref, ...deps]);
}
