package web

import (
	"os"
	"path/filepath"
	"testing"

	"tgcontrol/internal/cdp"
)

func TestBrowserNoticeMessageCarriesNativeUIFields(t *testing.T) {
	file := browserNoticeMessage(cdp.Notice{Kind: "download", File: "report.pdf"})
	if file["file"] != "report.pdf" {
		t.Fatalf("download filename lost: %#v", file)
	}

	chooser := browserNoticeMessage(cdp.Notice{Kind: "file_chooser", ID: 7, Multiple: true})
	if chooser["id"] != uint64(7) || chooser["multiple"] != true {
		t.Fatalf("chooser fields lost: %#v", chooser)
	}

	dialog := browserNoticeMessage(cdp.Notice{
		Kind: "dialog", ID: 8, DialogType: "prompt",
		Message: "Введите имя", DefaultPrompt: "Анна", URL: "https://example.test",
	})
	for key, want := range map[string]any{
		"id": uint64(8), "dialog_type": "prompt", "message": "Введите имя",
		"default_prompt": "Анна", "url": "https://example.test",
	} {
		if dialog[key] != want {
			t.Fatalf("dialog[%s] = %#v, want %#v; full=%#v", key, dialog[key], want, dialog)
		}
	}
}

func TestValidateBrowserUploadPathOnlyAllowsDirectRegularPtyUpload(t *testing.T) {
	root := t.TempDir()
	good := filepath.Join(root, "pty-upload-123-report.pdf")
	if err := os.WriteFile(good, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := validateBrowserUploadPath(root, good)
	if err != nil || got != good {
		t.Fatalf("valid upload = %q, %v", got, err)
	}

	plain := filepath.Join(root, "report.pdf")
	if err := os.WriteFile(plain, []byte("no"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := validateBrowserUploadPath(root, plain); err == nil {
		t.Fatal("plain file must be rejected")
	}

	nestedDir := filepath.Join(root, "nested")
	if err := os.Mkdir(nestedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(nestedDir, "pty-upload-456-secret.txt")
	if err := os.WriteFile(nested, []byte("no"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := validateBrowserUploadPath(root, nested); err == nil {
		t.Fatal("nested path must be rejected")
	}
}
