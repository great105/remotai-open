package version

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
)

// TestCheckForUpdateTargets: multi-target манифест выбирает бинарь под текущий
// GOOS/GOARCH (запускать и на linux, и на windows).
func TestCheckForUpdateTargets(t *testing.T) {
	manifest := UpdateInfo{
		Version:     "999.0.0",
		DownloadURL: "https://x/remotai.exe",
		SHA256:      "winsha",
		Size:        10,
		Targets: map[string]TargetInfo{
			"windows/amd64": {DownloadURL: "https://x/remotai.exe", SHA256: "winsha", Size: 10},
			"linux/amd64":   {DownloadURL: "https://x/remotai-linux-amd64", SHA256: "lxsha", Size: 20},
			"linux/arm64":   {DownloadURL: "https://x/remotai-linux-arm64", SHA256: "armsha", Size: 30},
			// Мак: два отдельных бинаря без универсального fat-файла — автообновление
			// должно взять СВОЙ, иначе на Apple Silicon приедет Intel-сборка.
			"darwin/arm64": {DownloadURL: "https://x/remotai-darwin-arm64", SHA256: "macarm", Size: 40},
			"darwin/amd64": {DownloadURL: "https://x/remotai-darwin-amd64", SHA256: "macamd", Size: 50},
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(manifest)
	}))
	defer srv.Close()

	info, err := CheckForUpdate(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"windows/amd64": "https://x/remotai.exe",
		"linux/amd64":   "https://x/remotai-linux-amd64",
		"linux/arm64":   "https://x/remotai-linux-arm64",
		"darwin/arm64":  "https://x/remotai-darwin-arm64",
		"darwin/amd64":  "https://x/remotai-darwin-amd64",
	}[runtime.GOOS+"/"+runtime.GOARCH]
	if want == "" {
		t.Skipf("no fixture target for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	if info.DownloadURL != want {
		t.Errorf("DownloadURL = %q, want %q (%s/%s)", info.DownloadURL, want, runtime.GOOS, runtime.GOARCH)
	}
}

// TestCheckForUpdateFlatManifest: плоский (легаси) манифест без targets отдаёт
// URL только на Windows; на любой другой платформе URL зануляется, чтобы .exe
// не применился как ELF.
func TestCheckForUpdateFlatManifest(t *testing.T) {
	manifest := UpdateInfo{Version: "999.0.0", DownloadURL: "https://x/remotai.exe", SHA256: "s", Size: 1}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(manifest)
	}))
	defer srv.Close()

	info, err := CheckForUpdate(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		if info.DownloadURL == "" {
			t.Error("windows must keep the flat legacy download_url")
		}
	} else if info.DownloadURL != "" {
		t.Errorf("non-windows flat manifest must clear DownloadURL, got %q", info.DownloadURL)
	}
}
