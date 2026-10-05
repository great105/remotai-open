package web

import (
	"strings"
	"testing"
)

// Каталог — единственное место, где записана граница власти агента. Если она
// поедет, агент начнёт менять то, что может отрезать человека от компьютера.
func TestCatalogMarksDangerousSettings(t *testing.T) {
	// peer_access здесь по той же причине, что и cloud: он раздаёт права на
	// ЭТОТ компьютер другому устройству аккаунта (серверу в чужом дата-центре).
	// Агент, работающий в терминале этого компьютера, включить себе полный
	// доступ извне сам не может — только с подтверждением владельца.
	danger := map[string]bool{"web_port": true, "autostart": true, "cloud": true, "peer_access": true}
	seen := map[string]bool{}
	for _, spec := range settingsCatalog() {
		seen[spec.Key] = true
		if danger[spec.Key] && spec.Risk != RiskDangerous {
			t.Fatalf("%s обязана быть опасной: она отрезает человека от компьютера", spec.Key)
		}
		if !danger[spec.Key] && spec.Risk != RiskSafe {
			t.Fatalf("%s помечена опасной без причины — агент не сможет настроить очевидное", spec.Key)
		}
		if strings.TrimSpace(spec.Title) == "" || strings.TrimSpace(spec.Hint) == "" {
			t.Fatalf("%s без описания: агент будет угадывать, что это", spec.Key)
		}
	}
	for key := range danger {
		if !seen[key] {
			t.Fatalf("опасная настройка %q пропала из каталога", key)
		}
	}
}

// Значение из списка проверяется ДО записи.
//
// Живой прогон ловил ровно это: список допустимых строился из НАЙДЕННЫХ на
// машине агентов, на чистом стенде оказывался пустым — и `default_agent =
// несуществующий` записывался молча.
func TestValidateSettingRejectsUnknownAgent(t *testing.T) {
	spec, ok := findSetting("default_agent")
	if !ok {
		t.Fatal("настройка default_agent пропала")
	}
	if len(spec.Options) == 0 {
		t.Fatal("список агентов пуст — проверка значения превращается в пропуск чего угодно")
	}
	if _, err := validateSetting(spec, "несуществующий"); err == nil {
		t.Fatal("чужое значение принято")
	}
	if _, err := validateSetting(spec, ""); err == nil {
		t.Fatal("пустое значение принято: кнопка запуска перестанет работать")
	}
	if _, err := validateSetting(spec, "claude"); err != nil {
		t.Fatalf("настоящий агент отвергнут: %v", err)
	}
}

// Человеческие «да/нет» агент пишет по-разному — принимаем оба языка, но не
// произвольный мусор.
func TestValidateSettingBoolAndInt(t *testing.T) {
	boolSpec, _ := findSetting("notifications_enabled")
	for _, yes := range []string{"true", "1", "да", "ON"} {
		if v, err := validateSetting(boolSpec, yes); err != nil || v != true {
			t.Fatalf("%q не понято как «включить»: %v", yes, err)
		}
	}
	for _, no := range []string{"false", "0", "нет", "выкл"} {
		if v, err := validateSetting(boolSpec, no); err != nil || v != false {
			t.Fatalf("%q не понято как «выключить»: %v", no, err)
		}
	}
	if _, err := validateSetting(boolSpec, "может быть"); err == nil {
		t.Fatal("непонятное значение принято за булево")
	}

	intSpec, _ := findSetting("max_concurrent_sessions")
	if _, err := validateSetting(intSpec, "-1"); err == nil {
		t.Fatal("отрицательное число принято")
	}
	if v, err := validateSetting(intSpec, "3"); err != nil || v != 3 {
		t.Fatalf("число не разобрано: %v (%v)", v, err)
	}
}
