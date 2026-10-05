package pty

// Установка публичного ключа на сервер — то же, что делает ssh-copy-id.
//
// Без этого шага «сгенерировать ключ» остаётся упражнением: ключ есть, а
// сервер о нём не знает, и человеку всё равно предлагают вводить пароль. Здесь
// один вход по паролю превращается в «дальше по ключу».
//
// Команда намеренно идемпотентна и говорит, что именно произошло: повторное
// нажатие не должно плодить одинаковые строки в authorized_keys, а «уже стоял»
// и «добавлен» — разные новости для человека.

import (
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/ssh"
)

// ErrAuthorizedKeyShell — сервер не смог выполнить POSIX-команду (типичный
// случай: Windows-сервер с cmd.exe в качестве шелла). Ключ не установлен.
var ErrAuthorizedKeyShell = errors.New("ssh: server did not accept the authorized_keys command")

// Маркеры в выводе: слово вместо кода возврата, потому что «уже стоял» — не
// ошибка и обязано доезжать до интерфейса как обычный успешный исход.
const (
	authorizedKeyAdded  = "REMOTAI_KEY_ADDED"
	authorizedKeyExists = "REMOTAI_KEY_EXISTS"
)

// InstallAuthorizedKey дописывает publicKey в ~/.ssh/authorized_keys сервера.
// already=true — ключ там уже был.
func InstallAuthorizedKey(cfg SSHConfig, publicKey string) (already bool, err error) {
	publicKey = strings.TrimSpace(publicKey)
	if publicKey == "" {
		return false, errors.New("public key is empty")
	}
	// Ключ обязан быть разобран ДО подключения: строка с переводом строки или
	// мусором дописала бы в authorized_keys что угодно.
	if _, _, _, _, parseErr := ssh.ParseAuthorizedKey([]byte(publicKey)); parseErr != nil {
		return false, fmt.Errorf("public key is not valid: %w", parseErr)
	}

	client, err := dialSSH(cfg)
	if err != nil {
		return false, err
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		return false, err
	}
	defer session.Close()

	out, err := session.CombinedOutput(authorizedKeyCommand(publicKey))
	text := string(out)
	switch {
	case strings.Contains(text, authorizedKeyExists):
		return true, nil
	case strings.Contains(text, authorizedKeyAdded):
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("%w: %s", ErrAuthorizedKeyShell, strings.TrimSpace(text))
	}
	return false, fmt.Errorf("%w: unexpected output %q", ErrAuthorizedKeyShell, strings.TrimSpace(text))
}

// authorizedKeyCommand собирает POSIX-команду. Права выставляем явно: sshd
// молча игнорирует authorized_keys с слишком широкими правами, и человек потом
// ищет причину «ключ добавлен, но не пускает».
func authorizedKeyCommand(publicKey string) string {
	quoted := shellSingleQuote(publicKey)
	return "umask 077; mkdir -p ~/.ssh && touch ~/.ssh/authorized_keys && " +
		"chmod 700 ~/.ssh && chmod 600 ~/.ssh/authorized_keys && " +
		"{ if grep -qxF " + quoted + " ~/.ssh/authorized_keys; then echo " + authorizedKeyExists + "; " +
		"else printf '%s\\n' " + quoted + " >> ~/.ssh/authorized_keys && echo " + authorizedKeyAdded + "; fi; }"
}

// shellSingleQuote заключает строку в одинарные кавычки по правилам sh.
// Комментарий ключа пишет человек, и апостроф в нём («Женин ноутбук») не должен
// разваливать команду на сервере.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
