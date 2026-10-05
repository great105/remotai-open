package server

import (
	"net/http"
	"time"

	"tgcontrol-relay/internal/db"
)

var startedAt = time.Now()

type healthResponse struct {
	OK         bool    `json:"ok"`
	UptimeSec  float64 `json:"uptime_sec"`
	DBOk       bool    `json:"db_ok"`
	Version    string  `json:"version"`
	BotEnabled bool    `json:"bot_enabled"`
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	dbOK := true
	if err := db.Ping(s.DB); err != nil {
		dbOK = false
	}
	writeJSON(w, http.StatusOK, healthResponse{
		OK:         true,
		UptimeSec:  time.Since(startedAt).Seconds(),
		DBOk:       dbOK,
		Version:    Version,
		BotEnabled: s.Bot != nil,
	})
}

// Version устанавливается при сборке через -ldflags.
var Version = "dev"
