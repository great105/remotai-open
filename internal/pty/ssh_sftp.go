package pty

// Пул SFTP-подключений к SSH-серверам.
//
// Каждый файловый запрос (list/download/upload/…) поднимать SSH+SFTP заново —
// дорого (хендшейк сотни миллисекунд), поэтому клиенты кешируются в памяти
// агента. Пароль в пуле НЕ живёт: он нужен только на момент хендшейка,
// ключ кеша содержит лишь ПРИЗНАК password-auth, а не сам пароль.
//
// Пул не персистентен: рестарт агента закрывает все соединения.

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/pkg/sftp"
)

// sftpIdleTimeout — сколько неиспользуемое соединение держим в пуле.
const sftpIdleTimeout = 10 * time.Minute

// ErrSFTPSubsystem — SSH поднялся, а SFTP-подсистема на сервере недоступна
// (частая настройка: `Subsystem sftp` выключен, но shell работает). Отдельная
// ошибка нужна веб-слою: без неё отказ уезжал клиенту как безликие 500 без
// машинного кода, и экран файлов писал «Ошибка сервера» про исправный сервер.
var ErrSFTPSubsystem = errors.New("sftp subsystem unavailable")

// sftpEntry — закешированное соединение.
type sftpEntry struct {
	client   *sftp.Client
	ssh      *sshClient // закрывается вместе с клиентом
	lastUsed time.Time
	// leases — сколько долгих операций держат соединение прямо сейчас
	// (переносы ПК ↔ сервер, см. internal/web/ssh_transfers.go). Пока лиз хотя
	// бы один, janitor соединение не выбрасывает: перенос 200 МБ на медленном
	// канале идёт дольше idle-таймаута и без этого убивал бы сам себя.
	leases int
}

// SFTPPool — потокобезопасный кеш *sftp.Client с janitor'ом по idle.
type SFTPPool struct {
	mu      sync.Mutex
	conns   map[string]*sftpEntry
	now     func() time.Time // подменяется в тестах
	janitor chan struct{}
	once    sync.Once
}

// NewSFTPPool создаёт пул и запускает janitor (idle > 10 мин → close).
func NewSFTPPool() *SFTPPool {
	p := &SFTPPool{
		conns:   make(map[string]*sftpEntry),
		now:     time.Now,
		janitor: make(chan struct{}),
	}
	go p.janitorLoop()
	return p
}

// sftpPoolKey — ключ кеша: user@host:port + признак password-auth.
// Сам пароль в ключ НЕ входит (секрет, да ещё и в памяти дольше хендшейка).
func sftpPoolKey(cfg SSHConfig) string {
	port := cfg.Port
	if port <= 0 {
		port = 22
	}
	return fmt.Sprintf(
		"%s@%s:%d|pw=%t|id=%s|kp=%t|jump=%s|jpw=%t",
		cfg.User, cfg.Host, port, cfg.Password != "", cfg.IdentityFile,
		cfg.KeyPassphrase != "", cfg.ProxyJump, cfg.ProxyPassword != "",
	)
}

