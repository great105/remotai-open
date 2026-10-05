package pty

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	sshagent "golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"

	"tgcontrol/internal/paths"
)

// SSHConfig — параметры SSH-подключения, которое АГЕНТ устанавливает от своего
// имени (бастион): телефон шлёт хост/юзера/пароль, агент коннектится к серверу,
// и сессия показывается как обычный PTY-терминал (тот же /ws/pty/{id}).
type SSHConfig struct {
	HostID   string
	Host     string
	Port     int
	User     string
	Password string // нигде не логируется и не сохраняется
	Cols     int
	Rows     int
	// TrustHost — принять неизвестный хост-ключ и дописать его в known_hosts
	// агента (TOFU). На изменившийся ключ (MITM) не действует. Действует и на
	// бастион ProxyJump, и на целевой хост.
	TrustHost bool
	// ProxyJump — бастион в формате "user@host:port" (user и порт опциональны,
	// порт по умолчанию 22). Поддерживается один прыжок, как в подавляющем
	// большинстве конфигов; цепочки через запятую не разбираем.
	ProxyJump string
	// ProxyPassword — пароль для бастиона (тот же принцип: только память
	// хендшейка, не логируется и не сохраняется).
	ProxyPassword string
	// IdentityFile is an explicit deploy key from ~/.ssh/config or a saved
	// host. KeyPassphrase unlocks an encrypted key for this handshake only.
	IdentityFile  string
	KeyPassphrase string
	// PrivateKeyPEM — ключ из хранилища приложения (см. ssh_keys_store.go).
	// Он не лежит файлом на диске в открытом виде, поэтому приходит сюда
	// содержимым и пробуется ПЕРВЫМ: человек назначил серверу именно его.
	PrivateKeyPEM string
}

// ErrHostKeyUnknown — сервер предъявил ключ, которого нет в known_hosts агента.
// Клиент показывает Fingerprint пользователю и спрашивает доверие; повторный
// запрос с TrustHost:true дописывает ключ (TOFU).
type ErrHostKeyUnknown struct {
	Fingerprint string // sha256, формат как у ssh-keygen ("SHA256:…")
}

type ErrHostKeyMismatch struct {
	Host         string
	Got          string
	Fingerprints []string
}

func (e *ErrHostKeyMismatch) Error() string {
	return "host key mismatch for " + e.Host
}

type ErrSSHKeyEncrypted struct {
	Path string
}

func (e *ErrSSHKeyEncrypted) Error() string {
	if e.Path == "" {
		// Ключ из хранилища приложения файлом на диске не лежит — называть
		// нечего, и «encrypted: » с пустым хвостом читалось бы как обрыв.
		return "ssh private key is encrypted"
	}
	return "ssh private key is encrypted: " + e.Path
}

type ErrSSHAuthDetails struct {
	Tried []string
}

func (e *ErrSSHAuthDetails) Error() string { return ErrSSHAuth.Error() }
func (e *ErrSSHAuthDetails) Unwrap() error { return ErrSSHAuth }

func (e *ErrHostKeyUnknown) Error() string {
	return "unknown host key " + e.Fingerprint
}

// Маркерные ошибки для маппинга в машинные code на API (см. web/api_ssh.go).
var (
	// ErrSSHAuth — сервер отклонил все методы аутентификации (пароль/ключи).
	ErrSSHAuth = errors.New("ssh: authentication failed")
	// ErrSSHUnreachable — хост недоступен с ПК-агента (dial/таймаут/хендшейк).
	ErrSSHUnreachable = errors.New("ssh: host unreachable")
)

