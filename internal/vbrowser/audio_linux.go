//go:build linux

package vbrowser

// Virtual-browser audio on Linux: the browser's sound is routed into a
// PulseAudio null sink (PULSE_SINK), captured from the sink's monitor with
// parec, encoded to Opus (libopus via internal/codec) and fanned out to
// WebRTC sessions as 20 ms packets. Everything here is best-effort: without
// pactl/parec/libopus the session simply stays silent, video is unaffected.

import (
	"context"
	"encoding/binary"
	"errors"
	"log"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"tgcontrol/internal/codec"
)

// audioSinkName is the null sink the browser is routed into (PULSE_SINK);
// its .monitor source is what parec records.
const audioSinkName = "remotai_vbrowser"

// Системный PulseAudio под root слушает здесь. Обычный клиент выбирает сокет
// по UID и потому ищет /run/user/0 или /run/user/<browser>, где ничего нет.
const systemPulseServer = "unix:/var/run/pulse/native"

var (
	audioSinkReady  atomic.Bool // set by SetupAudio, never reset (module stays)
	audioSystemMode atomic.Bool // root fallback uses /var/run/pulse/native
	audioHubInst    = newAudioHub(runAudioPump)
)

var errNoAudio = errors.New("vbrowser audio is not available")

// SetupAudio prepares the PulseAudio side of a session: makes sure a
// pulse/pipewire daemon answers pactl (starting one best-effort when absent)
// and loads our null sink. Returns true when the sink is ready — the caller
// then adds PULSE_SINK to the browser environment so libpulse routes its
// output here. Called from Start after Xvfb is up, before the browser launch.
func SetupAudio() bool {
	if _, err := exec.LookPath("pactl"); err != nil {
		log.Printf("[VBROWSER] audio: pactl not found — звука не будет (sudo apt install pulseaudio-utils libopus0)")
		return false
	}
	if !pactlOK() {
		// No daemon answers: try to start one. A root-run service cannot use
		// the per-user mode, so it gets system mode — a fallback that distros
		// frown upon, hence the loud log line (PipeWire with pipewire-pulse
		// answers pactl too and needs none of this).
		if os.Geteuid() == 0 {
			log.Printf("[VBROWSER] audio: демон pulse не отвечает — fallback: pulseaudio --system (агент под root)")
			_ = exec.Command("pulseaudio", "--system=true", "-D").Run()
			audioSystemMode.Store(true)
		} else {
			_ = exec.Command("pulseaudio", "--start").Run()
		}
	}
	if !pactlOK() {
		log.Printf("[VBROWSER] audio: pulse/pipewire не отвечает — звука не будет")
		return false
	}
	out, err := pactlOutput("list", "short", "sinks")
	if err != nil || !strings.Contains(out, audioSinkName) {
		if _, err := pactlOutput("load-module", "module-null-sink",
			"sink_name="+audioSinkName,
			"sink_properties=device.description=RemotaiVBrowser"); err != nil {
			log.Printf("[VBROWSER] audio: null-sink не поднялся: %v — звука не будет", err)
			return false
		}
	}
	audioSinkReady.Store(true)
	log.Printf("[VBROWSER] audio: sink %s готов", audioSinkName)
	return true
}

// pulseAccessGroup возвращает системную группу, которой PulseAudio разрешает
// подключение к system-mode сокету. Добавлять пользователей в /etc/group не
// нужно: root задаёт её основной группой только дочернему pactl/parec или
// Chromium, не расширяя права всего агента.
func pulseAccessGroup() (uint32, bool) {
	group, err := user.LookupGroup("pulse-access")
	if err != nil {
		return 0, false
	}
	gid, err := strconv.ParseUint(group.Gid, 10, 32)
	if err != nil {
		return 0, false
	}
	return uint32(gid), true
}

// configurePulseClient направляет root-клиент к system-mode PulseAudio и даёт
// ему ровно pulse-access. Без обоих пунктов pactl/parec отвечают Access denied,
// хотя демон успешно запущен.
func configurePulseClient(cmd *exec.Cmd) {
	if os.Geteuid() != 0 || !audioSystemMode.Load() {
		return
	}
	cmd.Env = append(os.Environ(), "PULSE_SERVER="+systemPulseServer)
	if gid, ok := pulseAccessGroup(); ok {
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{
			Uid: 0, Gid: gid, Groups: []uint32{0},
		}}
	}
}

func pulseCommandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	configurePulseClient(cmd)
	return cmd
}

// pactlOK reports whether a pulse/pipewire daemon answers a cheap query.
func pactlOK() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return pulseCommandContext(ctx, "pactl", "info").Run() == nil
}

func pactlOutput(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := pulseCommandContext(ctx, "pactl", args...).Output()
	return string(out), err
}

// AudioAvailable reports whether browser audio can actually be captured and
// encoded right now: the null sink is up AND libopus is installed.
func AudioAvailable() bool {
	return audioSinkReady.Load() && codec.FindOpusLib() != ""
}

// AudioActive reports whether at least one viewer is receiving audio (the
// capture+encode pump is running). Cheap, for diagnostics.
func AudioActive() bool {
	return audioHubInst.active()
}

// SubscribeAudio attaches the caller to the Opus packet fan-out (20 ms
// packets, buffered ~1s, drop-on-full). Capture starts lazily with the first
// subscriber and stops with the last one leaving. The returned func detaches
// and is idempotent.
func SubscribeAudio() (<-chan []byte, func(), error) {
	if !AudioAvailable() {
		return nil, nil, errNoAudio
	}
	ch, unsub := audioHubInst.subscribe()
	return ch, unsub, nil
}

// stopAudioAll detaches every subscriber and kills parec (session Stop). The
// sink module stays loaded — it is cheap and the next Start reuses it.
func stopAudioAll() {
	audioHubInst.stopAll()
}

// runAudioPump is the capture→encode→broadcast loop of the hub. A dying
// parec is restarted within a small budget; when the budget is spent the
// pump quits (the hub then re-runs it for the next subscriber).
func runAudioPump(broadcast func([]byte), stop <-chan struct{}) {
	enc, err := codec.NewOpus(codec.FindOpusLib())
	if err != nil {
		log.Printf("[VBROWSER] audio: opus encoder init failed: %v", err)
		<-stop // keep the hub state consistent until the last viewer leaves
		return
	}
	defer enc.Close()

	fails := 0
	for {
		upAt := time.Now()
		cerr := captureAudio(stop, enc, broadcast)
		select {
		case <-stop:
			return
		default:
		}
		f, delay, ok := audioRestartDecision(fails, time.Since(upAt) >= audioStableAfter)
		fails = f
		if !ok {
			log.Printf("[VBROWSER] audio: parec умирает (%v) — бюджет перезапусков исчерпан", cerr)
			return
		}
		log.Printf("[VBROWSER] audio: parec умер (%v) — перезапуск %d/%d через %s", cerr, fails, audioRestartMaxFails, delay)
		select {
		case <-stop:
			return
		case <-time.After(delay):
		}
	}
}

// captureAudio runs one parec generation and pumps its stdout through the
// chunker + encoder until the process dies or stop closes.
func captureAudio(stop <-chan struct{}, enc pcmEncoder, broadcast func([]byte)) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-stop:
			cancel() // CommandContext kills parec for us
		case <-ctx.Done():
		}
	}()
	cmd := pulseCommandContext(ctx, "parec",
		"--format=s16le", "--rate=48000", "--channels=2",
		"--device="+audioSinkName+".monitor", "--latency-msec=20")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	defer func() {
		cancel()
		_ = cmd.Wait()
	}()

	var chunk frameChunker
	pcm := make([]int16, audioFrameSamples*audioChannels)
	buf := make([]byte, audioFrameBytes*2)
	encErrs := 0
	for {
		n, rerr := stdout.Read(buf)
		if n > 0 {
			chunk.feed(buf[:n], func(frame []byte) {
				for i := range pcm {
					pcm[i] = int16(binary.LittleEndian.Uint16(frame[i*2:]))
				}
				pkt, eerr := enc.Encode(pcm)
				if eerr != nil {
					encErrs++
					if encErrs == 1 || encErrs%50 == 0 {
						log.Printf("[VBROWSER] audio: opus encode (×%d): %v", encErrs, eerr)
					}
					return
				}
				if len(pkt) > 0 {
					broadcast(pkt)
				}
			})
		}
		if rerr != nil {
			return rerr
		}
	}
}
