package pty

import (
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// Пульс SSH-соединения: сессия обязана оставаться живой, пока человек смотрит
// в другой экран.
//
// Живая жалоба владельца (2026-07-27): «захожу в сервер, запускаю команду,
// выхожу обратно — сервер закрывается». Пока на терминал никто не смотрит, по
// соединению не идёт ни байта, и его рвёт первый же таймаут по пути: sshd с
// ClientAliveInterval, NAT провайдера, роутер. Обычный ssh-клиент лечит это
// ServerAliveInterval — здесь то же самое, только внутри агента.
//
// Сервер в тесте настоящий (golang.org/x/crypto/ssh) и считает входящие global
// requests keepalive@openssh.com — проверяется факт отправки, а не намерение.
func TestSSHKeepalivePingsServer(t *testing.T) {
	guardKnownHosts(t)

	// Такт ускорен, иначе тест ждал бы полминуты.
	restore := sshKeepaliveEvery
	sshKeepaliveEvery = 40 * time.Millisecond
	t.Cleanup(func() { sshKeepaliveEvery = restore })

	srv := startKeepaliveServer(t)

	conn, err := ConnectSSH(SSHConfig{
		Host: "127.0.0.1", Port: srv.port, User: "tester", Password: "pw",
		TrustHost: true, Cols: 80, Rows: 24,
	})
	if err != nil {
		t.Fatalf("подключение к тестовому серверу: %v", err)
	}

	// Три такта: одного мало, чтобы отличить пульс от случайного запроса.
	deadline := time.Now().Add(3 * time.Second)
	for srv.keepalives() < 3 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := srv.keepalives(); got < 3 {
		t.Fatalf("сервер получил %d пульсов — простаивающее соединение никто не держит", got)
	}

	// Закрытие останавливает пульс: иначе горутина живёт после сессии.
	if err := conn.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	select {
	case <-conn.(*sshConn).keepaliveDone:
	default:
		t.Fatal("Close вернулся до остановки keepalive")
	}
	// The server may still drain a request sent before Close. Wait for the
	// request stream to close instead of treating an in-flight packet as a leak.
	select {
	case <-srv.requestsDone:
	case <-time.After(3 * time.Second):
		t.Fatal("сервер продолжает ждать запросы после закрытия соединения")
	}
}

// keepaliveServer — минимальный sshd, который принимает пароль, поднимает
// shell и считает keepalive-запросы.
type keepaliveServer struct {
	port         int
	mu           sync.Mutex
	pings        int
	requestsDone chan struct{}
}

func (s *keepaliveServer) keepalives() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pings
}

func startKeepaliveServer(t *testing.T) *keepaliveServer {
	t.Helper()
	hostKey, err := generateTestSigner()
	if err != nil {
		t.Fatalf("host key: %v", err)
	}
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(_ ssh.ConnMetadata, _ []byte) (*ssh.Permissions, error) {
			return &ssh.Permissions{}, nil
		},
	}
	cfg.AddHostKey(hostKey)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	_, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)
	srv := &keepaliveServer{port: port, requestsDone: make(chan struct{})}

	go func() {
		for {
			raw, err := ln.Accept()
			if err != nil {
				return
			}
			go srv.serve(raw, cfg)
		}
	}()
	return srv
}

func (s *keepaliveServer) serve(raw net.Conn, cfg *ssh.ServerConfig) {
	defer raw.Close()
	conn, chans, reqs, err := ssh.NewServerConn(raw, cfg)
	if err != nil {
		return
	}
	defer conn.Close()

	go func() {
		defer close(s.requestsDone)
		for req := range reqs {
			if req.Type == "keepalive@openssh.com" {
				s.mu.Lock()
				s.pings++
				s.mu.Unlock()
			}
			// Отвечаем отказом, как настоящий sshd: клиенту важен сам ответ.
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
		}
	}()

	for newChan := range chans {
		if newChan.ChannelType() != "session" {
			_ = newChan.Reject(ssh.UnknownChannelType, "only sessions")
			continue
		}
		ch, requests, err := newChan.Accept()
		if err != nil {
			return
		}
		go func() {
			for req := range requests {
				switch req.Type {
				case "pty-req", "shell":
					if req.WantReply {
						_ = req.Reply(true, nil)
					}
				default:
					if req.WantReply {
						_ = req.Reply(false, nil)
					}
				}
			}
		}()
		// Приветствие, чтобы у сессии был вывод, как у настоящего shell.
		_, _ = ch.Write([]byte("test shell\r\n"))
	}
}
