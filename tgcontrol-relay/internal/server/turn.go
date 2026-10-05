package server

// TURN/STUN server for WebRTC Remote Desktop.
//
// Both peers (the PC agent and the phone/browser) sit behind NAT, so on
// symmetric NAT a direct P2P path won't form and media must be relayed. We run
// an embedded pion/turn server on the VPS and hand out short-lived credentials
// via GET /v1/turn/credentials using the standard "TURN REST API" scheme
// (ephemeral username = "<unix-expiry>:<id>", credential = base64(HMAC-SHA1(
// secret, username))). The TURN server validates by recomputing the HMAC, so no
// per-user state is stored — see pion's LongTermTURNRESTAuthHandler.
//
// Disabled by default (TURN_ENABLED=false): until the VPS firewall exposes the
// UDP port, cloud WebRTC falls back to the WS tunnel and LAN WebRTC works off
// host candidates without any TURN.

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/pion/turn/v5"

	"tgcontrol-relay/internal/config"
	"tgcontrol-relay/internal/db"
)

// iceServer mirrors the browser RTCIceServer shape so the JSON we return can be
// fed straight into new RTCPeerConnection({iceServers}).
type iceServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

type turnCredsResponse struct {
	ICEServers []iceServer `json:"iceServers"`
	TTL        int         `json:"ttl"`
}

// StartTURN brings up the embedded UDP STUN/TURN listener. Returns a nil server
// (and nil error) when TURN is disabled, so callers can defer Close() safely.
func StartTURN(cfg *config.Config) (*turn.Server, error) {
	if !cfg.TURNEnabled {
		log.Printf("[TURN] disabled (set TURN_ENABLED=true + TURN_PUBLIC_IP + open UDP %d to enable cloud WebRTC media)", cfg.TURNPort)
		return nil, nil
	}

	udpListener, err := net.ListenPacket("udp4", fmt.Sprintf("0.0.0.0:%d", cfg.TURNPort))
	if err != nil {
		return nil, fmt.Errorf("turn udp listen :%d: %w", cfg.TURNPort, err)
	}
	// TURN по TCP на том же порту. Зачем: у владельца VPN в tun-режиме и на
	// ПК, и на телефоне, UDP через такие тоннели ходит плохо — 02.09.2026
	// соединение собиралось 7 с (таймаут сбора ICE 5 с), через минуту
	// рвалось, вторая попытка падала в failed. AnyDesk «пробивается» везде
	// именно потому, что всегда умеет TCP. Клиент получает оба адреса
	// (`?transport=udp` и `?transport=tcp`) и ICE сам выбирает рабочий.
	tcpListener, err := net.Listen("tcp4", fmt.Sprintf("0.0.0.0:%d", cfg.TURNPort))
	if err != nil {
		udpListener.Close()
		return nil, fmt.Errorf("turn tcp listen :%d: %w", cfg.TURNPort, err)
	}
	relayGen := func() *turn.RelayAddressGeneratorStatic {
		return &turn.RelayAddressGeneratorStatic{
			RelayAddress: net.ParseIP(cfg.TURNPublicIP), // candidate address advertised to peers
			Address:      "0.0.0.0",                     // bind on all interfaces
		}
	}

	srv, err := turn.NewServer(turn.ServerConfig{
		Realm: cfg.TURNRealm,
		// Validates ephemeral "<expiry>:<id>" usernames against the shared secret.
		AuthHandler: turn.LongTermTURNRESTAuthHandler(cfg.TURNSecret, nil),
		// One UDP socket serves both STUN binding and TURN allocation.
		PacketConnConfigs: []turn.PacketConnConfig{{
			PacketConn:            udpListener,
			RelayAddressGenerator: relayGen(),
		}},
		// И тот же TURN по TCP: relay-кандидат для тех, у кого UDP не проходит.
		ListenerConfigs: []turn.ListenerConfig{{
			Listener:              tcpListener,
			RelayAddressGenerator: relayGen(),
		}},
	})
	if err != nil {
		udpListener.Close()
		tcpListener.Close()
		return nil, fmt.Errorf("turn new server: %w", err)
	}
	log.Printf("[TURN] listening udp+tcp :%d realm=%s public_ip=%s", cfg.TURNPort, cfg.TURNRealm, cfg.TURNPublicIP)
	return srv, nil
}

