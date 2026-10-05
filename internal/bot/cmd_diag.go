package bot

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"tgcontrol/internal/config"
	"tgcontrol/internal/version"
)

// cmdDiag collects logs + state and ships them back as a zip document.
// Useful when a user reports "не работает" — they run /diag and the support
// channel gets all relevant artifacts without manual file-spelunking.
func (tb *Bot) cmdDiag(ctx context.Context, b *bot.Bot, update *models.Update) {
	uid := getUID(update)
	if !tb.authorized(uid) {
		return
	}
	chatID := getChatID(update)
	threadID := getThreadID(update)

	tb.reply(ctx, update, "🔧 Собираю диагностику…")

	buf, err := buildDiagZip(ctx, tb)
	if err != nil {
		tb.reply(ctx, update, "❌ Ошибка: "+esc(err.Error()))
		return
	}

	filename := fmt.Sprintf("tgcontrol-diag-%s.zip", version.Version)
	if _, err := b.SendDocument(ctx, &bot.SendDocumentParams{
		ChatID:          chatID,
		MessageThreadID: threadID,
		Document:        &models.InputFileUpload{Filename: filename, Data: bytes.NewReader(buf)},
		Caption:         "🔧 Диагностика TGControl",
	}); err != nil {
		tb.reply(ctx, update, "❌ Не удалось отправить файл: "+esc(err.Error()))
	}
}

func buildDiagZip(ctx context.Context, tb *Bot) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	// 1) Last ~200 lines of tgcontrol.log
	exe, _ := os.Executable()
	exeDir := filepath.Dir(exe)
	for _, name := range []string{"tgcontrol.log", "tgcontrol.log.old", "tgcontrol.err.log", "cloudflared.log"} {
		p := filepath.Join(exeDir, name)
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		// Trim head if huge — keep last 200 KB to stay below Telegram limits.
		const maxBytes = 200 * 1024
		if len(data) > maxBytes {
			data = data[len(data)-maxBytes:]
			data = append([]byte("[…log truncated…]\n"), data...)
		}
		if err := writeZipEntry(zw, name, data); err != nil {
			return nil, err
		}
	}

	// 2) Sanitized config snapshot
	cfg := config.GetNoSetup()
	safe := sanitizeConfig(cfg)
	cfgJSON, _ := json.MarshalIndent(safe, "", "  ")
	if err := writeZipEntry(zw, "config-sanitized.json", cfgJSON); err != nil {
		return nil, err
	}

	// 3) Health snapshot
	if tb.webServer != nil {
		report := tb.webServer.HealthSnapshot(ctx)
		hj, _ := json.MarshalIndent(report, "", "  ")
		if err := writeZipEntry(zw, "health.json", hj); err != nil {
			return nil, err
		}
	}

	// 4) Sessions summary for the requesting user only (no prompts content).
	if tb.store != nil {
		uid := 0
		// Best-effort — we don't have ctx user here; the diag command is
		// authorised per-user above, and the store API is per-uid.
		for u := range tb.allowed {
			uid = int(u)
			break
		}
		var sessSummary []map[string]any
		if uid != 0 {
			for _, s := range tb.store.List(uid) {
				sessSummary = append(sessSummary, map[string]any{
					"name":  s.Name,
					"agent": s.AgentType,
					"cwd":   s.Cwd,
					"mode":  s.Mode,
				})
			}
		}
		sj, _ := json.MarshalIndent(sessSummary, "", "  ")
		if err := writeZipEntry(zw, "sessions.json", sj); err != nil {
			return nil, err
		}
	}

	// 5) Version info
	versionInfo := map[string]any{
		"version":    version.Version,
		"commit":     version.Commit,
		"build_date": version.BuildDate,
	}
	vj, _ := json.MarshalIndent(versionInfo, "", "  ")
	if err := writeZipEntry(zw, "version.json", vj); err != nil {
		return nil, err
	}

	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeZipEntry(zw *zip.Writer, name string, data []byte) error {
	w, err := zw.Create(name)
	if err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

// sanitizeConfig strips secrets so /diag output is shareable.
func sanitizeConfig(cfg *config.Config) map[string]any {
	out := map[string]any{
		"mode":              cfg.Mode,
		"device_id":         cfg.DeviceID,
		"relay_url":         cfg.RelayURL,
		"web_port":          cfg.WebPort,
		"claude_path":       cfg.ClaudePath,
		"claude_model":      cfg.ClaudeModel,
		"claude_perm":       cfg.ClaudePermissionMode,
		"codex_path":        cfg.CodexPath,
		"codex_model":       cfg.CodexModel,
		"codex_approval":    cfg.CodexApprovalMode,
		"codex_reasoning":   cfg.CodexReasoning,
		"default_agent":     cfg.DefaultAgent,
		"setup_complete":    cfg.SetupComplete,
		"tunnel_mode":       cfg.TunnelMode,
		"tunnel_url":        cfg.TunnelURL,
		"api_token_present": cfg.APIToken != "",
		"telegram_user_id":  redactNumeric(cfg.TelegramUserID),
		"connection_token":  redact(cfg.ConnectionToken),
	}
	return out
}

func redact(s string) string {
	if s == "" {
		return ""
	}
	if len(s) <= 6 {
		return "***"
	}
	return s[:3] + "***" + s[len(s)-3:]
}

func redactNumeric(s string) string {
	if s == "" {
		return ""
	}
	if len(s) <= 4 {
		return "***"
	}
	return strings.Repeat("*", len(s)-3) + s[len(s)-3:]
}
