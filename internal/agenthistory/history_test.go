package agenthistory

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func claudeFixture(t *testing.T, count int) (Source, []byte) {
	t.Helper()
	source := Source{Agent: "claude", SessionID: "fixture-session"}
	source.Transcript = filepath.Join(t.TempDir(), source.SessionID+".jsonl")
	var data []byte
	for i := 0; i < count; i++ {
		parent := ""
		if i > 0 {
			parent = fmt.Sprint(i - 1)
		}
		msg := map[string]any{"type": "assistant", "uuid": fmt.Sprint(i), "parentUuid": parent, "sessionId": source.SessionID, "version": "2.1.270", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": fmt.Sprintf("row-%03d\n  中😀 code", i)}}}}
		line, _ := json.Marshal(msg)
		data = append(data, append(line, '\n')...)
	}
	if err := os.WriteFile(source.Transcript, data, 0600); err != nil {
		t.Fatal(err)
	}
	return source, data
}

func TestClaudePagesAreFrozenOrderedAndBoundToSource(t *testing.T) {
	source, original := claudeFixture(t, 95)
	page, err := Read(context.Background(), source, "", "")
	if err != nil || !strings.HasPrefix(page.Text, "[assistant]\nrow-055") || page.Next == "" || page.Partial {
		t.Fatalf("first page: %+v %v", page, err)
	}
	// Appending cannot move the older page. Each of the 95 messages appears once.
	if err := os.WriteFile(source.Transcript, append(original, []byte("{\"type\":\"progress\"}\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	text := page.Text
	for page.Next != "" {
		page, err = Read(context.Background(), source, page.Next, "")
		if err != nil {
			t.Fatal(err)
		}
		text = page.Text + "\n\n" + text
	}
	for i := 0; i < 95; i++ {
		if strings.Count(text, fmt.Sprintf("row-%03d", i)) != 1 {
			t.Fatalf("missing/duplicate row %d", i)
		}
	}
	first, _ := Read(context.Background(), source, "", "")
	other := source
	other.ConfigHome = "changed"
	if _, err := Read(context.Background(), other, first.Next, ""); !errors.Is(err, ErrChanged) {
		t.Fatalf("cross-source cursor: %v", err)
	}
	changed := append([]byte(nil), original...)
	changed[20] = 'X'
	_ = os.WriteFile(source.Transcript, changed, 0600)
	if _, err := Read(context.Background(), source, first.Next, ""); !errors.Is(err, ErrChanged) {
		t.Fatalf("replaced source: %v", err)
	}
}

func TestClaudeBranchVersionAndOmissions(t *testing.T) {
	source, data := claudeFixture(t, 3)
	// Last branch points directly to message zero, abandoning messages one/two.
	line := `{"type":"assistant","uuid":"branch","parentUuid":"0","sessionId":"fixture-session","version":"2.1.270","message":{"content":[{"type":"text","text":"chosen branch"},{"type":"thinking","thinking":"excluded"},{"type":"tool_use","name":"Read","input":{"path":"example.txt"}}]}}` + "\n"
	_ = os.WriteFile(source.Transcript, append(data, []byte(line)...), 0600)
	page, err := Read(context.Background(), source, "", "")
	if err != nil || !page.Partial || strings.Contains(page.Text, "row-001") || strings.Contains(page.Text, "excluded") || !strings.Contains(page.Text, "[tool: Read]") {
		t.Fatalf("branch: %+v %v", page, err)
	}
	_ = os.WriteFile(source.Transcript, []byte(strings.ReplaceAll(string(data), "2.1.270", "9.0.0")), 0600)
	_, err = Read(context.Background(), source, "", "")
	if !errors.Is(err, ErrFormat) {
		t.Fatalf("unknown version: %v", err)
	}
	// ST-10 B: неподдержанная версия — отдельный код с самой версией, а не
	// безымянный «формат не поддержан».
	var version *VersionError
	if !errors.Is(err, ErrVersionUnsupported) || !errors.As(err, &version) || version.Agent != "claude" || version.Version != "9.0.0" {
		t.Fatalf("unsupported version is not named: %#v", err)
	}
	if err.Error() != "history_version_unsupported" {
		t.Fatalf("wire code %q", err.Error())
	}
}

// Версия из файла приходит извне: в код ответа уходит только короткая печатная
// строка, без переводов строк и управляющих байтов.
func TestVersionErrorIsBoundedAndPrintable(t *testing.T) {
	var version *VersionError
	err := versionError("claude", "9.0.0\n\x1b[31m"+strings.Repeat("7", 100))
	if !errors.As(err, &version) || strings.ContainsAny(version.Version, "\n\x1b ") || len(version.Version) > 32 || !strings.HasPrefix(version.Version, "9.0.0") {
		t.Fatalf("version not sanitized: %q", version.Version)
	}
	if errors.Is(err, ErrChanged) || errors.Is(err, ErrUnavailable) {
		t.Fatal("version error matched an unrelated code")
	}
}

func TestClaudeStructuralLinksAndPartialAppend(t *testing.T) {
	source, data := claudeFixture(t, 3)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	lines[1] = `{"type":"system","uuid":"1","parentUuid":"0","sessionId":"fixture-session"}`
	// The incomplete append is outside the last complete message chain.
	_ = os.WriteFile(source.Transcript, []byte(strings.Join(lines, "\n")+"\n"+`{"type":"assistant","message":`), 0600)
	page, err := Read(context.Background(), source, "", "")
	if err != nil || !page.Partial || !strings.Contains(page.Text, "row-000") || !strings.Contains(page.Text, "row-002") || strings.Contains(page.Text, "row-001") {
		t.Fatalf("structural link/append: %+v %v", page, err)
	}
	lines[1] = `{"type":"system","uuid":"1","parentUuid":"2","sessionId":"fixture-session"}`
	_ = os.WriteFile(source.Transcript, []byte(strings.Join(lines, "\n")+"\n"), 0600)
	if _, err := Read(context.Background(), source, "", ""); !errors.Is(err, ErrFormat) {
		t.Fatalf("cycle accepted: %v", err)
	}
}

func TestCodexSchemaAndBounds(t *testing.T) {
	raw := `{"data":[{"items":[{"type":"agentMessage","text":"newest"}]},{"items":[{"type":"userMessage","content":[{"type":"text","text":"  command\n中😀"}]},{"type":"commandExecution","command":"echo ok","aggregatedOutput":"ok"},{"type":"reasoning","summary":["excluded"]}]}],"nextCursor":"older"}`
	page, err := codexPage(Page{Source: "fixture"}, json.RawMessage(raw))
	if err != nil || !page.Partial || page.Next == "" || strings.Contains(page.Text, "excluded") || !strings.HasPrefix(page.Text, "[user]\n  command\n中😀") || !strings.HasSuffix(page.Text, "newest") {
		t.Fatalf("page: %+v %v", page, err)
	}
	large, _ := json.Marshal(map[string]any{"data": []any{map[string]any{"items": []any{map[string]any{"type": "agentMessage", "text": strings.Repeat("😀", MaxText)}}}}})
	page, err = codexPage(Page{}, large)
	if err != nil || !page.Partial || len(page.Text) > MaxText || !json.Valid([]byte(fmt.Sprintf("%q", page.Text))) {
		t.Fatal("text limit/UTF-8")
	}
}

// The subprocess stands in for the installed CLI and rejects every method
// except initialize and the documented read-only pagination method.
func TestMain(m *testing.M) {
	if os.Getenv("REMOTAI_HISTORY_TEST_CLI") == "1" {
		if len(os.Args) == 2 && os.Args[1] == "--version" {
			if reported := os.Getenv("REMOTAI_HISTORY_TEST_CLI_VERSION"); reported != "" {
				fmt.Println(reported)
				os.Exit(0)
			}
			fmt.Println("codex-cli 0.154.0")
			os.Exit(0)
		}
		if len(os.Args) != 4 || os.Args[1] != "app-server" {
			os.Exit(4)
		}
		scan := bufio.NewScanner(os.Stdin)
		for scan.Scan() {
			var request struct {
				ID     int
				Method string
				Params struct{ ThreadID, ItemsView, SortDirection string }
			}
			_ = json.Unmarshal(scan.Bytes(), &request)
			switch request.Method {
			case "initialize":
				fmt.Println(`{"id":1,"result":{"userAgent":"fixture"}}`)
			case "initialized":
			case "thread/turns/list":
				if request.Params.ThreadID != "synthetic-thread" || request.Params.ItemsView != "full" || request.Params.SortDirection != "desc" {
					os.Exit(5)
				}
				fmt.Println(`{"id":2,"result":{"data":[{"items":[{"type":"agentMessage","text":"fixture reply"}]}],"nextCursor":null}}`)
			default:
				os.Exit(6)
			}
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestCodexReadOnlyProcess(t *testing.T) {
	t.Setenv("REMOTAI_HISTORY_TEST_CLI", "1")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	page, err := Read(context.Background(), Source{Agent: "codex", SessionID: "synthetic-thread", ConfigHome: t.TempDir()}, "", exe)
	if err != nil || page.Text != "[assistant]\nfixture reply" {
		t.Fatalf("process: %+v %v", page, err)
	}
}

// Другая версия Codex CLI: чтение не начинается (app-server не запускается),
// а ответ называет версию. Незнакомый вывод --version остаётся «формат не
// поддержан» — выдумывать версию из мусора нельзя.
func TestCodexUnsupportedVersionIsNamed(t *testing.T) {
	t.Setenv("REMOTAI_HISTORY_TEST_CLI", "1")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	source := Source{Agent: "codex", SessionID: "synthetic-thread", ConfigHome: t.TempDir()}

	t.Setenv("REMOTAI_HISTORY_TEST_CLI_VERSION", "codex-cli 0.155.0")
	_, err = Read(context.Background(), source, "", exe)
	var version *VersionError
	if !errors.As(err, &version) || version.Agent != "codex" || version.Version != "0.155.0" || !errors.Is(err, ErrVersionUnsupported) {
		t.Fatalf("codex version not named: %#v", err)
	}

	t.Setenv("REMOTAI_HISTORY_TEST_CLI_VERSION", "something else entirely")
	_, err = Read(context.Background(), source, "", exe)
	if !errors.Is(err, ErrFormat) || errors.Is(err, ErrVersionUnsupported) {
		t.Fatalf("garbage --version must stay a format error: %#v", err)
	}
}
