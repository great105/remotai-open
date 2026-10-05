package main

import (
	"fmt"
	"strings"

	"tgcontrol/internal/version"
)

// Живой Linux-стенд 06.09.2026: `remotai --help`, `remotai help`, `remotai -h`
// и `remotai version` не печатали ничего полезного. Аргумент никем не
// разбирался, процесс шёл запускать приложение и отвечал «Another Remotai
// instance is already listening on :8080 — activated its window». Первое, что
// человек набирает после установки в терминале, выглядело как поломка.

// isHelpArg — просьба о справке в любой привычной форме.
func isHelpArg(a string) bool {
	switch strings.ToLower(a) {
	case "help", "-h", "--help", "-help", "/?":
		return true
	}
	return false
}

// isVersionArg — просьба о версии, с дефисами или без.
func isVersionArg(a string) bool {
	switch a {
	case "version", "-v", "-V", "--version":
		return true
	}
	return false
}

type cliCommand struct {
	name  string // подкоманда, как её разбирает main
	usage string // как набирать
	what  string // что делает, одной строкой
}

// commandHelp — единственный список подкоманд для справки. Порядок — путь
// человека: поставить → привязать → посмотреть → починить → остальное.
var commandHelp = []cliCommand{
	{"install", "install [--user|--system] [--no-pair]", "Автозапуск и привязка этого компьютера к аккаунту"},
	{"uninstall", "uninstall [--purge]", "Убрать автозапуск; --purge — вместе с настройками"},
	{"pair", "pair", "Показать код и QR для привязки к аккаунту"},
	{"unpair", "unpair", "Отвязать этот компьютер от аккаунта"},
	{"status", "status", "Служба, связь с облаком, версия"},
	{"doctor", "doctor [--json]", "Проверить настройку и подсказать, что починить"},
	{"config", "config get|set|list …", "Настройки этого компьютера — то же видит ИИ-агент"},
	{"attach", "attach [терминал]", "Открыть терминал Remotai в этой консоли"},
	{"send", "send <текст> | --file <путь>", "Сообщение или файл владельцу в Telegram"},
	{"update", "update", "Обновить агент до последней версии"},
	{"vpn", "vpn [status|off|on]", "VPN этого компьютера"},
	{"remote", "remote <команда>", "Выполнить команду на другом компьютере аккаунта"},
	{"--version", "--version", "Версия и сборка"},
	{"--background", "--background", "Тихий запуск без окна (для автозапуска)"},
}

// usageText — общая справка `remotai help`.
func usageText() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s — удалённый доступ к ИИ-агентам на этом компьютере.\n\n", version.String())
	b.WriteString("Использование: remotai [команда] [флаги]\n")
	b.WriteString("Без команды запускает приложение: окно на Windows, фон и браузер на Linux и macOS.\n\n")
	b.WriteString("Команды:\n")
	width := 0
	for _, c := range commandHelp {
		if n := len([]rune(c.usage)); n > width {
			width = n
		}
	}
	for _, c := range commandHelp {
		fmt.Fprintf(&b, "  %-*s  %s\n", width, c.usage, c.what)
	}
	b.WriteString("\nСправка по команде: remotai <команда> --help\n")
	b.WriteString("Сайт и инструкции: https://remotai.ru\n")
	return b.String()
}

// commandUsage — справка по одной подкоманде для `remotai <команда> --help`.
// Команды со своей развёрнутой справкой (send, remote) сюда не попадают:
// у них текст богаче табличной строки.
func commandUsage(name string) (string, bool) {
	switch name {
	case "send", "remote", "peer":
		return "", false
	}
	for _, c := range commandHelp {
		if c.name == name {
			return fmt.Sprintf("Использование: remotai %s\n  %s\n", c.usage, c.what), true
		}
	}
	return "", false
}
