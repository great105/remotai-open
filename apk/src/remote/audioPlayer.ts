/**
 * Воспроизведение системного звука удалённого компьютера.
 *
 * Компьютер шлёт куски по 20 мс: PCM s16 моно 24 кГц (см. internal/audio).
 * Здесь они складываются в очередь и планируются на общий таймлайн
 * AudioContext — без этого куски играются «встык как получилось» и слышны
 * щелчки на каждой границе.
 *
 * Джиттер-буфер маленький намеренно: звук отстаёт от картинки настолько же,
 * насколько отстаёт сеть, и добавлять к 300 мс маршрута ещё полсекунды ради
 * гладкости — плохая сделка. Опоздавший кусок пропускается: догонять звук
 * растяжением времени мы не умеем, а копить его — значит уезжать всё дальше
 * от картинки.
 */

/** Сколько звука держим впереди курсора воспроизведения. */
const TARGET_BUFFER_SEC = 0.12;
/** Если очередь убежала дальше — сбрасываем: значит был долгий провал сети. */
const MAX_BUFFER_SEC = 0.6;

export class RemoteAudioPlayer {
  private ctx: AudioContext | null = null;
  private gain: GainNode | null = null;
  private nextAt = 0;
  private rate: number;
  private muted = false;

  constructor(sampleRate = 24000) {
    this.rate = sampleRate;
  }

  /** Заводится по жесту человека: до этого браузер не даст звучать. */
  async start(sampleRate?: number): Promise<void> {
    if (sampleRate) this.rate = sampleRate;
    if (!this.ctx) {
      const Ctor = (window.AudioContext || (window as unknown as { webkitAudioContext: typeof AudioContext }).webkitAudioContext);
      this.ctx = new Ctor({ sampleRate: this.rate, latencyHint: "interactive" });
      this.gain = this.ctx.createGain();
      this.gain.connect(this.ctx.destination);
    }
    if (this.ctx.state === "suspended") await this.ctx.resume();
    this.nextAt = 0;
  }

  get running(): boolean {
    return !!this.ctx && this.ctx.state === "running";
  }

  setMuted(muted: boolean): void {
    this.muted = muted;
    if (this.gain) this.gain.gain.value = muted ? 0 : 1;
  }

  /** Принять кусок PCM s16 моно с компьютера. */
  push(pcm: ArrayBuffer): void {
    const ctx = this.ctx;
    const gain = this.gain;
    if (!ctx || !gain || this.muted) return;

    const samples = new Int16Array(pcm);
    if (samples.length === 0) return;

    const buffer = ctx.createBuffer(1, samples.length, this.rate);
    const channel = buffer.getChannelData(0);
    for (let i = 0; i < samples.length; i++) channel[i] = samples[i] / 32768;

    const now = ctx.currentTime;
    // Первый кусок и восстановление после провала: начинаем с небольшим
    // запасом, иначе первые куски играются в прошлое и просто пропадают.
    if (this.nextAt < now + 0.005) this.nextAt = now + TARGET_BUFFER_SEC;
    // Убежали далеко вперёд — звук отстал от картинки сильнее, чем терпимо.
    if (this.nextAt - now > MAX_BUFFER_SEC) this.nextAt = now + TARGET_BUFFER_SEC;

    const src = ctx.createBufferSource();
    src.buffer = buffer;
    src.connect(gain);
    src.start(this.nextAt);
    this.nextAt += buffer.duration;
  }

  /** Насколько звук сейчас отстаёт от реального времени, в миллисекундах. */
  bufferedMs(): number {
    if (!this.ctx) return 0;
    return Math.max(0, Math.round((this.nextAt - this.ctx.currentTime) * 1000));
  }

  async stop(): Promise<void> {
    const ctx = this.ctx;
    this.ctx = null;
    this.gain = null;
    this.nextAt = 0;
    if (ctx) {
      try {
        await ctx.close();
      } catch { /* уже закрыт */ }
    }
  }
}
