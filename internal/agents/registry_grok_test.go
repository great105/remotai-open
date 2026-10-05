package agents

import (
	"strings"
	"testing"
)

// Дескриптор Grok закреплён тестом целиком, а не «на глаз при ревью».
//
// Причина ровно одна и она из памяти проекта: по документации в реестре уже
// оказывался несуществующий npm-пакет, а флаг, которого у CLI нет, валит запуск
// на разборе аргументов — человек с телефона видит только «агент не запустился».
// Всё, что здесь проверяется, снято ЖИВЫМ прогоном `grok --help` версии 1.0.4
// (Windows, 16.08.2026) и живой проверкой разбора аргументов. Меняется правкой
// теста ПОСЛЕ нового живого прогона, а не наоборот.
func TestGrokDescriptorMatchesLiveHelp(t *testing.T) {
	d := GetDescriptor("grok")
	if d == nil {
		t.Fatal("grok пропал из реестра")
	}

	// Официальный пакет xAI (издатель security@x.ai). Форки с похожим именем
	// существуют, и «поправить по памяти» на `grok-cli` здесь легко.
	if want := "npm i -g @xai-official/grok"; d.Install != want {
		t.Errorf("install = %q, want %q", d.Install, want)
	}
	if len(d.CLINames) != 1 || d.CLINames[0] != "grok" {
		t.Errorf("cli names = %#v, want [grok]", d.CLINames)
	}

	// `-p, --single <PROMPT>` — единственный headless-путь: печатает ответ и выходит.
	if got := strings.Join(d.RunArgs, " "); got != "-p {prompt}" {
		t.Errorf("run args = %q, want %q", got, "-p {prompt}")
	}

	// У `-r, --resume` значение НЕОБЯЗАТЕЛЬНОЕ, поэтому форма с `=` обязательна:
	// при разделении пробелом разбор зависит от следующего токена, и перестановка
	// аргументов молча превратит resume в «продолжить последнюю сессию».
	if !d.SupportsResume() {
		t.Fatal("grok потерял поддержку resume")
	}
	if got := d.ResumeArgs[0]; !strings.HasPrefix(got, "--resume=") {
		t.Errorf("resume передан как %q — нужна форма --resume=<id>", got)
	}
	if want := "grok --continue"; d.ResumeCLI != want {
		t.Errorf("resume cli = %q, want %q", d.ResumeCLI, want)
	}

	// `-m, --model <MODEL>` — без этого выбор модели через OpenRouter не собрать.
	if d.ModelFlag != "-m" {
		t.Errorf("model flag = %q, want -m", d.ModelFlag)
	}

	// GROK_HOME назван в трамплине npm-пакета и подтверждён запуском: в
	// подменённом каталоге завелись config.toml, agent_id, sessions/ и logs/,
	// то есть это сам каталог конфига, а не домашний (как у gemini).
	if !d.SupportsAccounts() || d.AccountEnv != "GROK_HOME" {
		t.Errorf("account env = %q, want GROK_HOME", d.AccountEnv)
	}
	if d.AccountEnvKind != "config_dir" {
		t.Errorf("account env kind = %q, want config_dir", d.AccountEnvKind)
	}
	if got := d.AccountCredentialsDir(`C:\profiles\grok2`); got != `C:\profiles\grok2` {
		t.Errorf("креды профиля ищутся в %q, а GROK_HOME и есть каталог конфига", got)
	}

	// Вход (`auth.json`), переписки (`sessions/`) и логи общими не становятся
	// никогда. `config.toml` и `memory/` вне списка намеренно: в первом
	// настраиваются свои модели и может оказаться ключ, второе набирается из
	// переписок.
	forbidden := map[string]bool{
		"auth.json": true, "sessions": true, "logs": true,
		"config.toml": true, "memory": true,
	}
	if len(d.AccountShared) == 0 {
		t.Error("у grok пропали общие ресурсы — второй аккаунт останется без скиллов")
	}
	for _, res := range d.AccountShared {
		if forbidden[res.Name] {
			t.Errorf("%q сделан общим, а он несёт вход или переписку", res.Name)
		}
	}

	// Каждый флаг ниже проверен живым запуском: все дошли до «Not signed in»,
	// то есть разобрались. Непроверенных флагов у агента быть не должно.
	live := map[string]bool{
		"--always-approve":              true,
		"--permission-mode acceptEdits": true,
		"--permission-mode plan":        true,
		"--continue":                    true,
	}
	if len(d.LaunchFlags) == 0 {
		t.Fatal("у grok пропали флаги запуска")
	}
	for _, f := range d.LaunchFlags {
		if !live[f.Flag] {
			t.Errorf("флаг %q не проверялся живым --help", f.Flag)
		}
		if f.Title == "" {
			t.Errorf("флаг %q без подписи на кнопке", f.Flag)
		}
	}

	// Режим прокрутки намеренно не задан: замера на живой сессии не было, а
	// режим по догадке уводит прокрутку не туда.
	if d.DefaultScrollMode != "" {
		t.Errorf("scroll mode = %q, а живого замера прокрутки у grok ещё не было", d.DefaultScrollMode)
	}
}