// buildICEServers returns the ICE server list for clients. Always includes a
// public STUN (helps srflx discovery); adds the relay's STUN + ephemeral TURN
// when TURN is enabled.
func (s *Server) buildICEServers(id string) (turnCredsResponse, error) {
	servers := []iceServer{
		{URLs: []string{"stun:stun.l.google.com:19302"}},
	}
	ttl := 0
	if s.Config.TURNEnabled && s.Config.TURNPublicIP != "" {
		user, cred, err := turn.GenerateLongTermTURNRESTCredentials(s.Config.TURNSecret, id, s.Config.TURNCredTTL)
		if err != nil {
			return turnCredsResponse{}, err
		}
		ttl = int(s.Config.TURNCredTTL.Seconds())
		host := net.JoinHostPort(s.Config.TURNPublicIP, strconv.Itoa(s.Config.TURNPort))
		servers = append(servers,
			iceServer{URLs: []string{"stun:" + host}},
			iceServer{
				// UDP и TCP на одном порту (StartTURN): ICE пробует оба и берёт
				// тот, что проходит через VPN/NAT человека. Одни креды на оба.
				URLs: []string{
					"turn:" + host + "?transport=udp",
					"turn:" + host + "?transport=tcp",
				},
				Username:   user,
				Credential: cred,
			},
		)
	}
	return turnCredsResponse{ICEServers: servers, TTL: ttl}, nil
}

// handleTURNCredentials — GET /v1/turn/credentials
//
// Returns ICE servers (STUN + ephemeral TURN) for a WebRTC session. Accepts any
// relay-issued token: a user JWT / Telegram initData (phone/web client) OR a
// device JWT (the PC agent). The credentials aren't user-scoped — they're just
// time-limited HMAC tokens the TURN server validates statelessly.
func (s *Server) handleTURNCredentials(w http.ResponseWriter, r *http.Request) {
	id, userID, ok := s.turnCallerID(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	// Shared devices use their owner's plan. Authenticate the caller's access
	// before accepting the device as the billing principal.
	if deviceID := r.URL.Query().Get("device_id"); strings.HasPrefix(id, "u") && deviceID != "" {
		dev, err := s.deviceForUser(r, deviceID, userID)
		if err != nil {
			writeErr(w, http.StatusForbidden, "device access required")
			return
		}
		userID = dev.UserID
	}
	if !s.cloudGate(w, r, userID) {
		return
	}
	resp, err := s.buildICEServers(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not mint turn credentials")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// turnCallerID authenticates the caller (user OR device token) and returns a
// short, URL-safe identifier to embed in the TURN username (for logging only).
func (s *Server) turnCallerID(r *http.Request) (string, int64, bool) {
	if claims, err := s.requireUserAuth(r); err == nil {
		return "u" + strconv.FormatInt(claims.UserID, 10), claims.UserID, true
	}
	tok := bearer(r.Header.Get("Authorization"))
	if tok == "" {
		tok = r.URL.Query().Get("jwt")
	}
	if tok != "" {
		if dc, err := s.JWT.ParseDevice(tok); err == nil {
			dev, err := db.GetDeviceForAgentAuth(r.Context(), s.DB, dc.DeviceID, dc.UserID)
			if err != nil || dev.RevokedAt.Valid {
				return "", 0, false
			}
			// device IDs are hex fingerprints; keep them URL-safe regardless.
			return "d" + url.QueryEscape(dc.DeviceID), dev.UserID, true
		}
	}
	return "", 0, false
}
