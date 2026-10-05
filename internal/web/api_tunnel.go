package web

import (
	"context"
	"log"
	"net/http"
	"time"

	"tgcontrol/internal/config"
	"tgcontrol/internal/tunnel"
)

// registerTunnelRoutes adds tunnel management endpoints.
func (s *Server) registerTunnelRoutes() {
	s.mux.HandleFunc("GET /api/tunnel/status", s.authWrap(s.apiTunnelStatus))
	s.mux.HandleFunc("POST /api/tunnel/start", s.authWrap(s.apiTunnelStart))
	s.mux.HandleFunc("POST /api/tunnel/stop", s.authWrap(s.apiTunnelStop))
	s.mux.HandleFunc("POST /api/tunnel/restart", s.authWrap(s.apiTunnelRestart))
	s.mux.HandleFunc("POST /api/tunnel/install", s.authWrap(s.apiTunnelInstall))
	s.mux.HandleFunc("PATCH /api/tunnel/config", s.authWrap(s.apiTunnelConfig))
	s.mux.HandleFunc("GET /api/tunnel/logs", s.authWrap(s.apiTunnelLogs))

	// Setup-mode endpoints (no auth). GET status is polled in a tight loop while
	// the tunnel comes up, so it must NOT be rate-limited (same as the other
	// GET /api/setup/* status reads). State-changing POSTs stay rate-limited.
	s.mux.HandleFunc("GET /api/setup/tunnel", s.apiSetupTunnelStatus)
	s.mux.HandleFunc("POST /api/setup/tunnel/install", s.rateLimit(s.setupLimiter, s.apiSetupTunnelInstall))
	s.mux.HandleFunc("POST /api/setup/tunnel/start", s.rateLimit(s.setupLimiter, s.apiSetupTunnelStart))
	s.mux.HandleFunc("POST /api/setup/tunnel/config", s.rateLimit(s.setupLimiter, s.apiSetupTunnelConfig))
}

// GET /api/tunnel/status
func (s *Server) apiTunnelStatus(w http.ResponseWriter, r *http.Request, _ int64) {
	jsonResp(w, s.tunnelStatusResponse())
}

// POST /api/tunnel/start
func (s *Server) apiTunnelStart(w http.ResponseWriter, r *http.Request, _ int64) {
	s.handleTunnelStart(w, r)
}

// POST /api/tunnel/stop
func (s *Server) apiTunnelStop(w http.ResponseWriter, r *http.Request, _ int64) {
	if s.tunnelManager == nil {
		jsonError(w, "Tunnel not initialized", 400)
		return
	}
	if err := s.tunnelManager.Stop(); err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	log.Println("[TUNNEL] Stopped via API")
	jsonResp(w, map[string]bool{"ok": true})
}

