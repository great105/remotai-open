export type ScrollSource = "touch" | "inertia" | "wheel" | "button";
export interface ScrollTicket {
  readonly identity: string;
  readonly revision: number;
  readonly kind: "touch" | "wheel" | "action";
  readonly sequence: number;
  /**
   * Сколько явных возвратов к live (returnToLive) было к выдаче ticket. Итог
   * действия, выданного ДО возврата, чтение за удалённым каналом не закрепляет
   * (см. mayPin).
   */
  readonly returns: number;
}
export interface ScrollRoute<T> { readonly ticket: ScrollTicket; readonly destination: T }

/** Пауза, после которой следующее событие колеса — новая серия (новое намерение). */
export const WHEEL_SERIES_GAP_MS = 200;
/**
 * Сколько после явного действия глотается хвост прежней серии колеса, идущий
 * без паузы (инерция трекпада). Потолок нужен, чтобы непрерывное колесо
 * человека, начатое прямо в хвост, не терялось бесконечно.
 */
export const WHEEL_TAIL_MAX_MS = 1500;

/** A route belongs to an intent, never to the current distance from an edge.
 * Identity includes the connection, process, parser epoch, modes and geometry.
 * Delayed sends and probes must validate their ticket again before acting.
 */
export class ScrollRouter<T> {
  private revision = 0;
  private touchSequence = 0;
  private wheelSequence = 0;
  private touchIdentity: string | null = null;
  private touchRoute: ScrollRoute<T> | null = null;
  private wheelRoute: ScrollRoute<T> | null = null;
  private wheelAt = -Infinity;
  /** Серию колеса завершило явное действие; её хвост без паузы не маршрутизируется. */
  private wheelTailFrom: number | null = null;
  private returns = 0;

