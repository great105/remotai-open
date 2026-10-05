package pty

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"
)

// socks5Handshake — клиентская сторона рукопожатия для тестов.
func socks5Handshake(t *testing.T, c net.Conn, domain string, port uint16) []byte {
	t.Helper()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	// greeting: VER=5, 1 метод (no auth)
	if _, err := c.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, 2)
	if _, err := io.ReadFull(c, reply); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(reply, []byte{0x05, 0x00}) {
		t.Fatalf("greeting reply = %v", reply)
	}
	// CONNECT домен:port
	req := []byte{0x05, 0x01, 0x00, 0x03, byte(len(domain))}
	req = append(req, []byte(domain)...)
	req = append(req, byte(port>>8), byte(port))
	if _, err := c.Write(req); err != nil {
		t.Fatal(err)
	}
	rep := make([]byte, 10) // VER REP RSV ATYP=1 + 4 + 2
	if _, err := io.ReadFull(c, rep); err != nil {
		t.Fatal(err)
	}
	return rep
}

func TestSocks5Connect(t *testing.T) {
	client, server := net.Pipe()
	remote, remoteEnd := net.Pipe()

	var gotAddr string
	dial := func(network, addr string) (net.Conn, error) {
		gotAddr = addr
		return remote, nil
	}
	go handleSocks5Conn(server, dial)

	rep := socks5Handshake(t, client, "example.com", 443)
	if rep[1] != 0x00 {
		t.Fatalf("CONNECT reply = %v, want success", rep)
	}
	if gotAddr != "example.com:443" {
		t.Fatalf("dial addr = %q", gotAddr)
	}

	// Relay: данные с «удалённой» стороны доезжают до клиента и обратно.
	go func() {
		remoteEnd.Write([]byte("pong"))
	}()
	buf := make([]byte, 4)
	client.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(client, buf); err != nil || string(buf) != "pong" {
		t.Fatalf("relay remote→client: %v %q", err, buf)
	}
	if _, err := client.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	remoteEnd.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(remoteEnd, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("relay client→remote: %v %q", err, buf)
	}

	// Закрытие одного конца гасит всё соединение.
	remoteEnd.Close()
	client.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := client.Read(make([]byte, 1)); err == nil {
		t.Fatal("ожидалось закрытие клиентского конца после обрыва remote")
	}
}

func TestSocks5DialFailure(t *testing.T) {
	client, server := net.Pipe()
	dial := func(network, addr string) (net.Conn, error) {
		return nil, io.ErrClosedPipe
	}
	go handleSocks5Conn(server, dial)

	rep := socks5Handshake(t, client, "example.com", 22)
	if rep[1] == 0x00 {
		t.Fatalf("ожидался отказ, reply = %v", rep)
	}
	// После отказа соединение закрыто.
	client.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := client.Read(make([]byte, 1)); err == nil {
		t.Fatal("ожидалось закрытие соединения после отказа CONNECT")
	}
}

func TestSocks5UnsupportedCommand(t *testing.T) {
	client, server := net.Pipe()
	go handleSocks5Conn(server, func(network, addr string) (net.Conn, error) {
		t.Error("dial не должен вызываться для BIND")
		return nil, nil
	})

	client.SetDeadline(time.Now().Add(5 * time.Second))
	client.Write([]byte{0x05, 0x01, 0x00})
	reply := make([]byte, 2)
	io.ReadFull(client, reply)
	// BIND (0x02) — не поддерживаем. Шлём только заголовок: сервер отвечает
	// отказом сразу после команды, а net.Pipe синхронный — лишние байты
	// заблокировали бы и клиента, и сервер.
	client.Write([]byte{0x05, 0x02, 0x00, 0x01})
	rep := make([]byte, 10)
	if _, err := io.ReadFull(client, rep); err != nil {
		t.Fatal(err)
	}
	if rep[1] != 0x07 {
		t.Fatalf("BIND reply = %v, want 0x07 (command not supported)", rep)
	}
}