// sshConn — ptyConn поверх golang.org/x/crypto/ssh: удалённая интерактивная
// shell-сессия с запрошенным PTY. Session/readLoop работают с ним точно так
// же, как с локальным ConPTY — нового клиентского протокола нет.
//
// Сессия живёт только в памяти агента и НЕ персистентна: рестарт агента её
// убивает (в отличие от persistent pty-host). Переподключаться после рестарта
// не из чего — пароль не храним, hostRecord не пишем.
type sshConn struct {
	client  *sshClient
	session *ssh.Session
	stdin   io.Writer
	stdout  io.Reader
	label   string // "ssh:user@host" — ярлык для UI (currentCWD)

	mu     sync.Mutex
	closed bool
	// stopKeepalive останавливает пульс соединения (см. keepaliveLoop).
	stopKeepalive chan struct{}
	keepaliveDone chan struct{}
}

// Пульс SSH-соединения — аналог ServerAliveInterval у обычного ssh-клиента.
//
// Живая жалоба владельца (2026-07-27): «захожу в сервер, запускаю команду,
// выхожу обратно — сервер закрывается». Терминал SSH живёт в памяти агента и
// сам по себе никуда не девается, но пока человек смотрит в другой экран, по
// соединению НЕ идёт ни байта, — и его рвёт первый же, кому надоест ждать:
// sshd с ClientAliveInterval, NAT провайдера, домашний роутер, файрвол. Со
// стороны это выглядит как «Remotai закрыл мой сервер», хотя закрыл его не он.
//
// 30 секунд — то же значение, что советует документация OpenSSH для ненадёжных
// NAT, и вчетверо меньше самого распространённого таймаута в 120 с.
// Переменная, а не константа: тест подменяет такт, чтобы не ждать полминуты.
var sshKeepaliveEvery = 30 * time.Second

// keepaliveLoop шлёт серверу «я ещё здесь» и умирает вместе с соединением.
func (c *sshConn) keepaliveLoop() {
	defer close(c.keepaliveDone)
	ticker := time.NewTicker(sshKeepaliveEvery)
	defer ticker.Stop()
	for {
		select {
		case <-c.stopKeepalive:
			return
		case <-ticker.C:
			// Запрос заведомо неизвестного типа: сервер обязан ответить отказом,
			// и этот отказ — доказательство живого канала. Ошибка означает, что
			// соединения больше нет: тогда молчим и выходим, а смерть сессии
			// заметит readLoop.
			if _, _, err := c.client.SendRequest("keepalive@openssh.com", true, nil); err != nil {
				return
			}
		}
	}
}

// ConnectSSH устанавливает SSH-подключение и открывает интерактивный shell с
// PTY. Аутентификация: сначала ключи агента (~/.ssh/id_ed25519, id_rsa,
// id_ecdsa — зашифрованные паролем молча пропускаем), затем пароль, если задан.
func ConnectSSH(cfg SSHConfig) (ptyConn, error) {
	if cfg.Cols <= 0 {
		cfg.Cols = 80
	}
	if cfg.Rows <= 0 {
		cfg.Rows = 24
	}
	client, err := dialSSH(cfg)
	if err != nil {
		return nil, err
	}

	session, err := client.NewSession()
	if err != nil {
		client.Close()
		return nil, err
	}
	// Pipes подключаем ДО Shell; stderr отдельно не читаем — с PTY он слит в stdout.
	stdout, err := session.StdoutPipe()
	if err != nil {
		session.Close()
		client.Close()
		return nil, err
	}
	stdin, err := session.StdinPipe()
	if err != nil {
		session.Close()
		client.Close()
		return nil, err
	}
	modes := ssh.TerminalModes{
		ssh.ECHO:          1, // эхо делает удалённая сторона
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}
	if err := session.RequestPty("xterm-256color", cfg.Rows, cfg.Cols, modes); err != nil {
		session.Close()
		client.Close()
		return nil, err
	}
	if err := session.Shell(); err != nil {
		session.Close()
		client.Close()
		return nil, err
	}

	conn := &sshConn{
		client:        client,
		session:       session,
		stdin:         stdin,
		stdout:        stdout,
		label:         "ssh:" + cfg.User + "@" + cfg.Host,
		stopKeepalive: make(chan struct{}),
		keepaliveDone: make(chan struct{}),
	}
	// Пульс соединения: без него простаивающую сессию рвёт первый же таймаут по
	// пути (sshd, NAT, роутер) — см. keepaliveLoop.
	go conn.keepaliveLoop()
	return conn, nil
}

