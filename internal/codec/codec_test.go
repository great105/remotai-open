package codec

import (
	"image"
	"math/rand"
	"testing"
)

// referenceI420 is the original single-pass RGBA→I420 conversion, kept as the
// oracle for the parallel strip version in fill/convertStrip.
func referenceI420(rgba *image.RGBA, w, h int) (yp, up, vp []byte) {
	cw := w / 2
	yp = make([]byte, w*h)
	up = make([]byte, cw*(h/2))
	vp = make([]byte, cw*(h/2))
	pix := rgba.Pix
	stride := rgba.Stride
	for y := 0; y < h; y++ {
		row := y * stride
		yrow := y * w
		for x := 0; x < w; x++ {
			i := row + x*4
			r := int(pix[i])
			g := int(pix[i+1])
			bl := int(pix[i+2])
			yp[yrow+x] = clampByte((66*r+129*g+25*bl+128)>>8 + 16)
		}
	}
	for cy := 0; cy < h/2; cy++ {
		for cx := 0; cx < cw; cx++ {
			x0, y0 := cx*2, cy*2
			var rr, gg, bb int
			for dy := 0; dy < 2; dy++ {
				for dx := 0; dx < 2; dx++ {
					i := (y0+dy)*stride + (x0+dx)*4
					rr += int(pix[i])
					gg += int(pix[i+1])
					bb += int(pix[i+2])
				}
			}
			rr >>= 2
			gg >>= 2
			bb >>= 2
			ci := cy*cw + cx
			up[ci] = clampByte((-38*rr-74*gg+112*bb+128)>>8 + 128)
			vp[ci] = clampByte((112*rr-94*gg-18*bb+128)>>8 + 128)
		}
	}
	return yp, up, vp
}

func TestFillMatchesReference(t *testing.T) {
	rnd := rand.New(rand.NewSource(1))
	// Sizes chosen to exercise strip boundaries: tiny, non-divisible-by-4
	// heights, typical encode dims.
	for _, s := range [][2]int{{2, 2}, {320, 240}, {646, 362}, {1278, 718}} {
		w, h := s[0], s[1]
		img := image.NewRGBA(image.Rect(0, 0, w, h))
		for i := range img.Pix {
			img.Pix[i] = byte(rnd.Intn(256))
		}
		b := newI420Buffer(w, h)
		b.fill(img)
		ry, ru, rv := referenceI420(img, w, h)
		for name, pair := range map[string][2][]byte{
			"Y": {b.y, ry}, "U": {b.u, ru}, "V": {b.v, rv},
		} {
			got, want := pair[0], pair[1]
			if len(got) != len(want) {
				t.Fatalf("%dx%d plane %s: len %d want %d", w, h, name, len(got), len(want))
			}
			for i := range got {
				if got[i] != want[i] {
					t.Fatalf("%dx%d plane %s differs at %d: %d want %d", w, h, name, i, got[i], want[i])
				}
			}
		}
	}
}

func BenchmarkFill720p(b *testing.B) {
	img := image.NewRGBA(image.Rect(0, 0, 1280, 720))
	rnd := rand.New(rand.NewSource(2))
	for i := range img.Pix {
		img.Pix[i] = byte(rnd.Intn(256))
	}
	buf := newI420Buffer(1280, 720)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buf.fill(img)
	}
}
