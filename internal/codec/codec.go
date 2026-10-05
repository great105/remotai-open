// Package codec provides an H.264 video encoder for the WebRTC Remote Desktop
// video track (Milestone 2). The encoder is OPTIONAL: when it can't be created
// (codec library absent, non-Windows, init failure) the caller falls back to the
// JPEG-over-DataChannel path from Milestone 1, so the feature is strictly
// additive.
//
// The Windows implementation binds Cisco's openh264 via a runtime-loaded DLL +
// raw syscall (NO cgo) so the agent stays a single CGO_ENABLED=0 portable exe.
// openh264.dll is treated as an optional runtime plugin (located next to the exe
// / in the app data dir), exactly like Firefox ships the WebRTC H.264 codec.
package codec

import (
	"image"
	"image/draw"
	"runtime"
	"sync"

	xdraw "golang.org/x/image/draw"
)

// Encoder encodes RGBA frames into Annex-B H.264 NAL units (with start codes),
// ready to hand to pion's TrackLocalStaticSample. Implementations are NOT
// safe for concurrent use — drive them from a single producer goroutine.
type Encoder interface {
	// EncodeRGBA encodes one frame. The image is scaled to the encoder's fixed
	// dimensions if needed. Returns the Annex-B bitstream (nil/empty when the
	// encoder skipped the frame), whether it was a keyframe (IDR), and an error.
	EncodeRGBA(img image.Image, forceKeyframe bool) (nal []byte, keyframe bool, err error)
	// Width/Height are the fixed encode dimensions (even numbers).
	Width() int
	Height() int
	Close()
}

// LiveTuner is implemented by encoders that can retune bitrate/frame-rate on
// the fly, without the re-create + IDR + visible stall of a full re-init.
// Callers type-assert: encoders lacking it must be re-created to retune.
type LiveTuner interface {
	SetBitrate(bps int) error
	SetFrameRate(fps float64) error
}

// PlaneEncoder is an optional Encoder extension: the frame arrives already in
// I420 planes (Y/U/V with explicit strides) and goes into the codec with no
// RGBA conversion at all. The CDP screencast JPEGs from Chrome decode to
// exactly YCbCr 4:2:0 (Cb→U, Cr→V), so this skips the most expensive step on
// the per-frame path. The frame MUST match the encoder's fixed dimensions —
// there is nothing to scale with here; mismatched sizes use EncodeRGBA.
type PlaneEncoder interface {
	EncodeI420(y, u, v []byte, yStride, cStride int, forceKeyframe bool) (nal []byte, keyframe bool, err error)
}

// i420Buffer holds a reusable I420 (YUV 4:2:0 planar) frame for the encoder.
type i420Buffer struct {
	w, h   int
	y      []byte // w*h
	u      []byte // (w/2)*(h/2)
	v      []byte // (w/2)*(h/2)
	scaled *image.RGBA
}

func newI420Buffer(w, h int) *i420Buffer {
	cw, ch := w/2, h/2
	return &i420Buffer{
		w: w, h: h,
		y:      make([]byte, w*h),
		u:      make([]byte, cw*ch),
		v:      make([]byte, cw*ch),
		scaled: image.NewRGBA(image.Rect(0, 0, w, h)),
	}
}

// fill converts an arbitrary image into this buffer's I420 planes, scaling to
// (w,h) first if the source size differs. Uses limited-range BT.601 (the H.264
// default) so browsers render correct colors. The conversion runs in parallel
// horizontal strips — it sits on the per-frame latency path.
func (b *i420Buffer) fill(src image.Image) {
	var rgba *image.RGBA
	sb := src.Bounds()
	if sb.Dx() == b.w && sb.Dy() == b.h {
		if r, ok := src.(*image.RGBA); ok {
			rgba = r
		} else {
			draw.Draw(b.scaled, b.scaled.Bounds(), src, sb.Min, draw.Src)
			rgba = b.scaled
		}
	} else {
		// Src, not Over: screen captures are opaque, and Over pays an extra
		// read+blend of the destination per pixel for an identical result.
		xdraw.ApproxBiLinear.Scale(b.scaled, b.scaled.Bounds(), src, sb, xdraw.Src, nil)
		rgba = b.scaled
	}

	workers := min(runtime.GOMAXPROCS(0), 4)
	if workers < 1 {
		workers = 1
	}
	// Strips are even-height so each owns whole 2x2 chroma blocks.
	strip := ((b.h/2 + workers - 1) / workers) * 2
	if strip < 2 {
		strip = 2
	}
	var wg sync.WaitGroup
	for y0 := 0; y0 < b.h; y0 += strip {
		y1 := min(y0+strip, b.h)
		wg.Add(1)
		go func(y0, y1 int) {
			defer wg.Done()
			b.convertStrip(rgba, y0, y1)
		}(y0, y1)
	}
	wg.Wait()
}

// convertStrip converts luma rows [y0,y1) and the matching chroma rows.
// y0/y1 must be even (except y1==h for odd heights, which never happens: the
// encoder enforces even dimensions).
func (b *i420Buffer) convertStrip(rgba *image.RGBA, y0, y1 int) {
	w := b.w
	cw := w / 2
	pix := rgba.Pix
	stride := rgba.Stride
	// Luma: full resolution.
	for y := y0; y < y1; y++ {
		row := y * stride
		yrow := y * w
		for x := 0; x < w; x++ {
			i := row + x*4
			r := int(pix[i])
			g := int(pix[i+1])
			bl := int(pix[i+2])
			// Y = 0.257R + 0.504G + 0.098B + 16  (fixed-point /256)
			b.y[yrow+x] = clampByte((66*r+129*g+25*bl+128)>>8 + 16)
		}
	}
	// Chroma: 2x2 average subsample.
	for cy := y0 / 2; cy < y1/2; cy++ {
		for cx := 0; cx < cw; cx++ {
			x0, yy0 := cx*2, cy*2
			var rr, gg, bb int
			for dy := 0; dy < 2; dy++ {
				for dx := 0; dx < 2; dx++ {
					i := (yy0+dy)*stride + (x0+dx)*4
					rr += int(pix[i])
					gg += int(pix[i+1])
					bb += int(pix[i+2])
				}
			}
			rr >>= 2
			gg >>= 2
			bb >>= 2
			ci := cy*cw + cx
			// U = -0.148R - 0.291G + 0.439B + 128
			b.u[ci] = clampByte((-38*rr-74*gg+112*bb+128)>>8 + 128)
			// V =  0.439R - 0.368G - 0.071B + 128
			b.v[ci] = clampByte((112*rr-94*gg-18*bb+128)>>8 + 128)
		}
	}
}

func clampByte(v int) byte {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return byte(v)
}

// evenDown returns the largest even number <= n (H.264 4:2:0 needs even dims).
func evenDown(n int) int {
	if n < 2 {
		return 2
	}
	return n &^ 1
}

// EncodeDims computes the encoder dimensions for a source of (srcW,srcH) capped
// to maxW, keeping aspect, with even width/height.
func EncodeDims(srcW, srcH, maxW int) (int, int) {
	if srcW <= 0 || srcH <= 0 {
		return 1280, 720
	}
	w := srcW
	if maxW > 0 && w > maxW {
		w = maxW
	}
	h := srcH * w / srcW
	return evenDown(w), evenDown(h)
}
