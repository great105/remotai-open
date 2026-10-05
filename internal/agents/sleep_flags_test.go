package agents

import (
	"reflect"
	"strings"
	"testing"
)

func TestSleepFlagsKeepsOnlyKnownModeFlags(t *testing.T) {
	claude := GetDescriptor("claude")
	argv := strings.Fields(`C:\x\claude.exe --dangerously-skip-permissions --settings {"hooks":1} --continue починить тесты`)
	if got := claude.SleepFlags(argv); !reflect.DeepEqual(got, []string{"--dangerously-skip-permissions"}) {
		t.Fatalf("claude: %v", got)
	}
	if got := claude.SleepFlags([]string{"claude", "--permission-mode=plan"}); !reflect.DeepEqual(got, []string{"--permission-mode plan"}) {
		t.Fatalf("флаг со значением через =: %v", got)
	}
	codex := GetDescriptor("codex")
	argv = []string{"node", "codex.js", "--ask-for-approval", "never", "--search"}
	if got := codex.SleepFlags(argv); !reflect.DeepEqual(got, []string{"--ask-for-approval never", "--search"}) {
		t.Fatalf("codex: %v", got)
	}
}

func TestCanSleepOnlyWithResumeByID(t *testing.T) {
	for _, id := range []string{"claude", "codex"} {
		d := GetDescriptor(id)
		if !d.CanSleep() || !strings.Contains(d.ResumeIDCommand(), "{session_id}") {
			t.Fatalf("%s должен усыпляться: %q", id, d.ResumeIDCommand())
		}
	}
	if GetDescriptor("gemini").CanSleep() {
		t.Fatal("gemini продолжения по номеру не сверяли — усыплять нельзя")
	}
	var none *AgentDescriptor
	if none.CanSleep() {
		t.Fatal("nil-дескриптор")
	}
}
