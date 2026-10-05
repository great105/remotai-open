package server

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pion/turn/v5"

	"tgcontrol-relay/internal/config"
)

// TURN обязан отвечать и по TCP: у владельца VPN в tun-режиме на обоих концах,
// UDP через него ходит плохо (02.09.2026: сбор ICE 5 с по таймауту, обрыв,
// вторая попытка failed). Тест поднимает наш сервер на свободном порту и
// делает настоящую TURN-аллокацию по TCP клиентом pion — как это сделает ICE у
// телефона или агента. Заодно проверяет, что клиенту отдаются ОБА адреса.
func TestTURNAllocatesOverTCP(t *testing.T) {
	// Свободный порт: занимаем UDP, снимаем, и просим TURN сесть на него же.
	probe, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.LocalAddr().(*net.UDPAddr).Port
	probe.Close()

	cfg := &config.Config{
		TURNEnabled:  true,
		TURNPublicIP: "127.0.0.1",
		TURNPort:     port,
		TURNRealm:    "test.local",
		TURNSecret:   "turn-test-secret",
		TURNCredTTL:  time.Minute,
	}
	srv, err := StartTURN(cfg)
	if err != nil {
		t.Fatalf("StartTURN: %v", err)
	}
	defer srv.Close()

	user, cred, err := turn.GenerateLongTermTURNRESTCredentials(cfg.TURNSecret, "utest", cfg.TURNCredTTL)
	if err != nil {
		t.Fatal(err)
	}

	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	tcpConn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("TURN по TCP не слушает: %v", err)
	}
	defer tcpConn.Close()

	client, err := turn.NewClient(&turn.ClientConfig{
		STUNServerAddr: addr,
		TURNServerAddr: addr,
		Conn:           turn.NewSTUNConn(tcpConn),
		Username:       user,
		Password:       cred,
		Realm:          cfg.TURNRealm,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer client.Close()
	if err := client.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	relay, err := client.Allocate()
	if err != nil {
		t.Fatalf("аллокация по TCP не удалась: %v", err)
	}
	defer relay.Close()
	if relay.LocalAddr() == nil {
		t.Fatal("аллокация без relay-адреса")
	}

	// Клиенту отдаются оба транспорта одними кредами.
	s := &Server{Config: cfg}
	resp, err := s.buildICEServers("utest")
	if err != nil {
		t.Fatal(err)
	}
	var urls []string
	for _, ice := range resp.ICEServers {
		urls = append(urls, ice.URLs...)
	}
	joined := strings.Join(urls, " ")
	for _, want := range []string{"?transport=udp", "?transport=tcp"} {
		if !strings.Contains(joined, fmt.Sprintf("turn:%s%s", addr, want)) {
			t.Fatalf("в списке ICE нет turn:%s%s: %s", addr, want, joined)
		}
	}
}