// Get возвращает живой клиент из кеша или поднимает новое SSH+SFTP
// подключение (вся auth/known_hosts/TOFU-логика — в dialSSH, ошибки уходят
// наверх как есть). Пароль из cfg используется только при (пере)подключении.
func (p *SFTPPool) Get(cfg SSHConfig) (*sftp.Client, error) {
	key := sftpPoolKey(cfg)

	p.mu.Lock()
	if e, ok := p.conns[key]; ok {
		e.lastUsed = p.now()
		p.mu.Unlock()
		return e.client, nil
	}
	p.mu.Unlock()

	// Подключение вне лока: хендшейк до 10 секунд, параллельные запросы к
	// другим серверам ждать не должны. Гонка «двое подключились» безвредна —
	// лишний клиент закрываем.
	sc, err := dialSSH(cfg)
	if err != nil {
		return nil, err
	}
	// UseConcurrentWrites: без него запись файла идёт по одному пакету (32 КБ)
	// за круговую задержку — на 30 мс RTT это ~1 МБ/с независимо от канала.
	// Чтение конкурентно и так (UseConcurrentReads включён по умолчанию).
	// ГРАБЛЯ: при ошибке посреди конкурентной записи файл на сервере может
	// остаться длиннее записанного и с «дырами», поэтому все пишущие пути
	// (upload/push) обязаны удалять недописанный файл — см. api_ssh_sftp.go.
	c, err := sftp.NewClient(sc.Client, sftp.UseConcurrentWrites(true))
	if err != nil {
		sc.Close()
		return nil, fmt.Errorf("%w: %v", ErrSFTPSubsystem, err)
	}

	p.mu.Lock()
	if e, ok := p.conns[key]; ok {
		// Кто-то успел раньше — отдаём его клиент, свой закрываем.
		e.lastUsed = p.now()
		p.mu.Unlock()
		c.Close()
		sc.Close()
		return e.client, nil
	}
	p.conns[key] = &sftpEntry{client: c, ssh: sc, lastUsed: p.now()}
	p.mu.Unlock()
	return c, nil
}

// Lease возвращает клиент и функцию освобождения. Пока лиз не отпущен,
// janitor это соединение по idle не выбрасывает — иначе долгий перенос файла
// (десятки минут без единого нового запроса) закрыл бы SSH сам себе.
// release идемпотентна; вызывать её ОБЯЗАТЕЛЬНО (defer в горутине переноса).
func (p *SFTPPool) Lease(cfg SSHConfig) (*sftp.Client, func(), error) {
	c, err := p.Get(cfg)
	if err != nil {
		return nil, nil, err
	}
	key := sftpPoolKey(cfg)

	p.mu.Lock()
	e, ok := p.conns[key]
	if ok && e.client == c {
		e.leases++
		e.lastUsed = p.now()
	} else {
		// Соединение уже успели заменить (Drop + новый Get) — клиент в руках
		// живой, но временем его жизни пул больше не управляет.
		ok = false
	}
	p.mu.Unlock()

	if !ok {
		return c, func() {}, nil
	}
	var once sync.Once
	return c, func() {
		once.Do(func() {
			p.mu.Lock()
			if cur, ok := p.conns[key]; ok && cur == e {
				if cur.leases > 0 {
					cur.leases--
				}
				cur.lastUsed = p.now()
			}
			p.mu.Unlock()
		})
	}, nil
}

// Drop выбрасывает соединение из пула (например, после сетевой ошибки —
// следующий запрос поднимет свежее, а не будет долбиться в мёртвое).
func (p *SFTPPool) Drop(cfg SSHConfig) {
	key := sftpPoolKey(cfg)
	p.mu.Lock()
	e, ok := p.conns[key]
	if ok {
		delete(p.conns, key)
	}
	p.mu.Unlock()
	if ok {
		e.client.Close()
		e.ssh.Close()
	}
}

// Close гасит janitor и все соединения (shutdown агента / тесты).
func (p *SFTPPool) Close() {
	p.once.Do(func() { close(p.janitor) })
	p.mu.Lock()
	entries := p.conns
	p.conns = make(map[string]*sftpEntry)
	p.mu.Unlock()
	for _, e := range entries {
		e.client.Close()
		e.ssh.Close()
	}
}

func (p *SFTPPool) janitorLoop() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-p.janitor:
			return
		case <-t.C:
			p.evictIdle()
		}
	}
}

func (p *SFTPPool) evictIdle() {
	cutoff := p.now().Add(-sftpIdleTimeout)
	p.mu.Lock()
	var dead []*sftpEntry
	for k, e := range p.conns {
		if e.leases > 0 {
			continue // идёт долгий перенос — соединение занято, не по idle
		}
		if e.lastUsed.Before(cutoff) {
			dead = append(dead, e)
			delete(p.conns, k)
		}
	}
	p.mu.Unlock()
	for _, e := range dead {
		e.client.Close()
		e.ssh.Close()
	}
}
