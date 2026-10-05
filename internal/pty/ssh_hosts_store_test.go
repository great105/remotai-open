package pty

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestSSHHostStoreCRUD(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ssh_hosts.json")
	s := NewSSHHostStore(path)

	if got := s.List(); len(got) != 0 {
		t.Fatalf("List = %v, want empty", got)
	}

	h, err := s.Add(SavedSSHHost{
		Name: "prod", Host: "10.0.0.1", Port: 2222, User: "deploy",
		ProxyJump: "admin@bastion", Tags: []string{"prod"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if h.ID == "" {
		t.Fatal("Add не присвоил id")
	}

	// Перечитали с диска — данные на месте.
	s2 := NewSSHHostStore(path)
	got, err := s2.Get(h.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "prod" || got.Port != 2222 || !reflect.DeepEqual(got.Tags, []string{"prod"}) {
		t.Fatalf("Get = %+v", got)
	}

	// PATCH: меняется только указанное поле.
	name := "prod-renamed"
	upd, err := s2.Update(h.ID, SSHHostPatch{Name: &name})
	if err != nil {
		t.Fatal(err)
	}
	if upd.Name != "prod-renamed" || upd.Host != "10.0.0.1" {
		t.Fatalf("Update = %+v", upd)
	}

	// Несуществующий id — ErrSSHHostNotFound и на Update, и на Delete.
	if _, err := s2.Update("nope", SSHHostPatch{Name: &name}); err != ErrSSHHostNotFound {
		t.Fatalf("Update nope: %v", err)
	}
	if err := s2.Delete("nope"); err != ErrSSHHostNotFound {
		t.Fatalf("Delete nope: %v", err)
	}

	if err := s2.Delete(h.ID); err != nil {
		t.Fatal(err)
	}
	if got := NewSSHHostStore(path).List(); len(got) != 0 {
		t.Fatalf("после Delete List = %v", got)
	}
}

func TestSSHHistoryStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ssh_history.json")
	s := NewSSHHistoryStore(path)
	base := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return base }

	// Порт 0 нормализуется в 22.
	if err := s.Record("10.0.0.1", 0, "deploy", ""); err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return base.Add(time.Hour) }
	if err := s.Record("10.0.0.1", 22, "deploy", "jump@bastion"); err != nil { // тот же ключ
		t.Fatal(err)
	}
	if err := s.Record("10.0.0.2", 22, "root", ""); err != nil {
		t.Fatal(err)
	}

	list := NewSSHHistoryStore(path).List(20)
	if len(list) != 2 {
		t.Fatalf("List = %+v", list)
	}
	// Свежие сверху; у 10.0.0.1 count=2 (две записи схлопнулись по ключу).
	if list[0].Host != "10.0.0.1" || list[0].Count != 2 || list[0].Port != 22 {
		t.Fatalf("list[0] = %+v", list[0])
	}
	if list[1].Host != "10.0.0.2" || list[1].Count != 1 {
		t.Fatalf("list[1] = %+v", list[1])
	}

	// Лимит работает.
	if got := s.List(1); len(got) != 1 {
		t.Fatalf("List(1) = %+v", got)
	}

	// Snapshot для мержа с хостами.
	snap := s.Snapshot()
	e, ok := snap[SSHHistoryKey("deploy", "10.0.0.1", 22)]
	if !ok || e.Count != 2 {
		t.Fatalf("Snapshot = %+v", snap)
	}
}
