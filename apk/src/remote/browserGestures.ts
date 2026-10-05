/**
 * Правила жестов для браузера на машине.
 *
 * Экран показывает страницу, а не рабочий стол, поэтому палец должен вести себя
 * как палец: тап нажимает, движение тащит, отпускание оставляет инерцию, два
 * пальца меняют масштаб. Всё это считается ЗДЕСЬ, а не в компоненте: правило,
 * которое можно прогнать тестом, не приходится проверять руками на телефоне.
 *
 * Готовый жест протокола (synthesizeScrollGesture) на виртуальном экране не
 * работает — он требует настоящего дисплея и на Xvfb молча ничего не делает.
 * Поэтому инерцию мы досылаем сами прокруткой после того, как палец оторвался.
 */

/** Точка касания в долях кадра (0..1) — в них же считает серверная сторона. */
export interface TouchDot {
  x: number;
  y: number;
}

/** Замер движения пальца: точка и время. */
export interface TouchSample {
  x: number;
  y: number;
  t: number;
}

/** Один шаг инерции: сколько прокрутить и через сколько миллисекунд. */
export interface FlingStep {
  dy: number;
  delay: number;
}

/** Ниже этой скорости (доли кадра в секунду) инерции нет — это был просто тап. */
const FLING_MIN_SPEED = 0.25;
/** Затухание за шаг: 0.86 даёт примерно секунду наката, как в мобильных браузерах. */
const FLING_DECAY = 0.86;
const FLING_STEP_MS = 32;
const FLING_MAX_STEPS = 30;

/**
 * Скорость пальца в момент отрыва — по последним замерам, а не по всему пути:
 * человек может вести медленно, а в конце «стрельнуть», и накат должен слушать
 * именно конец жеста.
 */
export function flingVelocity(samples: TouchSample[]): number {
  if (samples.length < 2) return 0;
  const last = samples[samples.length - 1];
  // Берём отрезок не длиннее 120 мс: более старые точки к броску не относятся.
  // Идём с конца и оставляем ПОСЛЕДНЮЮ точку, попавшую в окно, — взяв первую
  // за его пределами, мы усреднили бы бросок с медленной ползучей частью
  // жеста и получили инерцию втрое слабее настоящей.
  let first = last;
  for (let i = samples.length - 1; i >= 0; i--) {
    if (last.t - samples[i].t > 120) break;
    first = samples[i];
  }
  const dt = last.t - first.t;
  if (dt <= 0) return 0;
  return ((last.y - first.y) / dt) * 1000; // доли кадра в секунду
}

/**
 * Инерция после отрыва пальца: серия шагов прокрутки с затуханием.
 * `frameHeight` — высота кадра в пикселях страницы, чтобы доли превратились в
 * пиксели прокрутки.
 */
export function flingSteps(velocity: number, frameHeight: number): FlingStep[] {
  const speed = Math.abs(velocity);
  if (speed < FLING_MIN_SPEED || frameHeight <= 0) return [];
  const steps: FlingStep[] = [];
  let v = velocity;
  for (let i = 0; i < FLING_MAX_STEPS; i++) {
    v *= FLING_DECAY;
    if (Math.abs(v) < FLING_MIN_SPEED / 2) break;
    // Знак: палец вниз (y растёт) прокручивает страницу вверх.
    const dy = -(v * (FLING_STEP_MS / 1000)) * frameHeight;
    if (Math.abs(dy) < 1) break;
    steps.push({ dy, delay: FLING_STEP_MS });
  }
  return steps;
}

/** Расстояние между двумя касаниями (в долях кадра). */
export function pinchDistance(a: TouchDot, b: TouchDot): number {
  return Math.hypot(a.x - b.x, a.y - b.y);
}

/**
 * Новый масштаб страницы по щипку. Границы те же, что у мобильных браузеров:
 * мельче четверти нечитаемо, крупнее пяти — бессмысленно.
 */
export function pinchScale(base: number, startDistance: number, distance: number): number {
  if (startDistance <= 0.001) return base;
  const next = base * (distance / startDistance);
  return Math.max(0.25, Math.min(5, next));
}

