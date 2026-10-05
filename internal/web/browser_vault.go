package web

// Пароли и анкета для виртуального браузера.
//
// Зачем это вообще: с телефона на сервере РЕГИСТРИРУЮТСЯ. Регистрация — это
// каждый раз одно и то же: имя, почта, телефон и новый пароль, который надо
// придумать и потом не потерять. На телефоне через удалённый экран это самая
// утомительная часть: длинный пароль набирается по буквам, а в следующий раз
// его негде взять — свой менеджер паролей на телефоне о сайтах, открытых на
// сервере, ничего не знает.
//
// Поэтому пароли и анкета живут РЯДОМ С БРАУЗЕРОМ, на той же машине:
//   - шифрует internal/secretbox (Windows — DPAPI учётной записи, иначе
//     AES-256-GCM ключом 0600), как у SSH-доступов;
//   - наружу отдаются только логины и хосты, пароль не покидает машину ни
//     одним эндпоинтом — он попадает прямо в поле страницы через протокол
//     браузера;
//   - чужой контейнер (файл с другой машины) даёт честное «зашифровано другой
//     учётной записью», а не пустой список.
//
// Это тот же принцип, который владелец утвердил для SSH: доступы хранятся, но
// не покидают ПК.

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"tgcontrol/internal/paths"
	"tgcontrol/internal/secretbox"
)

// BrowserLogin — одна сохранённая пара для сайта.
type BrowserLogin struct {
	ID       string `json:"id"`
	Host     string `json:"host"`
	Login    string `json:"login"`
	Password string `json:"-"` // наружу не отдаётся никогда
	Note     string `json:"note,omitempty"`
	Created  int64  `json:"created"`
	UsedAt   int64  `json:"used_at,omitempty"`
}

// BrowserProfile — анкета, которой заполняются формы регистрации.
type BrowserProfile struct {
	FirstName string `json:"first_name,omitempty"`
	LastName  string `json:"last_name,omitempty"`
	Email     string `json:"email,omitempty"`
	Phone     string `json:"phone,omitempty"`
	Birthday  string `json:"birthday,omitempty"` // YYYY-MM-DD
	Country   string `json:"country,omitempty"`
	City      string `json:"city,omitempty"`
	Address   string `json:"address,omitempty"`
	ZIP       string `json:"zip,omitempty"`
}

// Filled — есть ли в анкете хоть что-то (интерфейс не должен предлагать
// «Заполнить», когда заполнять нечем).
func (p BrowserProfile) Filled() bool {
	return p.FirstName != "" || p.LastName != "" || p.Email != "" || p.Phone != "" ||
		p.Birthday != "" || p.City != "" || p.Address != "" || p.ZIP != ""
}

type browserVaultFile struct {
	Version int             `json:"version"`
	Logins  []vaultLoginRec `json:"logins"`
	Profile BrowserProfile  `json:"profile"`
}

type vaultLoginRec struct {
	ID       string `json:"id"`
	Host     string `json:"host"`
	Login    string `json:"login"`
	Password string `json:"password"`
	Note     string `json:"note,omitempty"`
	Created  int64  `json:"created"`
	UsedAt   int64  `json:"used_at,omitempty"`
}

type browserVault struct {
	mu      sync.RWMutex
	path    string
	logins  []vaultLoginRec
	profile BrowserProfile
	loaded  bool
	// foreign — файл цел, но зашифрован другой учётной записью: пароли не
	// потеряны, просто недоступны отсюда, и об этом надо сказать словами.
	foreign bool
}

var vault = &browserVault{}

func (v *browserVault) ensureLoaded() {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.loaded {
		return
	}
	v.loaded = true
	v.path = paths.StateFile("browser-vault.enc")
	raw, err := secretbox.ReadFile(v.path)
	if err != nil {
		if errors.Is(err, secretbox.ErrForeign) {
			v.foreign = true
			log.Printf("[VBROWSER] хранилище паролей зашифровано другой учётной записью")
		}
		return
	}
	if len(raw) == 0 {
		return
	}
	var file browserVaultFile
	if err := json.Unmarshal(raw, &file); err != nil {
		log.Printf("[VBROWSER] хранилище паролей не прочитано: %v", err)
		return
	}
	v.logins = file.Logins
	v.profile = file.Profile
}

