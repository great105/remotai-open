package web

import (
	"net/http/httptest"
	"testing"

	"tgcontrol/internal/license"
)

func TestRequestTransport(t *testing.T) {
	cases := []struct {
		name string
		url  string
		want string
	}{
		{"без метки — дом", "/ws/screen", license.TransportLAN},
		{"метка релея", "/ws/screen?transport=relay", license.TransportRelay},
		{"чужое значение — дом", "/ws/screen?transport=wat", license.TransportLAN},
		{"пустое значение — дом", "/ws/screen?transport=", license.TransportLAN},
		{"метка среди прочих", "/ws/screen?resume=1:2&transport=relay", license.TransportRelay},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", c.url, nil)
			if got := requestTransport(r); got != c.want {
				t.Errorf("requestTransport(%q) = %q, want %q", c.url, got, c.want)
			}
		})
	}
}

// nil-запрос не должен ронять сервер и обязан трактоваться как локальный:
// ошибиться в сторону «дома без ограничений» безопаснее, чем резать домашних.
func TestRequestTransportNil(t *testing.T) {
	if got := requestTransport(nil); got != license.TransportLAN {
		t.Errorf("requestTransport(nil) = %q, want %q", got, license.TransportLAN)
	}
}

// Без менеджера лицензий сервер не должен внезапно резать качество.
func TestLimitsForWithoutManager(t *testing.T) {
	s := &Server{}
	got := s.limitsFor(httptest.NewRequest("GET", "/ws/screen?transport=relay", nil))
	if got.RemoteDesktopMaxFPS != license.Limits[license.TierPro].RemoteDesktopMaxFPS {
		t.Errorf("без менеджера ожидались Pro-лимиты, получили fps=%d", got.RemoteDesktopMaxFPS)
	}
}
