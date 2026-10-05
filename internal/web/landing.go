package web

import (
	"bytes"
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"tgcontrol/internal/config"
	"tgcontrol/internal/version"
)

// serveLandingPage serves the marketing landing page at /.
func (s *Server) serveLandingPage(w http.ResponseWriter, r *http.Request) {
	// Try on-disk landing page first
	exe, _ := os.Executable()
	landingPath := filepath.Join(filepath.Dir(exe), "landing", "index.html")
	if _, err := os.Stat(landingPath); err == nil {
		http.ServeFile(w, r, landingPath)
		return
	}

	// Embedded landing page
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	language := r.URL.Query().Get("lang")
	if language != "en" && language != "ru" {
		language = "ru"
		if header := r.Header.Get("Accept-Language"); header != "" && !strings.HasPrefix(strings.ToLower(header), "ru") {
			language = "en"
		}
	}
	w.Write(buildLandingHTMLInLanguage(language))
}

// landingTmpl is parsed once at startup; html/template auto-escapes the data
// fields (Version etc.) so no manual escaping is needed.
var landingTmpl = template.Must(template.New("landing").Parse(landingTemplate))
var landingTmplEN = template.Must(template.New("landing-en").Parse(landingTemplateEN))

type landingData struct {
	AppName string
	Version string
	CtaURL  string
	CtaText string
}

func buildLandingHTML() []byte {
	return buildLandingHTMLInLanguage("ru")
}

func buildLandingHTMLInLanguage(language string) []byte {
	cfg := config.GetNoSetup()
	data := landingData{
		AppName: "Remotai",
		Version: version.Version,
		CtaURL:  "/setup",
		CtaText: "Setup",
	}
	if cfg.IsConfigured() {
		data.CtaURL = "/miniapp"
		data.CtaText = "Open App"
	}
	var buf bytes.Buffer
	tmpl := landingTmpl
	if language == "en" {
		tmpl = landingTmplEN
	}
	if err := tmpl.Execute(&buf, data); err != nil {
		return []byte(`<!DOCTYPE html><html lang="ru"><body style="background:#0d1117;color:#c9d1d9;font-family:sans-serif;text-align:center;padding:40px">Remotai</body></html>`)
	}
	return buf.Bytes()
}