// sshClient — *ssh.Client плюс опциональный клиент бастиона (ProxyJump):
// Close закрывает и туннель через него. Промежуточный тип нужен, потому что
// у *ssh.Client не подменить Close.
type sshClient struct {
	*ssh.Client
	bastion *sshClient
}

// Close закрывает основное подключение и (если был) бастион.
func (c *sshClient) Close() error {
	err := c.Client.Close()
	if c.bastion != nil {
		if berr := c.bastion.Close(); err == nil {
			err = berr
		}
	}
	return err
}

// dialSSH устанавливает аутентифицированное SSH-подключение по cfg (с ProxyJump
// или напрямую). Используется и для интерактивных сессий (ConnectSSH), и для
// форвардингов/SFTP — вся машинерия auth/known_hosts/TOFU живёт здесь.
func dialSSH(cfg SSHConfig) (*sshClient, error) {
	if cfg.Port <= 0 {
		cfg.Port = 22
	}
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))

	sshCfg, agentConn, tried, err := newSSHClientConfig(cfg, cfg.User, cfg.Password)
	if err != nil {
		return nil, err
	}
	if agentConn != nil {
		defer agentConn.Close()
	}

	var raw net.Conn
	var bastion *sshClient
	if cfg.ProxyJump != "" {
		// Сначала поднимаем соединение с бастионом (та же логика auth/TOFU),
		// затем диалим целевой хост ЧЕРЕЗ него. У бастиона своего ProxyJump
		// нет — поддерживаем один прыжок.
		bcfg, err := bastionConfig(cfg)
		if err != nil {
			return nil, err
		}
		bc, err := dialSSH(bcfg)
		if err != nil {
			return nil, fmt.Errorf("proxy jump %s: %w", bcfg.Host, err)
		}
		bastion = bc
		raw, err = bastion.Dial("tcp", addr)
		if err != nil {
			bastion.Close()
			return nil, fmt.Errorf("%w: proxy dial: %v", ErrSSHUnreachable, err)
		}
	} else {
		raw, err = net.DialTimeout("tcp", addr, 10*time.Second)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrSSHUnreachable, err)
		}
		// TCP keep-alive — второй, независимый рубеж против «сервер закрылся,
		// пока я смотрел в другой экран»: прикладной пульс (keepaliveLoop)
		// держит живым сам SSH, а этот не даёт NAT провайдера и домашнему
		// роутеру выбросить простаивающую TCP-запись из таблицы трансляций.
		if tcp, ok := raw.(*net.TCPConn); ok {
			_ = tcp.SetKeepAlive(true)
			_ = tcp.SetKeepAlivePeriod(sshKeepaliveEvery)
		}
	}

	// Дедлайн на весь SSH-хендшейк (обмен ключами + аутентификация); после
	// успешного NewClientConn снимаем — дальше сессия живёт без таймаутов.
	_ = raw.SetDeadline(time.Now().Add(10 * time.Second))
	conn, chans, reqs, err := ssh.NewClientConn(raw, addr, sshCfg)
	if err != nil {
		raw.Close()
		if bastion != nil {
			bastion.Close()
		}
		var hkErr *ErrHostKeyUnknown
		var keyErr *knownhosts.KeyError
		switch {
		case errors.As(err, &hkErr):
			return nil, hkErr
		case errors.As(err, &keyErr):
			return nil, err
		case strings.Contains(err.Error(), "unable to authenticate"):
			return nil, &ErrSSHAuthDetails{Tried: tried}
		default:
			return nil, fmt.Errorf("%w: %v", ErrSSHUnreachable, err)
		}
	}
	_ = raw.SetDeadline(time.Time{})
	return &sshClient{Client: ssh.NewClient(conn, chans, reqs), bastion: bastion}, nil
}

