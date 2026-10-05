package web

import (
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"tgcontrol/internal/config"
)

// Проводка настройки «Разметка команд» (ST-10, T-39) до менеджера терминалов:
// config.ShellIntegration → NewServer → Manager.SetShellIntegration и
// POST /api/settings/apply → Manager.SetShellIntegration. Без неё переключатель
// сохранялся бы в файл, а новые терминалы открывались бы по-старому — и ни один
// тест этого не заметил бы (ревью B2: подмена любого звена на false проходила
// зелёной). Дальше по цепочке (Manager → запуск шелла) — тесты проводки в
// internal/pty/shell_integration_test.go.
//
// paths.Base и config.GetNoSetup — одиночки процесса, поэтому каждая проверка
// идёт в свежем процессе с временным HOME (как в server_auth_test.go): иначе
// тест прочёл бы или переписал настоящий config.json машины.
func TestShellIntegrationSettingReachesPTYManager(t *testing.T) {
	const childEnv = "REMOTAI_TEST_SHELL_INTEGRATION_CHILD"
	switch os.Getenv(childEnv) {
	case "off":
		// Умолчание — выключено: владелец включает после проверки на устройстве.
		s := NewServer(nil, nil, "", nil)
		if s.ptyManager.ShellIntegration() {
			t.Fatal("в config.json нет shell_integration, а новые терминалы пойдут с разметкой")
		}
		return
	case "on":
		s := NewServer(nil, nil, "", nil)
		if !s.ptyManager.ShellIntegration() {
			t.Fatal("config.json: shell_integration=true, а менеджер терминалов после NewServer выключен")
		}
		apply := func(value string, want bool) {
			t.Helper()
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("POST", "/api/settings/apply",
				strings.NewReader(`{"key":"shell_integration","value":"`+value+`"}`))
			s.apiSettingsApply(rec, req, 12345)
			if rec.Code != 200 {
				t.Fatalf("apply %s: код %d, тело %s", value, rec.Code, rec.Body.String())
			}
			if got := s.ptyManager.ShellIntegration(); got != want {
				t.Fatalf("apply %s: менеджер терминалов = %v, ждали %v — настройка не дошла до новых терминалов", value, got, want)
			}
			if got := config.GetNoSetup().ShellIntegration; got != want {
				t.Fatalf("apply %s: в конфиге %v, ждали %v", value, got, want)
			}
		}
		apply("false", false)
		apply("true", true)
		return
	}

	for _, mode := range []string{"off", "on"} {
		home := t.TempDir()
		configDir := filepath.Join(home, ".config", "remotai")
		if runtime.GOOS == "windows" {
			configDir = filepath.Join(home, "AppData", "Local", "Remotai")
		}
		if err := os.MkdirAll(configDir, 0o755); err != nil {
			t.Fatal(err)
		}
		extra := ""
		if mode == "on" {
			extra = `, "shell_integration": true`
		}
		configJSON := `{"mode": "central_bot", "setup_complete": true, "web_port": "18081"` + extra + `}`
		if err := os.WriteFile(filepath.Join(configDir, "config.json"), []byte(configJSON), 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(os.Args[0], "-test.run=^TestShellIntegrationSettingReachesPTYManager$", "-test.count=1")
		cmd.Env = append(os.Environ(), "USERPROFILE="+home, "HOME="+home, childEnv+"="+mode)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("проверка %q в отдельном процессе упала: %v\n%s", mode, err, output)
		}
	}
}
