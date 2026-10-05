package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ── Политика / матчинг глобов ─────────────────────────────────────────

func TestMatchProtectedPathDefaults(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		path string
		want bool
	}{
		{".env", true},
		{".env.local", true},
		{"sub/.env", true},
		{"sub/dir/.env.prod", true},
		{"../.env", true}, // нормализация резолвит .., гейт всё равно ловит
		{"deploy.key", true},
		{"certs/server.pem", true},
		{"migrations/001_init.sql", true},
		{"migrations", true}, // глоб директории матчит и её саму
		{"db/migrations/002.sql", true},
		{".git/config", true},
		{".git", true},
		// Негатив: обычные пути не затронуты.
		{"main.go", false},
		{"internal/bot/bot.go", false},
		{"env", false},
		{"config.yaml", false},
		{"backup.key2", false},       // *.key не матчит .key2
		{"migrationsX/1.sql", false}, // сегментная граница: migrations ≠ migrationsX
		{"git/HEAD", false},
	}
	for _, c := range cases {
		if got := matchProtectedPath(c.path, dir, DefaultProtectedGlobs); got != c.want {
			t.Errorf("matchProtectedPath(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

func TestMatchProtectedPathNoOvermatch(t *testing.T) {
	dir := t.TempDir()
	globs := []string{"vendor"}
	if !matchProtectedPath("vendor/lib/x.go", dir, globs) {
		t.Error("vendor/lib/x.go must be protected by glob 'vendor'")
	}
	if matchProtectedPath("vendor2/lib/x.go", dir, globs) {
		t.Error("vendor2 must NOT be protected by glob 'vendor' (prefix over-match)")
	}
}

func TestMatchProtectedPathCustomGlobs(t *testing.T) {
	dir := t.TempDir()
	globs := []string{"secrets/**", "Makefile"}
	if !matchProtectedPath("secrets/prod/token.txt", dir, globs) {
		t.Error("secrets/** must cover nested files")
	}
	if !matchProtectedPath("Makefile", dir, globs) {
		t.Error("exact file glob must match")
	}
	if matchProtectedPath("src/Makefile", dir, globs) {
		t.Error("root-anchored glob must not match nested file")
	}
	if matchProtectedPath("other/x", dir, globs) {
		t.Error("unrelated path must stay unprotected")
	}
	// Пустой срез — защита отключена.
	if matchProtectedPath(".env", dir, []string{}) {
		t.Error("empty globs must protect nothing")
	}
}

// ── Эвристика упоминаний в командах/промптах ──────────────────────────

func TestMentionsProtected(t *testing.T) {
	dir := t.TempDir()
	globs := DefaultProtectedGlobs
	hits := []string{
		"cat migrations/001.sql",
		"type .env",
		"cp config/.env.local /tmp/x",
		"rm -rf .git",
		"echo secret > api.key",
		"git --git-dir=.git log", // флаги и = не мешают
	}
	for _, c := range hits {
		if tok, hit := mentionsProtected(c, dir, globs); !hit {
			t.Errorf("mentionsProtected(%q) = miss, want hit", c)
		} else if tok == "" {
			t.Errorf("mentionsProtected(%q): empty token", c)
		}
	}
	misses := []string{
		"go build ./...",
		"echo hello world",
		"git status --porcelain",
		"ls -la",
		"cat main.go",
	}
	for _, c := range misses {
		if _, hit := mentionsProtected(c, dir, globs); hit {
			t.Errorf("mentionsProtected(%q) = hit, want miss", c)
		}
	}
}

// ── Гейт execTool ─────────────────────────────────────────────────────

func gateTestOrchestrator(confirm ConfirmFunc) *Orchestrator {
	o := New("test-api-key", "sonnet", stubAgentOK("agent done", 0))
	if confirm != nil {
		o.SetConfirmFunc(confirm)
	}
	return o
}

func TestGateWriteFile(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	t.Run("no confirm func denies", func(t *testing.T) {
		o := gateTestOrchestrator(nil)
		_, errMsg := o.execTool(ctx, "write_file", map[string]any{"path": ".env", "content": "SECRET=1"}, dir)
		if !strings.Contains(errMsg, "PROTECTED") {
			t.Fatalf("errMsg = %q, want PROTECTED refusal", errMsg)
		}
		if _, err := os.Stat(filepath.Join(dir, ".env")); !os.IsNotExist(err) {
			t.Error("file must NOT be written without human approval")
		}
	})

	t.Run("confirm allow passes", func(t *testing.T) {
		var asked string
		o := gateTestOrchestrator(func(what string) bool { asked = what; return true })
		out, errMsg := o.execTool(ctx, "write_file", map[string]any{"path": ".env", "content": "SECRET=1"}, dir)
		if errMsg != "" {
			t.Fatalf("errMsg = %q (out %q)", errMsg, out)
		}
		if !strings.Contains(asked, ".env") {
			t.Errorf("confirm was asked about %q, want mention of .env", asked)
		}
		data, err := os.ReadFile(filepath.Join(dir, ".env"))
		if err != nil || string(data) != "SECRET=1" {
			t.Errorf("written content: %q, err %v", data, err)
		}
	})

	t.Run("confirm deny blocks", func(t *testing.T) {
		o := gateTestOrchestrator(func(string) bool { return false })
		_, errMsg := o.execTool(ctx, "write_file", map[string]any{"path": "db/migrations/002.sql", "content": "SELECT 1"}, dir)
		if !strings.Contains(errMsg, "PROTECTED") {
			t.Fatalf("errMsg = %q, want PROTECTED refusal", errMsg)
		}
		if _, err := os.Stat(filepath.Join(dir, "db/migrations/002.sql")); !os.IsNotExist(err) {
			t.Error("denied write must not create the file")
		}
	})

	t.Run("normal path unaffected without confirm", func(t *testing.T) {
		o := gateTestOrchestrator(nil)
		out, errMsg := o.execTool(ctx, "write_file", map[string]any{"path": "src/main.go", "content": "package main"}, dir)
		if errMsg != "" {
			t.Fatalf("errMsg = %q (out %q)", errMsg, out)
		}
	})
}

func TestGateRunCommand(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	t.Run("protected mention denied without confirm", func(t *testing.T) {
		o := gateTestOrchestrator(nil)
		_, errMsg := o.execTool(ctx, "run_command", map[string]any{"command": "echo ok && type .env"}, dir)
		if !strings.Contains(errMsg, "PROTECTED") {
			t.Fatalf("errMsg = %q, want PROTECTED refusal", errMsg)
		}
	})

	t.Run("confirm allow executes", func(t *testing.T) {
		o := gateTestOrchestrator(func(string) bool { return true })
		out, errMsg := o.execTool(ctx, "run_command", map[string]any{"command": "echo gated-ok && echo .env"}, dir)
		if errMsg != "" {
			t.Fatalf("errMsg = %q (out %q)", errMsg, out)
		}
		if !strings.Contains(out, "gated-ok") {
			t.Errorf("out = %q, want the command output", out)
		}
	})

	t.Run("plain command unaffected", func(t *testing.T) {
		o := gateTestOrchestrator(nil)
		out, errMsg := o.execTool(ctx, "run_command", map[string]any{"command": "echo plain-ok"}, dir)
		if errMsg != "" {
			t.Fatalf("errMsg = %q (out %q)", errMsg, out)
		}
		if !strings.Contains(out, "plain-ok") {
			t.Errorf("out = %q", out)
		}
	})
}

func TestGateRunAgent(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	t.Run("protected prompt denied without confirm", func(t *testing.T) {
		o := gateTestOrchestrator(nil)
		_, errMsg := o.execTool(ctx, "run_agent", map[string]any{
			"agent": "shell", "prompt": "обнови migrations/001.sql под новую схему",
		}, dir)
		if !strings.Contains(errMsg, "PROTECTED") {
			t.Fatalf("errMsg = %q, want PROTECTED refusal", errMsg)
		}
	})

	t.Run("confirm allow delegates", func(t *testing.T) {
		o := gateTestOrchestrator(func(string) bool { return true })
		out, errMsg := o.execTool(ctx, "run_agent", map[string]any{
			"agent": "shell", "prompt": "посмотри migrations/001.sql и опиши схему",
		}, dir)
		if errMsg != "" {
			t.Fatalf("errMsg = %q (out %q)", errMsg, out)
		}
		if out != "agent done" {
			t.Errorf("out = %q, want stub agent reply", out)
		}
	})

	t.Run("plain prompt unaffected", func(t *testing.T) {
		o := gateTestOrchestrator(nil)
		_, errMsg := o.execTool(ctx, "run_agent", map[string]any{
			"agent": "shell", "prompt": "отформатируй main.go",
		}, dir)
		if errMsg != "" {
			t.Fatalf("errMsg = %q", errMsg)
		}
	})
}

func TestGateCustomGlobsAndDisable(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	o := gateTestOrchestrator(nil)
	o.SetProtectedGlobs([]string{"secrets/**"})
	if _, errMsg := o.execTool(ctx, "write_file", map[string]any{"path": "secrets/x.txt", "content": "1"}, dir); !strings.Contains(errMsg, "PROTECTED") {
		t.Fatalf("custom glob: errMsg = %q, want PROTECTED", errMsg)
	}
	// Дефолтно-защищённый путь вне кастомной политики — не гейтится.
	if _, errMsg := o.execTool(ctx, "write_file", map[string]any{"path": ".env", "content": "1"}, dir); errMsg != "" {
		t.Fatalf("custom globs replace defaults: errMsg = %q", errMsg)
	}

	o.SetProtectedGlobs([]string{}) // защита отключена
	if _, errMsg := o.execTool(ctx, "write_file", map[string]any{"path": "secrets/y.txt", "content": "1"}, dir); errMsg != "" {
		t.Fatalf("empty globs disable protection: errMsg = %q", errMsg)
	}
}
