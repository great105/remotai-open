package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"tgcontrol/internal/config"
	"tgcontrol/internal/dnsfallback"
	"tgcontrol/internal/hermes"
	"tgcontrol/internal/relay"
	"time"
)

func hermesDeliveryOwner(cfg *config.Config, uid int64) bool {
	owner, err := strconv.ParseInt(cfg.TelegramUserID, 10, 64)
	// A device JWT authenticates the computer, not a Telegram binding. Only
	// the explicit central-bot owner is eligible for this relay delivery route.
	return err == nil && owner > 0 && cfg.IsCentralBot() && uid == hermesBackendUID(cfg, owner) && cfg.DeviceID != "" && cfg.RelayHTTPBase() != "" && !strings.EqualFold(cfg.NotificationsEnabled, "false")
}
func (s *Server) configureHermesDelivery(mgr *hermes.Manager, uid int64) {
	ready := func() bool {
		cfg := config.GetNoSetup()
		if !hermesDeliveryOwner(cfg, uid) {
			return false
		}
		if cfg.RelayJWT != "" {
			return true
		}
		jwt, _ := relay.LoadJWT()
		return jwt != ""
	}
	mgr.SetDeliveryTransport(func(ctx context.Context, record hermes.AttentionRecord) error {
		cfg := *config.GetNoSetup()
		if !hermesDeliveryOwner(&cfg, uid) {
			return errors.New("доставка недоступна для этого владельца")
		}
		if cfg.RelayJWT == "" {
			cfg.RelayJWT, _ = relay.LoadJWT()
		}
		return s.sendHermesDelivery(ctx, &http.Client{Timeout: 30 * time.Second, Transport: dnsfallback.Transport()}, &cfg, uid, record)
	}, ready)
}
func (s *Server) sendHermesDelivery(ctx context.Context, client *http.Client, cfg *config.Config, uid int64, record hermes.AttentionRecord) error {
	if !hermesDeliveryOwner(cfg, uid) || cfg.RelayJWT == "" {
		return errors.New("подключите Telegram владельца компьютера и включите уведомления")
	}
	base := cfg.RelayHTTPBase()
	query := url.Values{"device": {cfg.DeviceID}, "session": {record.StoredSessionID}, "attention": {record.ID}}
	label := "Hermes требует внимания"
	if record.Kind == "completed" {
		label = "Hermes завершил задачу"
	}
	if record.Kind == "failed" || record.Kind == "interrupted" {
		label = "Проверьте результат задачи Hermes"
	}
	// No command, question, task text, filename or provider error is public.
	text := label + ". Откройте пульт: " + base + "/app/#/hermes?" + query.Encode()
	raw, _ := json.Marshal(map[string]string{"text": text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/agent/send", bytes.NewReader(raw))
	if err != nil {
		return errors.New("не удалось подготовить доставку")
	}
	req.Header.Set("Authorization", "Bearer "+cfg.RelayJWT)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("исход доставки неизвестен; проверьте Telegram")
	}
	defer resp.Body.Close()
	var receipt struct {
		OK    bool `json:"ok"`
		Muted bool `json:"muted"`
	}
	if resp.StatusCode != 200 || json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 64<<10)).Decode(&receipt) != nil || !receipt.OK || receipt.Muted {
		return errors.New("Telegram не подтвердил доставку; проверьте привязку и уведомления")
	}
	return nil
}
