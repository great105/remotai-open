package pty

import (
	"path/filepath"
	"regexp"
	"strings"

	"github.com/shirou/gopsutil/v4/process"
)

// Снятие pty-host'ов по PID — страховка на случай, когда труба до хоста уже
// мертва и кадр Kill до него не доходит.
//
// Почему это вообще нужно. Хост сознательно живёт ВНЕ нашего Job Object
// (breakaway → WMI job-escape, см. spawnHost) — иначе он умирал бы вместе с
// агентом, а персистентные терминалы для того и заведены, чтобы переживать его
// перезапуск. Оборотная сторона: KILL_ON_JOB_CLOSE до хоста не дотягивается, и
// единственный канал «умри» — та самая труба. Если она порвалась, хост живёт
// вечно: записи о нём уже нет (Close стирал её первой), в списке терминалов он
// не виден, снять его можно только Диспетчером задач.
//
// Живой замер 02.08.2026 на машине владельца: 5 таких сирот, 80 процессов,
// 4197 МБ, самому старому почти трое суток — внутри каждого продолжал работать
// Claude Code. См. память pty-close-orphan-processes.
//
// Само убийство — платформенное (host_reap_windows.go / host_reap_unix.go): на
// Windows смерть хоста закрывает его Job Object и ОС снимает всё поддерево, а
// на POSIX дерево шелла гасит сам хост вызовом kill(-pgid) в своём Close, и
// SIGKILL снаружи этот путь только сломал бы — шелл с агентом внутри остался бы
// сиротой уже безымянной (в её командной строке нет ни --pty-host, ни id).
// Здесь — только общий разбор, он же покрыт тестами.

// hostIDCmdlineRe вытаскивает --id из командной строки pty-host. Формат
// отличается по платформам: Windows отдаёт строку с кавычками
// («"--id" "8dbf…"»), Linux — аргументы через пробел («--id 8dbf…»).
var hostIDCmdlineRe = regexp.MustCompile(`--id"?[\s=]+"?([0-9a-fA-F]{8,})`)

// hostIDFromCmdline возвращает id сессии из командной строки pty-host
// («» — это не наш хост или id не разобрался).
func hostIDFromCmdline(cmdline string) string {
	if !strings.Contains(cmdline, "--pty-host") {
		return ""
	}
	m := hostIDCmdlineRe.FindStringSubmatch(cmdline)
	if len(m) < 2 {
		return ""
	}
	return strings.ToLower(m[1])
}

// hostStillAlive — процесс хоста этой сессии всё ещё существует? Вопрос
// возникает после неудачного закрытия: если хост пережил и кадр Kill, и
// добивание, запись о терминале стирать нельзя — иначе он станет невидимым.
// Кроссплатформенно и безопасно: это только чтение.
func hostStillAlive(id string, pid uint32) bool {
	if pid == 0 || id == "" {
		return false
	}
	p, err := process.NewProcess(int32(pid))
	if err != nil {
		return false
	}
	cmdline, err := p.Cmdline()
	if err != nil {
		return false
	}
	return hostIDFromCmdline(cmdline) == strings.ToLower(id)
}

// updatedExeRe — хвост, который автообновление оставляет на имени работающего
// файла: update.Apply переименовывает занятый exe в «remotai.exe.old-<unix ms>»
// (Windows) или удаляет его на месте, отчего ядро показывает путь с пометкой
// « (deleted)» (Linux).
var updatedExeRe = regexp.MustCompile(`\.old-\d+$`)

// sameAgentExecutable — процесс запущен из НАШЕГО исполняемого файла?
//
// Сравнивать пути буквально нельзя, и это не теория: замер 02.08.2026 показал,
// что у трёх из семи живых хостов путь к образу — «…\remotai.exe.old-1785337946174».
// Так и должно быть: агент обновляется переименованием занятого файла
// (update.Apply), а хосты переживают обновление — в том и смысл персистентных
// терминалов. Буквальное сравнение отсекало бы ровно тех долгожителей, ради
// которых уборка и написана: сирота получается как раз из старого хоста.
//
// Поэтому сверяем каталог и базовое имя, разрешая хвосты обновления.
func sameAgentExecutable(exe, self string) bool {
	if exe == "" || self == "" {
		return false
	}
	norm := func(s string) string {
		return strings.ToLower(filepath.Clean(strings.ReplaceAll(s, `\`, `/`)))
	}
	exe, self = norm(exe), norm(self)
	if filepath.Dir(exe) != filepath.Dir(self) {
		return false
	}
	base := filepath.Base(exe)
	base = strings.TrimSuffix(base, " (deleted)")
	base = updatedExeRe.ReplaceAllString(base, "")
	return base == filepath.Base(self)
}
