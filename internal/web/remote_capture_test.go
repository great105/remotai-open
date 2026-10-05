package web

import "testing"

func TestRemotePreviewWidth(t *testing.T) {
	tests := []struct {
		raw  string
		want int
	}{
		{"", 640},
		{"bad", 640},
		{"0", 640},
		{"120", 320},
		{"640", 640},
		{"1920", 1280},
	}
	for _, tc := range tests {
		if got := remotePreviewWidth(tc.raw); got != tc.want {
			t.Fatalf("remotePreviewWidth(%q) = %d, want %d", tc.raw, got, tc.want)
		}
	}
}
