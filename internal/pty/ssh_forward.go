package pty

// SSH port-forwarding (-L / -R / -D) через агента.
//
// Каждый форвард держит СВОЁ SSH-подключение (та же машинерия dialSSH:
// ключи/пароль, known_hosts, TOFU, ProxyJump), чтобы его жизненный цикл не
// зависел от интерактивных терминалов. Форварды живут только в памяти агента
// и НЕ персистентны: рестарт агента их убивает (пароль не храним, поднять
// заново без него всё равно не можем).

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Типы форвардов — как у ssh: -L (local), -R (remote), -D (dynamic/SOCKS5).
const (
	ForwardLocal   = "local"
	ForwardRemote  = "remote"
	ForwardDynamic = "dynamic"
)

// ErrForwardNotFound — форварда с таким id нет (уже удалён или рестарт агента).
var ErrForwardNotFound = errors.New("ssh forward not found")

// ForwardSpec — параметры создания форварда.
type ForwardSpec struct {
	Type string // local | remote | dynamic

	SSH SSHConfig // куда подключаемся (Host/Port/User/Password/TrustHost/ProxyJump)

	BindAddr   string // где слушать (local/dynamic — на агенте, remote — на сервере); "" = 127.0.0.1
	BindPort   int
	TargetHost string // куда пересылать (не нужен для dynamic)
	TargetPort int
}

// ForwardInfo — публичное состояние форварда (то, что отдаём в API).
type ForwardInfo struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	Server     string `json:"server"` // "user@host:port"
	BindAddr   string `json:"bind_addr"`
	BindPort   int    `json:"bind_port"`
	TargetHost string `json:"target_host,omitempty"`
	TargetPort int    `json:"target_port,omitempty"`
	Status     string `json:"status"` // active | error
	Error      string `json:"error,omitempty"`
	Access     string `json:"access"`
}

// sshForward — живой форвард.
type sshForward struct {
	info     ForwardInfo
	mgr      *ForwardManager // обратная ссылка: accept-цикл сам помечает ошибку
	client   *sshClient
	listener net.Listener // local/dynamic — агентский, remote — серверный
	cancel   chan struct{}
	once     sync.Once
}

// ForwardManager — реестр активных форвардов агента.
type ForwardManager struct {
	mu   sync.Mutex
	fwds map[string]*sshForward
}

func NewForwardManager() *ForwardManager {
	return &ForwardManager{fwds: make(map[string]*sshForward)}
}

// Start поднимает форвард: сначала SSH-подключение (ошибки auth/TOFU уходят
// наверх как есть), затем слушатель и accept-цикл.
func (m *ForwardManager) Start(spec ForwardSpec) (ForwardInfo, error) {
	if spec.BindAddr == "" {
		spec.BindAddr = "127.0.0.1"
	}
	if spec.BindPort <= 0 || spec.BindPort > 65535 {
		return ForwardInfo{}, fmt.Errorf("bad bind_port")
	}
	switch spec.Type {
	case ForwardLocal, ForwardRemote:
		if spec.TargetHost == "" || spec.TargetPort <= 0 || spec.TargetPort > 65535 {
			return ForwardInfo{}, fmt.Errorf("target_host/target_port required")
		}
	case ForwardDynamic:
	default:
		return ForwardInfo{}, fmt.Errorf("unknown forward type %q", spec.Type)
	}

	client, err := dialSSH(spec.SSH)
	if err != nil {
		return ForwardInfo{}, err
	}
	if spec.SSH.Port <= 0 {
		spec.SSH.Port = 22
	}

	f := &sshForward{
		info: ForwardInfo{
			ID:         "fw-" + randomID(),
			Type:       spec.Type,
			Server:     fmt.Sprintf("%s@%s:%d", spec.SSH.User, spec.SSH.Host, spec.SSH.Port),
			BindAddr:   spec.BindAddr,
			BindPort:   spec.BindPort,
			TargetHost: spec.TargetHost,
			TargetPort: spec.TargetPort,
			Status:     "active",
			Access:     forwardAccess(spec),
		},
		mgr:    m,
		client: client,
		cancel: make(chan struct{}),
	}

	bind := net.JoinHostPort(spec.BindAddr, strconv.Itoa(spec.BindPort))
	switch spec.Type {
	case ForwardRemote:
		// Слушаем на СЕРВЕРЕ: входящие там соединения диалим в target с агента.
		l, err := client.Listen("tcp", bind)
		if err != nil {
			client.Close()
			return ForwardInfo{}, fmt.Errorf("remote listen %s: %w", bind, err)
		}
		f.listener = l
		go f.acceptLoop(func() (net.Conn, error) {
			return net.DialTimeout("tcp", net.JoinHostPort(spec.TargetHost, strconv.Itoa(spec.TargetPort)), 10*time.Second)
		})
	default: // local / dynamic — слушаем на агенте
		l, err := net.Listen("tcp", bind)
		if err != nil {
			client.Close()
			return ForwardInfo{}, fmt.Errorf("listen %s: %w", bind, err)
		}
		f.listener = l
		if spec.Type == ForwardLocal {
			go f.acceptLoop(func() (net.Conn, error) {
				return client.Dial("tcp", net.JoinHostPort(spec.TargetHost, strconv.Itoa(spec.TargetPort)))
			})
		} else {
			go f.socks5Loop()
		}
	}

	m.mu.Lock()
	m.fwds[f.info.ID] = f
	m.mu.Unlock()
	// Следим за жизнью SSH-подключения: client.Wait вернётся при обрыве —
	// без этого локальный слушатель переживал бы мёртвое SSH и форвард
	// вечно висел бы «active», хотя байты уже не текут.
	go func() {
		err := client.Wait()
		select {
		case <-f.cancel:
			return // штатное закрытие
		default:
		}
		if err == nil {
			err = errors.New("ssh connection closed")
		}
		f.fail(err)
	}()
	return f.info, nil
}

