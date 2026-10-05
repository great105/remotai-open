package web

import (
	"strings"
	"testing"
)

// Задача автозапуска обязана содержать ровно то, без чего управление
// компьютером теряется. Каждая проверка здесь — про живой отказ:
//
//   - БЕЗ второго триггера (повтор) упавший агент лежал до следующего входа в
//     систему: человек считал, что компьютер под управлением, а он был не на
//     связи часами;
//   - БЕЗ AllowStartIfOnBatteries Windows не запускает задачу на ноутбуке от
//     батареи и останавливает уже запущенную (это её умолчание!);
//   - массив триггеров ОБЯЗАН передаваться как @(...): «-Trigger $a, $b»
//     PowerShell разбирает как два аргумента и падает с
//     PositionalParameterNotFound (проверено на живой машине);
//   - длительность повтора задавать НЕЛЬЗЯ: любое явное значение планировщик
//     отвергает («XML содержит значение в неправильном формате»), а пустая
//     означает «бесконечно».
func TestWindowsAutostartTaskScript(t *testing.T) {
	script := windowsAutostartScriptForTest()

	must := map[string]string{
		"$ErrorActionPreference = 'Stop'": "ошибка регистрации будет выдана за успешное включение",
		"-AtLogOn":                        "нет триггера входа — агент не поднимется после перезагрузки",
		"-RepetitionInterval":             "нет повтора — упавший агент не вернётся сам",
		// Без --background каждый тик сторожа (раз в 5 минут) поднимал ОКНО
		// приложения поверх работы человека: агент жив, значит запущенный
		// экземпляр «активирует существующее окно». Живая жалоба 31.07.
		"-Argument '--background'":                      "сторож будет поднимать окно поверх работы каждые несколько минут",
		"-AllowStartIfOnBatteries":                      "от батареи Windows не запустит задачу",
		"-DontStopIfGoingOnBatteries":                   "при переходе на батарею Windows остановит агент",
		"-MultipleInstances IgnoreNew":                  "без этого сторож поднимет второй экземпляр",
		"@($atLogon, $watchdog)":                        "триггеры обязаны идти массивом",
		"-ExecutionTimeLimit (New-TimeSpan -Seconds 0)": "без этого задачу убьёт лимит времени",
	}
	for needle, why := range must {
		if !strings.Contains(script, needle) {
			t.Errorf("в скрипте нет %q — %s", needle, why)
		}
	}
	if strings.Contains(script, "-RepetitionDuration") {
		t.Error("длительность повтора задана явно — планировщик отвергнет задачу целиком")
	}
	if !strings.Contains(script, "Register-ScheduledTask") {
		t.Error("скрипт ничего не регистрирует")
	}
}

// Проверка «нужно ли обновлять задачу» читает у неё И повтор, И батарею: задача
// от прежней версии проходит по первому признаку, но остаётся выключенной на
// ноутбуке.
func TestAutostartUpgradeChecksBothSigns(t *testing.T) {
	probe := windowsAutostartProbeForTest()
	for _, needle := range []string{"Repetition", "DisallowStartIfOnBatteries", "StopIfGoingOnBatteries", "upgrade", "$t.State -eq 'Disabled'"} {
		if !strings.Contains(probe, needle) {
			t.Errorf("проверка задачи не смотрит на %q", needle)
		}
	}
}
