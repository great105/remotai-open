//go:build windows

package pty

import (
	"strings"
	"syscall"
	"testing"
	"time"
)

// procEntry builds a snapshot entry with the given pid/parent/exe name.
func mkEntry(pid, ppid uint32, exe string) processEntry32W {
	e := processEntry32W{ProcessID: pid, ParentProcessID: ppid}
	copy(e.ExeFile[:], syscall.StringToUTF16(exe))
	return e
}

func buildChildren(entries ...processEntry32W) map[uint32][]processEntry32W {
	m := map[uint32][]processEntry32W{}
	for _, e := range entries {
		m[e.ParentProcessID] = append(m[e.ParentProcessID], e)
	}
	return m
}

// TestForegroundPrefersAgent reproduces the real tree of an AI-agent terminal:
// shell → claude.exe → long-lived MCP helper children (node/cmd/pwsh). The
// foreground must resolve to claude (so the status badge reflects the agent and
// can flip to waiting/idle), NOT the deepest MCP leaf (which pinned it to
// "working" forever — the reported bug).
func TestForegroundPrefersAgent(t *testing.T) {
	const shell = 100
	children := buildChildren(
		mkEntry(200, shell, "powershell.exe"), // shell itself is child of host
		mkEntry(300, 200, "claude.exe"),       // the agent
		mkEntry(400, 300, "node.exe"),         // MCP server (context7)
		mkEntry(401, 300, "cmd.exe"),          // MCP wrapper
		mkEntry(402, 300, "pwsh.exe"),         // MCP server (last child)
		mkEntry(500, 400, "node.exe"),         // deeper MCP leaf
	)

	leaf := pickAgentLeaf(200, children)
	if leaf == nil {
		t.Fatalf("pickAgentLeaf returned nil; expected claude")
	}
	if got := strings.TrimSuffix(strings.ToLower(syscall.UTF16ToString(leaf.entry.ExeFile[:])), ".exe"); got != "claude" {
		t.Fatalf("foreground = %q, want claude", got)
	}
	if leaf.kind != "claude" {
		t.Fatalf("resolved kind = %q, want claude", leaf.kind)
	}

	// Sanity: the old deepest-leaf walk would have picked a non-agent helper.
	if deep := pickDeepestLeaf(200, children); deep != nil {
		if got := strings.TrimSuffix(strings.ToLower(syscall.UTF16ToString(deep.ExeFile[:])), ".exe"); got == "claude" {
			t.Fatalf("deepest leaf unexpectedly = claude; test no longer guards the regression")
		}
	}
}

// TestForegroundPlainCommandUsesDeepestLeaf ensures non-agent sessions keep the
// original deepest-leaf behaviour (e.g. `npm run build` → node).
func TestForegroundPlainCommandUsesDeepestLeaf(t *testing.T) {
	const shell = 100
	children := buildChildren(
		mkEntry(200, shell, "powershell.exe"),
		mkEntry(300, 200, "node.exe"),
	)
	if a := pickAgentLeaf(200, children); a != nil {
		t.Fatalf("pickAgentLeaf should be nil for a non-agent tree, got pid=%d", a.entry.ProcessID)
	}
	leaf := pickDeepestLeaf(200, children)
	if leaf == nil || strings.TrimSuffix(strings.ToLower(syscall.UTF16ToString(leaf.ExeFile[:])), ".exe") != "node" {
		t.Fatalf("expected deepest leaf = node")
	}
}

func TestWindowsConPTYEcho(t *testing.T) {
	m := NewLocalManager()
	sess, err := m.Create(0, `C:\`, "cmd", 80, 24)
	if err != nil {
		t.Fatalf("Create PTY: %v", err)
	}
	defer func() {
		if err := m.Close(sess.ID); err != nil {
			t.Logf("Close PTY: %v", err)
		}
	}()

	ch, scrollback := sess.Subscribe()
	defer sess.Unsubscribe(ch)

	marker := "tgc-test-pty-echo"
	var output strings.Builder
	output.Write(scrollback)

	if _, err := sess.Write([]byte("echo " + marker + "\r")); err != nil {
		t.Fatalf("Write PTY: %v", err)
	}

	deadline := time.After(8 * time.Second)
	for {
		if strings.Count(output.String(), marker) >= 2 {
			return
		}
		select {
		case data, ok := <-ch:
			if !ok {
				t.Fatalf("PTY output channel closed before echo; output=%q", output.String())
			}
			output.Write(data)
		case <-sess.Done():
			t.Fatalf("PTY exited before echo; output=%q", output.String())
		case <-deadline:
			t.Fatalf("PTY did not echo marker; output=%q", output.String())
		}
	}
}
