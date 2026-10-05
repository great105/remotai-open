package web

import (
	"bytes"
	"image"
	"image/jpeg"
	"testing"
)

// fakeEncoder записывает, каким путём ушёл кадр (RGBA-конверсия или прямые
// I420-плоскости), и возвращает заготовленный битстрим.
type fakeEncoder struct {
	w, h                       int
	rgbaCalls, i420Calls       int
	gotY, gotU, gotV           []byte
	gotYStride, gotCStride     int
	gotForceRGBA, gotForceI420 bool
}

func (f *fakeEncoder) EncodeRGBA(_ image.Image, force bool) ([]byte, bool, error) {
	f.rgbaCalls++
	f.gotForceRGBA = force
	return []byte{0, 0, 1, 5}, true, nil
}

func (f *fakeEncoder) EncodeI420(y, u, v []byte, yStride, cStride int, force bool) ([]byte, bool, error) {
	f.i420Calls++
	f.gotY, f.gotU, f.gotV = y, u, v
	f.gotYStride, f.gotCStride = yStride, cStride
	f.gotForceI420 = force
	return []byte{0, 0, 1, 7}, true, nil
}

func (f *fakeEncoder) Width() int  { return f.w }
func (f *fakeEncoder) Height() int { return f.h }
func (f *fakeEncoder) Close()      {}

// JPEG 4:2:0 ровно размера энкодера — плоскости уходят в EncodeI420 как есть
// (Y, Cb→U, Cr→V, страйды сохраняются), RGBA-конверсии нет.
func TestEncodeFrameYCbCrFastPath(t *testing.T) {
	const w, h = 96, 64
	img, err := jpeg.Decode(bytes.NewReader(testJPEG(t, w, h)))
	if err != nil {
		t.Fatalf("jpeg decode: %v", err)
	}
	yc, ok := img.(*image.YCbCr)
	if !ok || yc.SubsampleRatio != image.YCbCrSubsampleRatio420 {
		t.Fatalf("synthetic jpeg decoded to %T, want *image.YCbCr 4:2:0", img)
	}

	enc := &fakeEncoder{w: w, h: h}
	nal, key, err := encodeFrame(enc, img, true)
	if err != nil {
		t.Fatalf("encodeFrame: %v", err)
	}
	if enc.i420Calls != 1 || enc.rgbaCalls != 0 {
		t.Fatalf("i420=%d rgba=%d, want fast path (i420=1 rgba=0)", enc.i420Calls, enc.rgbaCalls)
	}
	if &enc.gotY[0] != &yc.Y[0] || &enc.gotU[0] != &yc.Cb[0] || &enc.gotV[0] != &yc.Cr[0] {
		t.Fatalf("planes must pass through verbatim: Y Cb Cr → Y U V без копий")
	}
	if enc.gotYStride != yc.YStride || enc.gotCStride != yc.CStride {
		t.Fatalf("strides = %d/%d, want %d/%d", enc.gotYStride, enc.gotCStride, yc.YStride, yc.CStride)
	}
	if !enc.gotForceI420 {
		t.Fatalf("force flag lost on the fast path")
	}
	if !key || len(nal) == 0 {
		t.Fatalf("key=%v nal=%d, want keyframe bitstream", key, len(nal))
	}
}

// Иной chroma-формат (4:4:4) или размер кадра ≠ размеру энкодера (разовый
// рассинхрон при смене viewport) — обычный путь RGBA→I420, там же и
// масштабирование.
func TestEncodeFrameFallback(t *testing.T) {
	// 4:4:4 — fast path не положен.
	yc444 := image.NewYCbCr(image.Rect(0, 0, 96, 64), image.YCbCrSubsampleRatio444)
	enc := &fakeEncoder{w: 96, h: 64}
	if _, _, err := encodeFrame(enc, yc444, false); err != nil {
		t.Fatalf("encodeFrame 444: %v", err)
	}
	if enc.rgbaCalls != 1 || enc.i420Calls != 0 {
		t.Fatalf("4:4:4: rgba=%d i420=%d, want fallback (rgba=1 i420=0)", enc.rgbaCalls, enc.i420Calls)
	}

	// Размер не совпал — энкодер ещё старый, кадр уже новый: EncodeRGBA
	// отмасштабирует, а следующая итерация пересоздаст энкодер.
	img, err := jpeg.Decode(bytes.NewReader(testJPEG(t, 96, 64)))
	if err != nil {
		t.Fatalf("jpeg decode: %v", err)
	}
	enc2 := &fakeEncoder{w: 320, h: 240}
	if _, _, err := encodeFrame(enc2, img, false); err != nil {
		t.Fatalf("encodeFrame size mismatch: %v", err)
	}
	if enc2.rgbaCalls != 1 || enc2.i420Calls != 0 {
		t.Fatalf("size mismatch: rgba=%d i420=%d, want fallback", enc2.rgbaCalls, enc2.i420Calls)
	}
}
