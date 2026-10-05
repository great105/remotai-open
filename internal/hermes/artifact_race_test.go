package hermes

import (
	"os"
	"path/filepath"
	"testing"
)

func TestArtifactComponentReplacementNeverFollowsPrivateAlias(t *testing.T) {
	for _, parent := range []bool{false, true} {
		t.Run(map[bool]string{false: "leaf", true: "parent"}[parent], func(t *testing.T) {
			root := t.TempDir()
			os.WriteFile(filepath.Join(root, ".env"), []byte("private fixture"), 0600)
			os.Mkdir(filepath.Join(root, "safe"), 0700)
			os.WriteFile(filepath.Join(root, "safe", "report.txt"), []byte("public fixture"), 0600)
			path := "safe/report.txt"
			target := "safe/report.txt"
			if parent {
				target = "safe"
			}
			replacement := filepath.Join(root, target)
			// Validate symlink capability before the actual deterministic swap.
			probe := filepath.Join(root, "probe")
			if err := os.Symlink(".env", probe); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			os.Remove(probe)
			f, err := openArtifactNoFollow(root, filepath.FromSlash(path), func(component string) {
				if component != filepath.Base(target) {
					return
				}
				if parent {
					os.Rename(replacement, replacement+"-old")
					os.Mkdir(filepath.Join(root, ".private"), 0700)
					os.WriteFile(filepath.Join(root, ".private", "report.txt"), []byte("private fixture"), 0600)
					if e := os.Symlink(filepath.Join(root, ".private"), replacement); e != nil {
						t.Fatal(e)
					}
				} else {
					os.Remove(replacement)
					if e := os.Symlink(filepath.Join(root, ".env"), replacement); e != nil {
						t.Fatal(e)
					}
				}
			})
			if f != nil {
				f.Close()
			}
			if err == nil {
				t.Fatal("replacement link was opened")
			}
		})
	}
}

func TestArtifactPinnedParentSurvivesReplacement(t *testing.T) {
	root := t.TempDir()
	os.Mkdir(filepath.Join(root, "safe"), 0700)
	os.WriteFile(filepath.Join(root, "safe", "report.txt"), []byte("public fixture"), 0600)
	probe := filepath.Join(root, "probe")
	if err := os.Symlink("safe", probe); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	os.Remove(probe)
	f, err := openArtifactNoFollow(root, filepath.FromSlash("safe/report.txt"), func(component string) {
		if component != "report.txt" {
			return
		}
		if e := os.Rename(filepath.Join(root, "safe"), filepath.Join(root, "old")); e != nil {
			t.Fatal(e)
		}
		os.Mkdir(filepath.Join(root, ".private"), 0700)
		os.WriteFile(filepath.Join(root, ".private", "report.txt"), []byte("private fixture"), 0600)
		if e := os.Symlink(filepath.Join(root, ".private"), filepath.Join(root, "safe")); e != nil {
			t.Fatal(e)
		}
	})
	if err != nil {
		return
	}
	defer f.Close()
	data := make([]byte, 64)
	n, _ := f.Read(data)
	if string(data[:n]) != "public fixture" {
		t.Fatal("parent replacement redirected pinned handle")
	}
}
