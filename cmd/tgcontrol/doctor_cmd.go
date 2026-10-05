package main

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strings"
	"tgcontrol/internal/localize"

	"tgcontrol/internal/config"
	"tgcontrol/internal/relay"
	"tgcontrol/internal/version"
)

// `remotai doctor` — что с Remotai не так и что из этого чинится.
//
// ЗАЧЕМ. Агент, которого просят «настрой тут всё», первым делом должен узнать
// СОСТОЯНИЕ, а не список настроек: половина просьб человека («почему телефон
// не видит компьютер?») — это не настройка, а поломка. Раньше такой ответ
// собирался из `remotai status`, логов и догадок; теперь это одна команда,
// которая называет проблему словами и говорит, чем лечится.
//
// ПРАВИЛО ЭТОЙ КОМАНДЫ: каждая находка обязана нести ДЕЙСТВИЕ. Строка «облако
// не в сети» без «что делать» отправляет агента гадать, а гадает он уверенно.
type finding struct {
	// Level: ok | warn | problem.
	Level string `json:"level"`
	Title string `json:"title"`
	// Fix — что сделать. Пусто только у ok.
	Fix string `json:"fix,omitempty"`
	// SelfFix — команда, которую агент может выполнить САМ (безопасная).
	SelfFix string `json:"self_fix,omitempty"`
}

// doctorStartFix — как поднять агента НА ЭТОЙ системе.
//
// Совет обязан быть выполнимым: на маке про systemctl говорить нельзя, его там
// нет (см. serviceStartHint — то же правило для подсказок привязки).
func doctorStartFix() string {
	switch runtime.GOOS {
	case "darwin":
		return "запустите: " + serviceStartHint() + "; если уже стоит — " + serviceRestartHint()
	case "windows":
		return localize.Text("запустите приложение Remotai на этом компьютере")
	default:
		return doctorLinuxFix(userUnitInstalled())
	}
}

// doctorLinuxFix — тому, кто ставил без sudo, системная команда ответит «Unit
// remotai.service not found»: его служба живёт в менеджере пользователя.
func doctorLinuxFix(userUnit bool) string {
	if userUnit {
		return localize.Text("запустите приложение Remotai на этом компьютере (на сервере: systemctl --user start remotai)")
	}
	return localize.Text("запустите приложение Remotai на этом компьютере (на сервере: sudo systemctl start remotai)")
}

