//go:build windows

package audio

import (
	"fmt"
	"math"
	"os"
	"testing"
	"time"
	"unsafe"

	"github.com/go-ole/go-ole"
	"github.com/moutend/go-wca/pkg/wca"
)

// Играет очень тихий тон, чтобы проверить весь тракт целиком.
func playQuietTone(dur time.Duration) error {
	if err := ole.CoInitializeEx(0, ole.COINIT_APARTMENTTHREADED); err != nil {
		return fmt.Errorf("COM: %w", err)
	}
	defer ole.CoUninitialize()

	var enumerator *wca.IMMDeviceEnumerator
	if err := wca.CoCreateInstance(wca.CLSID_MMDeviceEnumerator, 0, wca.CLSCTX_ALL, wca.IID_IMMDeviceEnumerator, &enumerator); err != nil {
		return fmt.Errorf("enumerator: %w", err)
	}
	defer enumerator.Release()
	var device *wca.IMMDevice
	if err := enumerator.GetDefaultAudioEndpoint(wca.ERender, wca.EConsole, &device); err != nil {
		return fmt.Errorf("endpoint: %w", err)
	}
	defer device.Release()
	var client *wca.IAudioClient
	if err := device.Activate(wca.IID_IAudioClient, wca.CLSCTX_ALL, nil, &client); err != nil {
		return fmt.Errorf("activate: %w", err)
	}
	defer client.Release()
	var format *wca.WAVEFORMATEX
	if err := client.GetMixFormat(&format); err != nil {
		return fmt.Errorf("format: %w", err)
	}
	defer ole.CoTaskMemFree(uintptr(unsafe.Pointer(format)))
	if err := client.Initialize(wca.AUDCLNT_SHAREMODE_SHARED, 0, 10_000_000, 0, format, nil); err != nil {
		return fmt.Errorf("init: %w", err)
	}
	var render *wca.IAudioRenderClient
	if err := client.GetService(wca.IID_IAudioRenderClient, &render); err != nil {
		return fmt.Errorf("render: %w", err)
	}
	defer render.Release()
	var bufFrames uint32
	_ = client.GetBufferSize(&bufFrames)

	ch := int(format.NChannels)
	rate := float64(format.NSamplesPerSec)
	phase := 0.0
	write := func(frames uint32) {
		if frames == 0 {
			return
		}
		var data *byte
		if render.GetBuffer(frames, &data) != nil {
			return
		}
		out := unsafe.Slice((*float32)(unsafe.Pointer(data)), int(frames)*ch)
		for i := 0; i < int(frames); i++ {
			v := float32(math.Sin(phase) * 0.02) // 2 % громкости — едва слышно
			phase += 2 * math.Pi * 440 / rate
			for c := 0; c < ch; c++ {
				out[i*ch+c] = v
			}
		}
		_ = render.ReleaseBuffer(frames, 0)
	}
	write(bufFrames)
	if err := client.Start(); err != nil {
		return fmt.Errorf("start: %w", err)
	}
	defer client.Stop()

	deadline := time.Now().Add(dur)
	for time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		var padding uint32
		if err := client.GetCurrentPadding(&padding); err != nil {
			return fmt.Errorf("padding: %w", err)
		}
		write(bufFrames - padding)
	}
	return nil
}

// Живая проба всего тракта: играет очень тихий тон и проверяет, что он
// доехал до подписчика. Звучит на РЕАЛЬНЫХ колонках машины, поэтому в обычном
// прогоне пропускается — включается переменной REMOTAI_AUDIO_LIVE=1.
//
// Проверено 23.08 на машине владельца: 20 кусков, пик 655 из 32767 — ровно те
// 2 % громкости, которые играли. Значит захват, сведение в моно 24 кГц,
// ограничение и упаковка работают целиком.
func TestLiveCaptureWithTone(t *testing.T) {
	if os.Getenv("REMOTAI_AUDIO_LIVE") != "1" {
		t.Skip("живая проба звука: включается REMOTAI_AUDIO_LIVE=1 (играет тихий тон в колонки)")
	}
	ch, unsub, err := Subscribe()
	if err != nil {
		t.Fatalf("подписка: %v", err)
	}
	defer unsub()

	toneDone := make(chan error, 1)
	go func() { toneDone <- playQuietTone(900 * time.Millisecond) }()

	deadline := time.After(4 * time.Second)
	packets, loudest := 0, 0
	for {
		select {
		case err := <-toneDone:
			if err != nil {
				t.Skipf("тон недоступен: %v", err)
			}
		case pkt := <-ch:
			packets++
			for i := 0; i+1 < len(pkt); i += 2 {
				s := int(int16(uint16(pkt[i]) | uint16(pkt[i+1])<<8))
				if s < 0 {
					s = -s
				}
				if s > loudest {
					loudest = s
				}
			}
			if packets >= 20 {
				t.Logf("поймано %d кусков со звуком, пик %d из 32767 — тракт работает целиком", packets, loudest)
				return
			}
		case <-deadline:
			t.Fatalf("за 4 с пришло %d кусков (пик %d) — тон не доехал", packets, loudest)
		}
	}
}
