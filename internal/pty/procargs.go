package pty

import (
	"encoding/binary"
	"strings"
)

// parseProcArgs2 разбирает буфер sysctl `kern.procargs2` (macOS) в командную
// строку вида «exe arg1 arg2 …» — тот же формат, что processCmdline на Linux
// и Windows отдаёт в matchAgentCmdline.
//
// Формат буфера (xnu, kern_sysctl.c → sysctl_procargsx): int32 argc (в
// порядке байт машины, для нас little-endian), затем путь исполняемого файла с
// завершающим NUL, затем выравнивающие NUL-ы, затем argc строк по NUL, затем
// переменные окружения — их не берём. Разбор без /proc и без `ps`, поэтому годен
// для детектора, который тикает раз в секунду на каждую сессию.
//
// Зачем: на маке Gemini CLI живёт как `node /usr/local/bin/gemini`, и без
// командной строки передний план виден как «node» (на Linux то же самое
// нашлось живьём 07.09.2026). Файл без build-тега — парсер проверяется тестом
// на любой ОС, сам вызов sysctl — только в foreground_darwin.go.
func parseProcArgs2(buf []byte) string {
	if len(buf) < 5 {
		return ""
	}
	argc := int(int32(binary.LittleEndian.Uint32(buf[:4])))
	if argc < 0 || argc > 4096 {
		return ""
	}
	rest := buf[4:]
	// Путь исполняемого файла до первого NUL.
	end := indexByte(rest, 0)
	if end < 0 {
		return ""
	}
	exe := string(rest[:end])
	rest = rest[end:]
	// Пропустить выравнивающие NUL-ы до первого аргумента.
	for len(rest) > 0 && rest[0] == 0 {
		rest = rest[1:]
	}
	parts := []string{exe}
	for i := 0; i < argc && len(rest) > 0; i++ {
		end = indexByte(rest, 0)
		if end < 0 {
			parts = append(parts, string(rest))
			rest = nil
			break
		}
		parts = append(parts, string(rest[:end]))
		rest = rest[end+1:]
	}
	return strings.TrimSpace(strings.Join(parts, " "))
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}