/** Тап или всё-таки протяжка: тап не должен «съезжать» при дрожании пальца. */
export function isTap(start: TouchDot, end: TouchDot, elapsedMs: number): boolean {
  return elapsedMs < 500 && Math.hypot(end.x - start.x, end.y - start.y) < 0.02;
}

/**
 * Свайп от края экрана — «назад» и «вперёд», как в мобильном Chrome.
 *
 * Это главный способ вернуться на телефоне: кнопка «назад» внизу есть, но
 * палец тянется к краю сам. Условия строгие, иначе жест крал бы обычные
 * горизонтальные жесты страницы (карусели, свайп по карточкам): начинать надо
 * У САМОГО края, вести заметно и почти горизонтально.
 *
 * @param startX  где палец коснулся (px от левого края области экрана)
 * @param dx      сколько прошёл по горизонтали (px, вправо положительно)
 * @param dy      сколько прошёл по вертикали
 * @param width   ширина области экрана
 */
export function edgeSwipe(
  startX: number, dx: number, dy: number, width: number,
): "back" | "forward" | null {
  if (width <= 0) return null;
  const edge = Math.max(18, Math.min(48, width * 0.08));
  const far = Math.max(56, width * 0.18);
  // Почти горизонтально: иначе это прокрутка страницы с наклоном.
  if (Math.abs(dx) < far || Math.abs(dx) < Math.abs(dy) * 1.5) return null;
  if (startX <= edge && dx > 0) return "back";
  if (startX >= width - edge && dx < 0) return "forward";
  return null;
}

/** Порог «потянуть вниз, чтобы обновить» (px пальца по экрану клиента). */
export const PULL_REFRESH_THRESHOLD = 90;

/**
 * «Потянуть вниз, чтобы обновить»: доля натяжения 0..1 и признак срабатывания.
 *
 * Жест разрешён ТОЛЬКО когда страница стоит у самого верха — иначе он крал бы
 * обычное листание. Положение страницы приходит с машины отдельным событием
 * (наблюдатель прокрутки), спрашивать его в момент жеста нельзя: через облако
 * это лишние сотни миллисекунд там, где палец ждёт мгновенного отклика.
 */
export function pullRefresh(
  scrollY: number, dy: number, threshold = PULL_REFRESH_THRESHOLD,
): { progress: number; fire: boolean } {
  if (scrollY > 4 || dy <= 0) return { progress: 0, fire: false };
  // Натяжение растёт медленнее пальца: так это ощущается «резинкой», а не
  // мгновенным срывом на первом же миллиметре.
  const progress = Math.min(1, (dy * 0.8) / threshold);
  return { progress, fire: progress >= 1 };
}

/** Масштаб по двойному тапу: приблизить к содержимому и обратно. */
export function doubleTapScale(current: number): number {
  return current > 1.2 ? 1 : 2.5;
}

/**
 * Прятание хрома (верхней строки и нижней панели) при прокрутке — как в
 * настоящем мобильном браузере: человек листает дальше (палец едет вверх) —
 * панели прячутся, листает обратно (палец вниз) — возвращаются.
 *
 * Движение копим в аккумуляторе, иначе каждая дрожь пальца дёргала бы панели
 * туда-сюда. Решение принимается, только когда накопился заметный ход в одну
 * сторону; после решения аккумулятор обнуляется, чтобы следующий перелом был
 * таким же осмысленным.
 *
 * @param acc  накопленное движение с прошлого решения (px клиента)
 * @param dy   движение пальца за это событие (px клиента, вниз положительно)
 * @param threshold  сколько px в одну сторону считается «повёл»
 * @returns новый аккумулятор и решение: true — спрятать, false — показать,
 *          null — оставить как есть.
 */
export function chromeOnDrag(
  acc: number,
  dy: number,
  threshold = 36,
): { acc: number; hidden: boolean | null } {
  const next = acc + dy;
  // Палец вверх (dy отрицательный) — читатель листает дальше вниз по странице,
  // и настоящие браузеры убирают панели с глаз, отдавая место тексту.
  if (next <= -threshold) return { acc: 0, hidden: true };
  // Палец вниз — листает обратно, панели нужны под рукой.
  if (next >= threshold) return { acc: 0, hidden: false };
  return { acc: next, hidden: null };
}
