package vbrowser

// Platform-neutral pieces of the virtual-browser audio pipeline, kept here so
// unit tests exercise them without PulseAudio/Opus (same discipline as
// supervisor.go). audio_linux.go wires these to parec + libopus; the capture
// parameters below are the contract between the two.

import (
	"sync"
	"time"
)

const (
	audioSampleRate   = 48000
	audioChannels     = 2
	audioFrameSamples = 960 // 20 ms @ 48 kHz — the standard WebRTC audio frame

	// audioFrameBytes is one 20 ms PCM frame as s16le interleaved stereo:
	// 960 samples × 2 channels × 2 bytes.
	audioFrameBytes = audioFrameSamples * audioChannels * 2

	// audioSubBufferSize buffers ~1s of packets per subscriber; a slower
	// viewer drops packets (audio late by a second is worse than a gap).
	audioSubBufferSize = 50
)

// pcmEncoder turns one 20 ms PCM frame (audioFrameSamples × audioChannels,
// s16 interleaved) into an Opus packet. codec.NewOpus satisfies it on Linux;
// tests inject a fake so no library/hardware is needed.
type pcmEncoder interface {
	Encode(pcm []int16) ([]byte, error)
	Close()
}

// frameChunker splits the parec stdout byte stream into exact audioFrameBytes
// frames. Reads arrive in arbitrary sizes; a short tail is carried over and
// glued to the next read. Emitted slices are borrowed: the consumer must
// finish with them before the next feed call (the capture loop encodes
// synchronously, so this holds by construction).
type frameChunker struct {
	tail [audioFrameBytes]byte
	n    int
}

func (c *frameChunker) feed(p []byte, emit func([]byte)) {
	for len(p) > 0 {
		if c.n == 0 && len(p) >= audioFrameBytes {
			emit(p[:audioFrameBytes])
			p = p[audioFrameBytes:]
			continue
		}
		m := copy(c.tail[c.n:], p)
		c.n += m
		p = p[m:]
		if c.n == audioFrameBytes {
			emit(c.tail[:])
			c.n = 0
		}
	}
}

// Restart budget for the capture process: deliberately smaller than the
// session supervisor's — a parec that cannot stay up three times in a row
// means the audio stack is broken, and a fresh subscriber retries anyway.
var (
	audioRestartMaxFails = 3
	audioStableAfter     = 5 * time.Minute
	audioBackoffBase     = time.Second
)

// audioRestartDecision spends one unit of the parec restart budget (same idea
// as restartDecision: a process that survived audioStableAfter resets the
// budget — an occasional death is not a crash loop).
func audioRestartDecision(fails int, stable bool) (newFails int, delay time.Duration, ok bool) {
	if stable {
		fails = 0
	}
	if fails >= audioRestartMaxFails {
		return fails, 0, false
	}
	return fails + 1, audioBackoffBase << uint(fails), true
}

// audioHub fans encoded Opus packets out to subscribers. Capture+encode (the
// pump) starts lazily with the first subscriber and stops when the last one
// leaves — a silent browser session must not burn CPU on encoding.
type audioHub struct {
	mu      sync.Mutex
	subs    map[chan []byte]struct{}
	pump    func(broadcast func([]byte), stop <-chan struct{}) // blocking runner
	running bool
	stopCh  chan struct{}
}

func newAudioHub(pump func(broadcast func([]byte), stop <-chan struct{})) *audioHub {
	return &audioHub{subs: map[chan []byte]struct{}{}, pump: pump}
}

// subscribe attaches a listener (buffered ~1s, drop-on-full) and returns the
// unsubscribe func. The first subscriber starts the pump.
func (h *audioHub) subscribe() (<-chan []byte, func()) {
	ch := make(chan []byte, audioSubBufferSize)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	if !h.running {
		h.running = true
		h.stopCh = make(chan struct{})
		stopCh := h.stopCh
		go func() {
			h.pump(h.broadcast, stopCh)
			// The pump quit on its own (restart budget spent): mark it dead so
			// the NEXT subscriber retries the capture instead of joining silence.
			h.mu.Lock()
			if h.stopCh == stopCh {
				h.running = false
			}
			h.mu.Unlock()
		}()
	}
	h.mu.Unlock()
	var once sync.Once
	unsub := func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subs, ch)
			if len(h.subs) == 0 && h.running {
				h.running = false
				close(h.stopCh) // the pump sees this and shuts the capture down
			}
			h.mu.Unlock()
		})
	}
	return ch, unsub
}

// broadcast hands one packet to every subscriber. Non-blocking: a full
// buffer means a slow viewer, and late audio is dropped rather than queued.
// The packet slice is shared — subscribers must treat it as read-only.
func (h *audioHub) broadcast(pkt []byte) {
	h.mu.Lock()
	for ch := range h.subs {
		select {
		case ch <- pkt:
		default:
		}
	}
	h.mu.Unlock()
}

// active reports whether anyone is listening (cheap, for Status/diagnostics).
func (h *audioHub) active() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs) > 0
}

// stopAll drops every subscriber and stops the pump (session Stop). The null
// sink itself stays loaded — it is cheap and the next Start reuses it.
func (h *audioHub) stopAll() {
	h.mu.Lock()
	h.subs = map[chan []byte]struct{}{}
	if h.running {
		h.running = false
		close(h.stopCh)
	}
	h.mu.Unlock()
}
