package hermes

import (
	"encoding/json"
	"strings"
)

func completionFailureDetail(output []byte) string {
	for _, line := range strings.Split(string(output), "\n") {
		if !strings.HasPrefix(line, "REMOTAI_HERMES_COMPLETE ") {
			continue
		}
		var report struct {
			Steps []string `json:"steps"`
			Kind  string   `json:"kind"`
		}
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "REMOTAI_HERMES_COMPLETE ")), &report) != nil {
			continue
		}
		phase := "Hermes не подтвердил готовность своей установки"
		failed := map[string]bool{}
		for _, step := range report.Steps {
			switch step {
			case "build", "maintenance", "launchers":
				failed[step] = true
			default:
				return ""
			}
		}
		if failed["build"] {
			phase = "Не удалось собрать компоненты Hermes"
		} else if failed["launchers"] {
			phase = "Не удалось подготовить команду запуска Hermes"
		} else if failed["maintenance"] {
			phase = "Не удалось завершить настройку профиля Hermes"
		}
		cause := map[string]string{
			"unknown":     "повторите подготовку; подробности сохранены в журнале установки на этом компьютере",
			"certificate": "не удалось проверить сертификат сервера загрузки; проверьте дату компьютера и настройки защищённого соединения",
			"disk":        "недостаточно свободного места на диске",
			"memory":      "не хватило оперативной памяти для подготовки компонентов",
			"permission":  "нет доступа к файлам установки; проверьте права и блокировку защитными программами",
			"network":     "серверы загрузки компонентов недоступны; проверьте соединение этого компьютера и повторите подготовку",
		}[report.Kind]
		if cause == "" {
			return ""
		}
		return phase + ": " + cause
	}
	return ""
}
