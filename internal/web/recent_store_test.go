package web

import (
	"path/filepath"
	"testing"
)

// Главное свойство истории: она переживает закрытие терминала и перезапуск
// агента. Ради него стор и заводился — до него «Недавние» выводились из живых
// сессий и исчезали вместе с ними («недавнее не всегда недавние», владелец
// 01.09.2026).
func TestRecentStoreSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "recent-folders.json")

	s := newRecentStoreAt(path)
	alpha := filepath.Join(dir, "work", "alpha")
	beta := filepath.Join(dir, "work", "beta")
	s.Note(1, alpha)
	s.Note(1, beta)

	// Новый стор — как после перезапуска remotai.exe.
	again := newRecentStoreAt(path)
	got := again.List(1)
	if len(got) != 2 {
		t.Fatalf("после перезапуска история потеряна: %+v", got)
	}
	if got[0].Path != beta {
		t.Errorf("свежая папка обязана быть первой, получили %q", got[0].Path)
	}
	if got[0].Name != "beta" {
		t.Errorf("имя = последний сегмент пути, получили %q", got[0].Name)
	}
}

// Повторное открытие той же папки не плодит строки, а поднимает её наверх:
// иначе история из десяти строк состояла бы из одной папки, открытой десять раз.
func TestRecentStoreDedupesAndPromotes(t *testing.T) {
	s := newRecentStoreAt(filepath.Join(t.TempDir(), "r.json"))
	s.Note(1, `C:\a`)
	s.Note(1, `C:\b`)
	s.Note(1, `C:\a`)

	got := s.List(1)
	if len(got) != 2 {
		t.Fatalf("ожидали 2 записи без дублей, получили %d: %+v", len(got), got)
	}
	if got[0].Path != `C:\a` {
		t.Errorf("повторно открытая папка обязана подняться наверх, получили %q", got[0].Path)
	}
}

// Потолок держит список коротким: «недавние» перестают быть недавними, если их
// сорок экранов.
func TestRecentStoreCapsLength(t *testing.T) {
	s := newRecentStoreAt(filepath.Join(t.TempDir(), "r.json"))
	for i := 0; i < recentPerUserMax+10; i++ {
		s.Note(1, filepath.Join(`C:\p`, string(rune('a'+i%26))+string(rune('0'+i/26))))
	}
	if got := len(s.List(1)); got > recentPerUserMax {
		t.Errorf("список не обрезан: %d > %d", got, recentPerUserMax)
	}
}

// Пустой путь в историю не попадает: терминал без cwd — не место работы.
func TestRecentStoreIgnoresEmpty(t *testing.T) {
	s := newRecentStoreAt(filepath.Join(t.TempDir(), "r.json"))
	s.Note(1, "")
	s.Note(1, ".")
	if got := s.List(1); len(got) != 0 {
		t.Errorf("пустые пути попали в историю: %+v", got)
	}
}

// Истории разных пользователей не смешиваются.
func TestRecentStoreSeparatesUsers(t *testing.T) {
	s := newRecentStoreAt(filepath.Join(t.TempDir(), "r.json"))
	s.Note(1, `C:\one`)
	s.Note(2, `C:\two`)
	if len(s.List(1)) != 1 || s.List(1)[0].Path != `C:\one` {
		t.Errorf("история первого пользователя испорчена: %+v", s.List(1))
	}
	if len(s.List(2)) != 1 || s.List(2)[0].Path != `C:\two` {
		t.Errorf("история второго пользователя испорчена: %+v", s.List(2))
	}
}
