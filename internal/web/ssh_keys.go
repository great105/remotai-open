package web

// Мост между хранилищем ключей (internal/pty/ssh_keys_store.go) и тремя
// местами, где агент открывает SSH: терминал, файлы (SFTP) и проброс портов.
// Все три обязаны понимать назначенный серверу ключ одинаково — иначе
// «Подключиться» работало бы, а «Файлы» того же сервера просили пароль.

import (
	"errors"
	"net/http"

	"tgcontrol/internal/pty"
)

// sshHostKeyMaterial возвращает приватный ключ и его пароль для хоста.
// Пустой keyID — не ошибка: у сервера просто нет назначенного ключа, дальше
// работают обычные источники (файл, ssh-agent, пароль).
func (s *Server) sshHostKeyMaterial(keyID string) (pemData, passphrase string, err error) {
	if keyID == "" || s == nil || s.sshKeys == nil {
		return "", "", nil
	}
	return s.sshKeys.Material(keyID)
}

// writeSSHKeyError отвечает на проблемы с назначенным ключом. Их всего две, и
// обе про действие человека, а не про сбой: ключ удалили из хранилища либо
// приватная часть зашифрована другой учётной записью Windows.
func writeSSHKeyError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, pty.ErrSSHKeyLocked):
		jsonErrorCode(w, 409, "key_locked", "ssh key belongs to another account", nil)
	case errors.Is(err, pty.ErrSSHKeyNotFound):
		jsonErrorCode(w, 409, "key_gone", "ssh key is no longer stored", nil)
	default:
		jsonError(w, "ssh key is unavailable", 500)
	}
}