// newSSHClientConfig собирает ssh.ClientConfig: ключи агента + пароль,
// known_hosts из paths.Base() с TOFU по TrustHost.
func newSSHClientConfig(cfg SSHConfig, user, password string) (*ssh.ClientConfig, net.Conn, []string, error) {
	// known_hosts агента — рядом с конфигом (paths.Base()). Файл создаём
	// заранее: knownhosts.New падает на отсутствующем.
	khPath := filepath.Join(paths.Base(), "known_hosts")
	if err := os.MkdirAll(filepath.Dir(khPath), 0o755); err != nil {
		return nil, nil, nil, err
	}
	if f, err := os.OpenFile(khPath, os.O_CREATE, 0o600); err != nil {
		return nil, nil, nil, fmt.Errorf("known_hosts: %w", err)
	} else {
		f.Close()
	}
	knownHostFiles := []string{khPath}
	if home, homeErr := os.UserHomeDir(); homeErr == nil && home != "" {
		userKH := filepath.Join(home, ".ssh", "known_hosts")
		if _, statErr := os.Stat(userKH); statErr == nil && userKH != khPath {
			knownHostFiles = append(knownHostFiles, userKH)
		}
	}
	kh, err := knownhosts.New(knownHostFiles...)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("known_hosts: %w", err)
	}

	hostKeyCallback := func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		err := kh(hostname, remote, key)
		if err == nil {
			return nil
		}
		var keyErr *knownhosts.KeyError
		if !errors.As(err, &keyErr) {
			return err
		}
		if len(keyErr.Want) > 0 {
			// Ключ ИЗМЕНИЛСЯ — возможен MITM. Никогда не обходим, TrustHost не помогает.
			want := make([]string, 0, len(keyErr.Want))
			for _, known := range keyErr.Want {
				want = append(want, ssh.FingerprintSHA256(known.Key))
			}
			return &ErrHostKeyMismatch{
				Host:         hostname,
				Got:          ssh.FingerprintSHA256(key),
				Fingerprints: want,
			}
		}
		// Хост неизвестен: либо отказ с фингерпринтом (клиент спросит доверие),
		// либо TOFU — принять и дописать в known_hosts.
		if !cfg.TrustHost {
			return &ErrHostKeyUnknown{Fingerprint: ssh.FingerprintSHA256(key)}
		}
		return appendKnownHost(khPath, hostname, key)
	}

	var auth []ssh.AuthMethod
	tried := make([]string, 0, 8)
	signers, signerTried, err := loadKeySigners(cfg)
	if err != nil {
		return nil, nil, nil, err
	}
	tried = append(tried, signerTried...)
	if len(signers) > 0 {
		auth = append(auth, ssh.PublicKeys(signers...))
	}
	var agentConn net.Conn
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		if conn, dialErr := net.DialTimeout("unix", sock, 2*time.Second); dialErr == nil {
			agentClient := sshagent.NewClient(conn)
			if agentSigners, signerErr := agentClient.Signers(); signerErr == nil && len(agentSigners) > 0 {
				auth = append(auth, ssh.PublicKeys(agentSigners...))
				tried = append(tried, "ssh-agent")
				agentConn = conn
			} else {
				conn.Close()
			}
		}
	}
	if password != "" {
		auth = append(auth, ssh.Password(password))
		tried = append(tried, "password")
	}

	return &ssh.ClientConfig{
		User:            user,
		Auth:            auth,
		HostKeyCallback: hostKeyCallback,
		Timeout:         10 * time.Second,
	}, agentConn, tried, nil
}

