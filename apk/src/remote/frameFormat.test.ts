import { describe, expect, it } from "vitest";

import { FRAME_KIND_AUDIO, FRAME_KIND_H264, FRAME_KIND_REGIONS, VIDEO_FLAG_KEYFRAME, paintRegions, parseScreenFrame } from "./frameFormat";

function fullFrame(seq: number, jpegBytes: number[]): ArrayBuffer {
  const out = new Uint8Array(4 + jpegBytes.length);
  new DataView(out.buffer).setUint32(0, seq);
  out.set(jpegBytes, 4);
  return out.buffer;
}

function regionFrame(seq: number, regions: { x: number; y: number; w: number; h: number; bytes: number[] }[]): ArrayBuffer {
  const size = 6 + regions.reduce((n, r) => n + 12 + r.bytes.length, 0);
  const out = new Uint8Array(size);
  const view = new DataView(out.buffer);
  view.setUint32(0, seq);
  view.setUint8(4, FRAME_KIND_REGIONS);
  view.setUint8(5, regions.length);
  let off = 6;
  for (const r of regions) {
    view.setUint16(off, r.x);
    view.setUint16(off + 2, r.y);
    view.setUint16(off + 4, r.w);
    view.setUint16(off + 6, r.h);
    view.setUint32(off + 8, r.bytes.length);
    out.set(r.bytes, off + 12);
    off += 12 + r.bytes.length;
  }
  return out.buffer;
}

describe("parseScreenFrame", () => {
  it("читает полный кадр (JPEG начинается с SOI)", () => {
    const f = parseScreenFrame(fullFrame(5, [0xff, 0xd8, 0xff, 0xe0, 1, 2, 3]));
    expect(f?.kind).toBe("full");
    expect(f?.seq).toBe(5);
    if (f?.kind === "full") expect(new Uint8Array(f.data)[0]).toBe(0xff);
  });

  it("читает частичный кадр с координатами кусков", () => {
    const f = parseScreenFrame(regionFrame(9, [
      { x: 0, y: 0, w: 128, h: 128, bytes: [0xff, 0xd8, 1] },
      { x: 256, y: 128, w: 256, h: 128, bytes: [0xff, 0xd8, 2, 3] },
    ]));
    expect(f?.kind).toBe("regions");
    expect(f?.seq).toBe(9);
    if (f?.kind !== "regions") return;
    expect(f.regions).toHaveLength(2);
    expect(f.regions[1]).toMatchObject({ x: 256, y: 128, w: 256, h: 128 });
    expect(new Uint8Array(f.regions[1].data)).toEqual(new Uint8Array([0xff, 0xd8, 2, 3]));
  });

  it("не путает форматы: у полного кадра после номера всегда 0xFF", () => {
    // Если бы маркер частичного кадра совпал с SOI, зритель разбирал бы JPEG
    // как список областей и показывал бы пустой экран.
    const f = parseScreenFrame(fullFrame(1, [0xff, 0xd8, 0, 1, 0, 0]));
    expect(f?.kind).toBe("full");
  });

  it("отбрасывает обрезанное сообщение вместо мусора на экране", () => {
    const truncated = regionFrame(3, [{ x: 0, y: 0, w: 64, h: 64, bytes: [0xff, 0xd8, 7, 7] }]).slice(0, 10);
    expect(parseScreenFrame(truncated)).toBeNull();
    expect(parseScreenFrame(new ArrayBuffer(2))).toBeNull();
  });

  it("отбрасывает кадр с длиной куска больше самого кадра", () => {
    const buf = regionFrame(4, [{ x: 0, y: 0, w: 64, h: 64, bytes: [0xff, 0xd8, 1, 2] }]);
    new DataView(buf).setUint32(6 + 8, 9999); // солгали про длину
    expect(parseScreenFrame(buf)).toBeNull();
  });
});

