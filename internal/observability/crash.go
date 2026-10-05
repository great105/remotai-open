// Package observability — crash reporter (P3.5).
//
// Сохраняем panic-репорты в crash-*.json рядом с exe, и при следующем старте
// предлагаем пользователю отправить их в support endpoint (или, в локальном
// режиме, ознакомиться вручную).
package observability

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"time"

	"tgcontrol/internal/paths"
	"tgcontrol/internal/version"
)

// CrashReport — каркас отчёта.
type CrashReport struct {
	Time       string            `json:"time"`
	Version    string            `json:"version"`
	Commit     string            `json:"commit"`
	BuildDate  string            `json:"build_date"`
	Platform   string            `json:"platform"`
	Panic      string            `json:"panic"`
	Stack      string            `json:"stack"`
	Goroutines int               `json:"goroutines"`
	Extra      map[string]string `json:"extra,omitempty"`
}

// reportDir — папка данных приложения; если не получится — temp.
func reportDir() string {
	dir := paths.Base()
	if st, err := os.Stat(dir); err == nil && st.IsDir() {
		return dir
	}
	return os.TempDir()
}

// SaveCrashReport — вызывается из defer recover() в каждой fatal-зоне (main, важные горутины).
func SaveCrashReport(panicVal any, extra map[string]string) {
	report := &CrashReport{
		Time:       time.Now().UTC().Format(time.RFC3339),
		Version:    version.Version,
		Commit:     version.Commit,
		BuildDate:  version.BuildDate,
		Platform:   runtime.GOOS + "/" + runtime.GOARCH,
		Panic:      fmt.Sprint(panicVal),
		Stack:      string(debug.Stack()),
		Goroutines: runtime.NumGoroutine(),
		Extra:      extra,
	}
	dir := reportDir()
	file := filepath.Join(dir, fmt.Sprintf("crash-%d.json", time.Now().Unix()))
	data, _ := json.MarshalIndent(report, "", "  ")
	if err := os.WriteFile(file, data, 0o600); err != nil {
		log.Printf("[CRASH] write %s: %v", file, err)
		return
	}
	log.Printf("[CRASH] report saved: %s", file)
}

// InstallGlobal — вешает defer recover() на текущую горутину; вызывайте
// явно в main() и в каждой важной фоновой горутине.
//
//	defer observability.InstallGlobal("main")()
//
// Возвращает функцию-trigger, чтобы оборачивать через defer xxx().
func InstallGlobal(name string) func() {
	return func() {
		if r := recover(); r != nil {
			SaveCrashReport(r, map[string]string{"site": name})
			panic(r) // не глотаем — пусть процесс падает, но репорт уже на диске
		}
	}
}

// ScanReports возвращает список crash-*.json файлов в reportDir, отсортированных
// по убыванию timestamp.
func ScanReports() []string {
	dir := reportDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var found []fs.DirEntry
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			continue
		}
		if !startsWith(name, "crash-") || !endsWith(name, ".json") {
			continue
		}
		found = append(found, e)
	}
	sort.Slice(found, func(i, j int) bool { return found[i].Name() > found[j].Name() })
	out := make([]string, 0, len(found))
	for _, e := range found {
		out = append(out, filepath.Join(dir, e.Name()))
	}
	return out
}

// PromptIfAny логирует найденные crash-репорты при старте программы и возвращает их количество.
func PromptIfAny() int {
	reports := ScanReports()
	if len(reports) == 0 {
		return 0
	}
	log.Printf("[CRASH] обнаружено %d непрочитанных crash-репортов:", len(reports))
	for _, p := range reports {
		log.Printf("[CRASH]   %s", p)
	}
	log.Printf("[CRASH] файлы можно посмотреть вручную и удалить, когда они больше не нужны.")
	return len(reports)
}

func startsWith(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

func endsWith(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}
