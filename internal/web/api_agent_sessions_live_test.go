package web

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"tgcontrol/internal/agentsessions"
	"tgcontrol/internal/pty"
)

// Живой замер на настоящих беседах этого компьютера — ТОЛЬКО ЧТЕНИЕ, кэш во
// временной папке. Печатаются одни числа: ни заголовков, ни папок.
//
//	REMOTAI_LIVE_AGENT_SESSIONS=1 go test ./internal/web -run TestAgentSessionsLive -v
//
// REMOTAI_LIVE_COUNT_SECONDS — сколько дать фоновому подсчёту (0 — не считать).
var liveTagName = regexp.MustCompile(`</?([A-Za-z][A-Za-z0-9_-]*)`)

func TestAgentSessionsLive(t *testing.T) {
	if os.Getenv("REMOTAI_LIVE_AGENT_SESSIONS") != "1" {
		t.Skip("живой замер: REMOTAI_LIVE_AGENT_SESSIONS=1")
	}
	roots := agentSessionRoots()
	perAgentRoots := map[string]int{}
	for _, r := range roots {
		perAgentRoots[r.Agent]++
	}
	t.Logf("каталогов аккаунтов: claude=%d codex=%d", perAgentRoots["claude"], perAgentRoots["codex"])

	state := filepath.Join(t.TempDir(), "agent-sessions.json")
	ix := agentsessions.NewIndex(state)
	ix.NoBackground = true
	ix.PIDAlive = pty.PIDAlive

	measure := func(label string, q agentsessions.Query) agentsessions.Page {
		start := time.Now()
		page, err := ix.List(context.Background(), roots, nil, q)
		if err != nil {
			t.Fatal(err)
		}
		titled, running := 0, 0
		for _, it := range page.Sessions {
			if it.Title != "" {
				titled++
			}
			if it.Running {
				running++
			}
		}
		t.Logf("%s: %v, всего=%d claude=%d codex=%d partial=%v, на странице %d (с заголовком %d, открыты вне Remotai %d)",
			label, time.Since(start).Round(time.Millisecond), page.Total, page.Agents["claude"], page.Agents["codex"],
			page.Partial, len(page.Sessions), titled, running)
		return page
	}
	measure("холодный список", agentsessions.Query{Limit: 200})
	measure("тёплый список", agentsessions.Query{Limit: 50})
	measure("поиск «remotai»", agentsessions.Query{Q: "remotai", Limit: 50})

	// Качество заголовков без их печати: сколько похожи на служебную вставку.
	all := measure("первые 200", agentsessions.Query{Limit: agentsessions.MaxLimit})
	tagLike, command, noCWD := 0, 0, 0
	for _, it := range all.Sessions {
		if strings.HasPrefix(it.Title, "<") || strings.Contains(it.Title, "</") {
			tagLike++
			// Только имя тега — не текст беседы.
			if m := liveTagName.FindStringSubmatch(it.Title); m != nil {
				t.Logf("  тег в заголовке: <%s> (%s)", m[1], it.Agent)
			}
		}
		if strings.HasPrefix(it.Title, "/") {
			command++
		}
		if it.CWD == "" {
			noCWD++
		}
	}
	t.Logf("заголовки: похожи на тег %d, слеш-команда %d, без папки %d", tagLike, command, noCWD)

	secs, _ := strconv.Atoi(os.Getenv("REMOTAI_LIVE_COUNT_SECONDS"))
	if secs > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(secs)*time.Second)
		start := time.Now()
		err := ix.CountPass(ctx)
		cancel()
		page, _ := ix.List(context.Background(), roots, nil, agentsessions.Query{Limit: agentsessions.MaxLimit})
		counted, msgs := 0, 0
		for _, it := range page.Sessions {
			if it.Messages != nil {
				counted++
				msgs += *it.Messages
			}
		}
		t.Logf("подсчёт сообщений: %v (err=%v), посчитано бесед на первой странице %d из %d, сообщений %d, counting=%v",
			time.Since(start).Round(time.Millisecond), err, counted, len(page.Sessions), msgs, page.Counting)
	}
	if info, err := os.Stat(state); err == nil {
		t.Logf("кэш на диске: %d КБ", info.Size()/1024)
	}
}
