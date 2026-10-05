package web

import (
	"encoding/binary"
	"image"

	"tgcontrol/internal/codec"
	"tgcontrol/internal/paths"
)

// H.264 по обычному веб-сокету — для зрителей, которым не досталось WebRTC.
//
// Замер 23.08 объяснил, почему это нужно: ноутбук владельца (Ubuntu,
// WebKitGTK) не поднял WebRTC вовсе и работал по JPEG-пути. WebRTC требует
// ICE, TURN и поддержки H.264 именно в стеке WebRTC — там, где чего-то из
// этого нет, видеокодек был недоступен в принципе, хотя ДЕКОДЕР в браузере
// есть: его отдаёт WebCodecs (VideoDecoder).
//
// Поэтому тот же openh264, что кормит WebRTC-трек, умеет отдавать поток и
// в веб-сокет: клиент объявляет `{t:"client", h264:true}` (только если
// VideoDecoder действительно поддерживает avc1), получает Annex-B кадры и
// декодирует их сам. Не смог — остаются частичные JPEG-кадры, они никуда
// не делись.
//
// Кадр на проводе:
//
//	[seq:4 BE][0x02][flags:1][annex-b…]   flags bit0 = ключевой кадр
//
// Отличается от JPEG (0xFF) и от частичного кадра (0x01) тем же первым байтом
// после номера.

const (
	// frameKindH264 — маркер видеокадра.
	frameKindH264 = 0x02
	// videoFlagKeyframe — бит «это ключевой кадр»: с него декодер начинает.
	videoFlagKeyframe = 0x01
	// frameKindAudio — маркер куска системного звука (PCM s16 моно 24 кГц).
	frameKindAudio = 0x03
)

// wsVideoEncoder — кодер H.264 для одного зрителя.
type wsVideoEncoder struct {
	dll     string
	enc     codec.Encoder
	w, h    int
	fps     int
	bitrate int
	// Первый кадр после (пере)создания обязан быть ключевым: декодер клиента
	// без него не стартует.
	needKey bool
}

func newWSVideoEncoder() *wsVideoEncoder {
	return &wsVideoEncoder{dll: codec.FindOpenH264DLL(paths.Base())}
}

// available — есть ли чем кодировать. Без библиотеки путь просто не предлагаем.
func (e *wsVideoEncoder) available() bool { return e.dll != "" }

func (e *wsVideoEncoder) close() {
	if e.enc != nil {
		e.enc.Close()
		e.enc = nil
	}
}

// encode кодирует кадр. reconfigured=true означает, что размер кадра сменился
// и клиенту нужно пересобрать декодер (кадр при этом ключевой).
func (e *wsVideoEncoder) encode(img *image.RGBA, targetWidth, fps int, profile string, forceKey bool) (
	nal []byte, key, reconfigured bool, err error,
) {
	src := img.Bounds()
	encW, encH := codec.EncodeDims(src.Dx(), src.Dy(), targetWidth)
	bitrate := h264Bitrate(profile, encW, encH, fps)

	if e.enc == nil || encW != e.w || encH != e.h || fps != e.fps {
		e.close()
		enc, err := codec.NewH264(e.dll, encW, encH, fps, bitrate)
		if err != nil {
			return nil, false, false, err
		}
		e.enc, e.w, e.h, e.fps, e.bitrate = enc, encW, encH, fps, bitrate
		e.needKey = true
		reconfigured = true
	} else if bitrate != e.bitrate {
		// Битрейт умеют менять на лету — пересоздавать кодек ради этого нельзя:
		// каждый пересоздан — это IDR и видимый рывок картинки.
		if tuner, ok := e.enc.(codec.LiveTuner); ok {
			if err := tuner.SetBitrate(bitrate); err == nil {
				e.bitrate = bitrate
			}
		}
	}

	nal, key, err = e.enc.EncodeRGBA(img, forceKey || e.needKey)
	if err != nil {
		return nil, false, reconfigured, err
	}
	if key {
		e.needKey = false
	}
	return nal, key, reconfigured, nil
}

// packVideoFrame собирает видеокадр для провода.
func packVideoFrame(seq uint32, nal []byte, key bool) []byte {
	out := make([]byte, 0, 6+len(nal))
	var num [4]byte
	binary.BigEndian.PutUint32(num[:], seq)
	out = append(out, num[:]...)
	var flags byte
	if key {
		flags |= videoFlagKeyframe
	}
	out = append(out, frameKindH264, flags)
	return append(out, nal...)
}
