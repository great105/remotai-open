package web

import (
	"net/http"

	"tgcontrol/internal/license"
)

// requestTransport отвечает на вопрос «человек сейчас дома или далеко».
//
// Всё, что идёт через облако, приходит в локальный веб-сервер с 127.0.0.1:
// мост открывает сам агент (см. internal/relay/stream.go), поэтому по адресу
// отправителя облачное подключение от домашнего не отличается никак. Различает
// их метка transport=relay, которую ставит агент при дозвоне.
//
// Подделать метку в свою пользу нельзя: параметры, пришедшие от клиента через
// релей, отбрасываются белым списком, а клиент в локальной сети, дописавший
// transport=relay руками, сделает хуже только себе — получит лимиты тарифа
// вместо безлимитного локального режима.
func requestTransport(r *http.Request) string {
	if r == nil {
		return license.TransportLAN
	}
	if r.URL != nil && r.URL.Query().Get("transport") == license.TransportRelay {
		return license.TransportRelay
	}
	return license.TransportLAN
}

// limitsFor возвращает лимиты для конкретного запроса с учётом транспорта.
// Локальные подключения не ограничиваются ни на одном тарифе.
func (s *Server) limitsFor(r *http.Request) license.TierLimits {
	if s.licenseManager == nil {
		return license.Limits[license.TierPro]
	}
	return s.licenseManager.LimitsForTransport(requestTransport(r))
}