func forwardAccess(spec ForwardSpec) string {
	host := spec.BindAddr
	if spec.Type == ForwardRemote {
		host = spec.SSH.Host
	} else if host == "0.0.0.0" || host == "::" {
		host = firstLANAddress()
	}
	if host == "" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, strconv.Itoa(spec.BindPort))
}

func firstLANAddress() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "0.0.0.0"
	}
	for _, addr := range addrs {
		var ip net.IP
		switch v := addr.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			continue
		}
		if v4 := ip.To4(); v4 != nil {
			return v4.String()
		}
	}
	return "0.0.0.0"
}

// List — снапшот активных форвардов (по id, для стабильного порядка в UI).
func (m *ForwardManager) List() []ForwardInfo {
	m.mu.Lock()
	out := make([]ForwardInfo, 0, len(m.fwds))
	for _, f := range m.fwds {
		out = append(out, f.info)
	}
	m.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Stop гасит форвард (слушатель + SSH-подключение). Идемпотентно.
func (m *ForwardManager) Stop(id string) error {
	m.mu.Lock()
	f, ok := m.fwds[id]
	if ok {
		delete(m.fwds, id)
	}
	m.mu.Unlock()
	if !ok {
		return ErrForwardNotFound
	}
	f.shutdown()
	return nil
}

// fail переводит форвард в status=error (accept-цикл умер сам — например,
// remote listener отвалился вместе с SSH-подключением) и гасит его. Запись
// остаётся в списке: UI должен увидеть, ЧТО случилось, а не исчезновение.
func (f *sshForward) fail(err error) {
	f.mgr.mu.Lock()
	if cur, ok := f.mgr.fwds[f.info.ID]; ok && cur == f {
		f.info.Status = "error"
		f.info.Error = err.Error()
	}
	f.mgr.mu.Unlock()
	f.shutdown()
}

func (f *sshForward) shutdown() {
	f.once.Do(func() {
		close(f.cancel)
		if f.listener != nil {
			f.listener.Close()
		}
		f.client.Close()
	})
}

// acceptLoop — общий цикл для local и remote: принять соединение, открыть
// второй конец через dialTarget, перекачивать в обе стороны.
func (f *sshForward) acceptLoop(dialTarget func() (net.Conn, error)) {
	for {
		in, err := f.listener.Accept()
		if err != nil {
			select {
			case <-f.cancel:
				return // штатное закрытие
			default:
			}
			f.fail(err) // слушатель умер сам (падение SSH и т.п.)
			return
		}
		go func() {
			out, err := dialTarget()
			if err != nil {
				in.Close()
				return
			}
			relayConns(in, out)
		}()
	}
}

// socks5Loop — accept-цикл dynamic-форварда: на каждом соединении минимальный
// SOCKS5-сервер, цель приходит в CONNECT-запросе клиента.
func (f *sshForward) socks5Loop() {
	for {
		in, err := f.listener.Accept()
		if err != nil {
			select {
			case <-f.cancel:
				return
			default:
			}
			f.fail(err)
			return
		}
		go handleSocks5Conn(in, f.client.Dial)
	}
}

// relayConns перекачивает байты в обе стороны до конца любого из концов.
func relayConns(a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	// Полузакрытие не поддерживаем: одна сторона закрылась — гасим обе,
	// иначе вторая копия может висеть на мёртвом конце вечно.
	a.Close()
	b.Close()
}

// ── минимальный SOCKS5-сервер (без зависимостей) ────────────────────

// handleSocks5Conn обслуживает одно SOCKS5-соединение: greeting без авторизации,
// единственная команда CONNECT, дальше прозрачный relay через dial
// (для dynamic-форварда это client.Dial на SSH-сервер).
func handleSocks5Conn(conn net.Conn, dial func(network, addr string) (net.Conn, error)) {
	// Greeting: VER=5, NMETHODS, METHODS...
	head := make([]byte, 2)
	if _, err := io.ReadFull(conn, head); err != nil || head[0] != 0x05 {
		conn.Close()
		return
	}
	methods := make([]byte, int(head[1]))
	if _, err := io.ReadFull(conn, methods); err != nil {
		conn.Close()
		return
	}
	// Всегда выбираем "no auth" (0x00) — порт слушаем локально, авторизация
	// SOCKS5 здесь смысла не имеет (защита — bind на 127.0.0.1).
	if _, err := conn.Write([]byte{0x05, 0x00}); err != nil {
		conn.Close()
		return
	}

	// Request: VER=5, CMD, RSV, ATYP, ADDR, PORT.
	req := make([]byte, 4)
	if _, err := io.ReadFull(conn, req); err != nil || req[0] != 0x05 {
		conn.Close()
		return
	}
	if req[1] != 0x01 { // только CONNECT; BIND/UDP ASSOCIATE не поддерживаем
		writeSocks5Reply(conn, 0x07)
		conn.Close()
		return
	}
	host, err := readSocks5Addr(conn, req[3])
	if err != nil {
		writeSocks5Reply(conn, 0x08) // address type not supported
		conn.Close()
		return
	}
	var portBuf [2]byte
	if _, err := io.ReadFull(conn, portBuf[:]); err != nil {
		conn.Close()
		return
	}
	target := net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(portBuf[:]))))

	out, err := dial("tcp", target)
	if err != nil {
		writeSocks5Reply(conn, 0x05) // connection refused / общий отказ
		conn.Close()
		return
	}
	if err := writeSocks5Reply(conn, 0x00); err != nil {
		out.Close()
		conn.Close()
		return
	}
	relayConns(conn, out)
}