// POST /api/tunnel/restart
func (s *Server) apiTunnelRestart(w http.ResponseWriter, r *http.Request, _ int64) {
	if s.tunnelManager == nil {
		jsonError(w, "Tunnel not initialized", 400)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := s.tunnelManager.Restart(ctx); err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	jsonResp(w, map[string]bool{"ok": true})
}

// POST /api/tunnel/install
func (s *Server) apiTunnelInstall(w http.ResponseWriter, r *http.Request, _ int64) {
	s.handleTunnelInstall(w)
}

// PATCH /api/tunnel/config
func (s *Server) apiTunnelConfig(w http.ResponseWriter, r *http.Request, _ int64) {
	s.handleTunnelConfig(w, r)
}

// GET /api/tunnel/logs
func (s *Server) apiTunnelLogs(w http.ResponseWriter, r *http.Request, _ int64) {
	if s.tunnelManager == nil {
		jsonResp(w, map[string][]string{"logs": {}})
		return
	}
	info := s.tunnelManager.Info()
	jsonResp(w, map[string][]string{"logs": info.LogTail})
}

// ── Setup-mode handlers (no auth) ───────────────────────────────────

// GET /api/setup/tunnel
func (s *Server) apiSetupTunnelStatus(w http.ResponseWriter, r *http.Request) {
	jsonResp(w, s.tunnelStatusResponse())
}

// POST /api/setup/tunnel/install
func (s *Server) apiSetupTunnelInstall(w http.ResponseWriter, r *http.Request) {
	s.handleTunnelInstall(w)
}

// POST /api/setup/tunnel/start
func (s *Server) apiSetupTunnelStart(w http.ResponseWriter, r *http.Request) {
	s.handleTunnelStart(w, r)
}

// POST /api/setup/tunnel/config
func (s *Server) apiSetupTunnelConfig(w http.ResponseWriter, r *http.Request) {
	s.handleTunnelConfig(w, r)
}

// ── Shared helpers ──────────────────────────────────────────────────

func (s *Server) tunnelStatusResponse() map[string]any {
	path, installed := tunnel.IsCloudflaredInstalled()
	cfg := config.GetNoSetup()

	resp := map[string]any{
		"installed":        installed,
		"cloudflared_path": path,
		"mode":             cfg.TunnelMode,
		"autostart":        cfg.TunnelAutoStart,
		"tunnel_name":      cfg.TunnelName,
		"tunnel_url":       cfg.TunnelURL,
		"status":           string(tunnel.StatusStopped),
	}

	if s.tunnelManager != nil {
		info := s.tunnelManager.Info()
		resp["status"] = string(info.Status)
		if info.URL != "" {
			resp["url"] = info.URL
		}
		if info.Error != "" {
			resp["error"] = info.Error
		}
		resp["pid"] = info.PID
		resp["uptime"] = info.Uptime
	}

	return resp
}

func (s *Server) handleTunnelStart(w http.ResponseWriter, r *http.Request) {
	if s.tunnelManager == nil {
		jsonError(w, "Tunnel not initialized", 400)
		return
	}

	var req struct {
		Mode string `json:"mode"` // "quick" or "named"
	}
	readJSON(r, &req)

	cfg := config.GetNoSetup()

	// Apply mode from request or use saved config
	mode := tunnel.Mode(cfg.TunnelMode)
	if req.Mode != "" {
		mode = tunnel.Mode(req.Mode)
	}
	if mode == "" || mode == tunnel.ModeDisabled {
		mode = tunnel.ModeQuick
	}

	s.tunnelManager.SetConfig(tunnel.Config{
		Mode:            mode,
		CloudflaredPath: cfg.CloudflaredPath,
		TunnelName:      cfg.TunnelName,
		TunnelURL:       cfg.TunnelURL,
		AutoStart:       cfg.TunnelAutoStart,
	})

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	if err := s.tunnelManager.Start(ctx); err != nil {
		jsonError(w, err.Error(), 500)
		return
	}

	// Save mode to config
	cfg.TunnelMode = string(mode)
	cfg.Save()

	jsonResp(w, map[string]bool{"ok": true})
}

func (s *Server) handleTunnelInstall(w http.ResponseWriter) {
	path, err := tunnel.DownloadCloudflared(nil)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}

	// Save path to config
	cfg := config.GetNoSetup()
	cfg.CloudflaredPath = path
	cfg.Save()

	jsonResp(w, map[string]any{
		"ok":   true,
		"path": path,
	})
}

func (s *Server) handleTunnelConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Mode      *string `json:"mode"`
		AutoStart *bool   `json:"autostart"`
		Name      *string `json:"tunnel_name"`
		URL       *string `json:"tunnel_url"`
	}
	if err := readJSON(r, &req); err != nil {
		jsonError(w, "Invalid request", 400)
		return
	}

	cfg := config.GetNoSetup()
	if req.Mode != nil {
		cfg.TunnelMode = *req.Mode
	}
	if req.AutoStart != nil {
		cfg.TunnelAutoStart = *req.AutoStart
	}
	if req.Name != nil {
		cfg.TunnelName = *req.Name
	}
	if req.URL != nil {
		cfg.TunnelURL = *req.URL
	}
	if err := cfg.Save(); err != nil {
		jsonError(w, "Failed to save config", 500)
		return
	}

	// Update tunnel manager config
	if s.tunnelManager != nil {
		s.tunnelManager.SetConfig(tunnel.Config{
			Mode:            tunnel.Mode(cfg.TunnelMode),
			CloudflaredPath: cfg.CloudflaredPath,
			TunnelName:      cfg.TunnelName,
			TunnelURL:       cfg.TunnelURL,
			AutoStart:       cfg.TunnelAutoStart,
		})
	}

	jsonResp(w, map[string]bool{"ok": true})
}
