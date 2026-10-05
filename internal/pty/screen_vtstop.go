package pty

import (
	"io"

	"github.com/charmbracelet/x/vt"
)

// stopEmulator закрывает эмулятор и дожидается его вычитывающей горутины
// (drainEmulator) без гонки данных внутри самой vt.
//
// ⚠ ЗАЧЕМ. Emulator.Read читает поле closed, а Emulator.Close пишет его — без
// синхронизации (go test -race в Ubuntu WSL, 15.09: «DATA RACE … Close() /
// Read()»). Горутина всё время сидит в Read, так что прежнее «Close, потом
// ждать горутину» было гонкой при каждом закрытии зеркала, а с пересборкой
// после паники (screen_vtpanic.go) — и при каждой пересборке. Поэтому сначала
// закрывается только труба ответов: InputPipe — тот же *io.PipeWriter, и
// поле closed при этом не трогается; Read в горутине получает EOF, горутина
// выходит; и лишь потом Close — когда Read уже никто не зовёт.
//
// Если библиотека однажды перестанет отдавать трубу этим типом — прежний
// порядок (Close разблокирует Read сам), только без этой гарантии.
func stopEmulator(em *vt.Emulator, done <-chan struct{}) {
	if pw, ok := em.InputPipe().(interface{ CloseWithError(error) error }); ok {
		_ = pw.CloseWithError(io.EOF)
		<-done
		_ = em.Close()
		return
	}
	_ = em.Close()
	<-done
}
