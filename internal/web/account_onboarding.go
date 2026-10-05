package web

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// Мастер первого запуска у аккаунта со своим каталогом.
//
// ЖАЛОБА, С КОТОРОЙ ЭТО НАЧАЛОСЬ (14.08.2026, владелец, с телефона): «зашёл во
// второй аккаунт, а когда выбираю рабочий и нажимаю „Войти“ — пишет, что не
// зайдено, надо заново заходить».
//
// ЧТО ПРОИСХОДИТ НА САМОМ ДЕЛЕ (снято живым прогоном в настоящем ConPTY, а не
// вычитано): Claude Code решает, показывать ли мастер первого запуска, по
// ОДНОМУ ключу `hasCompletedOnboarding` в `.claude.json` СВОЕГО каталога. У
// второй подписки каталог свой и чистый — ключа там нет, и мастер идёт с
// самого начала при КАЖДОМ запуске:
//
//	Welcome to Claude Code → выбор темы → «Select login method» → «Opening
//	browser to sign in…»
//
// То есть человек с живым, только что выданным токеном видит требование войти
// заново. Проверено обеими сторонами: `claude auth status` в том же каталоге
// отвечает `loggedIn: true`, а мастер всё равно доходит до экрана входа.
//
// ЗАМКНУТЫЙ КРУГ, из-за которого это не проходит само. Ключ ставится в САМОМ
// КОНЦЕ мастера (и ещё внутри `claude auth login`). Человек входит на середине
// — токен уже записан, а мастер он закрывает, не дожав до конца: цель-то
// достигнута, он вошёл. Ключа нет — в следующий раз всё сначала. Именно это и
// случилось у владельца: токен от 21:33, а `hasCompletedOnboarding` в файле
// отсутствовал вовсе.
//
// ПОЧЕМУ ЛЕЧИМ МЫ, А НЕ ЖДЁМ ОТ ЧЕЛОВЕКА. Каталог аккаунта завели мы, и пустым
// он стал из-за нашей же схемы «аккаунт = каталог». Для человека это выглядит
// поломкой продукта, а не незавершённым мастером чужого CLI.
//
// ГРАНИЦА: ключ дописываем ТОЛЬКО когда вход уже есть. Поставить его раньше
// было бы вредно — мастер как раз и ведёт человека ко входу, и без него
// незалогиненный агент молча дошёл бы до первого запроса и упал на отказе
// провайдера.

// claudeOnboardingKey — ключ, по которому Claude Code решает, показывать ли
// мастер. Проверено запуском: `.claude.json` РОВНО с этим одним ключом уже
// снимает и выбор темы, и экран входа (остаётся только вопрос о доверии к
// папке — это согласие человека, и подделывать его мы не вправе).
const claudeOnboardingKey = "hasCompletedOnboarding"

// healClaudeOnboarding — дописать «мастер пройден» в каталог аккаунта, в
// который уже вошли.
//
// Возвращает true, только если файл действительно изменили: это едет в лог,
// чтобы разбор следующей такой жалобы начинался с факта, а не с догадки.
func healClaudeOnboarding(dir string) bool {
	if strings.TrimSpace(dir) == "" {
		return false // основной аккаунт: его каталог настроен человеком, не нами
	}
	if !claudeSignedIn(dir) {
		return false
	}

	target := filepath.Join(dir, ".claude.json")
	config := map[string]json.RawMessage{}
	if raw, err := os.ReadFile(target); err == nil {
		if err := json.Unmarshal(raw, &config); err != nil {
			// Чужой файл с мусором не чиним: перезапись стоила бы человеку
			// настроек, а мастер — это неудобство, а не потеря.
			log.Printf("[ACCOUNTS] %s не разобрать, мастер не чиним: %v", target, err)
			return false
		}
	} else if !os.IsNotExist(err) {
		return false
	}
	if done, ok := config[claudeOnboardingKey]; ok && strings.TrimSpace(string(done)) == "true" {
		return false
	}
	config[claudeOnboardingKey] = json.RawMessage("true")

	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return false
	}
	// Пишем через временный файл: в тот же `.claude.json` пишет и сам агент,
	// а частично записанный конфиг он читает как испорченный.
	tmp := target + ".remotai-tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		log.Printf("[ACCOUNTS] мастер первого запуска не починен (%s): %v", dir, err)
		return false
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		log.Printf("[ACCOUNTS] мастер первого запуска не починен (%s): %v", dir, err)
		return false
	}
	log.Printf("[ACCOUNTS] аккаунт claude %s: вход есть, дописан %s — мастер больше не спросит логин", dir, claudeOnboardingKey)
	return true
}

// claudeSignedIn — есть ли в каталоге аккаунта живой вход.
//
// Смотрим ровно факт наличия токена; само значение не читаем дальше проверки
// на пустоту и никуда не выносим.
func claudeSignedIn(dir string) bool {
	raw, err := os.ReadFile(filepath.Join(dir, ".credentials.json"))
	if err != nil {
		return false
	}
	var creds struct {
		OAuth struct {
			AccessToken string `json:"accessToken"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(raw, &creds); err != nil {
		return false
	}
	return strings.TrimSpace(creds.OAuth.AccessToken) != ""
}

// healAccountsOnboarding — пройтись по нашим каталогам аккаунтов Claude.
//
// Зовётся там, где человек и так смотрит на список (открыл раздел «Агенты» или
// шторку запуска) и там, где он выбирает аккаунт: к моменту запуска агента
// каталог уже вылечен. Стоимость — два маленьких файла на аккаунт, и только у
// заведённых нами: у основного каталог чужой, туда не лезем.
func healAccountsOnboarding(f accountsFile) {
	for _, a := range f.Accounts {
		if a.AgentID != "claude" || a.Dir == "" {
			continue
		}
		if !withinAccountsRoot(a.Dir) {
			continue
		}
		healClaudeOnboarding(a.Dir)
	}
}
