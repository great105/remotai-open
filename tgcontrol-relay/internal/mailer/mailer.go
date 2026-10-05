// Package mailer — отправка писем через SMTP (коды входа и прочие
// транзакционные письма). Работает с любым SMTP: Яндекс 360, Mail.ru для
// бизнеса, Timeweb, собственный postfix (RU-инстанс) или SES/Resend через
// SMTP-мост (global-инстанс). Отдельный SDK ни к чему провайдеру не привязан.
package mailer

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/smtp"
	"strings"
	"time"
)

type Config struct {
	Host string // SMTP_HOST
	Port int    // SMTP_PORT: 465 = implicit TLS, 587 = STARTTLS
	User string // SMTP_USER
	Pass string // SMTP_PASS
	From string // SMTP_FROM: "Remotai <login@remotai.ru>"
}

func (c Config) Enabled() bool { return c.Host != "" && c.From != "" }

type Mailer struct{ cfg Config }

func New(cfg Config) *Mailer { return &Mailer{cfg: cfg} }

// Send шлёт простое текстовое письмо. Таймаут жёсткий: SMTP-затык не должен
// вешать HTTP-хендлер.
func (m *Mailer) Send(to, subject, body string) error {
	if !m.cfg.Enabled() {
		return fmt.Errorf("smtp not configured")
	}
	addr := net.JoinHostPort(m.cfg.Host, fmt.Sprint(m.cfg.Port))
	d := &net.Dialer{Timeout: 10 * time.Second}

	var c *smtp.Client
	var err error
	if m.cfg.Port == 465 {
		conn, derr := tls.DialWithDialer(d, "tcp", addr, &tls.Config{ServerName: m.cfg.Host})
		if derr != nil {
			return fmt.Errorf("smtp tls dial: %w", derr)
		}
		c, err = smtp.NewClient(conn, m.cfg.Host)
	} else {
		var conn net.Conn
		conn, err = d.Dial("tcp", addr)
		if err == nil {
			c, err = smtp.NewClient(conn, m.cfg.Host)
		}
	}
	if err != nil {
		return fmt.Errorf("smtp connect: %w", err)
	}
	defer c.Close()

	if m.cfg.Port == 587 {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(&tls.Config{ServerName: m.cfg.Host}); err != nil {
				return fmt.Errorf("smtp starttls: %w", err)
			}
		}
	}
	if m.cfg.User != "" {
		if err := c.Auth(smtp.PlainAuth("", m.cfg.User, m.cfg.Pass, m.cfg.Host)); err != nil {
			return fmt.Errorf("smtp auth: %w", err)
		}
	}
	from := m.cfg.From
	if i := strings.Index(from, "<"); i >= 0 {
		from = strings.TrimSuffix(strings.TrimPrefix(from[strings.Index(from, "<"):], "<"), ">")
	}
	if err := c.Mail(from); err != nil {
		return fmt.Errorf("smtp MAIL FROM: %w", err)
	}
	if err := c.Rcpt(to); err != nil {
		return fmt.Errorf("smtp RCPT TO: %w", err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("smtp DATA: %w", err)
	}
	msg := "From: " + m.cfg.From + "\r\n" +
		"To: " + to + "\r\n" +
		"Subject: " + subject + "\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" + body
	if _, err := w.Write([]byte(msg)); err != nil {
		return fmt.Errorf("smtp write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp data close: %w", err)
	}
	return c.Quit()
}
