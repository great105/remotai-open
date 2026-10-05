package tunnel

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// extractTgzBinary достаёт из .tgz один файл с указанным именем и кладёт его по
// пути dest. Ищем по базовому имени: у разных релизов внутри архива бывает и
// «cloudflared», и «./cloudflared».
func extractTgzBinary(archivePath, dest, wantName string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return fmt.Errorf("в архиве нет файла %q", wantName)
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg || filepath.Base(hdr.Name) != wantName {
			continue
		}
		out, err := os.Create(dest)
		if err != nil {
			return err
		}
		// Лимит на распаковку: архив приходит из сети, и без потолка битый или
		// подменённый файл мог бы забить диск.
		if _, err := io.Copy(out, io.LimitReader(tr, 512<<20)); err != nil {
			out.Close()
			os.Remove(dest)
			return err
		}
		return out.Close()
	}
}

// DownloadProgress reports download state.
type DownloadProgress struct {
	Downloaded int64  `json:"downloaded"`
	Total      int64  `json:"total"`
	Done       bool   `json:"done"`
	Error      string `json:"error,omitempty"`
	Path       string `json:"path,omitempty"`
}

// DownloadCloudflared downloads the cloudflared binary next to tgcontrol executable.
// progress is called periodically with download state (may be nil).
func DownloadCloudflared(progress func(DownloadProgress)) (string, error) {
	url, filename := downloadURL()
	if url == "" {
		return "", fmt.Errorf("unsupported platform: %s/%s", runtime.GOOS, runtime.GOARCH)
	}

	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("cannot determine exe path: %w", err)
	}
	destPath := filepath.Join(filepath.Dir(exe), filename)

	log.Printf("[TUNNEL] Downloading cloudflared from %s", url)
	if progress != nil {
		progress(DownloadProgress{})
	}

	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return "", fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}

	total := resp.ContentLength

	tmpPath := destPath + ".tmp"
	f, err := os.Create(tmpPath)
	if err != nil {
		return "", fmt.Errorf("create file: %w", err)
	}

	var downloaded int64
	buf := make([]byte, 64*1024)
	lastReport := time.Now()

	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, wErr := f.Write(buf[:n]); wErr != nil {
				f.Close()
				os.Remove(tmpPath)
				return "", fmt.Errorf("write: %w", wErr)
			}
			downloaded += int64(n)
			if progress != nil && time.Since(lastReport) > 200*time.Millisecond {
				progress(DownloadProgress{Downloaded: downloaded, Total: total})
				lastReport = time.Now()
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			f.Close()
			os.Remove(tmpPath)
			return "", fmt.Errorf("read: %w", readErr)
		}
	}
	f.Close()

	// macOS: скачали архив — достаём из него сам бинарь. Без этого шага на диск
	// ложился .tgz под именем исполняемого файла, и туннель молча не работал.
	if strings.HasSuffix(filename, ".tgz") {
		binPath := filepath.Join(filepath.Dir(destPath), "cloudflared")
		if err := extractTgzBinary(tmpPath, binPath, "cloudflared"); err != nil {
			os.Remove(tmpPath)
			return "", fmt.Errorf("распаковка cloudflared: %w", err)
		}
		os.Remove(tmpPath)
		os.Chmod(binPath, 0o755)
		log.Printf("[TUNNEL] Downloaded cloudflared: %s (%d bytes архива)", binPath, downloaded)
		if progress != nil {
			progress(DownloadProgress{Downloaded: downloaded, Total: total, Done: true, Path: binPath})
		}
		return binPath, nil
	}

	// Make executable on Linux/Mac
	if runtime.GOOS != "windows" {
		os.Chmod(tmpPath, 0755)
	}

	// Atomic rename
	os.Remove(destPath)
	if err := os.Rename(tmpPath, destPath); err != nil {
		return "", fmt.Errorf("rename: %w", err)
	}

	log.Printf("[TUNNEL] Downloaded cloudflared: %s (%d bytes)", destPath, downloaded)
	if progress != nil {
		progress(DownloadProgress{Downloaded: downloaded, Total: total, Done: true, Path: destPath})
	}

	return destPath, nil
}

// IsCloudflaredInstalled checks if cloudflared is available.
func IsCloudflaredInstalled() (string, bool) {
	path := findCloudflared()
	return path, path != ""
}

func downloadURL() (url string, filename string) {
	const base = "https://github.com/cloudflare/cloudflared/releases/latest/download"
	switch runtime.GOOS {
	case "windows":
		switch runtime.GOARCH {
		case "amd64":
			return base + "/cloudflared-windows-amd64.exe", "cloudflared.exe"
		case "386":
			return base + "/cloudflared-windows-386.exe", "cloudflared.exe"
		case "arm64":
			return base + "/cloudflared-windows-arm64.exe", "cloudflared.exe"
		}
	case "linux":
		switch runtime.GOARCH {
		case "amd64":
			return base + "/cloudflared-linux-amd64", "cloudflared"
		case "arm64":
			return base + "/cloudflared-linux-arm64", "cloudflared"
		case "arm":
			return base + "/cloudflared-linux-arm", "cloudflared"
		case "386":
			return base + "/cloudflared-linux-386", "cloudflared"
		}
	case "darwin":
		// Cloudflare раздаёт мак-сборки только архивом .tgz, а внутри лежит
		// исполняемый файл. Раньше здесь на ЛЮБОЙ архитектуре возвращался
		// amd64-архив, и он же сохранялся под именем «cloudflared» без
		// распаковки — то есть на маке скачивался мусор, который потом не
		// запускался. Отдаём честную ссылку по архитектуре и помечаем, что это
		// архив (см. распаковку в DownloadCloudflared).
		switch runtime.GOARCH {
		case "amd64":
			return base + "/cloudflared-darwin-amd64.tgz", "cloudflared.tgz"
		case "arm64":
			return base + "/cloudflared-darwin-arm64.tgz", "cloudflared.tgz"
		}
	}
	return "", ""
}