  cancel(): void {
    this.revision++;
    this.touchIdentity = null;
    this.touchRoute = this.wheelRoute = null;
  }
  beginTouch(identity: string): void {
    this.touchSequence++;
    this.touchIdentity = identity;
    this.touchRoute = null;
  }
  action(identity: string): ScrollTicket {
    return { identity, revision: this.revision, kind: "action", sequence: 0, returns: this.returns };
  }
  /**
   * Явное действие человека (кнопки ⇈↑↓⇊, «↑ К команде») — НОВОЕ намерение: оно
   * завершает жест пальцем так же, как новое касание, и серию колеса так же, как
   * пауза. Хвост прежнего жеста — инерция пальца или трекпада и неотправленные
   * фрагменты — больше не маршрутизируется.
   *
   * ⚠ Волна 7 (probe-reading-pin-live, мир C): после серии свайпов ⇊ возвращал
   * к live, а докрутка последнего рывка (source "inertia") шла прежним
   * маршрутом «своя история, закреплено»: поднимала вьюпорт обратно и снова
   * закрепляла источник чтения. ⇊ «не возвращал», а следующий свайп решался
   * закреплением, а не заново (ST-03, I-01).
   *
   * ⚠ Волна 8 (скептик волны 7, S7; probe-reading-pin-live, мир D): то же для
   * колеса. Серия жила до 200 мс после последнего события, и хвост инерции
   * трекпада после ⇊ шёл прежним маршрутом: 435/435 → 426/435, чтение снова
   * закреплено, новый вывод к live не вёл. Сбросить только маршрут мало: хвост
   * решился бы заново — и у низа своей истории снова ушёл бы в неё.
   */
  explicit(identity: string): ScrollTicket {
    this.touchIdentity = null;
    this.touchRoute = null;
    this.wheelRoute = null;
    this.wheelSequence++;
    this.wheelTailFrom = this.wheelAt;
    return this.action(identity);
  }
  /**
   * Явный возврат к live (⇊, «Вернуться к новому»): explicit() плюс отметка.
   * Итог действия, выданного ДО возврата (вердикт пробы кнопки ↑, досылка
   * очереди), больше не закрепляет чтение за удалённым каналом — волна 8,
   * скептик волны 7 [4]. Само действие остаётся действительным: PgUp уже ушла,
   * и её итог — свидетельство о канале.
   */
  returnToLive(identity: string): ScrollTicket {
    this.returns++;
    return this.explicit(identity);
  }
  /** После выдачи ticket явного возврата к live не было: его итог вправе закрепить чтение. */
  mayPin(ticket: ScrollTicket): boolean {
    return ticket.returns === this.returns;
  }
  /** Жест пальцем с этой идентичностью ещё владеет прокруткой (его инерции можно продолжаться). */
  touching(identity: string): boolean {
    return this.touchIdentity === identity;
  }
  valid(ticket: ScrollTicket, identity: string): boolean {
    return ticket.identity === identity && ticket.revision === this.revision
      && (ticket.kind !== "touch" || ticket.sequence === this.touchSequence && this.touchIdentity === identity)
      && (ticket.kind !== "wheel" || ticket.sequence === this.wheelSequence);
  }
  /**
   * Сырое вертикальное событие колеса — КАЖДОЕ, в том числе то, из которого
   * накопитель строк ещё не набрал целой строки. Серия колеса и хвост после
   * явного действия меряются по этим событиям, а не по маршрутизированным.
   * Возвращает true — событие из хвоста серии, завершённой явным действием:
   * его не маршрутизировать и остаток накопителя сбросить.
   *
   * ⚠ Волна 8 (скептик доработки, мир E probe-reading-pin-live): серия мерилась
   * по событиям, дошедшим до route(), а туда приходят лишь те, что набрали
   * строку. Затухающая инерция трекпада (−2…−1 px за кадр) набирает строку раз
   * в ~270 мс — дольше паузы серии, хотя сырые события идут каждые 16 мс. Хвост
   * после ⇊ начинал новую серию, уходил в свою историю и снова закреплял
   * чтение: 435 → 433 из 435, после 30 новых строк 409/436. Та же ошибка
   * дробила обычную медленную серию на «новые намерения».
   */
  wheelActivity(now: number): boolean {
    if (now - this.wheelAt > WHEEL_SERIES_GAP_MS) {
      // Пауза — прежняя серия кончилась: и её маршрут, и глотаемый хвост.
      this.wheelRoute = null;
      this.wheelTailFrom = null;
    } else if (this.wheelTailFrom != null && now - this.wheelTailFrom > WHEEL_TAIL_MAX_MS) {
      this.wheelTailFrom = null;
    }
    this.wheelAt = now;
    return this.wheelTailFrom != null;
  }
  route(source: ScrollSource, identity: string, now: number, choose: () => T): ScrollRoute<T> | null {
    if (source === "button") return { ticket: this.explicit(identity), destination: choose() };
    if (source === "wheel") {
      if (this.wheelTailFrom != null) {
        // Хвост серии, завершённой явным действием: события идут без паузы
        // серии — это всё ещё прежний жест, а не новое намерение.
        if (now - this.wheelAt <= WHEEL_SERIES_GAP_MS && now - this.wheelTailFrom <= WHEEL_TAIL_MAX_MS) {
          this.wheelAt = now;
          return null;
        }
        this.wheelTailFrom = null;
      }
      if (!this.wheelRoute || now - this.wheelAt > WHEEL_SERIES_GAP_MS || !this.valid(this.wheelRoute.ticket, identity)) {
        this.wheelRoute = {
          ticket: { ...this.action(identity), kind: "wheel", sequence: ++this.wheelSequence },
          destination: choose(),
        };
      }
      this.wheelAt = now;
      return this.wheelRoute;
    }
    // A cancelled touch or late inertia frame cannot start a new gesture.
    if (this.touchIdentity !== identity) return null;
    if (!this.touchRoute) this.touchRoute = {
      ticket: { ...this.action(identity), kind: "touch", sequence: this.touchSequence },
      destination: choose(),
    };
    return this.touchRoute;
  }
}
