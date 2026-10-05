package bot

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

// SendFileToUser delivers a local file to the user's last active Telegram chat.
func (tb *Bot) SendFileToUser(ctx context.Context, uid int64, path string) error {
	if tb.b == nil {
		return fmt.Errorf("telegram bot is not ready yet")
	}

	tb.chatMu.RLock()
	target, ok := tb.chatTargets[uid]
	tb.chatMu.RUnlock()
	if !ok || target.ChatID == 0 {
		return fmt.Errorf("open the bot chat first so I know where to send the file")
	}

	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return fmt.Errorf("file not found")
	}

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = tb.b.SendDocument(ctx, &bot.SendDocumentParams{
		ChatID:          target.ChatID,
		MessageThreadID: target.ThreadID,
		Document:        &models.InputFileUpload{Filename: filepath.Base(path), Data: f},
	})
	if err != nil {
		return err
	}
	return nil
}
