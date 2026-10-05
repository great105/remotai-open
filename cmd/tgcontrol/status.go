package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"tgcontrol/internal/config"
	"tgcontrol/internal/paths"
	"tgcontrol/internal/relay"
	"tgcontrol/internal/version"
)

type localCLIStatus struct {
	Configured      bool `json:"configured"`
	RelayConfigured bool `json:"relay_configured"`
	RelayConnected  bool `json:"relay_connected"`
	Port            int  `json:"port"`
	RelayError      struct {
		Kind       string `json:"kind"`
		HTTPStatus int    `json:"http"`
		Message    string `json:"message"`
	} `json:"relay_error"`
	Autostart struct {
		Enabled bool   `json:"enabled"`
		Method  string `json:"method"`
	} `json:"autostart"`
}

func runStatus(args []string) int {
	if len(args) != 0 {
		fmt.Fprintln(os.Stderr, "Использование: remotai status")
		return 2
	}
	cfg := config.GetNoSetup()
	port := cfg.Port()
	live, ok := readLocalCLIStatus(port)

	fmt.Println(version.String())
	fmt.Printf("Привязка: %s\n", yesNo(relay.Available(), "есть", "нет"))
	if ok {
		fmt.Printf("Процесс:   работает, 0.0.0.0:%d\n", live.Port)
		fmt.Printf("Облако:    %s\n", yesNo(live.RelayConnected, "в сети", "не в сети"))
		if !live.RelayConnected && live.RelayError.Kind != "" {
			reason := relayReasonText(live.RelayError.Kind, live.RelayError.HTTPStatus)
			fmt.Printf("Причина:   %s\n", reason)
		}
		fmt.Printf("Автозапуск: %s (%s)\n", yesNo(live.Autostart.Enabled, "включён", "выключен"), live.Autostart.Method)
	} else {
		fmt.Printf("Процесс:   не отвечает на 127.0.0.1:%d\n", port)
		enabled, method := cliAutostartState()
		fmt.Printf("Автозапуск: %s (%s)\n", yesNo(enabled, "включён", "выключен"), method)
	}
	fmt.Printf("Лог:       %s\n", filepath.Join(paths.Base(), "remotai.log"))
	if info, err := version.CheckForUpdate(""); err == nil && info != nil {
		if info.Available {
			fmt.Printf("Обновление: доступна v%s\n", info.Version)
		} else {
			fmt.Println("Обновление: актуальная версия")
		}
	} else if err != nil {
		fmt.Printf("Обновление: проверить не удалось (%v)\n", err)
	}
	return 0
}

func readLocalCLIStatus(port int) (localCLIStatus, bool) {
	var out localCLIStatus
	client := &http.Client{Timeout: 1500 * time.Millisecond}
	resp, err := client.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/api/setup/status")
	if err != nil {
		return out, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&out) != nil {
		return out, false
	}
	if out.Port == 0 {
		out.Port = port
	}
	return out, true
}

func relayReasonText(kind string, status int) string {
	switch kind {
	case "revoked":
		return "привязка отозвана — выполните remotai pair"
	case "dns":
		return "не удаётся найти адрес сервиса (DNS)"
	case "tls":
		return "ошибка защищённого соединения TLS"
	case "timeout":
		return "тайм-аут сети; проверьте VPN или прокси"
	case "service":
		return fmt.Sprintf("облачный сервис временно недоступен (HTTP %d)", status)
	default:
		if status > 0 {
			return fmt.Sprintf("сетевая ошибка (HTTP %d)", status)
		}
		return "сетевая ошибка"
	}
}

func yesNo(v bool, yes, no string) string {
	if v {
		return yes
	}
	return no
}

func runUnpair(args []string) int {
	if len(args) != 0 {
		fmt.Fprintln(os.Stderr, "Использование: remotai unpair")
		return 2
	}
	cfg := config.GetNoSetup()
	jwt := cfg.RelayJWT
	if jwt == "" {
		jwt, _ = relay.LoadJWT()
	}
	if jwt != "" {
		base := cfg.RelayHTTPBase()
		if base == "" {
			base = config.DefaultRelayURL
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := relay.RevokeSelf(ctx, base, jwt)
		cancel()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Предупреждение: релей недоступен, локальная привязка всё равно будет удалена: %v\n", err)
		}
	}
	if err := config.Update(func(c *config.Config) {
		c.RelayJWT = ""
		c.RelayJWTExpiry = 0
	}); err != nil {
		fmt.Fprintf(os.Stderr, "remotai unpair: не удалось сохранить конфигурацию: %v\n", err)
		return 1
	}
	if err := relay.ClearJWT(); err != nil {
		fmt.Fprintf(os.Stderr, "remotai unpair: не удалось очистить защищённое хранилище: %v\n", err)
		return 1
	}
	fmt.Println("✅ Компьютер удалён из аккаунта. Сессии входа не затронуты; повторное подключение: remotai pair")
	return 0
}

func runUninstallCleanup(args []string) int {
	if len(args) != 0 {
		fmt.Fprintln(os.Stderr, "Использование: remotai --uninstall-cleanup")
		return 2
	}
	unpairCode := runUnpair(nil)
	if err := cleanupInstalledArtifacts(); err != nil {
		fmt.Fprintf(os.Stderr, "remotai cleanup: %v\n", err)
		if unpairCode == 0 {
			return 1
		}
	}
	return unpairCode
}
