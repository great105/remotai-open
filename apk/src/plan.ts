// Состояние тарифа Remotai для показа в интерфейсе.
//
// Чистая функция, вынесенная из экрана, чтобы правила показа тарифа можно было
// проверить тестом без React. Источник данных — CloudMe (/v1/me с релея).
//
// Порядок важен и повторяет серверный EffectiveTier:
//   founder → вечный Pro (перебивает всё, даже бету, чтобы показать «навсегда»);
//   beta    → пока идёт бесплатная бета, все на Pro;
//   trial   → пробный Pro с обратным отсчётом;
//   pro/team/fleet → оплаченный Pro или Флит;
//   иначе   → Free.
//
// Про id полок: релей отдаёт id старшей полки как `fleet`
// (tgcontrol-relay/internal/server/pricing.go), а лицензия агента и старые
// ответы /v1/me зовут её `team`. Клиент ждал только `team` — и оплативший Флит
// человек видел бы «Локально» (аудит ИА 02.09.2026, P0-8). Здесь оба id — одна
// и та же полка «Флит».

export interface PlanMe {
  tier: string;
  effective_tier?: string;
  founder?: boolean;
  beta?: boolean;
  billing_enabled?: boolean;
  trial_days_left?: number;
  devices_count: number;
  max_devices: number;
  /**
   * Работает ли удалённый доступ прямо сейчас. Считает релей (db.CloudAccess) —
   * клиент не выводит это сам из тарифа, пробы и режима беты, иначе правило
   * разъедется между сервером и экраном.
   */
  cloud_allowed?: boolean;
}

export type PlanKind = "founder" | "beta" | "trial" | "pro" | "free";

export interface PlanState {
  kind: PlanKind;
  /** Имя полки, как на витрине: «Локально», «Про» или «Флит». */
  planName: "Локально" | "Про" | "Флит";
  /** Показывать ли кнопку оформления подписки. */
  showUpgrade: boolean;
  /**
   * Показывать ли ход «оставить заявку» ВМЕСТО оплаты.
   *
   * Кассы в продукте ещё нет (`POST /api/license/activate` = 501), и при
   * выключенном биллинге кнопки не было вовсе: человек, у которого проба
   * заканчивается, не видел ни цены, ни способа заплатить — молчание читается
   * как «мне здесь ничего не продают» (аудит онбординга 30.08.2026). Заявка —
   * честный ход: её мы действительно можем выполнить руками.
   */
  showRequest: boolean;
  /** Дней пробного Про осталось (только при kind === "trial"). */
  trialDaysLeft: number;
  devices: { used: number; max: number; percent: number };
  /**
   * Работает ли удалённый доступ. false — человек на полке «Локально»: дома всё
   * работает как раньше, выключено только расстояние. Это НЕ «доступ заблокирован».
   */
  cloudAllowed: boolean;
}

export function planState(me: PlanMe): PlanState {
  const eff = me.effective_tier || me.tier;
  const fleet = eff === "team" || eff === "fleet";
  const paid = eff === "pro" || fleet;
  const trialLeft = me.trial_days_left && me.trial_days_left > 0 ? me.trial_days_left : 0;

  let kind: PlanKind;
  if (me.founder) kind = "founder";
  else if (me.beta) kind = "beta";
  else if (trialLeft > 0) kind = "trial";
  else if (paid) kind = "pro";
  else kind = "free";

  // Оформить Pro предлагаем только тому, кто реально ещё не платит: касса
  // включена, не founder, базовый (сырой) tier всё ещё free. При триале tier
  // остаётся free (Pro даёт только trial_end), поэтому пробнику кнопку тоже
  // показываем — оформить до конца пробы. Платящий (tier=pro) и founder не видят.
  const showUpgrade = !!me.billing_enabled && !me.founder && me.tier === "free";
  // Тот же круг людей, но при выключенной кассе: вместо оплаты — заявка.
  // Бете не предлагаем ничего: там Про открыт всем и платить пока не за что.
  const showRequest = !me.billing_enabled && !me.founder && !me.beta && me.tier === "free";

  const max = me.max_devices || 0;
  const used = me.devices_count || 0;
  const percent = max > 0 ? Math.min(100, Math.round((used / max) * 100)) : 0;

  // Имя полки — по виду, а не по сырому tier: founder/beta/trial дают Про,
  // даже если релей не проставил effective_tier явно. «Флит» показываем только
  // тому, кто реально на нём: это другая полка, а не «Про побольше».
  const planName: PlanState["planName"] =
    kind === "free" ? "Локально" : fleet ? "Флит" : "Про";

  // Доверяем серверу, если он ответил; иначе выводим сами — но так же, как он.
  const cloudAllowed = me.cloud_allowed ?? kind !== "free";

  return {
    kind,
    planName,
    showUpgrade,
    showRequest,
    trialDaysLeft: trialLeft,
    devices: { used, max, percent },
    cloudAllowed,
  };
}

