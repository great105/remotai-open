package pty

import (
	"testing"
	"time"
)

// Спящая беседа должна находиться экраном «Беседы»: иначе нажатие на неё
// запустило бы вторую копию агента рядом со спящим терминалом.
func TestAgentConversationsIncludesSleeping(t *testing.T) {
	m := newSleepTestManager(t, &Session{ID: "t1"}, &Session{ID: "t2"}, &Session{ID: "ssh1", Shell: "ssh"})
	rec := &SleepRecord{Agent: "claude", SessionID: "7caa5648-8121-42c3-8868-d270b407a029", At: time.Now().UnixMilli()}
	if err := m.meta.SetSleep("t1", rec); err != nil {
		t.Fatal(err)
	}
	if err := m.meta.SetSleep("ssh1", &SleepRecord{Agent: "codex", SessionID: "019a1b2c-3d4e-5f60-7182-93a4b5c6d7e8"}); err != nil {
		t.Fatal(err)
	}
	got := m.AgentConversations()
	if len(got) != 1 {
		t.Fatalf("ждали одну спящую беседу (SSH-терминал не в счёт): %+v", got)
	}
	c := got[0]
	if c.PtyID != "t1" || c.Agent != "claude" || c.SessionID != rec.SessionID || !c.Sleeping {
		t.Fatalf("спящая беседа: %+v", c)
	}
}