// readSocks5Addr читает ADDR по ATYP: IPv4 / домен / IPv6.
func readSocks5Addr(conn net.Conn, atyp byte) (string, error) {
	switch atyp {
	case 0x01: // IPv4
		b := make([]byte, 4)
		if _, err := io.ReadFull(conn, b); err != nil {
			return "", err
		}
		return net.IP(b).String(), nil
	case 0x03: // домен: LEN, байты
		var l [1]byte
		if _, err := io.ReadFull(conn, l[:]); err != nil {
			return "", err
		}
		b := make([]byte, int(l[0]))
		if _, err := io.ReadFull(conn, b); err != nil {
			return "", err
		}
		return string(b), nil
	case 0x04: // IPv6
		b := make([]byte, 16)
		if _, err := io.ReadFull(conn, b); err != nil {
			return "", err
		}
		return net.IP(b).String(), nil
	default:
		return "", fmt.Errorf("unsupported atyp %d", atyp)
	}
}

// writeSocks5Reply — ответ на CONNECT: VER=5, REP, RSV, BND.ADDR=0.0.0.0:0
// (точный bind на удалённой стороне мы не знаем — клиентам хватает REP).
func writeSocks5Reply(conn net.Conn, rep byte) error {
	_, err := conn.Write([]byte{0x05, rep, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
	return err
}
