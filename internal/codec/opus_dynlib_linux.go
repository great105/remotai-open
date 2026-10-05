//go:build linux

package codec

// Opus-энкодер поверх системной libopus.so.0 (пакет libopus0) — звук
// виртуального браузера для WebRTC-аудиотрека. Загрузка та же, что у
// openh264 (dynlib_linux.go): purego, без cgo, библиотека опциональна —
// без неё звука просто нет, видео и управление работают как раньше.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"unsafe"
)

// OpusEncoder кодирует PCM s16le 48 кГц/стерео в Opus-пакеты по 20 мс.
// НЕ потокобезопасен (как и Encoder) — звать из одной горутины захвата.
type OpusEncoder interface {
	// Encode кодирует ровно один кадр: 960 сэмплов × 2 канала = 1920 int16
	// (interleaved L/R). Пакет возвращается свежим буфером: он уходит в
	// fan-out подписчикам и не должен делить память со следующим кадром.
	Encode(pcm []int16) ([]byte, error)
	Close()
}

const (
	opusSampleRate   = 48000
	opusChannels     = 2
	opusFrameSamples = 960 // 20 мс при 48 кГц — стандартный кадр WebRTC-звука

	opusAppAudio      = 2049 // OPUS_APPLICATION_AUDIO
	opusSetBitrateReq = 4002 // OPUS_SET_BITRATE_REQUEST
	opusBitrate       = 96000

	// Потолок пакета: реальный выход 96kbps×20мс — ~240 байт, запас велик,
	// зато libopus точно не упрётся в размер буфера (иначе OPUS_BUFFER_TOO_SMALL).
	opusMaxPacket = 4000
)

// opusLibNames — soname рантайм-пакета идёт первым: симлинк без номера
// живёт в -dev пакете, которого на сервере обычно нет.
var opusLibNames = []string{
	"libopus.so.0",
	"libopus.so",
}

// FindOpusLib ищет libopus в переданных каталогах и системных путях
// ("" — кодировать нечем, звук отключён). Пакет: sudo apt install libopus0.
func FindOpusLib(dirs ...string) string {
	search := append([]string{}, dirs...)
	if exe, err := os.Executable(); err == nil {
		search = append(search, filepath.Dir(exe))
	}
	search = append(search, systemLibDirs...)
	for _, d := range search {
		if d == "" {
			continue
		}
		for _, name := range opusLibNames {
			p := filepath.Join(d, name)
			if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
				return p
			}
		}
	}
	return ""
}

type opusEncoder struct {
	lib     *dynLib
	enc     uintptr // C OpusEncoder* — память libopus, Go её не трогает
	encode  uintptr
	destroy uintptr
}

// NewOpus создаёт энкодер 48 кГц/стерео/96 кбит. libPath — от FindOpusLib;
// ошибка означает «звука не будет», это штатный исход (пакет не установлен).
func NewOpus(libPath string) (OpusEncoder, error) {
	if libPath == "" {
		return nil, errors.New("libopus not available")
	}
	lib, err := openDynLib(libPath)
	if err != nil {
		return nil, err
	}
	create, err := lib.proc("opus_encoder_create")
	if err != nil {
		lib.close()
		return nil, err
	}
	ctl, err := lib.proc("opus_encoder_ctl")
	if err != nil {
		lib.close()
		return nil, err
	}
	encode, err := lib.proc("opus_encode")
	if err != nil {
		lib.close()
		return nil, err
	}
	destroy, err := lib.proc("opus_encoder_destroy")
	if err != nil {
		lib.close()
		return nil, err
	}

	e := &opusEncoder{lib: lib, encode: encode, destroy: destroy}
	var cerr int32
	e.enc = callAddr(create,
		uintptr(opusSampleRate), uintptr(opusChannels), uintptr(opusAppAudio),
		uintptr(unsafe.Pointer(&cerr)))
	if cerr != 0 || e.enc == 0 {
		lib.close()
		return nil, fmt.Errorf("opus_encoder_create rc=%d", cerr)
	}
	// Битрейт 96k: речь и музыка из браузера звучат прилично, а канал до
	// телефона может быть узким (облачный релей). Complexity не трогаем —
	// дефолтная и так единицы процентов ядра. opus_encoder_ctl вариадичен, но
	// наш аргумент — целое: оно едет в обычном регистре, худшее от мусорного
	// AL по SysV — безобидный сброс xmm-регистров в кадре самой libopus.
	callAddr(ctl, e.enc, uintptr(opusSetBitrateReq), uintptr(opusBitrate))
	return e, nil
}

func (e *opusEncoder) Encode(pcm []int16) ([]byte, error) {
	if len(pcm) != opusFrameSamples*opusChannels {
		return nil, fmt.Errorf("opus frame must be %d samples, got %d",
			opusFrameSamples*opusChannels, len(pcm))
	}
	out := make([]byte, opusMaxPacket)
	r := callAddr(e.encode,
		e.enc,
		uintptr(unsafe.Pointer(&pcm[0])),
		uintptr(opusFrameSamples),
		uintptr(unsafe.Pointer(&out[0])),
		uintptr(len(out)))
	n := int(int32(r))
	if n < 0 {
		return nil, fmt.Errorf("opus_encode rc=%d", n)
	}
	return out[:n], nil
}

func (e *opusEncoder) Close() {
	if e.enc != 0 {
		callAddr(e.destroy, e.enc)
		e.enc = 0
	}
	e.lib.close()
}
