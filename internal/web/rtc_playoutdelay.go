package web

// playoutDelayInterceptor — sender-side RTP header extension
// «playout-delay» с min=max=0. Это самый дешёвый выигрыш задержки интерактивного
// стрима: приёмный WebRTC (jitter buffer) по умолчанию ПРИДЕРЖИВАЕТ кадры «для
// плавности» — у Multi замер дал −90 мс на p50 после обнуления, у Selkies то же
// значение штатно через это расширение. Браузерный клиент патчить не нужно:
// расширение читается стоковым libwebrtc.
// Ноль с клиентской стороны (jitterBufferTarget=0 в цикле) использовать нельзя —
// даёт статтер; правильный путь именно sender-side.
//
// Формат расширения (draft-ietf-avtext-rtp-hdrext-playout-delay): два uint16
// big-endian — min и max задержка показа в миллисекундах.

import (
	"github.com/pion/interceptor"
	"github.com/pion/rtp"
)

const playoutDelayURI = "http://www.webrtc.org/experiments/rtp-hdrext/playout-delay"

// playoutDelayFactory — фабрика интерсептора (интерфейс pion).
type playoutDelayFactory struct{}

func (playoutDelayFactory) NewInterceptor(_ string) (interceptor.Interceptor, error) {
	return &playoutDelayInterceptor{}, nil
}

type playoutDelayInterceptor struct {
	interceptor.NoOp
}

func (p *playoutDelayInterceptor) BindLocalStream(
	info *interceptor.StreamInfo,
	writer interceptor.RTPWriter,
) interceptor.RTPWriter {
	var extID uint8
	for _, e := range info.RTPHeaderExtensions {
		if e.URI == playoutDelayURI {
			extID = uint8(e.ID) //nolint:gosec // ID в SDP всегда 1..14
			break
		}
	}
	if extID == 0 {
		// Клиент не согласовал расширение — просто ничего не добавляем.
		return writer
	}
	zero := []byte{0, 0, 0, 0}
	return interceptor.RTPWriterFunc(
		func(header *rtp.Header, payload []byte, attributes interceptor.Attributes) (int, error) {
			if header != nil {
				_ = header.SetExtension(extID, zero)
			}
			return writer.Write(header, payload, attributes)
		},
	)
}
