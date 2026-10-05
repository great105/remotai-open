// Command sshd — минимальный SSH-сервер для e2e-тестов SSH-центра Remotai
// (МОЕ/e2e/tests/ssh-center.spec.ts). НЕ для продакшена:
//
//   - один пользователь/пароль (флаги -user/-pass);
//   - детерминированный host-ключ (фиксированный seed), чтобы повторные
//     прогоны не ловили host_key_mismatch в TOFU known_hosts агента;
//   - «шелл» — баннер + echo ввода.
//
// Адрес: -addr host:port (порт 0 = свободный; реальный печатается в stdout
// строкой "READY host:port" — её ждёт тестовый харнесс).
package main

import (
	"crypto/ed25519"
	"flag"
	"fmt"
	"io"
	"log"
	"net"

	"golang.org/x/crypto/ssh"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:0", "listen address (port 0 = pick free)")
	user := flag.String("user", "e2e", "единственный разрешённый пользователь")
	pass := flag.String("pass", "e2e-secret", "его пароль")
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	// Фиксированный seed = тот же host-ключ между прогонами тестов.
	seed := make([]byte, ed25519.SeedSize)
	copy(seed, []byte("remotai-e2e-sshd-fixed-seed-32b"))
	_, priv, err := ed25519.GenerateKey(bytesReader(seed))
	if err != nil {
		log.Fatalf("host key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		log.Fatalf("signer: %v", err)
	}

	cfg := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, p []byte) (*ssh.Permissions, error) {
			if c.User() == *user && string(p) == *pass {
				return nil, nil
			}
			return nil, fmt.Errorf("access denied")
		},
	}
	cfg.AddHostKey(signer)

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	// Харнесс парсит эту строку, менять формат нельзя (см. e2e/fixtures/harness.ts).
	fmt.Printf("READY %s\n", ln.Addr().String())
	log.Printf("[E2E-SSHD] on %s, user=%s", ln.Addr(), *user)

	for {
		raw, err := ln.Accept()
		if err != nil {
			return
		}
		go handleConn(raw, cfg)
	}
}

func handleConn(raw net.Conn, cfg *ssh.ServerConfig) {
	defer raw.Close()
	conn, chans, reqs, err := ssh.NewServerConn(raw, cfg)
	if err != nil {
		log.Printf("handshake: %v", err)
		return
	}
	defer conn.Close()
	go ssh.DiscardRequests(reqs)

	for chReq := range chans {
		if chReq.ChannelType() != "session" {
			chReq.Reject(ssh.UnknownChannelType, "only session")
			continue
		}
		ch, requests, err := chReq.Accept()
		if err != nil {
			continue
		}
		go handleSession(ch, requests)
	}
}

func handleSession(ch ssh.Channel, requests <-chan *ssh.Request) {
	defer ch.Close()
	shellStarted := false
	for req := range requests {
		switch req.Type {
		case "pty-req", "env", "window-change":
			req.Reply(true, nil)
		case "shell":
			if shellStarted {
				req.Reply(false, nil)
				continue
			}
			shellStarted = true
			req.Reply(true, nil)
			io.WriteString(ch, "remotai e2e sshd ready\r\n$ ")
			// echo ввода — как шелл с включённым эхом.
			go io.Copy(ch, ch) //nolint:errcheck
		case "exec":
			// Агент использует интерактивный shell; exec не нужен.
			req.Reply(false, nil)
		default:
			req.Reply(false, nil)
		}
	}
}

// bytesReader — io.Reader, отдающий seed один раз (для ed25519.GenerateKey).
type seedReader struct {
	b    []byte
	read bool
}

func bytesReader(b []byte) *seedReader { return &seedReader{b: b} }

func (r *seedReader) Read(p []byte) (int, error) {
	if r.read {
		return 0, io.EOF
	}
	r.read = true
	return copy(p, r.b), nil
}
