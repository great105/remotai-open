package pty

// Проверка на живом SSH: поднимаем настоящий сервер (x/crypto/ssh) внутри
// теста и идём к нему тем же путём, что и продукт.
//
// Два обещания раздела проверяются здесь целиком, а не по частям:
//
//	1. сервер пускает по ключу ИЗ ХРАНИЛИЩА (пароля не даём вовсе);
//	2. «Установить ключ на сервер» доносит до сервера правильную команду и
//	   правильно читает ответ — «добавлен» против «уже стоял».

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"tgcontrol/internal/paths"
)

// guardKnownHosts возвращает known_hosts агента в исходное состояние: TOFU в
// тесте не должен оставлять следов в профиле, под которым идёт прогон.
func guardKnownHosts(t *testing.T) {
	t.Helper()
	path := filepath.Join(paths.Base(), "known_hosts")
	before, err := os.ReadFile(path)
	hadFile := err == nil
	t.Cleanup(func() {
		if hadFile {
			_ = os.WriteFile(path, before, 0o600)
			return
		}
		_ = os.Remove(path)
	})
}

// testSSHServer — минимальный sshd: пускает по одному разрешённому ключу и
// выполняет ровно одну команду, запоминая её текст.
type testSSHServer struct {
	addr     string
	port     int
	mu       sync.Mutex
	commands []string
	// reply — что сервер печатает в ответ на команду (маркер исхода).
	reply string
}

func startTestSSHServer(t *testing.T, authorized ssh.PublicKey, reply string) *testSSHServer {
	t.Helper()
	hostKey, err := generateTestSigner()
	if err != nil {
		t.Fatalf("host key: %v", err)
	}
	srv := &testSSHServer{reply: reply}
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if string(key.Marshal()) == string(authorized.Marshal()) {
				return &ssh.Permissions{}, nil
			}
			return nil, ssh.ErrNoAuth
		},
	}
	cfg.AddHostKey(hostKey)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	srv.addr = host
	srv.port, _ = strconv.Atoi(portStr)

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

func (s *testSSHServer) serve(raw net.Conn, cfg *ssh.ServerConfig) {
	defer raw.Close()
	conn, chans, reqs, err := ssh.NewServerConn(raw, cfg)
	if err != nil {
		return
	}
	defer conn.Close()
	go ssh.DiscardRequests(reqs)
	for newChan := range chans {
		if newChan.ChannelType() != "session" {
			_ = newChan.Reject(ssh.UnknownChannelType, "only sessions")
			continue
		}
		ch, requests, err := newChan.Accept()
		if err != nil {
			return
		}
		for req := range requests {
			if req.Type != "exec" {
				_ = req.Reply(false, nil)
				continue
			}
			// Полезная нагрузка exec: 4 байта длины + сама команда.
			if len(req.Payload) >= 4 {
				n := binary.BigEndian.Uint32(req.Payload[:4])
				if int(n)+4 <= len(req.Payload) {
					s.mu.Lock()
					s.commands = append(s.commands, string(req.Payload[4:4+n]))
					s.mu.Unlock()
				}
			}
			_ = req.Reply(true, nil)
			_, _ = ch.Write([]byte(s.reply + "\n"))
			_, _ = ch.SendRequest("exit-status", false, []byte{0, 0, 0, 0})
			_ = ch.Close()
		}
	}
}

func (s *testSSHServer) lastCommand() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.commands) == 0 {
		return ""
	}
	return s.commands[len(s.commands)-1]
}

func generateTestSigner() (ssh.Signer, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return ssh.NewSignerFromKey(priv)
}

// Вход по ключу из хранилища + установка ключа на сервер одним прогоном.
func TestInstallAuthorizedKeyOverRealSSH(t *testing.T) {
	isolateHome(t)
	guardKnownHosts(t)

	store := NewSSHKeyStore(filepath.Join(t.TempDir(), "ssh_keys.json"))
	key, err := store.Generate("Прод", "ed25519", "")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	pemData, _, err := store.Material(key.ID)
	if err != nil {
		t.Fatalf("Material: %v", err)
	}
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(key.PublicKey))
	if err != nil {
		t.Fatalf("public key: %v", err)
	}

	srv := startTestSSHServer(t, pub, authorizedKeyAdded)
	cfg := SSHConfig{
		Host: srv.addr,
		Port: srv.port,
		User: "root",
		// Пароля нет намеренно: пустить может только ключ из хранилища.
		PrivateKeyPEM: pemData,
		TrustHost:     true,
	}

	done := make(chan struct{})
	var already bool
	var installErr error
	go func() {
		already, installErr = InstallAuthorizedKey(cfg, key.PublicKey)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("InstallAuthorizedKey hung")
	}
	if installErr != nil {
		t.Fatalf("InstallAuthorizedKey: %v", installErr)
	}
	if already {
		t.Fatal("server said ADDED, but the result claims the key was already there")
	}
	cmd := srv.lastCommand()
	if !strings.Contains(cmd, key.PublicKey) {
		t.Fatalf("public key never reached the server: %q", cmd)
	}
	if !strings.Contains(cmd, "authorized_keys") {
		t.Fatalf("unexpected command: %q", cmd)
	}
}

// Повторная установка — обычный успешный исход «уже стоял», а не ошибка.
func TestInstallAuthorizedKeyIdempotent(t *testing.T) {
	isolateHome(t)
	guardKnownHosts(t)

	store := NewSSHKeyStore(filepath.Join(t.TempDir(), "ssh_keys.json"))
	key, _ := store.Generate("Прод", "ed25519", "")
	pemData, _, _ := store.Material(key.ID)
	pub, _, _, _, _ := ssh.ParseAuthorizedKey([]byte(key.PublicKey))

	srv := startTestSSHServer(t, pub, authorizedKeyExists)
	already, err := InstallAuthorizedKey(SSHConfig{
		Host: srv.addr, Port: srv.port, User: "root",
		PrivateKeyPEM: pemData, TrustHost: true,
	}, key.PublicKey)
	if err != nil {
		t.Fatalf("InstallAuthorizedKey: %v", err)
	}
	if !already {
		t.Fatal("want already=true when the server reports the key exists")
	}
}

// Сервер без POSIX-шелла (типичный Windows-сервер) обязан давать отдельную
// ошибку: человеку нужен не «сбой», а «добавьте ключ вручную».
func TestInstallAuthorizedKeyUnexpectedOutput(t *testing.T) {
	isolateHome(t)
	guardKnownHosts(t)

	store := NewSSHKeyStore(filepath.Join(t.TempDir(), "ssh_keys.json"))
	key, _ := store.Generate("Прод", "ed25519", "")
	pemData, _, _ := store.Material(key.ID)
	pub, _, _, _, _ := ssh.ParseAuthorizedKey([]byte(key.PublicKey))

	srv := startTestSSHServer(t, pub, "'grep' is not recognized as an internal or external command")
	_, err := InstallAuthorizedKey(SSHConfig{
		Host: srv.addr, Port: srv.port, User: "root",
		PrivateKeyPEM: pemData, TrustHost: true,
	}, key.PublicKey)
	if err == nil || !strings.Contains(err.Error(), ErrAuthorizedKeyShell.Error()) {
		t.Fatalf("want ErrAuthorizedKeyShell, got %v", err)
	}
}