func runDoctor(args []string) int {
	asJSON := len(args) > 0 && args[0] == "--json"
	cfg := config.GetNoSetup()
	out := make([]finding, 0, 8)

	// 1. Процесс и локальный доступ: без него не работает ничего остального,
	// поэтому проверяем первым и остальное считаем только при живом агенте.
	var status localCLIStatus
	live, ok := readLocalCLIStatus(cfg.Port())
	if ok {
		status = live
		out = append(out, finding{Level: "ok", Title: fmt.Sprintf(localize.Text("Remotai работает на порту %d"), live.Port)})
	} else {
		// Совет — командой ТОЙ системы, где человек находится. На маке
		// «systemctl start remotai» человек честно выполнил и получил
		// «command not found» (живой мак 10.08.2026): диагностика отправила
		// его в тупик вместо починки.
		out = append(out, finding{
			Level: "problem",
			Title: fmt.Sprintf(localize.Text("Remotai не отвечает на 127.0.0.1:%d"), cfg.Port()),
			Fix:   doctorStartFix(),
		})
	}

	// 2. Привязка к аккаунту: без неё компьютера нет в приложении.
	if relay.Available() {
		if ok && status.RelayConnected {
			out = append(out, finding{Level: "ok", Title: localize.Text("Компьютер в сети и виден в приложении")})
		} else if ok {
			reason := relayReasonText(status.RelayError.Kind, status.RelayError.HTTPStatus)
			out = append(out, finding{
				Level: "problem",
				Title: "Компьютер привязан, но не выходит на связь: " + reason,
				Fix:   localize.Text("проверьте интернет и VPN; если привязка отозвана — remotai pair"),
			})
		}
	} else {
		out = append(out, finding{
			Level: "warn",
			Title: localize.Text("Компьютер не привязан к аккаунту — с телефона его не видно"),
			Fix:   localize.Text("remotai pair (код появится в терминале, подтвердите в приложении)"),
		})
	}

	// 3. Автозапуск: самая частая причина «компьютер пропал после перезагрузки».
	enabled, method := false, "none"
	if ok {
		enabled, method = status.Autostart.Enabled, status.Autostart.Method
	} else {
		enabled, method = cliAutostartState()
	}
	if enabled {
		out = append(out, finding{Level: "ok", Title: "Автозапуск включён (" + method + ")"})
	} else {
		out = append(out, finding{
			Level: "warn",
			Title: localize.Text("Автозапуск выключен — после перезагрузки компьютер не появится сам"),
			Fix:   localize.Text("включите в приложении («Система») или попросите владельца: remotai config set autostart true"),
		})
	}

	// 3-бис. Что было в прошлый перерыв и что со связью сейчас.
	//
	// ЭТО ПЕРВОЕ, О ЧЁМ СПРАШИВАЮТ, и раньше ответа не было ни у кого: причину
	// пропажи компьютера приходилось выковыривать из лога вручную. Теперь
	// компьютер помнит её сам (см. internal/selfheal, internal/netwatch).
	if ok {
		var boot struct {
			Available bool `json:"available"`
			Report    struct {
				Title      string `json:"title"`
				Detail     string `json:"detail"`
				Unexpected bool   `json:"unexpected"`
			} `json:"report"`
		}
		if localGET("/api/system/boot-report", &boot) == 0 && boot.Available && boot.Report.Title != "" {
			level := "ok"
			if boot.Report.Unexpected {
				level = "warn"
			}
			f := finding{Level: level, Title: boot.Report.Title}
			if boot.Report.Unexpected {
				f.Fix = localize.Text("если это повторяется — посмотрите лог: remotai doctor --json и %LOCALAPPDATA%\\Remotai\\remotai.log")
			}
			out = append(out, f)
		}

		var net struct {
			Internet bool `json:"internet"`
			VPN      *struct {
				Name      string `json:"name"`
				Running   bool   `json:"running"`
				StartHint string `json:"start_hint"`
			} `json:"vpn"`
			Last *struct {
				Text string `json:"text"`
			} `json:"last_action"`
		}
		if localGET("/api/system/network", &net) == 0 {
			if !status.RelayConnected && net.VPN != nil && net.VPN.Running {
				out = append(out, finding{
					Level:   "warn",
					Title:   "Компьютер не в сети, а " + net.VPN.Name + " запущен — частая причина именно в нём",
					Fix:     localize.Text("проверить и при необходимости выключить: remotai vpn status / remotai vpn off"),
					SelfFix: "remotai vpn status",
				})
			}
			if net.VPN != nil && !net.VPN.Running && net.VPN.StartHint != "" {
				out = append(out, finding{
					Level: "warn",
					Title: net.VPN.Name + " выключен",
					Fix:   localize.Text("включить обратно: remotai vpn on"),
				})
			}
			if net.Last != nil && net.Last.Text != "" {
				out = append(out, finding{Level: "ok", Title: "Сторож связи: " + net.Last.Text})
			}
		}
	}

	// 4. Версия: устаревший агент — источник половины странностей.
	//
	// ГРАБЛЯ, пойманная на живом компьютере владельца: `CheckForUpdate`
	// возвращает манифест ВСЕГДА, а «есть что обновлять» — это отдельное поле
	// `Available` (строгое сравнение semver). Без него doctor писал «Есть новая
	// версия 2.53.1 (сейчас 2.53.1)» — то есть посылал агента чинить то, что не
	// сломано. Диагностика, которая врёт, хуже отсутствующей.
	if info, err := version.CheckForUpdate(""); err == nil && info != nil && info.Available {
		out = append(out, finding{
			Level: "warn",
			Title: fmt.Sprintf(localize.Text("Есть новая версия %s (сейчас %s)"), info.Version, version.Version),
			Fix:   localize.Text("обновится само; можно ускорить в приложении: «Настройки → Обновить сейчас»"),
		})
	}

	// 5. Аккаунты нейросетей: агент видит, куда упирается его же работа.
	if ok {
		var usage struct {
			Providers []struct {
				ID           string `json:"id"`
				Name         string `json:"name"`
				Status       string `json:"status"`
				AccountLabel string `json:"account_label"`
				Windows      []struct {
					ID          string  `json:"id"`
					UsedPercent float64 `json:"used_percent"`
				} `json:"windows"`
			} `json:"providers"`
		}
		if localGET("/api/ai-usage", &usage) == 0 {
			for _, p := range usage.Providers {
				name := p.Name
				if p.AccountLabel != "" {
					name += " · " + p.AccountLabel
				}
				if p.Status == "signed_out" {
					out = append(out, finding{
						Level: "warn",
						Title: name + ": вход не выполнен",
						Fix:   localize.Text("запустите этого агента и войдите внутри него (/login)"),
					})
					continue
				}
				for _, w := range p.Windows {
					if w.ID == "five_hour" && w.UsedPercent >= 85 {
						out = append(out, finding{
							Level: "warn",
							Title: fmt.Sprintf(localize.Text("%s: осталось %d%% пятичасового окна"), name, int(100-w.UsedPercent)),
							Fix:   localize.Text("переключитесь на другой аккаунт в приложении («Агенты») или подождите сброса"),
						})
					}
				}
			}
		}
	}

	if asJSON {
		data, _ := json.MarshalIndent(map[string]any{"findings": out}, "", "  ")
		fmt.Println(string(data))
	} else {
		for _, f := range out {
			fmt.Printf("%s %s\n", doctorMark(f.Level), f.Title)
			if f.Fix != "" {
				fmt.Printf("    → %s\n", f.Fix)
			}
		}
	}
	// Код возврата — чтобы скрипт мог отличить «всё хорошо» от «есть беда».
	for _, f := range out {
		if f.Level == "problem" {
			return 1
		}
	}
	return 0
}

func doctorMark(level string) string {
	switch level {
	case "ok":
		return "✅"
	case "warn":
		return "⚠️"
	}
	return "❌"
}

// cliDoctorUsage — короткая справка, если позвали с непонятным аргументом.
func cliDoctorUsage() {
	fmt.Fprintln(os.Stderr, localize.Text("Использование: remotai doctor [--json]"))
}

var _ = strings.TrimSpace // держим импорт: строковые помощники используются ниже по мере роста проверок