describe("paintRegions", () => {
  it("кладёт каждый кусок на его место и освобождает битмап", () => {
    const calls: Array<{ id: string; x: number; y: number }> = [];
    const closed: string[] = [];
    const bitmap = (id: string) => ({
      id,
      close: () => closed.push(id),
    }) as unknown as CanvasImageSource & { close: () => void; id: string };

    const ctx = {
      drawImage: (img: CanvasImageSource, dx: number, dy: number) => {
        calls.push({ id: (img as unknown as { id: string }).id, x: dx, y: dy });
      },
    };

    paintRegions(ctx, [
      { bitmap: bitmap("верх"), x: 0, y: 0 },
      { bitmap: bitmap("низ"), x: 256, y: 640 },
    ]);

    expect(calls).toEqual([
      { id: "верх", x: 0, y: 0 },
      { id: "низ", x: 256, y: 640 },
    ]);
    // Незакрытые ImageBitmap текут памятью браузера — на длинном сеансе это
    // сотни мегабайт.
    expect(closed).toEqual(["верх", "низ"]);
  });
});

describe("parseScreenFrame: видеокадр", () => {
  function videoFrame(seq: number, keyframe: boolean, nal: number[]): ArrayBuffer {
    const out = new Uint8Array(6 + nal.length);
    const view = new DataView(out.buffer);
    view.setUint32(0, seq);
    view.setUint8(4, FRAME_KIND_H264);
    view.setUint8(5, keyframe ? VIDEO_FLAG_KEYFRAME : 0);
    out.set(nal, 6);
    return out.buffer;
  }

  it("узнаёт ключевой кадр и дельту", () => {
    const key = parseScreenFrame(videoFrame(11, true, [0, 0, 0, 1, 0x67]));
    expect(key).toMatchObject({ kind: "h264", keyframe: true, seq: 11 });

    const delta = parseScreenFrame(videoFrame(12, false, [0, 0, 0, 1, 0x41]));
    expect(delta).toMatchObject({ kind: "h264", keyframe: false, seq: 12 });
  });

  it("отдаёт Annex-B без нашего заголовка", () => {
    const f = parseScreenFrame(videoFrame(1, true, [0, 0, 0, 1, 0x67, 0x42]));
    if (f?.kind !== "h264") throw new Error("не видеокадр");
    expect(new Uint8Array(f.data)).toEqual(new Uint8Array([0, 0, 0, 1, 0x67, 0x42]));
  });

  it("три формата не путаются между собой", () => {
    // Один байт после номера различает JPEG (0xFF), куски (0x01) и видео (0x02).
    expect(parseScreenFrame(fullFrame(1, [0xff, 0xd8, 1]))?.kind).toBe("full");
    expect(parseScreenFrame(regionFrame(2, [{ x: 0, y: 0, w: 8, h: 8, bytes: [0xff, 0xd8] }]))?.kind).toBe("regions");
    expect(parseScreenFrame(videoFrame(3, true, [0, 0, 0, 1]))?.kind).toBe("h264");
  });

  it("отбрасывает видеокадр без полезной нагрузки", () => {
    const empty = new Uint8Array(6);
    new DataView(empty.buffer).setUint8(4, FRAME_KIND_H264);
    expect(parseScreenFrame(empty.buffer)).toBeNull();
  });
});

describe("parseScreenFrame: звук", () => {
  it("узнаёт кусок звука и отдаёт чистый PCM", () => {
    const out = new Uint8Array(5 + 4);
    new DataView(out.buffer).setUint8(4, FRAME_KIND_AUDIO);
    out.set([1, 2, 3, 4], 5);
    const f = parseScreenFrame(out.buffer);
    expect(f?.kind).toBe("audio");
    if (f?.kind !== "audio") return;
    expect(new Uint8Array(f.data)).toEqual(new Uint8Array([1, 2, 3, 4]));
  });

  it("пустой звуковой кадр отбрасывается", () => {
    const empty = new Uint8Array(5);
    new DataView(empty.buffer).setUint8(4, FRAME_KIND_AUDIO);
    expect(parseScreenFrame(empty.buffer)).toBeNull();
  });

  it("звук не путается с картинкой и видео", () => {
    const audio = new Uint8Array(5 + 2);
    new DataView(audio.buffer).setUint8(4, FRAME_KIND_AUDIO);
    expect(parseScreenFrame(audio.buffer)?.kind).toBe("audio");
    expect(parseScreenFrame(fullFrame(1, [0xff, 0xd8, 1]))?.kind).toBe("full");
  });
});