// bastionConfig разбирает cfg.ProxyJump ("user@host:port", user и порт
// опциональны) в SSHConfig бастиона. User по умолчанию — как у целевого хоста.
func bastionConfig(cfg SSHConfig) (SSHConfig, error) {
	jump := strings.TrimSpace(cfg.ProxyJump)
	user := cfg.User
	hostport := jump
	if i := strings.LastIndex(jump, "@"); i >= 0 {
		if u := jump[:i]; u != "" {
			user = u
		}
		hostport = jump[i+1:]
	}
	host, portStr, err := net.SplitHostPort(hostport)
	if err != nil {
		// Порта нет (или голый хост) — SplitHostPort требует "host:port".
		host, portStr = hostport, ""
	}
	host = strings.Trim(host, "[]")
	if host == "" {
		return SSHConfig{}, fmt.Errorf("%w: bad proxy_jump %q", ErrSSHUnreachable, cfg.ProxyJump)
	}
	port := 22
	if portStr != "" {
		n, err := strconv.Atoi(portStr)
		if err != nil || n <= 0 || n > 65535 {
			return SSHConfig{}, fmt.Errorf("%w: bad proxy_jump port %q", ErrSSHUnreachable, portStr)
		}
		port = n
	}
	return SSHConfig{
		Host:          host,
		Port:          port,
		User:          user,
		Password:      cfg.ProxyPassword,
		TrustHost:     cfg.TrustHost,
		IdentityFile:  cfg.IdentityFile,
		KeyPassphrase: cfg.KeyPassphrase,
		PrivateKeyPEM: cfg.PrivateKeyPEM,
	}, nil
}

// loadKeySigners loads an explicit IdentityFile first, then conventional keys.
// An encrypted explicit key is actionable (key_encrypted); encrypted default
// keys are skipped because another key/agent/password may still work.
//
// Перед файлами идёт ключ из хранилища приложения: его человек назначил
// серверу осознанно, и молча уступить очередь случайному ~/.ssh/id_rsa значило
// бы «выбор в интерфейсе ни на что не влияет».
func loadKeySigners(cfg SSHConfig) ([]ssh.Signer, []string, error) {
	var storedSigners []ssh.Signer
	var storedTried []string
	if strings.TrimSpace(cfg.PrivateKeyPEM) != "" {
		signer, err := parseSignerPEM([]byte(cfg.PrivateKeyPEM), cfg.KeyPassphrase)
		if err != nil {
			return nil, nil, err
		}
		storedSigners = append(storedSigners, signer)
		storedTried = append(storedTried, "key:"+signer.PublicKey().Type())
	}

	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return storedSigners, storedTried, nil
	}
	pathsToTry := make([]string, 0, 4)
	if cfg.IdentityFile != "" {
		pathsToTry = append(pathsToTry, expandHomePath(cfg.IdentityFile))
	}
	for _, name := range []string{"id_ed25519", "id_rsa", "id_ecdsa"} {
		candidate := filepath.Join(home, ".ssh", name)
		duplicate := false
		for _, existing := range pathsToTry {
			if filepath.Clean(existing) == filepath.Clean(candidate) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			pathsToTry = append(pathsToTry, candidate)
		}
	}
	out := storedSigners
	tried := storedTried
	for index, keyPath := range pathsToTry {
		data, err := os.ReadFile(keyPath)
		if err != nil {
			continue
		}
		var signer ssh.Signer
		if cfg.KeyPassphrase != "" && index == 0 && cfg.IdentityFile != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase(data, []byte(cfg.KeyPassphrase))
		} else {
			signer, err = ssh.ParsePrivateKey(data)
		}
		if err != nil {
			var passErr *ssh.PassphraseMissingError
			if index == 0 && cfg.IdentityFile != "" && errors.As(err, &passErr) {
				return nil, nil, &ErrSSHKeyEncrypted{Path: cfg.IdentityFile}
			}
			continue
		}
		out = append(out, signer)
		tried = append(tried, "key:"+filepath.Base(keyPath))
	}
	return out, tried, nil
}

