//go:build windows || linux

package codec

import (
	"image"
	"image/color"
	"os"
	"testing"
	"unsafe"
)

// TestStructSizes asserts the openh264 struct layouts match the C ABI. A
// mismatch here means EncodeFrame would read/write garbage.
func TestStructSizes(t *testing.T) {
	cases := []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"encParamBase", unsafe.Sizeof(encParamBase{}), 24},
		{"sourcePicture", unsafe.Sizeof(sourcePicture{}), 80},
		{"layerBSInfo", unsafe.Sizeof(layerBSInfo{}), 40},
		{"frameBSInfo", unsafe.Sizeof(frameBSInfo{}), 5144},
		{"sliceArgument", unsafe.Sizeof(sliceArgument{}), 152},
		{"spatialLayerConfig", unsafe.Sizeof(spatialLayerConfig{}), 200},
		// C SEncParamExt (2.4.x) is 920; ours adds a 64-byte reserved tail.
		{"encParamExt", unsafe.Sizeof(encParamExt{}), 984},
		{"bitrateInfo", unsafe.Sizeof(bitrateInfo{}), 8},
	}
	// Field offsets the runtime layout probe and SetOption ABI rely on.
	offs := []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"encParamExt.maxFrameRate", unsafe.Offsetof(encParamExt{}.maxFrameRate), 20},
		{"encParamExt.spatialLayers", unsafe.Offsetof(encParamExt{}.spatialLayers), 32},
		{"encParamExt.complexityMode", unsafe.Offsetof(encParamExt{}.complexityMode), 832},
		{"encParamExt.enableFrameSkip", unsafe.Offsetof(encParamExt{}.enableFrameSkip), 860},
		{"encParamExt.maxQp", unsafe.Offsetof(encParamExt{}.maxQp), 868},
		{"encParamExt.minQp", unsafe.Offsetof(encParamExt{}.minQp), 872},
		{"encParamExt.multipleThreadIdc", unsafe.Offsetof(encParamExt{}.multipleThreadIdc), 892},
		{"encParamExt.enableSceneChangeDetect", unsafe.Offsetof(encParamExt{}.enableSceneChangeDetect), 912},
		{"encParamExt.idrBitrateRatio", unsafe.Offsetof(encParamExt{}.idrBitrateRatio), 916},
	}
	for _, c := range offs {
		if c.got != c.want {
			t.Errorf("offset %s=%d want %d", c.name, c.got, c.want)
		}
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s size=%d want %d", c.name, c.got, c.want)
		}
	}
}

// TestEncodeIfDLLPresent does a real encode round-trip when an openh264 DLL is
// available (set OPENH264_DIR or place openh264.dll next to the test binary).
func TestEncodeIfDLLPresent(t *testing.T) {
	dll := FindOpenH264DLL(os.Getenv("OPENH264_DIR"))
	if dll == "" && os.Getenv("OPENH264_DOWNLOAD") != "" {
		d, err := EnsureOpenH264DLL(t.TempDir(), "", "")
		if err != nil {
			t.Skipf("openh264 download failed: %v", err)
		}
		dll = d
	}
	if dll == "" {
		t.Skip("openh264.dll not found (set OPENH264_DIR or OPENH264_DOWNLOAD=1)")
	}
	const w, h = 320, 240
	enc, err := NewH264(dll, w, h, 15, 800_000)
	if err != nil {
		t.Fatalf("NewH264: %v", err)
	}
	defer enc.Close()

	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		for y := 0; y < h; y++ {
			img.SetRGBA(x, y, color.RGBA{uint8(x), uint8(y), 64, 255})
		}
	}
	nal, key, err := enc.EncodeRGBA(img, true)
	if err != nil {
		t.Fatalf("encode IDR: %v", err)
	}
	if len(nal) < 5 || nal[0] != 0 || nal[1] != 0 {
		t.Fatalf("expected Annex-B start code, got % x", nal[:minInt(8, len(nal))])
	}
	if !key {
		t.Fatalf("first frame should be a keyframe")
	}
	t.Logf("IDR frame: %d bytes", len(nal))
	// Parse Annex-B NAL units; a decodable keyframe MUST contain the actual IDR
	// slice (type 5), not just SPS(7)+PPS(8).
	var hasSPS, hasIDRSlice bool
	for _, u := range parseAnnexB(nal) {
		t.Logf("  NAL type=%d len=%d firstBytes=% x", u.typ, u.len, u.head)
		if u.typ == 7 {
			hasSPS = true
		}
		if u.typ == 5 {
			hasIDRSlice = true
		}
	}
	if !hasSPS || !hasIDRSlice {
		t.Fatalf("keyframe missing SPS(%v) or IDR slice(%v) — decoder can't render", hasSPS, hasIDRSlice)
	}

	total := 0
	for f := 0; f < 8; f++ {
		for x := 0; x < w; x++ {
			for y := 0; y < h; y++ {
				img.SetRGBA(x, y, color.RGBA{uint8((x + f*17) % 256), uint8((y + f*7) % 256), uint8(f * 30 % 256), 255})
			}
		}
		n, _, err := enc.EncodeRGBA(img, false)
		if err != nil {
			t.Fatalf("encode P[%d]: %v", f, err)
		}
		total += len(n)
	}
	if total == 0 {
		t.Fatalf("no P-frame output across 8 frames")
	}
	t.Logf("8 P-frames: %d bytes total", total)
}

