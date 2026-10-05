//go:build !windows

package secretbox

// Вне Windows платформенного хранилища нет: агенту на Linux-сервере остаётся
// AES-GCM с secret.key. Отдельный маркер ошибки нужен, чтобы Seal отличал
// «здесь так не умеют» от настоящего сбоя шифрования.
func protect([]byte) ([]byte, error) { return nil, errPlatformUnavailable }

func unprotect([]byte) ([]byte, error) { return nil, errPlatformUnavailable }
