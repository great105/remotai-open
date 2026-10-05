package openrouter

import (
	"encoding/json"
	"log"
	"os"
	"strings"
	"sync"

	"tgcontrol/internal/secretbox"
)

// EnvName — переменная окружения, из которой CLI-агенты берут ключ OpenRouter.
//
// ПРОВЕРЕНО ЗАПУСКОМ (WSL Ubuntu, opencode 1.18.15, 07.08.2026), а не доками:
// одной этой переменной достаточно — `opencode models openrouter` показал
// каталог, а `opencode run -m openrouter/cohere/north-mini-code:free "…"`
// выполнил задачу и ответил. Интерактивный `/connect` не нужен вовсе.
// Ту же переменную читают crush и aider, поэтому путь общий, а не «под один CLI».
const EnvName = "OPENROUTER_API_KEY"

// ГЛАВНОЕ ПРАВИЛО ЭТОГО ФАЙЛА: КЛЮЧ НЕ ПОПАДАЕТ В ТЕКСТ КОМАНДЫ.
//
// Команду запуска агента собирает КЛИЕНТ (apk/src/ptyTerm/agentLaunch.ts) и
// отправляет её строкой в терминал — а клиент живёт на телефоне и ходит через
// облако. Подставь мы ключ туда, как подставляем каталог аккаунта, ключ уехал бы
// с ПК на телефон и через релей, да ещё и осел бы в истории терминала.
//
// Поэтому ключ живёт в ОКРУЖЕНИИ ПРОЦЕССА АГЕНТА: каждый терминал — это
// отдельный pty-host, порождаемый агентом, и окружение он наследует
// (buildEnvBlock в internal/pty/pty_windows.go, os.Environ() на POSIX).
// Наружу не уходит ничего, кроме маскированной метки от самого OpenRouter.
//
// СЛЕДСТВИЕ, о котором обязан сказать интерфейс: уже открытые терминалы ключа не
// получат — они родились раньше. Действует в новых.

// Store — ключ и выбранная модель на этом компьютере.
type Store struct {
	mu    sync.RWMutex
	path  string
	key   string
	model string
	// foreign — файл цел, но зашифрован другой учётной записью Windows. Ключ не
	// потерян, просто недоступен отсюда; сказать это словами — не то же самое,
	// что показать пустое поле (та же граблю уже проходили с паролями SSH).
	foreign bool
}

type vault struct {
	Version int    `json:"version"`
	Key     string `json:"key,omitempty"`
	Model   string `json:"model,omitempty"`
}

// NewStore поднимает хранилище и СРАЗУ применяет ключ к окружению агента.
// Ошибка чтения не мешает старту: без ключа продукт работает ровно как раньше.
func NewStore(path string) *Store {
	s := &Store{path: path}
	if path == "" {
		return s
	}
	blob, err := secretbox.ReadFile(path)
	if err != nil {
		if err == secretbox.ErrForeign {
			s.foreign = true
			log.Printf("[OPENROUTER] сохранённый ключ принадлежит другой учётной записи Windows")
		} else {
			log.Printf("[OPENROUTER] не читается сохранённый ключ: %v", err)
		}
		return s
	}
	if len(blob) == 0 {
		return s
	}
	var v vault
	if err := json.Unmarshal(blob, &v); err != nil {
		log.Printf("[OPENROUTER] сохранённый ключ нечитаем: %v", err)
		return s
	}
	s.key, s.model = v.Key, v.Model
	s.applyEnv()
	return s
}

// Set сохраняет ключ и применяет его к окружению. Пустой ключ = забыть.
func (s *Store) Set(key string) error {
	s.mu.Lock()
	s.key = strings.TrimSpace(key)
	s.mu.Unlock()
	s.applyEnv()
	return s.save()
}

// SetModel запоминает модель, которой запускать агента.
func (s *Store) SetModel(model string) error {
	s.mu.Lock()
	s.model = strings.TrimSpace(model)
	s.mu.Unlock()
	return s.save()
}

// Clear забывает ключ И снимает его с окружения. Второе обязательно: иначе
// «отключить» означало бы «не показывать», а терминалы продолжали бы работать
// с ключом, который человек считает удалённым.
func (s *Store) Clear() error {
	s.mu.Lock()
	s.key, s.model = "", ""
	s.mu.Unlock()
	os.Unsetenv(EnvName)
	if s.path == "" {
		return nil
	}
	if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (s *Store) Key() string {
	if s == nil {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.key
}

func (s *Store) Model() string {
	if s == nil {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.model
}

func (s *Store) Configured() bool { return s.Key() != "" }

func (s *Store) Foreign() bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.foreign
}

// applyEnv кладёт ключ в окружение самого агента — отсюда его унаследуют все
// терминалы, которые агент откроет дальше.
func (s *Store) applyEnv() {
	key := s.Key()
	if key == "" {
		os.Unsetenv(EnvName)
		return
	}
	// Уважаем ключ, заданный человеком снаружи (в системных переменных или в
	// .env): он был раньше нас и мог быть выставлен намеренно.
	if existing := strings.TrimSpace(os.Getenv(EnvName)); existing != "" && existing != key {
		log.Printf("[OPENROUTER] переменная %s уже задана в системе — сохранённый ключ не подставляем", EnvName)
		return
	}
	os.Setenv(EnvName, key)
}

func (s *Store) save() error {
	if s.path == "" {
		return nil
	}
	s.mu.RLock()
	v := vault{Version: 1, Key: s.key, Model: s.model}
	s.mu.RUnlock()
	blob, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err := secretbox.WriteFile(s.path, blob); err != nil {
		return err
	}
	s.mu.Lock()
	s.foreign = false
	s.mu.Unlock()
	return nil
}
