package codec

import (
	"compress/bzip2"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// CiscoOpenH264URL is Cisco's official openh264 binary (bz2). Using Cisco's
// distributed binary is what grants the royalty-free patent license (the same
// model Firefox uses).
const CiscoOpenH264URL = "https://ciscobinary.openh264.org/openh264-2.4.1-win64.dll.bz2"

// CiscoOpenH264SHA256 pins the DECOMPRESSED openh264-2.4.1-win64.dll. Verified
// 2026-07-03 against Cisco's signed md5 (openh264-2.4.1-win64.dll.signed.md5.txt
// = c85406e6b73812ec3fb9da5f898c6a9e) — we execute this binary, so the default
// download path must be supply-chain-pinned.
const CiscoOpenH264SHA256 = "081b0c081480d177cbfddfbc90b1613640e702f875897b30d8de195cde73dd34"

// EnsureOpenH264DLL returns a path to openh264.dll in dir, downloading Cisco's
// official binary on first use (bz2-decompressed). When wantSHA256 is non-empty
// the decompressed bytes are verified against it (supply-chain pin).
//
// SECURITY: this performs a network download over Cisco's CDN. Call it only when
// the user opts into H.264 video, and prefer pinning wantSHA256. The agent works
// without it — H.264 is an optional enhancement over the JPEG video path.
func EnsureOpenH264DLL(dir, url, wantSHA256 string) (string, error) {
	if p := FindOpenH264DLL(dir); p != "" {
		return p, nil
	}
	if url == "" {
		url = CiscoOpenH264URL
		if wantSHA256 == "" {
			wantSHA256 = CiscoOpenH264SHA256
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	dst := filepath.Join(dir, "openh264.dll")

	client := &http.Client{Timeout: 90 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return "", fmt.Errorf("download openh264: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download openh264: HTTP %d", resp.StatusCode)
	}

	raw, err := io.ReadAll(bzip2.NewReader(resp.Body))
	if err != nil {
		return "", fmt.Errorf("decompress openh264: %w", err)
	}
	if len(raw) < 100_000 {
		return "", fmt.Errorf("openh264 payload too small (%d bytes)", len(raw))
	}
	if wantSHA256 != "" {
		sum := sha256.Sum256(raw)
		if got := hex.EncodeToString(sum[:]); got != wantSHA256 {
			return "", fmt.Errorf("openh264 sha256 mismatch: got %s want %s", got, wantSHA256)
		}
	}

	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return dst, nil
}
