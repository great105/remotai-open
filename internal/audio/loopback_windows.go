//go:build windows

package audio

import (
	"errors"
	"log"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"github.com/go-ole/go-ole"
	"github.com/moutend/go-wca/pkg/wca"
)

// Захват системного звука Windows через WASAPI loopback — чистым Go, без cgo
// (как и весь остальной наш нативный слой).
//
// Две вещи, которые узнаются только запуском:
//
//  1. При ПОЛНОЙ тишине loopback не отдаёт ни одного пакета: замер 23.08 дал
//     0 кадров за 1,5 с на молчащей машине. Поэтому рядом держится «поток
//     тишины» — обычный поток вывода, в который пишутся нули (флаг
//     AUDCLNT_BUFFERFLAGS_SILENT). Слышно ничего не будет, зато захват идёт
//     ровно: с ним те же 1,5 с дали 76 320 кадров.
//  2. Микшер отдаёт float32, а не s16 — формат берём у самой системы
//     (GetMixFormat) и приводим сами (convert.go).
//
// Никакой звук не захватывается, пока на него никто не подписан: насос
// стартует с первым слушателем и умирает с последним.

// packetsBuffered — сколько кусков держим на подписчика (20 мс каждый).
// Полсекунды: слушатель, который не успевает, теряет звук, а не тормозит
// захват для остальных.
const packetsBuffered = 25

var errNoAudio = errors.New("системный звук недоступен")

type hub struct {
	mu     sync.Mutex
	subs   map[chan []byte]struct{}
	stop   chan struct{}
	active bool
}

var hubInst = &hub{subs: map[chan []byte]struct{}{}}

// Available — можно ли вообще брать звук с этой машины.
func Available() bool { return true }

// Subscribe подключает слушателя к потоку кусков (PCM s16 моно 24 кГц, 20 мс).
// Возвращённая функция отписывает и идемпотентна.
func Subscribe() (<-chan []byte, func(), error) {
	ch := make(chan []byte, packetsBuffered)

	hubInst.mu.Lock()
	hubInst.subs[ch] = struct{}{}
	if !hubInst.active {
		hubInst.active = true
		hubInst.stop = make(chan struct{})
		go runPump(hubInst.stop)
	}
	hubInst.mu.Unlock()

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			hubInst.mu.Lock()
			delete(hubInst.subs, ch)
			last := len(hubInst.subs) == 0
			if last && hubInst.active {
				close(hubInst.stop)
				hubInst.active = false
			}
			hubInst.mu.Unlock()
			close(ch)
		})
	}, nil
}

// broadcast рассылает кусок слушателям. Тот, кто не успевает, теряет кусок:
// звук — поток реального времени, копить его бессмысленно.
func (h *hub) broadcast(pkt []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- pkt:
		default:
		}
	}
}

func runPump(stop <-chan struct{}) {
	// COM-объекты привязаны к потоку — держим его за собой.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if err := ole.CoInitializeEx(0, ole.COINIT_APARTMENTTHREADED); err != nil {
		log.Printf("[AUDIO] COM не поднялся: %v — звука не будет", err)
		return
	}
	defer ole.CoUninitialize()

	if err := capture(stop); err != nil {
		log.Printf("[AUDIO] захват остановлен: %v", err)
	}
}

