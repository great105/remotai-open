package cdp

import (
	"encoding/json"
	"testing"
)

func TestFileChooserOpenedPublishesVisiblePicker(t *testing.T) {
	client := &Client{}
	var notices []Notice
	client.SetNotify(func(n Notice) { notices = append(notices, n) })

	client.onFileChooserOpened(json.RawMessage(`{
		"frameId":"frame-1",
		"mode":"selectMultiple",
		"backendNodeId":42
	}`))

	if len(notices) != 1 {
		t.Fatalf("notices = %d, want 1", len(notices))
	}
	n := notices[0]
	if n.Kind != "file_chooser" || n.ID == 0 || !n.Multiple {
		t.Fatalf("unexpected notice: %+v", n)
	}
	if client.fileChooser == nil || client.fileChooser.BackendNodeID != 42 {
		t.Fatalf("pending chooser = %+v", client.fileChooser)
	}

	if err := client.CancelFileChooser(n.ID + 1); err == nil {
		t.Fatal("stale chooser id must be rejected")
	}
	if err := client.CancelFileChooser(n.ID); err != nil {
		t.Fatalf("cancel current chooser: %v", err)
	}
	if client.fileChooser != nil {
		t.Fatalf("chooser was not cleared: %+v", client.fileChooser)
	}
	if got := notices[len(notices)-1]; got.Kind != "file_chooser_closed" || got.ID != n.ID {
		t.Fatalf("close notice = %+v", got)
	}
}

func TestJavaScriptDialogOpeningKeepsPromptAndClampsHostileText(t *testing.T) {
	client := &Client{}
	var notice Notice
	client.SetNotify(func(n Notice) { notice = n })
	long := make([]rune, 4100)
	for i := range long {
		long[i] = 'я'
	}
	raw, err := json.Marshal(map[string]any{
		"type":          "prompt",
		"message":       string(long),
		"defaultPrompt": "готово",
		"url":           "https://example.test/form",
	})
	if err != nil {
		t.Fatal(err)
	}
	client.onJavaScriptDialogOpening(raw)

	if notice.Kind != "dialog" || notice.ID == 0 || notice.DialogType != "prompt" {
		t.Fatalf("unexpected notice: %+v", notice)
	}
	if got := len([]rune(notice.Message)); got != 4000 {
		t.Fatalf("message runes = %d, want 4000", got)
	}
	if notice.DefaultPrompt != "готово" || notice.URL != "https://example.test/form" {
		t.Fatalf("dialog data lost: %+v", notice)
	}

	client.onJavaScriptDialogClosed()
	if client.dialog != nil {
		t.Fatal("closed event did not clear pending dialog")
	}
}

func TestFileChooserRejectsMissingBackendNode(t *testing.T) {
	client := &Client{}
	called := false
	client.SetNotify(func(Notice) { called = true })
	client.onFileChooserOpened(json.RawMessage(`{"mode":"selectSingle"}`))
	if called || client.fileChooser != nil {
		t.Fatal("invalid chooser event must be ignored")
	}
}

func TestClosingClientDismissesPendingNativeUI(t *testing.T) {
	client := &Client{done: make(chan struct{})}
	client.fileChooser = &FileChooser{ID: 4, BackendNodeID: 12}
	client.dialog = &JavaScriptDialog{ID: 5, Type: "confirm"}
	var notices []Notice
	client.SetNotify(func(n Notice) { notices = append(notices, n) })

	client.markClosed()
	client.markClosed() // idempotent: повторное Close не дублирует события.

	if client.fileChooser != nil || client.dialog != nil {
		t.Fatal("pending native UI was not cleared")
	}
	if len(notices) != 2 {
		t.Fatalf("notices = %+v, want chooser+dialog close", notices)
	}
	if notices[0].Kind != "file_chooser_closed" || notices[0].ID != 4 {
		t.Fatalf("chooser close = %+v", notices[0])
	}
	if notices[1].Kind != "dialog_closed" || notices[1].ID != 5 {
		t.Fatalf("dialog close = %+v", notices[1])
	}
}