// saveLocked записывает хранилище. Вызывать под взятым замком.
func (v *browserVault) saveLocked() error {
	data, err := json.Marshal(browserVaultFile{Version: 1, Logins: v.logins, Profile: v.profile})
	if err != nil {
		return err
	}
	return secretbox.WriteFile(v.path, data)
}

// Foreign — хранилище недоступно этой учётной записи.
func (v *browserVault) Foreign() bool {
	v.ensureLoaded()
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.foreign
}

// List отдаёт сохранённые входы БЕЗ паролей, свежие сверху. host="" — все.
func (v *browserVault) List(host string) []BrowserLogin {
	v.ensureLoaded()
	v.mu.RLock()
	defer v.mu.RUnlock()
	host = normalizeVaultHost(host)
	out := make([]BrowserLogin, 0, len(v.logins))
	for _, rec := range v.logins {
		if host != "" && !hostMatches(rec.Host, host) {
			continue
		}
		out = append(out, BrowserLogin{
			ID: rec.ID, Host: rec.Host, Login: rec.Login, Note: rec.Note,
			Created: rec.Created, UsedAt: rec.UsedAt,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].UsedAt != out[j].UsedAt {
			return out[i].UsedAt > out[j].UsedAt
		}
		return out[i].Created > out[j].Created
	})
	return out
}

// Secret отдаёт пароль записи ТОЛЬКО внутрь агента: он уходит прямо в поле
// страницы, наружу не возвращается.
func (v *browserVault) Secret(id string) (vaultLoginRec, bool) {
	v.ensureLoaded()
	v.mu.Lock()
	defer v.mu.Unlock()
	for i := range v.logins {
		if v.logins[i].ID == id {
			v.logins[i].UsedAt = time.Now().Unix()
			rec := v.logins[i]
			if err := v.saveLocked(); err != nil {
				log.Printf("[VBROWSER] отметка использования пароля: %v", err)
			}
			return rec, true
		}
	}
	return vaultLoginRec{}, false
}

// Save добавляет или обновляет запись. Пара «хост + логин» одна: повторный
// вход на тот же сайт обновляет пароль, а не плодит двойников, иначе список
// быстро превращается в свалку, где непонятно, какой пароль действующий.
func (v *browserVault) Save(host, login, password, note string) (BrowserLogin, error) {
	host = normalizeVaultHost(host)
	login = strings.TrimSpace(login)
	if host == "" {
		return BrowserLogin{}, fmt.Errorf("не указан сайт")
	}
	if password == "" {
		return BrowserLogin{}, fmt.Errorf("не указан пароль")
	}
	v.ensureLoaded()
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.foreign {
		return BrowserLogin{}, fmt.Errorf("хранилище зашифровано другой учётной записью")
	}
	now := time.Now().Unix()
	for i := range v.logins {
		if v.logins[i].Host == host && strings.EqualFold(v.logins[i].Login, login) {
			v.logins[i].Password = password
			v.logins[i].Note = note
			v.logins[i].UsedAt = now
			rec := v.logins[i]
			if err := v.saveLocked(); err != nil {
				return BrowserLogin{}, err
			}
			return BrowserLogin{ID: rec.ID, Host: rec.Host, Login: rec.Login, Note: rec.Note,
				Created: rec.Created, UsedAt: rec.UsedAt}, nil
		}
	}
	rec := vaultLoginRec{
		ID: newVaultID(), Host: host, Login: login, Password: password,
		Note: note, Created: now, UsedAt: now,
	}
	v.logins = append(v.logins, rec)
	if err := v.saveLocked(); err != nil {
		return BrowserLogin{}, err
	}
	return BrowserLogin{ID: rec.ID, Host: rec.Host, Login: rec.Login, Note: rec.Note,
		Created: rec.Created, UsedAt: rec.UsedAt}, nil
}

