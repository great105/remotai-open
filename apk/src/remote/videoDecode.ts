/**
 * Декодирование H.264, пришедшего обычным веб-сокетом (WebCodecs).
 *
 * Зачем это отдельно от WebRTC. Замер 23.08: ноутбук владельца (Ubuntu,
 * WebKitGTK) не поднял WebRTC вовсе и работал по JPEG-пути, хотя ДЕКОДЕР
 * H.264 в браузере есть — его отдаёт `VideoDecoder`. WebRTC требует ещё ICE,
 * TURN и поддержки кодека именно в WebRTC-стеке; где чего-то нет, видеокодек
 * был недоступен в принципе.
 *
 * Правило простое: спрашиваем браузер честно (`isConfigSupported`) и говорим
 * компьютеру «умею» только если он ответил «да». Не умеем — остаются
 * частичные JPEG-кадры, они никуда не делись.
 */

/** Профиль, который кодирует openh264 на компьютере: baseline 3.1. */
export const WS_H264_CODEC = "avc1.42E01F";

/**
 * Умеет ли браузер декодировать наш поток. Спрашиваем ОДИН раз за сеанс:
 * ответ не меняется, а вызов асинхронный и не бесплатный.
 */
let supportPromise: Promise<boolean> | null = null;

export function canDecodeWsH264(): Promise<boolean> {
  if (supportPromise) return supportPromise;
  supportPromise = (async () => {
    const Decoder = (globalThis as { VideoDecoder?: typeof VideoDecoder }).VideoDecoder;
    if (!Decoder?.isConfigSupported) return false;
    try {
      const res = await Decoder.isConfigSupported({
        codec: WS_H264_CODEC,
        optimizeForLatency: true,
      });
      return res.supported === true;
    } catch {
      return false; // старый браузер бросает вместо ответа
    }
  })();
  return supportPromise;
}

export interface WsVideoDecoderOptions {
  /** Кадр готов к показу. Забирать сразу: VideoFrame держит память декодера. */
  onFrame: (frame: VideoFrame) => void;
  /** Декодер сломался — поток надо начать заново с ключевого кадра. */
  onError: (err: unknown) => void;
}

/**
 * Обёртка над VideoDecoder, знающая наш формат кадров.
 *
 * Отдельный класс, а не пара строк в экране: правило «до ключевого кадра
 * ничего не декодируем» легко потерять при правке разметки, а потерянное оно
 * даёт зелёную кашу на экране вместо картинки.
 */
export class WsVideoDecoder {
  private decoder: VideoDecoder | null = null;
  private configured = false;
  /** Пока не увидели ключевой кадр, дельты бессмысленны. */
  private sawKeyframe = false;
  private opts: WsVideoDecoderOptions;

  constructor(opts: WsVideoDecoderOptions) {
    this.opts = opts;
  }

  /** Пересобрать декодер под новый размер кадра (компьютер сменил геометрию). */
  configure(): void {
    this.close();
    const Decoder = (globalThis as { VideoDecoder?: typeof VideoDecoder }).VideoDecoder;
    if (!Decoder) return;
    this.decoder = new Decoder({
      output: (frame) => this.opts.onFrame(frame),
      error: (err) => {
        this.sawKeyframe = false;
        this.opts.onError(err);
      },
    });
    this.decoder.configure({ codec: WS_H264_CODEC, optimizeForLatency: true });
    this.configured = true;
    this.sawKeyframe = false;
  }

  /** Готов ли принимать кадры. */
  get ready(): boolean {
    return this.configured && this.decoder?.state === "configured";
  }

  /** Ждёт ли декодер ключевой кадр (значит, его стоит попросить у компьютера). */
  get needsKeyframe(): boolean {
    return !this.sawKeyframe;
  }

  /** Скормить кадр. Возвращает false, если кадр отброшен. */
  decode(data: ArrayBuffer, keyframe: boolean, timestampUs: number): boolean {
    if (!this.ready) return false;
    if (!keyframe && !this.sawKeyframe) return false; // дельта без опоры — мусор
    try {
      this.decoder!.decode(new EncodedVideoChunk({
        type: keyframe ? "key" : "delta",
        timestamp: timestampUs,
        data,
      }));
      if (keyframe) this.sawKeyframe = true;
      return true;
    } catch (err) {
      this.sawKeyframe = false;
      this.opts.onError(err);
      return false;
    }
  }

  close(): void {
    if (this.decoder && this.decoder.state !== "closed") {
      try {
        this.decoder.close();
      } catch { /* уже закрыт */ }
    }
    this.decoder = null;
    this.configured = false;
    this.sawKeyframe = false;
  }
}
