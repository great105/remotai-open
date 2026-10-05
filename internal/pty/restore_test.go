package pty

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Терминал, не переживший ПЕРЕЗАГРУЗКУ компьютера, обязан остаться в списке —
// с именем, папкой и рабочим каталогом. До этого reattach удалял запись, и
// человек, включив компьютер, встречал пустой список.
func TestMetaStore_MarkLostKeepsWork(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pty.json")
	s := NewMetaStoreAt(path)

	if err := s.PutHost("abc", hostRecord{
		HostPID: 42, PipeName: "p", CWD: `C:\work\proj`, Shell: "powershell.exe",
		Created: 1000, UID: 7,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetName("abc", "Ночная сборка"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPlacement("abc", "Работа", 10); err != nil {
		t.Fatal(err)
	}
	if err := s.SetModes("abc", []int{1049}); err != nil {
		t.Fatal(err)
	}

	lostAt := time.UnixMilli(2000)
	if err := s.MarkLost("abc", lostAt); err != nil {
		t.Fatal(err)
	}

	// Перезапуск агента: свежий store читает тот же файл.
	s2 := NewMetaStoreAt(path)
	m := s2.Get("abc")
	if m.Lost == nil {
		t.Fatal("запись о потерянном терминале не пережила перезапуск")
	}
	if m.Host != nil {
		t.Fatal("мёртвый хост остался в записи — reattach будет ходить к нему вечно")
	}
	if m.Name != "Ночная сборка" || m.Group != "Работа" || m.Sort != 10 {
		t.Fatalf("имя/папка/порядок потеряны: %+v", m)
	}
	if m.Lost.CWD != `C:\work\proj` || m.Lost.Shell != "powershell.exe" || m.Lost.UID != 7 {
		t.Fatalf("рабочий каталог или шелл потеряны: %+v", m.Lost)
	}
	if m.Lost.Created != 1000 || m.Lost.LostAt != 2000 {
		t.Fatalf("времена потеряны: %+v", m.Lost)
	}
	// Режимы принадлежали умершему процессу — новый терминал начинает с чистого
	// экрана, иначе клиент восстановит alt-screen поверх обычного вывода.
	if m.Modes != nil {
		t.Fatalf("DEC-режимы мёртвого процесса сохранились: %v", m.Modes)
	}
}

// Переименование терминала, ждущего восстановления, не должно стирать запись:
// isEmpty() обязан считать Lost содержимым.
func TestMetaStore_LostSurvivesRename(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pty.json")
	s := NewMetaStoreAt(path)
	if err := s.PutHost("abc", hostRecord{HostPID: 1, CWD: "c", Shell: "sh"}); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkLost("abc", time.Now()); err != nil {
		t.Fatal(err)
	}
	// Человек стёр имя — запись обязана остаться.
	if err := s.SetName("abc", ""); err != nil {
		t.Fatal(err)
	}
	if NewMetaStoreAt(path).Get("abc").Lost == nil {
		t.Fatal("очистка имени удалила терминал, ждущий восстановления")
	}
}

// ClearLost снимает пометку, но оставляет имя и папку: терминал подняли заново
// под тем же id, и карточка обязана остаться прежней.
func TestMetaStore_ClearLostKeepsName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pty.json")
	s := NewMetaStoreAt(path)
	if err := s.PutHost("abc", hostRecord{HostPID: 1, CWD: "c", Shell: "sh"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetName("abc", "Сборка"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkLost("abc", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearLost("abc"); err != nil {
		t.Fatal(err)
	}
	m := NewMetaStoreAt(path).Get("abc")
	if m.Lost != nil {
		t.Fatal("пометка «ждёт восстановления» осталась на живом терминале")
	}
	if m.Name != "Сборка" {
		t.Fatalf("имя потеряно при восстановлении: %q", m.Name)
	}
}

// Хвост вывода — единственное, что остаётся от работы после выключения
// компьютера. Пишем только изменившееся, режем до последних байт, забываем
// вместе с терминалом.
func TestScrollbackStore(t *testing.T) {
	dir := t.TempDir()
	st := newScrollbackStoreAt(filepath.Join(dir, "pty-scrollback"))

	if st.has("abc") {
		t.Fatal("хвост нашёлся до первой записи")
	}
	if err := st.save("abc", []byte("первый вывод"), 12); err != nil {
		t.Fatal(err)
	}
	if !st.has("abc") || string(st.load("abc")) != "первый вывод" {
		t.Fatalf("хвост не сохранился: %q", st.load("abc"))
	}

	// Ничего не изменилось — второй записи быть не должно (файл не трогаем).
	path := st.path("abc")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if err := st.save("abc", []byte("другое, но total тот же"), 12); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(path)
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("файл перезаписан, хотя сессия ничего не вывела")
	}

	// Длинный вывод режется до хвоста.
	long := make([]byte, scrollbackKeepBytes+5000)
	for i := range long {
		long[i] = byte('a' + i%26)
	}
	if err := st.save("abc", long, 99999); err != nil {
		t.Fatal(err)
	}
	got := st.load("abc")
	if len(got) != scrollbackKeepBytes {
		t.Fatalf("сохранено %d байт, ждали %d", len(got), scrollbackKeepBytes)
	}
	if string(got) != string(long[len(long)-scrollbackKeepBytes:]) {
		t.Fatal("сохранён не хвост, а начало вывода")
	}

	st.forget("abc")
	if st.has("abc") || st.load("abc") != nil {
		t.Fatal("хвост пережил закрытие терминала")
	}
}

// Черта между архивом и новой работой обязана быть видимой и датированной:
// иначе человек примет прошлый вывод за живой.
func TestRestoreDivider(t *testing.T) {
	line := string(restoreDivider(time.Date(2026, 7, 27, 21, 5, 0, 0, time.Local).UnixMilli()))
	if !strings.Contains(line, "27.07 21:05") {
		t.Fatalf("в черте нет времени выключения: %q", line)
	}
	if !strings.Contains(line, "до выключения компьютера") {
		t.Fatalf("черта не объясняет, что выше: %q", line)
	}
	// Без времени (старая запись) черта всё равно рисуется.
	if got := string(restoreDivider(0)); !strings.Contains(got, "──") {
		t.Fatalf("черта пропала без времени: %q", got)
	}
}

// Папка могла исчезнуть вместе с внешним диском: восстановление обязано найти
// ближайшую существующую выше, а не отказать.
func TestNearestExistingDir(t *testing.T) {
	root := t.TempDir()
	deep := filepath.Join(root, "proj", "src")
	if err := os.MkdirAll(deep, 0700); err != nil {
		t.Fatal(err)
	}
	if got := nearestExistingDir(deep); got != deep {
		t.Fatalf("существующая папка подменена: %q", got)
	}
	gone := filepath.Join(deep, "нет", "и", "не", "было")
	if got := nearestExistingDir(gone); got != deep {
		t.Fatalf("не поднялись до существующей папки: %q", got)
	}
	if got := nearestExistingDir(""); got != "" {
		t.Fatalf("пустой путь дал %q", got)
	}
}

// Архив прошлой работы кладётся ПЕРЕД живым выводом и учитывается в счётчике
// потока.
//
// Раньше тест требовал обратного — «счётчик не сдвигать», и это было ошибкой,
// стоившей истории на экране. Кольцо и счётчик связаны инвариантом
// len(buf) ≤ totalBytes: на нём стоит вся арифметика резюме, а считается она в
// беззнаковой. Архив, не учтённый в счётчике, делал `total - len(buf)` числом
// под 2^64 — и у терминала, восстановленного после перезагрузки ПК, КАЖДОЕ
// переподключение приходило полным reset, то есть стирало прокрученное.
func TestSeedScrollback(t *testing.T) {
	s := &Session{buf: []byte("живой вывод"), totalBytes: uint64(len("живой вывод"))}
	archive := []byte("архив\n")
	s.seedScrollback(archive)
	if string(s.buf) != "архив\nживой вывод" {
		t.Fatalf("порядок нарушен: %q", s.buf)
	}
	if s.totalBytes != uint64(len(s.buf)) {
		t.Fatalf("счётчик потока %d разошёлся с длиной кольца %d", s.totalBytes, len(s.buf))
	}
	if got := bufStartOf(s.totalBytes, len(s.buf)); got != 0 {
		t.Fatalf("начало кольца %d вместо 0 — арифметика резюме сломана", got)
	}
}

// Инвариант держится и после подсева: первый же коннект обязан получить полный
// снимок с ВАЛИДНЫМ базовым offset, который клиент сможет прислать обратно.
func TestSeedScrollbackKeepsResumeUsable(t *testing.T) {
	s := &Session{epoch: "e1", subs: map[chan []byte]*subState{}, dec: newDecTracker()}
	s.seedScrollback([]byte("архив прошлой работы\n"))
	s.bufMu.Lock()
	s.buf = append(s.buf, []byte("свежий вывод")...)
	s.totalBytes += uint64(len("свежий вывод"))
	s.bufMu.Unlock()

	ch, payload, offset, _, _, _ := s.SubscribeResume("", 0)
	defer s.Unsubscribe(ch)
	base := offset - uint64(len(payload))
	if base != 0 {
		t.Fatalf("базовый offset %d — при полном снимке он обязан быть 0", base)
	}

	// Возврат с честной позиции: дельта пустая, история на экране цела.
	ch2, tail, _, _, isDelta, gap := s.SubscribeResume("e1", offset)
	defer s.Unsubscribe(ch2)
	if !isDelta || gap || len(tail) != 0 {
		t.Fatalf("возврат без нового вывода дал isDelta=%v gap=%v tail=%d — ожидалась пустая дельта",
			isDelta, gap, len(tail))
	}
}
