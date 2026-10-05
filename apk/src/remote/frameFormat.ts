/**
 * Разбор бинарного кадра экрана.
 *
 * Кадр приходит в одном из двух видов, и различаются они первым байтом после
 * номера: у JPEG там всегда SOI (0xFF), у частичного кадра — маркер 0x01.
 *
 *   полный:    [seq:4 BE][jpeg…]
 *   частичный: [seq:4 BE][0x01][count:1]{[x:2][y:2][w:2][h:2][len:4][jpeg…]}×count
 *
 * Частичные кадры компьютер шлёт только тому зрителю, который сам объявил их
 * поддержку (`{t:"client", tiles:true}`): старая сборка ждёт в бинарном
 * сообщении голый JPEG и на новый формат показала бы пустой экран.
 *
 * Разбор живёт отдельно от экрана намеренно: правило проверяется тестом в node,
 * без React, DOM и живого компьютера на том конце.
 */

export interface FrameRegion {
  x: number;
  y: number;
  w: number;
  h: number;
  data: ArrayBuffer;
}

export type ParsedFrame =
  | { seq: number; kind: "full"; data: ArrayBuffer }
  | { seq: number; kind: "regions"; regions: FrameRegion[] }
  | { seq: number; kind: "h264"; keyframe: boolean; data: ArrayBuffer }
  | { seq: number; kind: "audio"; data: ArrayBuffer };

/** Маркер частичного кадра. */
export const FRAME_KIND_REGIONS = 0x01;
/** Маркер видеокадра H.264 (Annex-B) — путь для браузеров без WebRTC. */
export const FRAME_KIND_H264 = 0x02;
/** Бит «ключевой кадр» во флагах видеокадра. */
export const VIDEO_FLAG_KEYFRAME = 0x01;
/** Маркер куска системного звука (PCM s16 моно). */
export const FRAME_KIND_AUDIO = 0x03;

/**
 * Разбирает кадр. null — мусор на проводе (обрезанное сообщение, битые
 * длины): такой кадр молча пропускается, рисовать по нему нечего.
 */
export function parseScreenFrame(buffer: ArrayBuffer): ParsedFrame | null {
  if (buffer.byteLength < 5) return null;
  const view = new DataView(buffer);
  const seq = view.getUint32(0);

  const kind = view.getUint8(4);
  if (kind === FRAME_KIND_AUDIO) {
    if (buffer.byteLength <= 5) return null;
    return { seq, kind: "audio", data: buffer.slice(5) };
  }
  if (kind === FRAME_KIND_H264) {
    if (buffer.byteLength < 7) return null;
    const flags = view.getUint8(5);
    return {
      seq,
      kind: "h264",
      keyframe: (flags & VIDEO_FLAG_KEYFRAME) !== 0,
      data: buffer.slice(6),
    };
  }
  if (kind !== FRAME_KIND_REGIONS) {
    return { seq, kind: "full", data: buffer.slice(4) };
  }
  if (buffer.byteLength < 6) return null;

  const count = view.getUint8(5);
  if (count === 0) return null;

  const regions: FrameRegion[] = [];
  let off = 6;
  for (let i = 0; i < count; i++) {
    if (off + 12 > buffer.byteLength) return null;
    const x = view.getUint16(off);
    const y = view.getUint16(off + 2);
    const w = view.getUint16(off + 4);
    const h = view.getUint16(off + 6);
    const len = view.getUint32(off + 8);
    off += 12;
    if (len === 0 || off + len > buffer.byteLength) return null;
    regions.push({ x, y, w, h, data: buffer.slice(off, off + len) });
    off += len;
  }
  return { seq, kind: "regions", regions };
}

/** Минимум от canvas-контекста, который нужен наложению кусков. */
export interface RegionPainter {
  drawImage(image: CanvasImageSource, dx: number, dy: number): void;
}

/** Раскодированный кусок: то, что вернул createImageBitmap. */
export interface DecodedRegion {
  bitmap: CanvasImageSource & { close?: () => void };
  x: number;
  y: number;
}

/**
 * Кладёт куски на холст в порядке прихода и закрывает битмапы.
 *
 * Вынесено из экрана намеренно: сдвиг координат здесь означает разъехавшуюся
 * картинку у человека, а поймать его глазами трудно — куски выглядят
 * правдоподобно на любом месте. Тест проверяет именно координаты.
 */
export function paintRegions(ctx: RegionPainter, parts: DecodedRegion[]): void {
  for (const part of parts) {
    ctx.drawImage(part.bitmap, part.x, part.y);
    part.bitmap.close?.();
  }
}