// TestEncodeI420IfDLLPresent exercises the direct I420 plane path (the CDP
// screencast fast path): planes with PADDED strides must encode into a
// decodable stream, the forced IDR must carry parameter sets (a mid-stream
// resolution switch relies on them), and undersized planes must be rejected.
func TestEncodeI420IfDLLPresent(t *testing.T) {
	dll := FindOpenH264DLL(os.Getenv("OPENH264_DIR"))
	if dll == "" {
		t.Skip("openh264.dll not found (set OPENH264_DIR or OPENH264_DOWNLOAD=1)")
	}
	const w, h = 320, 240
	enc, err := NewH264(dll, w, h, 30, 800_000)
	if err != nil {
		t.Fatalf("NewH264: %v", err)
	}
	defer enc.Close()
	pe, ok := enc.(PlaneEncoder)
	if !ok {
		t.Fatalf("h264Encoder does not implement PlaneEncoder")
	}

	// 60 fps — the new live-retune ceiling (was clamped to 30).
	lt, ok := enc.(LiveTuner)
	if !ok {
		t.Fatalf("h264Encoder does not implement LiveTuner")
	}
	if err := lt.SetFrameRate(60); err != nil {
		t.Fatalf("SetFrameRate(60): %v", err)
	}

	// External sources (the jpeg decoder) don't promise tight packing — feed
	// planes with padded strides.
	yStride, cStride := w+8, w/2+4
	ch := h / 2
	y := make([]byte, yStride*h)
	u := make([]byte, cStride*ch)
	v := make([]byte, cStride*ch)
	for i := range y {
		y[i] = uint8(i * 3)
	}
	for i := range u {
		u[i] = uint8(128 + i%3 - 1)
		v[i] = uint8(127 - i%3)
	}

	nal, key, err := pe.EncodeI420(y, u, v, yStride, cStride, true)
	if err != nil {
		t.Fatalf("EncodeI420 IDR: %v", err)
	}
	if !key {
		t.Fatalf("forced IDR not reported as keyframe")
	}
	var hasSPS, hasIDR bool
	for _, un := range parseAnnexB(nal) {
		if un.typ == 7 {
			hasSPS = true
		}
		if un.typ == 5 {
			hasIDR = true
		}
	}
	if !hasSPS || !hasIDR {
		t.Fatalf("IDR must carry parameter sets (SPS=%v, IDR slice=%v) — the resolution switch relies on them", hasSPS, hasIDR)
	}

	total := 0
	for f := 0; f < 4; f++ {
		for i := range y {
			y[i] = uint8(int(y[i]) + f*11 + 1)
		}
		n, _, err := pe.EncodeI420(y, u, v, yStride, cStride, false)
		if err != nil {
			t.Fatalf("EncodeI420 P[%d]: %v", f, err)
		}
		total += len(n)
	}
	if total == 0 {
		t.Fatalf("no P-frame output across 4 frames")
	}

	if _, _, err := pe.EncodeI420(y[:yStride], u, v, yStride, cStride, false); err == nil {
		t.Fatalf("undersized planes must be rejected")
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

type nalUnit struct {
	typ  byte
	len  int
	head []byte
}

// parseAnnexB splits an Annex-B bitstream into NAL units (handles 3- and 4-byte
// start codes) for test inspection.
func parseAnnexB(b []byte) []nalUnit {
	var starts []int
	i := 0
	for i+2 < len(b) {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			starts = append(starts, i)
			i += 3
		} else {
			i++
		}
	}
	var out []nalUnit
	for j, s := range starts {
		payload := s + 3
		end := len(b)
		if j+1 < len(starts) {
			end = starts[j+1]
		}
		if payload >= end {
			continue
		}
		h := b[payload:end]
		hb := h
		if len(hb) > 6 {
			hb = hb[:6]
		}
		out = append(out, nalUnit{typ: b[payload] & 0x1F, len: end - s, head: hb})
	}
	return out
}