// parseSignerPEM разбирает ключ, пришедший содержимым (хранилище приложения).
// Зашифрованный ключ без пароля — не «ключ не подошёл», а конкретная просьба:
// клиент по коду key_encrypted спрашивает пароль ключа.
func parseSignerPEM(pemData []byte, passphrase string) (ssh.Signer, error) {
	if passphrase != "" {
		signer, err := ssh.ParsePrivateKeyWithPassphrase(pemData, []byte(passphrase))
		if err == nil {
			return signer, nil
		}
		// Пароль был лишним (ключ не зашифрован) — это не повод отказывать.
		if plain, plainErr := ssh.ParsePrivateKey(pemData); plainErr == nil {
			return plain, nil
		}
		return nil, &ErrSSHKeyEncrypted{}
	}
	signer, err := ssh.ParsePrivateKey(pemData)
	if err != nil {
		var missing *ssh.PassphraseMissingError
		if errors.As(err, &missing) {
			return nil, &ErrSSHKeyEncrypted{}
		}
		return nil, err
	}
	return signer, nil
}

// appendKnownHost дописывает ключ хоста в known_hosts агента (TOFU после
// подтверждения пользователем). Формат строки — как у OpenSSH.
func appendKnownHost(path, hostname string, key ssh.PublicKey) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	line := knownhosts.Line([]string{knownhosts.Normalize(hostname)}, key)
	_, err = f.WriteString(line + "\n")
	return err
}

// RemoveKnownHost deletes only entries from Remotai's private known_hosts.
// The user's ~/.ssh/known_hosts is read as an additional trust source but is
// never rewritten by the app.
func RemoveKnownHost(host string, port int) (int, error) {
	if port <= 0 {
		port = 22
	}
	path := filepath.Join(paths.Base(), "known_hosts")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	targets := map[string]bool{
		host:                       true,
		knownhosts.Normalize(host): true,
		knownhosts.Normalize(net.JoinHostPort(host, strconv.Itoa(port))): true,
	}
	lines := strings.Split(string(data), "\n")
	kept := make([]string, 0, len(lines))
	removed := 0
	for _, line := range lines {
		fields := strings.Fields(line)
		match := false
		if len(fields) >= 3 {
			for _, encodedHost := range strings.Split(fields[0], ",") {
				if targets[encodedHost] {
					match = true
					break
				}
			}
		}
		if match {
			removed++
			continue
		}
		kept = append(kept, line)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(kept, "\n")), 0o600); err != nil {
		return 0, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return 0, err
	}
	return removed, nil
}

// ── ptyConn ────────────────────────────────────────────────────────

func (c *sshConn) Read(p []byte) (int, error)  { return c.stdout.Read(p) }
func (c *sshConn) Write(p []byte) (int, error) { return c.stdin.Write(p) }

// Resize — SSH WindowChange (порядок аргументов: rows, cols).
func (c *sshConn) Resize(cols, rows int) error {
	return c.session.WindowChange(rows, cols)
}

// Close закрывает SSH-сессию и TCP-подключение (идемпотентно).
func (c *sshConn) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	if c.stopKeepalive != nil {
		close(c.stopKeepalive)
	}
	c.mu.Unlock()
	_ = c.session.Close()
	err := c.client.Close() // Unblocks any in-flight SendRequest before waiting.
	if c.keepaliveDone != nil {
		<-c.keepaliveDone
	}
	return err
}

// shellPID — у удалённого shell нет локального PID; 0 отключает
// ForegroundProcess (бейджи агентов для SSH не вычисляются).
func (c *sshConn) shellPID() uint32 { return 0 }

// currentCWD — ярлык для UI: "ssh:user@host". Реальный cwd удалённого shell
// не запрашиваем (дорого и хрупко).
func (c *sshConn) currentCWD() (string, error) { return c.label, nil }
