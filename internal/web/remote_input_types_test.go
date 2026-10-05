package web

import (
	"os"
	"regexp"
	"testing"
)

// Оба транспорта обязаны принимать ОДИН И ТОТ ЖЕ набор событий ввода.
//
// Живая жалоба владельца «скролл не работает»: касания ("tp") были добавлены в
// веб-сокетный путь и забыты в WebRTC — а телефон в облаке ходит именно по
// нему, поэтому каждое касание молча пропадало. Тест сверяет списки в обоих
// файлах, чтобы следующий новый тип события не потерялся так же.
func TestInputEventTypesMatchAcrossTransports(t *testing.T) {
	re := regexp.MustCompile(`case ("(?:m|s|k|txt|tp)"(?:, "[a-z]+")*):`)
	read := func(path string) string {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("читаю %s: %v", path, err)
		}
		m := re.FindSubmatch(data)
		if m == nil {
			t.Fatalf("в %s не нашёлся список типов событий ввода", path)
		}
		return string(m[1])
	}
	ws := read("api_remote.go")
	rtc := read("api_remote_rtc.go")
	if ws != rtc {
		t.Fatalf("списки типов ввода разошлись:\n  веб-сокет: %s\n  webrtc:    %s", ws, rtc)
	}
}
