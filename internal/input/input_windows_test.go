//go:build windows

package input

import "testing"

func TestAbsCoord(t *testing.T) {
	cases := []struct {
		name             string
		px, origin, size int32
		want             int32
	}{
		{"left edge", 0, 0, 1920, 0},
		{"right edge", 1919, 0, 1920, 65535},
		{"center", 960, 0, 1920, 32784},
		{"below range clamps to 0", -100, 0, 1920, 0},
		{"above range clamps to max", 5000, 0, 1920, 65535},
		{"degenerate size", 5, 0, 1, 0},
		// Identity axis (size-1 == 65535): pixel maps to itself.
		{"identity", 40000, 0, 65536, 40000},
		// Virtual desktop spanning -1920..1919 (width 3840): x=0 ≈ midpoint.
		{"negative-origin midpoint", 0, -1920, 3840, 32776},
		{"negative-origin left edge", -1920, -1920, 3840, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := absCoord(tc.px, tc.origin, tc.size)
			if got != tc.want {
				t.Fatalf("absCoord(%d,%d,%d) = %d, want %d", tc.px, tc.origin, tc.size, got, tc.want)
			}
		})
	}
}
