package pty

import (
	"path/filepath"
	"slices"
	"testing"
)

// SetPlacement: папка и ручной порядок переживают перечитывание файла
// (клиентский drag-and-drop в списке терминалов хранится здесь же, в pty.json).
func TestMetaStoreSetPlacementPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pty.json")
	s := NewMetaStoreAt(path)

	if err := s.SetPlacement("sess1", "работа", 20); err != nil {
		t.Fatalf("SetPlacement: %v", err)
	}

	reloaded := NewMetaStoreAt(path)
	m := reloaded.Get("sess1")
	if m.Group != "работа" || m.Sort != 20 {
		t.Fatalf("после перечитывания Group=%q Sort=%v, ожидалось работа/20", m.Group, m.Sort)
	}

	// Очистка папки: запись без имени/host/group должна исчезнуть совсем.
	if err := reloaded.SetPlacement("sess1", "", 0); err != nil {
		t.Fatalf("SetPlacement clear: %v", err)
	}
	m = NewMetaStoreAt(path).Get("sess1")
	if m.Group != "" || m.Sort != 0 {
		t.Fatalf("после очистки Group=%q Sort=%v, ожидались нули", m.Group, m.Sort)
	}
}

// Ручной порядок нужен и в «Без папки»: group="" не означает, что placement
// пустой, если sort задан.
func TestMetaStoreUngroupedSortPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pty.json")
	s := NewMetaStoreAt(path)

	if err := s.SetPlacement("sess1", "", 30); err != nil {
		t.Fatalf("SetPlacement: %v", err)
	}
	m := NewMetaStoreAt(path).Get("sess1")
	if m.Group != "" || m.Sort != 30 {
		t.Fatalf("после перечитывания Group=%q Sort=%v, ожидалось пусто/30", m.Group, m.Sort)
	}

	if err := s.SetPlacement("sess1", "", 0); err != nil {
		t.Fatalf("SetPlacement clear: %v", err)
	}
	m = NewMetaStoreAt(path).Get("sess1")
	if m.Sort != 0 {
		t.Fatalf("после очистки Sort=%v, ожидался 0", m.Sort)
	}
}

// SetName не должен затирать placement и наоборот — поля независимы.
func TestMetaStoreNameAndPlacementIndependent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pty.json")
	s := NewMetaStoreAt(path)

	if err := s.SetPlacement("s1", "дом", 10); err != nil {
		t.Fatal(err)
	}
	if err := s.SetName("s1", "боевой"); err != nil {
		t.Fatal(err)
	}
	m := s.Get("s1")
	if m.Name != "боевой" || m.Group != "дом" || m.Sort != 10 {
		t.Fatalf("SetName затёр placement: %+v", m)
	}

	// Очистка имени не должна удалить запись, пока есть group.
	if err := s.SetName("s1", ""); err != nil {
		t.Fatal(err)
	}
	m = s.Get("s1")
	if m.Group != "дом" || m.Sort != 10 {
		t.Fatalf("очистка имени потеряла placement: %+v", m)
	}

	// То же для ручного порядка в «Без папки».
	if err := s.SetPlacement("s2", "", 40); err != nil {
		t.Fatal(err)
	}
	if err := s.SetName("s2", "временное"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetName("s2", ""); err != nil {
		t.Fatal(err)
	}
	m = NewMetaStoreAt(path).Get("s2")
	if m.Sort != 40 {
		t.Fatalf("очистка имени потеряла ungrouped sort: %+v", m)
	}
}

func TestMetaStoreFoldersPersistWithoutSessions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pty.json")
	s := NewMetaStoreAt(path)

	if err := s.SetFolders([]string{"Работа", "Личное", "Работа", ""}); err != nil {
		t.Fatal(err)
	}
	if got := NewMetaStoreAt(path).Folders(); !slices.Equal(got, []string{"Работа", "Личное"}) {
		t.Fatalf("folders after reload = %v, want [Работа Личное]", got)
	}

	if err := s.SetFolders(nil); err != nil {
		t.Fatal(err)
	}
	if got := NewMetaStoreAt(path).Folders(); len(got) != 0 {
		t.Fatalf("folders were not cleared: %v", got)
	}
}