// Delete убирает запись.
func (v *browserVault) Delete(id string) bool {
	v.ensureLoaded()
	v.mu.Lock()
	defer v.mu.Unlock()
	for i := range v.logins {
		if v.logins[i].ID == id {
			v.logins = append(v.logins[:i], v.logins[i+1:]...)
			if err := v.saveLocked(); err != nil {
				log.Printf("[VBROWSER] удаление пароля: %v", err)
			}
			return true
		}
	}
	return false
}

// Profile отдаёт анкету (её поля не секрет в том же смысле, что пароль, но
// лежат в том же шифрованном файле: адрес и телефон — это личные данные).
func (v *browserVault) Profile() BrowserProfile {
	v.ensureLoaded()
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.profile
}

// SetProfile сохраняет анкету целиком.
func (v *browserVault) SetProfile(p BrowserProfile) error {
	v.ensureLoaded()
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.foreign {
		return fmt.Errorf("хранилище зашифровано другой учётной записью")
	}
	v.profile = p
	return v.saveLocked()
}

// normalizeVaultHost приводит адрес к хосту: сохранять пароль «для страницы»
// бессмысленно, он нужен для сайта.
func normalizeVaultHost(raw string) string {
	value := strings.TrimSpace(strings.ToLower(raw))
	if value == "" {
		return ""
	}
	if strings.Contains(value, "://") {
		u, err := url.Parse(value)
		if err != nil || u.Host == "" {
			// Локальный файл или служебная страница браузера — это не сайт, и
			// «пароль для file» в списке выглядел бы мусором (живая находка на
			// стенде: запись сохранилась с хостом «file»).
			return ""
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return ""
		}
		value = u.Host
	}
	if i := strings.Index(value, "/"); i > 0 {
		value = value[:i]
	}
	if i := strings.Index(value, ":"); i > 0 {
		value = value[:i]
	}
	return strings.TrimPrefix(value, "www.")
}

// hostMatches — совпадает ли сохранённый сайт с текущим. Поддомен считается
// тем же сайтом: вход, сохранённый на accounts.google.com, нужен и на
// mail.google.com — иначе человек ищет пароль руками там, где он уже есть.
func hostMatches(saved, current string) bool {
	if saved == current {
		return true
	}
	return strings.HasSuffix(current, "."+saved) || strings.HasSuffix(saved, "."+current)
}

func newVaultID() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	out := make([]byte, 12)
	for i := range out {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			return fmt.Sprintf("id%d", time.Now().UnixNano())
		}
		out[i] = alphabet[n.Int64()]
	}
	return string(out)
}

// generatePassword придумывает пароль, который примут почти везде: длинный, с
// цифрой, заглавной и знаком, но без символов, на которых спотыкаются формы
// (кавычки, обратный слэш, пробел) и без похожих друг на друга букв — его ведь
// иногда приходится перепечатывать глазами с одного экрана на другой.
func generatePassword(length int) (string, error) {
	if length < 8 {
		length = 16
	}
	if length > 64 {
		length = 64
	}
	const (
		lower  = "abcdefghijkmnpqrstuvwxyz"
		upper  = "ABCDEFGHJKLMNPQRSTUVWXYZ"
		digits = "23456789"
		marks  = "!@#$%^&*-_=+?"
	)
	groups := []string{lower, upper, digits, marks}
	all := lower + upper + digits + marks

	pick := func(set string) (byte, error) {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(set))))
		if err != nil {
			return 0, err
		}
		return set[n.Int64()], nil
	}
	out := make([]byte, 0, length)
	for _, set := range groups { // по одному символу каждого вида — требование форм
		ch, err := pick(set)
		if err != nil {
			return "", err
		}
		out = append(out, ch)
	}
	for len(out) < length {
		ch, err := pick(all)
		if err != nil {
			return "", err
		}
		out = append(out, ch)
	}
	// Перемешиваем: иначе первые четыре символа всегда шли бы в одном порядке
	// видов, и пароль был бы предсказуемее, чем выглядит.
	for i := len(out) - 1; i > 0; i-- {
		j, err := rand.Int(rand.Reader, big.NewInt(int64(i+1)))
		if err != nil {
			return "", err
		}
		out[i], out[j.Int64()] = out[j.Int64()], out[i]
	}
	return string(out), nil
}
