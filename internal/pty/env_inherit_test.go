package pty

import (
	"os"
	"strings"
	"testing"
	"time"
)

// Терминал наследует окружение агента — на этом держится подключение OpenRouter.
//
// ЗАЧЕМ ЭТОТ ТЕСТ. Ключ OpenRouter нельзя подставлять в текст команды запуска:
// команду собирает клиент на телефоне, и она идёт через облако — ключ уехал бы
// с компьютера вместе с ней и осел в истории терминала. Поэтому он живёт в
// окружении процесса агента (internal/openrouter/store.go), а терминалы
// получают его по наследству.
//
// «По наследству» здесь — не рассуждение, а свойство кода: buildEnvBlock на
// Windows и os.Environ() на POSIX собирают окружение из процесса агента в
// момент создания сессии. Раз на этом построена целая функция, свойство обязано
// проверяться запуском, а не чтением.
//
// Прогоняется на живом PTY: печатаем переменную ИЗ САМОГО ТЕРМИНАЛА и читаем,
// что он ответил.
func TestNewTerminalInheritsAgentEnv(t *testing.T) {
	const name = "REMOTAI_ENV_INHERIT_PROBE"
	const value = "sk-or-v1-probe-value"
	t.Setenv(name, value)

	shell, cmd := probeShell()
	m := NewLocalManager()
	sess, err := m.Create(0, ".", shell, 80, 24)
	if err != nil {
		t.Skipf("PTY на этой машине не поднялся (%v) — проверять нечего", err)
	}
	defer func() {
		if err := m.Close(sess.ID); err != nil {
			t.Logf("закрытие PTY: %v", err)
		}
	}()

	ch, scrollback := sess.Subscribe()
	defer sess.Unsubscribe(ch)
	var out strings.Builder
	out.Write(scrollback)

	if _, err := sess.Write([]byte(cmd + "\r")); err != nil {
		t.Fatalf("запись в PTY: %v", err)
	}

	deadline := time.After(12 * time.Second)
	for {
		// Эхо самой команды содержит имя переменной, но не её значение —
		// поэтому ищем именно ЗНАЧЕНИЕ: оно попадает на экран только если
		// терминал переменную действительно получил.
		if strings.Contains(out.String(), value) {
			return
		}
		select {
		case data, ok := <-ch:
			if !ok {
				t.Fatalf("канал вывода закрылся раньше ответа; вывод=%q", out.String())
			}
			out.Write(data)
		case <-sess.Done():
			t.Fatalf("терминал завершился раньше ответа; вывод=%q", out.String())
		case <-deadline:
			t.Fatalf("терминал не показал переменную окружения агента — ключ OpenRouter до агентов не доедет; вывод=%q", out.String())
		}
	}
}

// probeShell подбирает шелл и команду печати переменной под текущую ОС.
func probeShell() (string, string) {
	if _, err := os.Stat(`C:\Windows\System32\cmd.exe`); err == nil {
		return "cmd", "echo %REMOTAI_ENV_INHERIT_PROBE%"
	}
	return "sh", "printf '%s\\n' \"$REMOTAI_ENV_INHERIT_PROBE\""
}
