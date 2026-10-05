package web

import (
	"net/http"
	"os"
	"path/filepath"
)

// servePrivacyPolicy serves the privacy policy HTML page.
// Required for Google Play and App Store submissions.
func (s *Server) servePrivacyPolicy(w http.ResponseWriter, r *http.Request) {
	// Try on-disk file first (for development)
	exe, _ := os.Executable()
	docPath := filepath.Join(filepath.Dir(exe), "docs", "privacy-policy.html")
	if _, err := os.Stat(docPath); err == nil {
		http.ServeFile(w, r, docPath)
		return
	}

	// Embedded fallback
	data, err := embeddedDocs.ReadFile("docs/privacy-policy.html")
	if err == nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(data)
		return
	}

	// Inline minimal policy
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(`<!DOCTYPE html><html><head><title>Remotai Privacy Policy</title></head><body>
<h1>Privacy Policy</h1>
<p>Remotai stores account, paired-device, minimal product analytics and voluntary support-chat data. Remote terminal, file and screen contents are not retained as analytics.</p>
<p>Contact support through https://t.me/Autocode1_bot.</p>
</body></html>`))
}
