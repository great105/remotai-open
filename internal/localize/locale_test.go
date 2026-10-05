package localize

import (
	"fmt"
	"strings"
	"testing"
)

func TestExplicitLanguageAndFormattedMessages(t *testing.T) {
	t.Setenv("REMOTAI_LANGUAGE", "en")
	if got := Text("Открыть окно"); got != "Open window" {
		t.Fatal(got)
	}
	message := fmt.Sprintf(Text("Remotai запущен на :%d"), 8080)
	if !strings.Contains(message, "8080") || strings.Contains(message, "%!") {
		t.Fatal(message)
	}
	if Text("My {name} project") != "My {name} project" {
		t.Fatal("Unknown text changed")
	}
	t.Setenv("REMOTAI_LANGUAGE", "ru")
	if got := Text("Открыть окно"); got != "Открыть окно" {
		t.Fatal(got)
	}
}