const landingTemplate = `<!DOCTYPE html>
<html lang="ru">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.AppName}} — Remote PC Control via Telegram</title>
<meta name="description" content="Control your PC from your phone via Telegram. Remote desktop, terminal, file manager, AI agents.">
<style>
*{margin:0;padding:0;box-sizing:border-box}
:root{--bg:#0d1117;--card:#161b22;--border:#30363d;--text:#c9d1d9;--dim:#8b949e;--bright:#f0f6fc;--accent:#58a6ff;--success:#3fb950}
body{font-family:-apple-system,system-ui,'Segoe UI',sans-serif;background:var(--bg);color:var(--text);min-height:100vh}
.container{max-width:960px;margin:0 auto;padding:0 20px}

/* Hero */
.hero{text-align:center;padding:80px 0 60px}
.hero h1{font-size:48px;font-weight:800;color:var(--bright);margin-bottom:12px;letter-spacing:-1px}
.hero h1 span{color:var(--accent)}
.hero p{font-size:18px;color:var(--dim);max-width:500px;margin:0 auto 32px;line-height:1.6}
.hero-cta{display:inline-flex;gap:12px}
.hero-cta a{display:inline-block;padding:14px 32px;border-radius:10px;font-size:16px;font-weight:600;text-decoration:none;transition:all .2s}
.btn-primary-l{background:var(--accent);color:#fff}
.btn-primary-l:hover{background:#79c0ff;transform:translateY(-1px)}
.btn-secondary-l{background:var(--card);color:var(--text);border:1px solid var(--border)}
.btn-secondary-l:hover{border-color:var(--accent);color:var(--bright)}
.hero-badge{display:inline-block;font-size:12px;background:rgba(63,185,80,.12);color:var(--success);padding:4px 12px;border-radius:99px;margin-bottom:20px}

/* Features */
.features{display:grid;grid-template-columns:repeat(auto-fit,minmax(250px,1fr));gap:20px;padding:40px 0}
.feature{background:var(--card);border:1px solid var(--border);border-radius:12px;padding:24px}
.feature-icon{font-size:32px;margin-bottom:12px}
.feature h3{font-size:16px;color:var(--bright);margin-bottom:6px}
.feature p{font-size:13px;color:var(--dim);line-height:1.5}

/* Pricing */
.pricing{padding:60px 0;text-align:center}
.pricing h2{font-size:32px;color:var(--bright);margin-bottom:8px}
.pricing>p{color:var(--dim);margin-bottom:32px}
.pricing-grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(240px,1fr));gap:16px;max-width:800px;margin:0 auto}
.plan{background:var(--card);border:1px solid var(--border);border-radius:12px;padding:24px;text-align:left}
.plan.featured{border-color:var(--accent);box-shadow:0 0 0 1px var(--accent)}
.plan-name{font-size:18px;font-weight:700;color:var(--bright)}
.plan-price{font-size:32px;font-weight:800;color:var(--bright);margin:12px 0 4px}
.plan-price span{font-size:14px;font-weight:400;color:var(--dim)}
.plan-features{list-style:none;margin:16px 0}
.plan-features li{padding:6px 0;font-size:13px;color:var(--text)}
.plan-features li::before{content:"✓ ";color:var(--success)}
.plan-btn{display:block;width:100%;padding:10px;border-radius:8px;border:none;font-size:14px;font-weight:600;cursor:pointer;text-align:center;text-decoration:none;transition:background .2s}
.plan-btn-accent{background:var(--accent);color:#fff}
.plan-btn-accent:hover{background:#79c0ff}
.plan-btn-muted{background:var(--border);color:var(--text)}

/* How it works */
.how{padding:60px 0;text-align:center}
.how h2{font-size:32px;color:var(--bright);margin-bottom:32px}
.steps-row{display:flex;gap:24px;justify-content:center;flex-wrap:wrap}
.step-card{background:var(--card);border:1px solid var(--border);border-radius:12px;padding:24px;width:200px;text-align:center}
.step-num{font-size:32px;font-weight:800;color:var(--accent);margin-bottom:8px}
.step-card h4{color:var(--bright);margin-bottom:4px}
.step-card p{font-size:12px;color:var(--dim)}

/* Footer */
.footer{border-top:1px solid var(--border);padding:24px 0;margin-top:40px;text-align:center;font-size:13px;color:var(--dim)}
.footer a{color:var(--accent);text-decoration:none}
.footer a:hover{text-decoration:underline}

@media(max-width:600px){
  .hero h1{font-size:32px}
  .hero p{font-size:15px}
  .hero-cta{flex-direction:column}
  .features{grid-template-columns:1fr}
  .steps-row{flex-direction:column;align-items:center}
}
</style>
</head>
<body>

<div class="container">
  <section class="hero">
    <div class="hero-badge">v{{.Version}} — Open Source</div>
    <h1>Remot<span>ai</span></h1>
    <p>Управляйте компьютером с телефона через Telegram. Рабочий стол, терминал, файлы, AI-агенты.</p>
    <div class="hero-cta">
      <a href="{{.CtaURL}}" class="btn-primary-l">{{.CtaText}}</a>
      <a href="/privacy" class="btn-secondary-l">Privacy Policy</a>
    </div>
  </section>

  <section class="features">
    <div class="feature">
      <div class="feature-icon">🖥️</div>
      <h3>Remote Desktop</h3>
      <p>Управляйте мышкой и клавиатурой прямо с телефона. Adaptive streaming, multi-monitor, clipboard sync.</p>
    </div>
    <div class="feature">
      <div class="feature-icon">💻</div>
      <h3>Terminal</h3>
      <p>Полноценный терминал через xterm.js. Запускайте Claude Code, shell, PowerShell прямо с телефона.</p>
    </div>
    <div class="feature">
      <div class="feature-icon">📁</div>
      <h3>File Manager</h3>
      <p>Просмотр, загрузка, отправка файлов в Telegram. Поиск, предпросмотр, закладки.</p>
    </div>
    <div class="feature">
      <div class="feature-icon">🤖</div>
      <h3>AI Agents</h3>
      <p>Claude Code, Codex, Gemini и другие. Orchestrator координирует агентов для сложных задач.</p>
    </div>
    <div class="feature">
      <div class="feature-icon">📊</div>
      <h3>System Monitor</h3>
      <p>CPU, RAM, диск, процессы. Shutdown, restart, sleep, lock — всё с телефона.</p>
    </div>
    <div class="feature">
      <div class="feature-icon">🔒</div>
      <h3>Secure</h3>
      <p>Telegram auth, JWT tokens, TLS encryption. Ваши данные не покидают ваш ПК.</p>
    </div>
  </section>

  <section class="how">
    <h2>Как это работает</h2>
    <div class="steps-row">
      <div class="step-card">
        <div class="step-num">1</div>
        <h4>Установите</h4>
        <p>Скачайте и запустите на ПК</p>
      </div>
      <div class="step-card">
        <div class="step-num">2</div>
        <h4>Настройте</h4>
        <p>3 шага в браузере</p>
      </div>
      <div class="step-card">
        <div class="step-num">3</div>
        <h4>Подключитесь</h4>
        <p>Откройте Mini App в Telegram</p>
      </div>
    </div>
  </section>

  <section class="pricing">
    <h2>Тарифы</h2>
    <p>Начните бесплатно, платите только за то, что нужно</p>
    <div class="pricing-grid">
      <div class="plan">
        <div class="plan-name">Free</div>
        <div class="plan-price">$0<span>/мес</span></div>
        <ul class="plan-features">
          <li>1 устройство</li>
          <li>Remote Desktop (720p, 10fps)</li>
          <li>1 PTY терминал</li>
          <li>2 AI-агента</li>
          <li>Файлы (только чтение)</li>
        </ul>
        <a href="/setup" class="plan-btn plan-btn-muted">Начать бесплатно</a>
      </div>
      <div class="plan featured">
        <div class="plan-name">Pro</div>
        <div class="plan-price">$9<span>/мес</span></div>
        <ul class="plan-features">
          <li>3 устройства</li>
          <li>Remote Desktop (1080p, 30fps)</li>
          <li>Безлимит терминалов</li>
          <li>Все AI-агенты</li>
          <li>Файлы (чтение + запись)</li>
          <li>Автообновление</li>
          <li>14 дней бесплатно</li>
        </ul>
        <a href="/miniapp/#/settings" class="plan-btn plan-btn-accent">Попробовать Pro</a>
      </div>
      <div class="plan">
        <div class="plan-name">Team</div>
        <div class="plan-price">$29<span>/мес</span></div>
        <ul class="plan-features">
          <li>10 устройств</li>
          <li>Всё из Pro</li>
          <li>Orchestrator</li>
          <li>Researcher</li>
          <li>Приоритетная поддержка</li>
        </ul>
        <a href="/miniapp/#/settings" class="plan-btn plan-btn-muted">Перейти на Team</a>
      </div>
    </div>
  </section>

  <footer class="footer">
    <p>{{.AppName}} v{{.Version}} · <a href="/privacy">Privacy</a> · <a href="mailto:support@tgcontrol.app">Support</a></p>
  </footer>
</div>

</body>
</html>`
