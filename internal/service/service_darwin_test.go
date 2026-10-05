//go:build darwin

package service

import (
	"encoding/xml"
	"io"
	"strings"
	"testing"
)

func TestLaunchAgentEnabledUnderstandsNativeOutput(t *testing.T) {
	for _, tc := range []struct {
		value   string
		enabled bool
	}{
		{"true", false}, {"false", true}, {"disabled", false}, {"enabled", true}, {"unknown", false},
	} {
		output := "disabled services = {\n\t\"ru.remotai.agent\" => " + tc.value + "\n}\n"
		if got := launchAgentEnabled(output); got != tc.enabled {
			t.Errorf("%s: got %v, want %v", tc.value, got, tc.enabled)
		}
	}
	if !launchAgentEnabled("disabled services = {\n\t\"other.agent\" => true\n}\n") {
		t.Fatal("an installed RunAtLoad agent without an override is enabled")
	}
}

func TestLaunchAgentPlistEscapesPaths(t *testing.T) {
	exe := "/Users/test/Apps & Tools/<Remotai>/remotai"
	logs := "/Users/test/Logs & Data"
	decoder := xml.NewDecoder(strings.NewReader(launchAgentPlist(exe, logs)))
	var values []string
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("launchd cannot read invalid XML: %v", err)
		}
		if start, ok := token.(xml.StartElement); ok && start.Name.Local == "string" {
			var value string
			if err := decoder.DecodeElement(&value, &start); err != nil {
				t.Fatal(err)
			}
			values = append(values, value)
		}
	}
	for _, want := range []string{exe, logs + "/remotai.out.log", logs + "/remotai.err.log"} {
		found := false
		for _, value := range values {
			found = found || value == want
		}
		if !found {
			t.Errorf("path did not survive XML decoding: %q", want)
		}
	}
}

// Состав LaunchAgent проверяем по тексту plist — как автозапуск Windows
// проверяется по тексту PowerShell-скрипта (autostart_task_test.go). Причина та
// же: ошибка здесь тихая. Агент либо не поднимется после сбоя, либо поднимется
// и не найдёт установленные инструменты, и человек узнает об этом, когда
// компьютер уже «не в сети».
func TestLaunchAgentPlist(t *testing.T) {
	p := launchAgentPlist("/Users/me/.local/bin/remotai", "/Users/me/Library/Logs/Remotai")

	must := []struct{ what, substr string }{
		{"метка службы", "<string>ru.remotai.agent</string>"},
		{"путь к бинарю", "<string>/Users/me/.local/bin/remotai</string>"},
		{"фоновый запуск", "<string>--background</string>"},
		{"старт при входе", "<key>RunAtLoad</key>\n\t<true/>"},
		// Homebrew в PATH: launchd даёт почти пустое окружение, и без этого
		// агенты (claude, codex, node) в терминале «не установлены».
		{"Homebrew в PATH", "/opt/homebrew/bin"},
		{"Intel-Homebrew в PATH", "/usr/local/bin"},
		{"лог ошибок", "remotai.err.log"},
	}
	for _, m := range must {
		if !strings.Contains(p, m.substr) {
			t.Errorf("в plist нет: %s (%q)", m.what, m.substr)
		}
	}

	// KeepAlive именно СЛОВАРЁМ с SuccessfulExit=false: голое <true/> означало бы
	// «поднимать всегда», и штатный выход при обновлении превращался бы в гонку
	// со стартом новой версии.
	if !strings.Contains(p, "<key>KeepAlive</key>") || !strings.Contains(p, "<key>SuccessfulExit</key>") {
		t.Error("KeepAlive должен быть словарём с SuccessfulExit — иначе launchd будет бороться с обновлением")
	}
	if strings.Contains(p, "<key>KeepAlive</key>\n\t<true/>") {
		t.Error("KeepAlive=true поднимет агент даже после штатного выхода")
	}
}