func capture(stop <-chan struct{}) error {
	var enumerator *wca.IMMDeviceEnumerator
	if err := wca.CoCreateInstance(wca.CLSID_MMDeviceEnumerator, 0, wca.CLSCTX_ALL,
		wca.IID_IMMDeviceEnumerator, &enumerator); err != nil {
		return err
	}
	defer enumerator.Release()

	// eRender + eConsole — то, что сейчас звучит в колонках этой машины.
	var device *wca.IMMDevice
	if err := enumerator.GetDefaultAudioEndpoint(wca.ERender, wca.EConsole, &device); err != nil {
		return err
	}
	defer device.Release()

	var client *wca.IAudioClient
	if err := device.Activate(wca.IID_IAudioClient, wca.CLSCTX_ALL, nil, &client); err != nil {
		return err
	}
	defer client.Release()

	var format *wca.WAVEFORMATEX
	if err := client.GetMixFormat(&format); err != nil {
		return err
	}
	defer ole.CoTaskMemFree(uintptr(unsafe.Pointer(format)))

	channels := int(format.NChannels)
	rate := int(format.NSamplesPerSec)
	bits := int(format.WBitsPerSample)
	if bits != 32 {
		// Микшер Windows всегда float32; другой формат мы не приводим, чтобы
		// не выдумывать преобразование, которого никто не проверял.
		return errors.New("неожиданный формат микшера")
	}

	// Буфер на 1 секунду (в единицах по 100 нс).
	if err := client.Initialize(wca.AUDCLNT_SHAREMODE_SHARED,
		wca.AUDCLNT_STREAMFLAGS_LOOPBACK, 10_000_000, 0, format, nil); err != nil {
		return err
	}

	var capturer *wca.IAudioCaptureClient
	if err := client.GetService(wca.IID_IAudioCaptureClient, &capturer); err != nil {
		return err
	}
	defer capturer.Release()

	stopSilence := startSilence(device)
	defer stopSilence()

	if err := client.Start(); err != nil {
		return err
	}
	defer client.Stop()

	var pending []int16
	for {
		select {
		case <-stop:
			return nil
		default:
		}

		var avail uint32
		if err := capturer.GetNextPacketSize(&avail); err != nil {
			return err
		}
		if avail == 0 {
			time.Sleep(5 * time.Millisecond)
			continue
		}

		var data *byte
		var frames, flags uint32
		var pos, qpc uint64
		if err := capturer.GetBuffer(&data, &frames, &flags, &pos, &qpc); err != nil {
			return err
		}
		if frames > 0 && data != nil {
			// AUDCLNT_BUFFERFLAGS_SILENT = 2: система говорит «здесь тишина» и
			// содержимое буфера не определено — читать его нельзя.
			if flags&2 == 0 {
				samples := unsafe.Slice((*float32)(unsafe.Pointer(data)), int(frames)*channels)
				pending = DownmixResample(samples, channels, rate, pending)
			} else {
				pending = append(pending, make([]int16, int(frames)*SampleRate/rate)...)
			}
		}
		if err := capturer.ReleaseBuffer(frames); err != nil {
			return err
		}

		for len(pending) >= FrameSamples {
			frame := pending[:FrameSamples]
			if !IsSilent(frame) {
				hubInst.broadcast(PackPCM(frame))
			}
			pending = append(pending[:0], pending[FrameSamples:]...)
		}
	}
}

// startSilence держит на устройстве вывода беззвучный поток. Без него Windows
// не отдаёт loopback ни одного пакета на молчащей машине (проверено замером).
func startSilence(device *wca.IMMDevice) func() {
	var client *wca.IAudioClient
	if err := device.Activate(wca.IID_IAudioClient, wca.CLSCTX_ALL, nil, &client); err != nil {
		return func() {}
	}
	var format *wca.WAVEFORMATEX
	if err := client.GetMixFormat(&format); err != nil {
		client.Release()
		return func() {}
	}
	if err := client.Initialize(wca.AUDCLNT_SHAREMODE_SHARED, 0, 10_000_000, 0, format, nil); err != nil {
		ole.CoTaskMemFree(uintptr(unsafe.Pointer(format)))
		client.Release()
		return func() {}
	}
	var render *wca.IAudioRenderClient
	if err := client.GetService(wca.IID_IAudioRenderClient, &render); err != nil {
		ole.CoTaskMemFree(uintptr(unsafe.Pointer(format)))
		client.Release()
		return func() {}
	}
	var bufFrames uint32
	_ = client.GetBufferSize(&bufFrames)
	fillSilence(render, bufFrames)
	if err := client.Start(); err != nil {
		render.Release()
		ole.CoTaskMemFree(uintptr(unsafe.Pointer(format)))
		client.Release()
		return func() {}
	}

	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				var padding uint32
				if client.GetCurrentPadding(&padding) != nil {
					return
				}
				if free := bufFrames - padding; free > 0 {
					fillSilence(render, free)
				}
			}
		}
	}()

	return func() {
		close(done)
		_ = client.Stop()
		render.Release()
		ole.CoTaskMemFree(uintptr(unsafe.Pointer(format)))
		client.Release()
	}
}

func fillSilence(render *wca.IAudioRenderClient, frames uint32) {
	if frames == 0 {
		return
	}
	var data *byte
	if render.GetBuffer(frames, &data) != nil {
		return
	}
	// Флаг SILENT: систему просим самой заполнить буфер тишиной.
	_ = render.ReleaseBuffer(frames, 2)
}
