package server

import "testing"

// Путь стрима приходит от клиента и доезжает до локального обработчика агента,
// поэтому проверка «что можно, а что нельзя» — граница доверия, а не формальность.
func TestValidStreamPath(t *testing.T) {
	good := []string{
		"/ws/pty/82da0816724e9b23",
		"/ws/screen",
		// Ради этого параметра проверка и переписана: без него каждое
		// переподключение телефона тянет весь буфер терминала заново.
		"/ws/pty/82da0816724e9b23?resume=6f1a2b3c:918273",
		"/ws/screen?resume=abc:1",
	}
	for _, p := range good {
		if !validStreamPath(p) {
			t.Errorf("путь должен приниматься: %q", p)
		}
	}

	bad := []struct {
		path string
		why  string
	}{
		{"/api/pty", "не /ws/ — агент не должен ходить в REST этим путём"},
		{"/ws/../api/files", "выход из каталога"},
		{"ws://evil.example/ws/pty/x", "подмена схемы и хоста"},
		{"/ws/pty/x?initData=token:foreign", "подмена авторизации: наш initData встал бы вторым"},
		{"/ws/pty/x?resume=a:1&initData=token:foreign", "то же самое, спрятанное за разрешённым параметром"},
		{"/ws/pty/x?jwt=stolen", "любой посторонний параметр"},
		{"/ws/pty/x with space", "пробелы в пути"},
		{"/ws/pty/x\r\nHost: evil", "перевод строки"},
	}
	for _, c := range bad {
		if validStreamPath(c.path) {
			t.Errorf("путь должен отвергаться (%s): %q", c.why, c.path)
		}
	}
}
