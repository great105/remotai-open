package web

import "testing"

func TestSafeUploadFilename(t *testing.T) {
	cases := map[string]string{
		`..\..\secret.txt`:       "secret.txt",
		`/tmp/report.txt`:        "report.txt",
		`Снимок 2026:07:25?.png`: "Снимок 20260725.png",
		"line\nbreak.txt":        "linebreak.txt",
		"   ...   ":              "upload",
	}
	for input, want := range cases {
		if got := safeUploadFilename(input); got != want {
			t.Errorf("safeUploadFilename(%q) = %q, want %q", input, got, want)
		}
	}
}
