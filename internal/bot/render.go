package bot

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

// renderHTML converts agent output (markdown-ish) to Telegram HTML.
// Simplified port — handles code blocks, inline code, bold, italic.
func renderHTML(text string) string {
	if text == "" {
		return ""
	}

	// Process code blocks first (``` ... ```)
	var result strings.Builder
	remaining := text

	for {
		idx := strings.Index(remaining, "```")
		if idx == -1 {
			result.WriteString(renderInline(remaining))
			break
		}
		// Before code block
		result.WriteString(renderInline(remaining[:idx]))
		remaining = remaining[idx+3:]

		// Find language hint (first line)
		lang := ""
		nlIdx := strings.Index(remaining, "\n")
		if nlIdx != -1 && nlIdx < 30 {
			candidate := strings.TrimSpace(remaining[:nlIdx])
			if isLangHint(candidate) {
				lang = candidate
				remaining = remaining[nlIdx+1:]
			}
		}

		// Find closing ```
		closeIdx := strings.Index(remaining, "```")
		var code string
		if closeIdx == -1 {
			code = remaining
			remaining = ""
		} else {
			code = remaining[:closeIdx]
			remaining = remaining[closeIdx+3:]
		}

		escaped := escHTML(code)
		if lang != "" {
			result.WriteString(fmt.Sprintf("<pre><code class=\"language-%s\">%s</code></pre>\n", escHTML(lang), escaped))
		} else {
			result.WriteString(fmt.Sprintf("<pre><code>%s</code></pre>\n", escaped))
		}
	}

	out := result.String()
	// Clean up excessive newlines
	out = regexp.MustCompile(`\n{3,}`).ReplaceAllString(out, "\n\n")
	return strings.TrimSpace(out)
}

func isLangHint(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '+' || c == '#' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

// renderInline handles inline markdown: **bold**, *italic*, `code`, ~~strikethrough~~
func renderInline(text string) string {
	// Escape HTML first
	text = escHTML(text)

	// Inline code
	text = regexp.MustCompile("`([^`]+)`").ReplaceAllString(text, "<code>$1</code>")

	// Bold (**text**)
	text = regexp.MustCompile(`\*\*([^*]+)\*\*`).ReplaceAllString(text, "<b>$1</b>")

	// Italic (*text*)
	text = regexp.MustCompile(`(?:^|[^*])\*([^*]+)\*(?:[^*]|$)`).ReplaceAllStringFunc(text, func(s string) string {
		// Simple approach: just replace *text* with <i>text</i>
		return regexp.MustCompile(`\*([^*]+)\*`).ReplaceAllString(s, "<i>$1</i>")
	})

	// Strikethrough
	text = regexp.MustCompile(`~~([^~]+)~~`).ReplaceAllString(text, "<s>$1</s>")

	// Headings (# text → bold)
	text = regexp.MustCompile(`(?m)^#{1,6}\s+(.+)$`).ReplaceAllString(text, "<b>$1</b>")

	// Bullet lists
	text = regexp.MustCompile(`(?m)^[-*]\s+`).ReplaceAllString(text, "• ")

	return text
}

func escHTML(text string) string {
	text = strings.ReplaceAll(text, "&", "&amp;")
	text = strings.ReplaceAll(text, "<", "&lt;")
	text = strings.ReplaceAll(text, ">", "&gt;")
	return text
}

// splitMessage splits a long message into chunks for Telegram's 4096 char limit.
func splitMessage(text string, limit int) []string {
	if limit <= 0 {
		limit = 4096
	}
	if len(text) <= limit {
		return []string{text}
	}

	var chunks []string
	remaining := text

	for len(remaining) > 0 {
		if len(remaining) <= limit {
			chunks = append(chunks, remaining)
			break
		}

		// Try paragraph boundary
		cut := strings.LastIndex(remaining[:limit], "\n\n")
		if cut > limit/4 {
			chunks = append(chunks, strings.TrimRight(remaining[:cut], "\n"))
			remaining = strings.TrimLeft(remaining[cut:], "\n")
			continue
		}

		// Try line boundary
		cut = strings.LastIndex(remaining[:limit], "\n")
		if cut > limit/4 {
			chunks = append(chunks, strings.TrimRight(remaining[:cut], "\n"))
			remaining = strings.TrimLeft(remaining[cut:], "\n")
			continue
		}

		// Hard cut
		chunks = append(chunks, remaining[:limit])
		remaining = remaining[limit:]
	}

	var result []string
	for _, c := range chunks {
		if c != "" {
			result = append(result, c)
		}
	}
	return result
}

// ── Reply helpers ────────────────────────────────────────────────────

func (tb *Bot) reply(ctx context.Context, update *models.Update, text string, markup ...models.ReplyMarkup) {
	chatID := getChatID(update)
	threadID := getThreadID(update)
	chunks := splitMessage(text, 4096)
	for i, chunk := range chunks {
		params := &bot.SendMessageParams{
			ChatID:          chatID,
			MessageThreadID: threadID,
			Text:            chunk,
			ParseMode:       models.ParseModeHTML,
		}
		if i == len(chunks)-1 && len(markup) > 0 {
			params.ReplyMarkup = markup[0]
		}
		_, err := tb.b.SendMessage(ctx, params)
		if err != nil {
			// Retry without parse mode
			params.ParseMode = ""
			tb.b.SendMessage(ctx, params)
		}
	}
}

func (tb *Bot) replySplit(ctx context.Context, chatID int64, threadID int, text string) {
	chunks := splitMessage(text, 4096)
	for _, chunk := range chunks {
		_, err := tb.b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:          chatID,
			MessageThreadID: threadID,
			Text:            chunk,
			ParseMode:       models.ParseModeHTML,
		})
		if err != nil {
			tb.b.SendMessage(ctx, &bot.SendMessageParams{
				ChatID:          chatID,
				MessageThreadID: threadID,
				Text:            chunk,
			})
		}
	}
}

func (tb *Bot) replySplitWithKB(ctx context.Context, chatID int64, threadID int, text string, kb *models.InlineKeyboardMarkup) {
	chunks := splitMessage(text, 4096)
	for i, chunk := range chunks {
		params := &bot.SendMessageParams{
			ChatID:          chatID,
			MessageThreadID: threadID,
			Text:            chunk,
			ParseMode:       models.ParseModeHTML,
		}
		if i == len(chunks)-1 {
			params.ReplyMarkup = kb
		}
		_, err := tb.b.SendMessage(ctx, params)
		if err != nil {
			params.ParseMode = ""
			tb.b.SendMessage(ctx, params)
		}
	}
}

func (tb *Bot) answerCB(ctx context.Context, update *models.Update, text string, markup ...models.ReplyMarkup) {
	if update.CallbackQuery == nil {
		return
	}
	msg := update.CallbackQuery.Message.Message
	if msg == nil {
		return
	}
	params := &bot.EditMessageTextParams{
		ChatID:    msg.Chat.ID,
		MessageID: msg.ID,
		Text:      text,
		ParseMode: models.ParseModeHTML,
	}
	if len(markup) > 0 {
		params.ReplyMarkup = markup[0]
	}
	_, err := tb.b.EditMessageText(ctx, params)
	if err != nil {
		// Retry without parse mode
		params.ParseMode = ""
		tb.b.EditMessageText(ctx, params)
	}
}

// downloadFile downloads a URL to a local file path.
func downloadFile(url, filepath string) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	out, err := os.Create(filepath)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, resp.Body)
	return err
}
