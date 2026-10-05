// Package localize contains the built-in CLI and desktop messages.
package localize

import (
	"os"
	"strings"
)

// Language uses an explicit override, then the user's operating system locale.
// C/POSIX locales retain Russian for compatibility with automation.
func Language() string {
	for _, key := range []string{"REMOTAI_LANGUAGE", "LC_ALL", "LC_MESSAGES", "LANG"} {
		value := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
		if strings.HasPrefix(value, "ru") {
			return "ru"
		}
		if strings.HasPrefix(value, "en") {
			return "en"
		}
		if key != "REMOTAI_LANGUAGE" && value != "" && value != "c" && !strings.HasPrefix(value, "c.") && value != "posix" {
			return "en"
		}
	}
	return systemLanguage()
}

func English(text string) string {
	if translated, ok := english[text]; ok {
		return translated
	}
	return text
}

func Text(text string) string {
	if Language() == "en" {
		return English(text)
	}
	return text
}
