package bundle

import (
	"os"
	"path/filepath"
	"testing"
)

// Ensure в пустом доме создаёт дерево и раскладывает скиллы; повторный вызов
// ничего не делает; изменённый скилл перезаписывается встроенным (обновление).
func TestEnsure(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)

	if err := Ensure(); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	for _, want := range []string{
		"files",
		filepath.Join("skills", "README.md"),
		filepath.Join("skills", "telegram", "SKILL.md"),
		filepath.Join("skills", "remotai", "SKILL.md"),
	} {
		if _, err := os.Stat(filepath.Join(tmp, "Remotai", want)); err != nil {
			t.Fatalf("нет %s: %v", want, err)
		}
	}

	// Идемпотентность: mtime нетронутого файла не меняется.
	skillPath := filepath.Join(tmp, "Remotai", "skills", "telegram", "SKILL.md")
	st1, _ := os.Stat(skillPath)
	if err := Ensure(); err != nil {
		t.Fatalf("Ensure повтор: %v", err)
	}
	st2, _ := os.Stat(skillPath)
	if st1.ModTime() != st2.ModTime() {
		t.Fatal("повторный Ensure перезаписал совпадающий файл")
	}

	// Изменённый/удалённый скилл восстанавливается из встроенного.
	if err := os.WriteFile(skillPath, []byte("чужая правка"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Ensure(); err != nil {
		t.Fatalf("Ensure после правки: %v", err)
	}
	content, _ := os.ReadFile(skillPath)
	if string(content) == "чужая правка" {
		t.Fatal("изменённый скилл не перезаписан встроенным")
	}
}
