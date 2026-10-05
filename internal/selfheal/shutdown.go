package selfheal

import (
	"strings"
	"time"
	"unicode/utf8"
)

// ShutdownInfo — что журнал операционной системы говорит о прошлом выключении.
//
// Kind: planned (кто-то или что-то попросило выключиться штатно) | power
// (питание пропало / система зависла) | bugcheck (синий экран).
type ShutdownInfo struct {
	Kind   string
	At     time.Time
	Reason string // человеческая формулировка: «установка обновлений Windows»
	Detail string // техническая подробность: код bugcheck и т.п.
}

// initiatorReason переводит процесс-инициатор из события 1074 в то, что человек
// и так знает про свой компьютер.
//
// ПОЧЕМУ ПО ПРОЦЕССУ, А НЕ ПО ТЕКСТУ СОБЫТИЯ: текстовое описание в журнале
// локализовано (на этой машине — «Операционная система: обновление») и приходит
// из wevtutil в консольной кодировке, то есть кириллица в нём бьётся ещё до
// нашего разбора. Имя процесса и код причины — ASCII и от языка не зависят.
func initiatorReason(image string) string {
	name := image
	if i := strings.LastIndexAny(name, `\/`); i >= 0 {
		name = name[i+1:]
	}
	if i := strings.IndexByte(name, ' '); i > 0 {
		name = name[:i] // wevtutil дописывает « (ИМЯ-КОМПЬЮТЕРА)»
	}
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "trustedinstaller.exe", "wuauclt.exe", "usoclient.exe", "musnotification.exe", "musnotificationux.exe":
		return "установка обновлений Windows"
	case "explorer.exe", "startmenuexperiencehost.exe", "shellexperiencehost.exe", "logonui.exe", "winlogon.exe":
		return "перезагрузку попросил человек за компьютером"
	case "shutdown.exe":
		return "команда shutdown"
	case "remotai.exe", "tgcontrol.exe":
		return "перезагрузку заказали через Remotai"
	case "svchost.exe", "services.exe":
		return "служба Windows"
	case "rundll32.exe", "sihost.exe":
		return "системный компонент Windows"
	}
	if name == "" {
		return "штатное завершение работы"
	}
	return "инициатор — " + name
}

// sanitizeUTF8 выбрасывает байты, не составляющие корректный UTF-8.
//
// ГРАБЛЯ, ради которой это написано: wevtutil печатает XML в кодировке консоли,
// поэтому локализованные поля («Операционная система: обновление») приезжают
// байтами CP1251/CP866. Один такой байт делает весь документ невалидным, и
// разбор падает целиком — вместе с полями, которые нам как раз и нужны.
func sanitizeUTF8(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			b.WriteByte('?')
			i++
			continue
		}
		b.WriteString(s[i : i+size])
		i += size
	}
	return b.String()
}
