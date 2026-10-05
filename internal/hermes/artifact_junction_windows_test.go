package hermes

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"tgcontrol/internal/procutil"
	"time"
)

func TestArtifactRejectsRealWindowsJunctionAndRootAlias(t *testing.T) {
	root := t.TempDir()
	private := filepath.Join(root, ".private")
	if err := os.Mkdir(private, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(private, "report.txt"), []byte("private junction fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "safe")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "cmd.exe", "/c", "mklink", "/J", alias, private)
	procutil.Hidden(cmd)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture junction creation failed: %v %s", err, output)
	}
	if data, _, err := scopedArtifact(root, filepath.Join("safe", "report.txt")); err == nil {
		t.Fatalf("junction followed: %d bytes", len(data))
	}
	if data, _, err := scopedArtifact(alias, "report.txt"); err == nil {
		t.Fatalf("root junction followed: %d bytes", len(data))
	}
}
