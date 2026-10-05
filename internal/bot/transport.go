package bot

import (
	"net"
	"net/http"
	"net/url"
	"time"
)

// newTelegramHTTPClient builds the HTTP client used for the Telegram Bot API.
//
// Two problems it solves vs the library default:
//  1. The default client has no proxy and only a coarse 60s whole-request
//     timeout, so when the ISP throttles the Telegram IP every getUpdates burns
//     ~21s on a TCP connect timeout and the bot goes silent. We add a bounded
//     dial/TLS timeout and an optional proxy (net/http dials socks5:// and
//     http(s):// proxy URLs natively — no extra dependency).
//  2. Timeout must exceed the long-poll window, so it's pollTimeout + margin.
func newTelegramHTTPClient(proxyURL string, pollTimeout time.Duration) *http.Client {
	tr := &http.Transport{
		// Default: honor HTTPS_PROXY/ALL_PROXY env. Overridden below if config set.
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          10,
		IdleConnTimeout:       90 * time.Second,
	}
	if proxyURL != "" {
		if u, err := url.Parse(proxyURL); err == nil && u.Scheme != "" {
			tr.Proxy = http.ProxyURL(u)
		}
	}
	return &http.Client{
		Transport: tr,
		// Allow the long-poll to complete (pollTimeout) plus a margin, but still
		// bound a wedged request instead of hanging until the OS TCP timeout.
		Timeout: pollTimeout + 15*time.Second,
	}
}