/** Что сказать про пробу на ГЛАВНОЙ (см. trialNotice). */
export interface TrialNotice {
  /** Показывать ли плашку вообще. */
  show: boolean;
  /** Сколько дней осталось. */
  days: number;
  /** Последний день — тон другой: это уже не напоминание, а срок. */
  urgent: boolean;
  /** Проба уже кончилась: удалённый доступ выключен прямо сейчас. */
  ended: boolean;
}

/**
 * Предупреждение о конце пробы для главного экрана.
 *
 * Аудит путей 29.08.2026: о конце пробы говорил только экран «Агенты», куда в
 * конце пробы никто не заходит, и день 29 отличался от дня 25 одной цифрой.
 * Человек узнавал о конце пробы в тот момент, когда переставало работать
 * удалённое — то есть худшим из возможных способов.
 *
 * Порог — семь дней: раньше плашка на главной каждый день целый месяц
 * превращается в мебель, которую перестают видеть к тому дню, когда она важна.
 */
export function trialNotice(state: PlanState): TrialNotice {
  const days = state.trialDaysLeft;
  // Проба кончилась, и человек уже на «Локально»: удалённый доступ выключен.
  //
  // ⚠ До 01.09.2026 в этом состоянии главная МОЛЧАЛА: баннер жил только
  // последние семь дней пробы и исчезал ровно тогда, когда доступ пропадал.
  // То есть в момент, когда человеку впервые понадобилось заплатить, ход к
  // оплате с главной исчезал. Отказ гейта при этом говорит «оформите Про» —
  // но не говорит где.
  const ended = state.kind === "free";
  const show = ended || (state.kind === "trial" && days > 0 && days <= 7);
  return { show, days, urgent: ended || (show && days <= 1), ended };
}

/**
 * Где на главной рисовать плашку про пробу: наверху или внизу.
 *
 * Замер 06.09.2026 (`probe-trial-over-cta`): в день 31 единственный ход к
 * оплате с главной — кнопка плашки — стоял на 750–794 px телефона 390×844, а
 * нижняя панель начинается на 768. Нажатие в центр кнопки попадало в
 * НАВИГАЦИЮ: в момент, когда человек впервые обязан заплатить, заплатить было
 * физически нечем. Та же болезнь, что у чеклиста «Первые шаги» (2.64.0), и
 * лечится так же — местом, а не видимостью.
 *
 * Наверх плашка идёт только когда это уже срок, а не напоминание: проба
 * кончилась или идёт последний день. «Осталось 5 дней» остаётся внизу, чтобы
 * не занимать первый экран у того, кто просто работает.
 */
export function trialBannerOnTop(notice: TrialNotice | null): boolean {
  return !!notice?.show && notice.urgent;
}

/** Строка про пробу в ЛИЧНОМ КАБИНЕТЕ: сколько осталось и до какого числа. */
export interface TrialLine {
  days: number;
  /** Конец пробы как «06.10.2026»; пустая строка, если релей дату не прислал. */
  date: string;
}

/**
 * Живой прогон нового аккаунта 06.09.2026: /v1/me отдавал trial_days_left=29 и
 * trial_end, а кабинет под «Сейчас у вас» писал только «Про» — человек,
 * решающий, платить ли, не видел ни того, что это проба, ни когда она кончится.
 * Дата считается здесь, а не в экране, чтобы правило проверялось без React.
 * Оплаченный срок важнее пробы: кто уже заплатил, видит «Оплачено до», не пробу.
 */
export function trialLine(me: PlanMe & { trial_end?: string }, paidUntil: string): TrialLine | null {
  const state = planState(me);
  if (state.kind !== "trial" || paidUntil) return null;
  const end = me.trial_end ? new Date(me.trial_end) : null;
  const date = end && !Number.isNaN(end.getTime()) ? end.toLocaleDateString("ru-RU") : "";
  return { days: state.trialDaysLeft, date };
}
