package web

// WebRTC transport for Remote Desktop (Milestone 1).
//
// This is the same screen-stream + input protocol as /ws/screen, but carried
// over a WebRTC PeerConnection instead of a WebSocket:
//
//   - "frames"  DataChannel (unreliable, maxRetransmits:0, created by client):
//                agent → client binary frames [seq:4 BE][jpeg], like the WS path.
//   - "ctrl"    DataChannel (reliable, ordered, created by client):
//                the existing JSON control protocol BOTH ways — client sends
//                input/ack/profile/client/ping/display/clip_*, agent sends
//                info/stats/clip/pong.
//
// Why: WebRTC media+data ride UDP/DTLS/SCTP with ICE (host on LAN, TURN-relayed
// over the cloud), eliminating the TCP head-of-line blocking of the WS tunnel on
// lossy/high-latency links. Signaling is non-trickle: the client gathers ICE,
// POSTs a complete offer to /api/webrtc/offer, the agent answers with a complete
// SDP in the HTTP response — so it works over BOTH the LAN path and the relay
// request-tunnel with ZERO relay changes.
//
// The H.264 media track (Milestone 2) is added to this same PeerConnection; the
// "frames" DataChannel remains the universal fallback where HW H.264 decode is
// unavailable (older iOS WKWebView / flaky Telegram WebView).

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"image"
	"io"
	"log"
	"math"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kbinani/screenshot"
	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"tgcontrol/internal/codec"
	"tgcontrol/internal/connstat"
	"tgcontrol/internal/input"
	"tgcontrol/internal/observability"
	"tgcontrol/internal/paths"
	"tgcontrol/internal/service"
	"tgcontrol/internal/vbrowser"
)

const (
	rtcMaxSessions    = 6         // soft cap on concurrent WebRTC remote-desktop sessions
	rtcMaxFrameBytes  = 240 << 10 // skip frames bigger than the browser DataChannel max-message-size (~256KB)
	rtcFramesBufLimit = 1 << 20   // backpressure: drop frames if the send buffer exceeds 1MB
	rtcGatherTimeout  = 5 * time.Second
	rtcOfferMaxBody   = 256 << 10

	// Frame-ACK backpressure (RustDesk model): with a vack-capable client never
	// run more than this many frames ahead of what it has presented — bounds
	// the invisible queue in the pacer/TURN when bandwidth dips below bitrate
	// (~inflight/fps of extra latency instead of unbounded drift into the past).
	rtcMaxInflight = 5
	// If the ack stream stalls this long the counter rebases instead of
	// throttling forever (acks ride the reliable ctrl channel, but a decoder
	// stall on the client must not freeze the producer).
	rtcVackStall = 500 * time.Millisecond
	// On a static screen frames are skipped entirely; a keep-alive frame still
	// goes out this often so the receiver track never mutes and a lost
	// PLI-recovery can't stall forever.
	rtcStaticKeepAlive = time.Second
	// Paint-over (модель Selkies): на неподвижном экране раз в это время
	// кодируем ОДИН кадр с сильно поднятым битрейтом и IDR — 4:2:0 мылит
	// мелкий текст, и пока человек читает статичную страницу, этот проход
	// делает его идеально чётким. Цена — один кадр, статика дешёвая.
	rtcPaintOverInterval = 5 * time.Second
	// Honour at most one PLI-forced IDR per this window — an IDR burst on a
	// lossy link tends to get lost itself, triggering a PLI→IDR→loss cascade.
	rtcPLIDebounce = 500 * time.Millisecond
)

// rtcReg keeps live sessions referenced (so pion's goroutines + this map prevent
// GC) and bounds concurrency. Single Server per process, so a package-level
// registry is fine.
var (
	rtcMu  sync.Mutex
	rtcReg = map[*rtcSession]struct{}{}
)

func rtcRegister(s *rtcSession) bool {
	rtcMu.Lock()
	defer rtcMu.Unlock()
	if len(rtcReg) >= rtcMaxSessions {
		return false
	}
	rtcReg[s] = struct{}{}
	return true
}

func rtcUnregister(s *rtcSession) {
	rtcMu.Lock()
	delete(rtcReg, s)
	rtcMu.Unlock()
}

// h264FmtpLine is the single H.264 variant we negotiate: Constrained Baseline
// 3.1, packetization-mode=1 — matches openh264's Baseline output and is decodable
// by every browser/WebView that does H.264 at all.
const h264FmtpLine = "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42001f"

// opusFmtpLine is the single Opus variant we negotiate: звук виртуального
// браузера (20 мс кадры, inband FEC против потерь на UDP-пути).
const opusFmtpLine = "minptime=10;useinbandfec=1;stereo=0"

// rtcEngineAPI builds (once) a pion API whose MediaEngine offers ONLY our H.264
// codec. Registering a single video codec removes the codec-matching ambiguity of
// the default engine (the browser offers VP8/VP9/AV1/H265/many H.264 profiles) so
// our sendonly track binds unambiguously and the client can actually assemble
// frames. Default interceptors add NACK/PLI + TWCC congestion feedback.
var (
	rtcAPIOnce sync.Once
	rtcAPIInst *webrtc.API
	rtcAPIErr  error
)

func rtcEngineAPI() (*webrtc.API, error) {
	rtcAPIOnce.Do(func() {
		m := &webrtc.MediaEngine{}
		fb := []webrtc.RTCPFeedback{
			{Type: "goog-remb"},
			{Type: "ccm", Parameter: "fir"},
			{Type: "nack"},
			{Type: "nack", Parameter: "pli"},
			{Type: webrtc.TypeRTCPFBTransportCC},
		}
		if err := m.RegisterCodec(webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType:     webrtc.MimeTypeH264,
				ClockRate:    90000,
				SDPFmtpLine:  h264FmtpLine,
				RTCPFeedback: fb,
			},
			PayloadType: 102,
		}, webrtc.RTPCodecTypeVideo); err != nil {
			rtcAPIErr = err
			return
		}
		// Opus — звук виртуального браузера. Клиент просит m=audio recvonly;
		// трек добавляется только когда vbrowser жив и sink+libopus на месте,
		// иначе audio m-line в ответе просто отклоняется (клиент без звука,
		// как раньше). OnTrack у нас sendonly — удалённые треки не принимаем.
		if err := m.RegisterCodec(webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType:    webrtc.MimeTypeOpus,
				ClockRate:   48000,
				Channels:    2,
				SDPFmtpLine: opusFmtpLine,
			},
			PayloadType: 111,
		}, webrtc.RTPCodecTypeAudio); err != nil {
			rtcAPIErr = err
			return
		}
		// playout-delay=0: просим приёмник не придерживать кадры ради плавности
		// (sender-side, читается стоковым libwebrtc; дешёвые −60..90 мс).
		if err := m.RegisterHeaderExtension(webrtc.RTPHeaderExtensionCapability{
			URI: playoutDelayURI,
		}, webrtc.RTPCodecTypeVideo); err != nil {
			rtcAPIErr = err
			return
		}
		ir := &interceptor.Registry{}
		ir.Add(playoutDelayFactory{})
		if err := webrtc.RegisterDefaultInterceptors(m, ir); err != nil {
			rtcAPIErr = err
			return
		}
		rtcAPIInst = webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(ir))
	})
	return rtcAPIInst, rtcAPIErr
}

// ── Wire types (mirror the browser RTCSessionDescription / RTCIceServer) ──

type rtcICEServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

type rtcOfferRequest struct {
	Type       string         `json:"type"` // "offer"
	SDP        string         `json:"sdp"`
	ICEServers []rtcICEServer `json:"iceServers,omitempty"`
}

type rtcAnswerResponse struct {
	Type string `json:"type"` // "answer"
	SDP  string `json:"sdp"`
}

// rtcSession holds the per-PeerConnection state — the WebRTC analogue of the
// locals in wsScreenHandler.
type rtcSession struct {
	pc      *webrtc.PeerConnection
	adapt   *adaptiveCtrl
	ctrl    input.Controller
	applier *inputApplier

	framesDC atomic.Pointer[webrtc.DataChannel]
	ctrlDC   atomic.Pointer[webrtc.DataChannel]

	// H.264 video track (Milestone 2). When videoMode is set the screen is sent
	// as an H.264 RTP track instead of JPEG over the frames DataChannel.
	videoTrack   *webrtc.TrackLocalStaticSample
	videoMode    atomic.Bool
	videoStarted atomic.Bool
	jpegStarted  atomic.Bool
	forceKey     atomic.Bool
	encDLL       string
	paused       atomic.Bool

	// Opus audio track — звук виртуального браузера (Linux). Трек есть только
	// когда клиент предложил m=audio и vbrowser отдал звук; audioUnsub —
	// отписка от fan-out, зовётся из close под mu (баланс с negotiate).
	audioTrack *webrtc.TrackLocalStaticSample
	audioUnsub func()

	// Frame-ACK backpressure state ({t:"vack"} from the client, see produceH264).
	vackOn   atomic.Bool
	sentN    atomic.Int64
	ackedN   atomic.Int64
	lastVack atomic.Int64 // unixnano of the last vack

	qos rtcQoS // RTCP-loss-driven fps ladder

	mu      sync.Mutex
	bounds  image.Rectangle
	screenW int
	screenH int

	numDisplays int
	displays    []map[string]any

	// Latest server/client stats for /api/diag/connections (loopback self-diag).
	diagMu     sync.Mutex
	lastSrv    map[string]any
	lastClient map[string]any

	cs atomic.Pointer[connstat.Conn] // connstat registration (methods are nil-safe)

	clipCh    chan func()
	done      chan struct{}
	closeOnce sync.Once
	uid       int64
}

// apiWebRTCOffer — POST /api/webrtc/offer  (authWrap'd: LAN X-API-Token, cloud
// relay-tunnel, Telegram initData). Accepts the client's offer + ICE servers,
// returns the agent's answer. Reachable over BOTH LAN and the relay request
// tunnel with no relay changes.
func (s *Server) apiWebRTCOffer(w http.ResponseWriter, r *http.Request, uid int64) {
	ensureH264DLL() // background, once — see the func doc
	if service.RunAsService() && !vbrowser.Running() {
		jsonErrorCode(w, http.StatusConflict, "service_mode",
			"Remote Desktop requires TGControl to run in the interactive user session.", nil)
		return
	}

	var req rtcOfferRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, rtcOfferMaxBody)).Decode(&req); err != nil {
		jsonError(w, "bad offer", http.StatusBadRequest)
		return
	}
	if req.SDP == "" {
		jsonError(w, "missing sdp", http.StatusBadRequest)
		return
	}

	numDisplays := screenshot.NumActiveDisplays()
	if numDisplays == 0 {
		jsonErrorCode(w, http.StatusServiceUnavailable, "no_display", "no displays found", nil)
		return
	}

	sess, err := s.newRTCSession(req, uid, numDisplays, requestTransport(r))
	if err != nil {
		log.Printf("[RTC] session setup failed uid=%d: %v", uid, err)
		jsonError(w, "webrtc setup failed", http.StatusInternalServerError)
		return
	}
	if !rtcRegister(sess) {
		sess.pc.Close()
		jsonErrorCode(w, http.StatusTooManyRequests, "remote_limit", "too many remote sessions", nil)
		return
	}

	answer, err := sess.negotiate(req.SDP)
	if err != nil {
		log.Printf("[RTC] negotiate failed uid=%d: %v", uid, err)
		sess.close()
		jsonError(w, "negotiation failed", http.StatusInternalServerError)
		return
	}

	jsonResp(w, rtcAnswerResponse{Type: "answer", SDP: answer})
}

func (s *Server) newRTCSession(req rtcOfferRequest, uid int64, numDisplays int, transport string) (*rtcSession, error) {
	api, err := rtcEngineAPI()
	if err != nil {
		return nil, err
	}
	cfg := webrtc.Configuration{ICEServers: toPionICEServers(req.ICEServers)}
	pc, err := api.NewPeerConnection(cfg)
	if err != nil {
		return nil, err
	}

	bounds := screenshot.GetDisplayBounds(0)
	displays := make([]map[string]any, numDisplays)
	for i := 0; i < numDisplays; i++ {
		b := screenshot.GetDisplayBounds(i)
		displays[i] = map[string]any{"id": i, "x": b.Min.X, "y": b.Min.Y, "w": b.Dx(), "h": b.Dy()}
	}

	var adapt *adaptiveCtrl
	if s.licenseManager != nil {
		// Лимиты качества — только для облачных подключений: дома без ограничений.
		l := s.licenseManager.LimitsForTransport(transport)
		adapt = newAdaptiveCtrlWithLimits(l.RemoteDesktopMaxFPS, l.RemoteDesktopQuality, l.RemoteDesktopMaxRes)
	} else {
		adapt = newAdaptiveCtrl()
	}

	sess := &rtcSession{
		pc:          pc,
		adapt:       adapt,
		ctrl:        input.New(),
		bounds:      bounds,
		screenW:     bounds.Dx(),
		screenH:     bounds.Dy(),
		numDisplays: numDisplays,
		displays:    displays,
		clipCh:      make(chan func(), 1),
		done:        make(chan struct{}),
		uid:         uid,
		encDLL:      codec.FindOpenH264DLL(paths.Base()),
	}
	// notify: предупреждения инъекции ввода («ввод не доходит — компьютер
	// заблокирован») уходят клиенту по ctrl-каналу. Канал может быть ещё не
	// открыт — sendCtrlJSON в этом случае молча пропускает.
	sess.applier = newInputApplier(routeInput(sess.ctrl), func() image.Rectangle {
		sess.mu.Lock()
		screen := sess.bounds
		sess.mu.Unlock()
		return remoteFrameBounds(screen)
	}, sess.done, func(v map[string]any) { sess.sendCtrlJSON(v) })
	go sess.watchCursor()

	// Clipboard worker (depth-1, drop-on-busy) — same DoS-safe pattern as WS.
	go func() {
		defer observability.RecoverPanic("rtc-clip-worker")
		for {
			select {
			case fn := <-sess.clipCh:
				fn()
			case <-sess.done:
				return
			}
		}
	}()

	pc.OnDataChannel(sess.onDataChannel)
	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		log.Printf("[RTC] uid=%d state=%s", uid, st)
		switch st {
		case webrtc.PeerConnectionStateConnected:
			// Каким путём пошла картинка — host (LAN/P2P), srflx (P2P через
			// STUN) или relay (TURN). До 2.61.18 этого в логе не было вовсе:
			// жалоба владельца 02.09.2026 «хуже AnyDesk» разбиралась по
			// state=connected/failed, а маршрут и RTT телефон присылал в
			// cstats, которые жили только в памяти и пропали с перезапуском.
			log.Printf("[RTC] uid=%d маршрут: %s", uid, sess.selectedPairSummary())
			if sess.cs.Load() == nil {
				id := strconv.FormatInt(atomic.AddInt64(&rtcConnSeq, 1), 10)
				sess.cs.Store(connstat.Default.Open("rtc", id, uid))
			}
			if sess.videoMode.Load() && !sess.videoStarted.Swap(true) {
				go sess.produceH264()
			}
		case webrtc.PeerConnectionStateFailed,
			webrtc.PeerConnectionStateClosed,
			webrtc.PeerConnectionStateDisconnected:
			sess.cs.Load().Close(st.String())
			sess.close()
		}
	})

	// Зритель считается на сессию, а не на продюсера: JPEG и H.264 взаимно
	// исключают друг друга и могут сменяться, а счётчик зрителей у idle-таймера
	// браузера должен оставаться ровно одним на смотрящего.
	browserViewerJoined()

	return sess, nil
}

var rtcConnSeq int64 // connstat needs a unique id per RTC connection

// castagnoli: hardware-accelerated CRC (SSE4.2) — the H.264 dedup hashes the
// FULL RGBA frame (~8MB at 1080p) every tick; the IEEE polynomial's software
// path would burn ~8ms/frame right on the latency path.
var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// watchCursor pushes the HOST cursor position to the client (~10Hz, only on
// change). The client renders a local zero-latency cursor; without this echo
// it silently diverges whenever the pointer moves without the client's input —
// physical mouse at the PC, apps warping the cursor, a second granted viewer.
func (s *rtcSession) watchCursor() {
	defer observability.RecoverPanic("rtc-cursor-watch")
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	lastX, lastY := int(^uint(0)>>1), int(^uint(0)>>1)
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
		}
		// While the client is actively steering, its local cursor is the truth —
		// echoing our (laggy) position back would fight the finger.
		if time.Since(time.Unix(0, s.applier.lastMoveAt.Load())) < 500*time.Millisecond {
			continue
		}
		x, y, ok := input.CursorPos()
		if !ok || (x == lastX && y == lastY) {
			continue
		}
		lastX, lastY = x, y
		s.mu.Lock()
		b := s.bounds
		s.mu.Unlock()
		if b.Dx() <= 0 || b.Dy() <= 0 {
			continue
		}
		nx := float64(x-b.Min.X) / float64(b.Dx())
		ny := float64(y-b.Min.Y) / float64(b.Dy())
		if nx < 0 || nx > 1 || ny < 0 || ny > 1 {
			continue // cursor is on another display
		}
		s.sendCtrlJSON(map[string]any{"t": "cursor", "x": nx, "y": ny})
	}
}

// negotiate applies the remote offer, builds an answer and blocks until ICE
// gathering completes (non-trickle) so the returned SDP carries all candidates.
func (s *rtcSession) negotiate(offerSDP string) (string, error) {
	if err := s.pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer, SDP: offerSDP,
	}); err != nil {
		return "", err
	}

	// If the client offered a video m-line and we have an H.264 encoder, attach a
	// sendonly H.264 track to fill it (must happen before CreateAnswer so the
	// answer carries the track). Otherwise we stay on the JPEG frames DataChannel.
	if s.encDLL != "" && strings.Contains(offerSDP, "m=video") {
		track, terr := webrtc.NewTrackLocalStaticSample(
			webrtc.RTPCodecCapability{
				MimeType:    webrtc.MimeTypeH264,
				ClockRate:   90000,
				SDPFmtpLine: h264FmtpLine, // matches the only codec our MediaEngine offers
			},
			"video", "remotai-screen",
		)
		if terr != nil {
			log.Printf("[RTC] uid=%d h264 track: %v — staying on JPEG", s.uid, terr)
		} else if sender, aerr := s.pc.AddTrack(track); aerr != nil {
			log.Printf("[RTC] uid=%d AddTrack: %v — staying on JPEG", s.uid, aerr)
		} else {
			s.videoTrack = track
			s.videoMode.Store(true)
			go s.readRTCP(sender)
			if codecs := sender.GetParameters().Codecs; len(codecs) > 0 {
				log.Printf("[RTC] uid=%d H.264 track negotiated, sender codec pt=%d mime=%s", s.uid, codecs[0].PayloadType, codecs[0].MimeType)
			} else {
				log.Printf("[RTC] uid=%d H.264 track negotiated (no sender codecs?!)", s.uid)
			}
		}
	}

	// Аудиотрек виртуального браузера: клиент предложил m=audio, сессия жива и
	// sink+libopus на месте (SetupAudio на старте vbrowser). Любое условие не
	// сошлось — ответ без audio m-line, поведение как раньше. Подписка идёт
	// ПЕРЕД AddTrack: не получилось подписаться — нечего и трек объявлять.
	if strings.Contains(offerSDP, "m=audio") && vbrowser.Running() && vbrowser.AudioAvailable() {
		audioCh, unsub, subErr := vbrowser.SubscribeAudio()
		if subErr != nil {
			log.Printf("[RTC] uid=%d audio subscribe: %v — без звука", s.uid, subErr)
		} else if atrack, terr := webrtc.NewTrackLocalStaticSample(
			webrtc.RTPCodecCapability{
				MimeType:    webrtc.MimeTypeOpus,
				ClockRate:   48000,
				Channels:    2,
				SDPFmtpLine: opusFmtpLine, // matches the MediaEngine registration
			},
			"audio", "remotai-vbrowser-audio",
		); terr != nil {
			log.Printf("[RTC] uid=%d opus track: %v — без звука", s.uid, terr)
			unsub()
		} else if _, aerr := s.pc.AddTrack(atrack); aerr != nil {
			log.Printf("[RTC] uid=%d AddTrack(audio): %v — без звука", s.uid, aerr)
			unsub()
		} else {
			// close() мог сработать, пока мы договаривались (ICE умер на
			// параллельной горутине): тогда отписываемся сразу, иначе parec
			// останется жить без зрителя до самого Stop vbrowser.
			s.mu.Lock()
			select {
			case <-s.done:
				s.mu.Unlock()
				unsub()
			default:
				s.audioTrack = atrack
				s.audioUnsub = unsub
				s.mu.Unlock()
				go s.produceAudio(audioCh)
				log.Printf("[RTC] uid=%d Opus audio track negotiated (vbrowser)", s.uid)
			}
		}
	}

	answer, err := s.pc.CreateAnswer(nil)
	if err != nil {
		return "", err
	}
	gatherComplete := webrtc.GatheringCompletePromise(s.pc)
	if err := s.pc.SetLocalDescription(answer); err != nil {
		return "", err
	}
	select {
	case <-gatherComplete:
	case <-time.After(rtcGatherTimeout):
		// Proceed with whatever candidates we have (host/srflx are quick; a slow
		// TURN relay candidate shouldn't block the whole handshake).
		log.Printf("[RTC] uid=%d ICE gather timeout — answering with partial candidates", s.uid)
	}
	local := s.pc.LocalDescription()
	if local == nil {
		return "", io.ErrUnexpectedEOF
	}
	return local.SDP, nil
}

func (s *rtcSession) onDataChannel(dc *webrtc.DataChannel) {
	switch dc.Label() {
	case "ctrl":
		s.ctrlDC.Store(dc)
		dc.OnOpen(func() {
			s.sendInfo()
		})
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			if msg.IsString {
				s.handleControl(msg.Data)
			}
		})
	case "frames":
		s.framesDC.Store(dc)
		dc.OnOpen(func() {
			// In H.264 video mode the screen rides the RTP video track; the JPEG
			// frames producer only runs as the universal fallback.
			if !s.videoMode.Load() && !s.jpegStarted.Swap(true) {
				go s.produce(dc)
			}
		})
	case "input":
		// Unreliable/unordered lane for pointer MOVES only (absolute coords are
		// idempotent — a lost move is harmless, a late one is dropped by seq).
		// Clicks/keys/scroll stay on the reliable ctrl channel: scroll deltas
		// are relative (a lost one is a lost wheel notch) and clicks must not
		// vanish. The client only opens this after info advertises moveCh.
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			if !msg.IsString {
				return
			}
			var ev struct {
				T   string  `json:"t"`
				A   string  `json:"a"`
				X   float64 `json:"x"`
				Y   float64 `json:"y"`
				Seq int64   `json:"seq"`
			}
			if json.Unmarshal(msg.Data, &ev) != nil {
				return
			}
			if ev.T == "m" && (ev.A == "move" || ev.A == "") {
				s.applier.Move(ev.X, ev.Y, ev.Seq)
			}
		})
	default:
		log.Printf("[RTC] uid=%d ignoring unknown data channel %q", s.uid, dc.Label())
	}
}

func (s *rtcSession) sendCtrlJSON(v any) {
	dc := s.ctrlDC.Load()
	if dc == nil || dc.ReadyState() != webrtc.DataChannelStateOpen {
		return
	}
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	_ = dc.SendText(string(b))
}

func (s *rtcSession) sendInfo() {
	s.mu.Lock()
	sw, sh, displays := s.screenW, s.screenH, s.displays
	s.mu.Unlock()
	// Capability flags gate the client's use of the new protocol pieces, so a
	// new client keeps working against an old agent (and vice versa):
	//   moveCh    — agent accepts the unreliable "input" DataChannel for moves
	//   cursorPos — agent pushes {t:"cursor"} host-cursor echoes
	//   vack      — agent understands {t:"vack"} frame-presented acks
	s.sendCtrlJSON(map[string]any{
		"t": "info", "sw": sw, "sh": sh, "displays": displays,
		"moveCh": true, "cursorPos": true, "vack": true,
	})
}

// handleControl mirrors the wsScreenHandler read-loop switch, but messages
// arrive over the ctrl DataChannel instead of the WS read pump.
func (s *rtcSession) handleControl(data []byte) {
	var evt map[string]any
	if err := json.Unmarshal(data, &evt); err != nil {
		return
	}
	t, _ := evt["t"].(string)
	switch t {
	case "ack":
		seq, _ := evt["seq"].(float64)
		dropped, _ := evt["dropped"].(float64)
		s.adapt.onACK(uint32(seq), int(dropped))
	case "vack":
		// Cumulative count of frames the client has PRESENTED (rvfc). Feeds the
		// frame-ACK backpressure in produceH264.
		n, _ := evt["n"].(float64)
		nn := int64(n)
		if nn < s.ackedN.Load() {
			// Client-side counter reset (decoder re-created) — rebase ours too.
			s.sentN.Store(nn)
		}
		s.ackedN.Store(nn)
		s.lastVack.Store(time.Now().UnixNano())
		s.vackOn.Store(true)
	case "cstats":
		// Client-measured WebRTC receiver stats (jitter buffer, decode, route,
		// RTT) — kept for GET /api/diag/connections so latency is decomposable
		// from the agent side (curl localhost:8080, no phone needed).
		delete(evt, "t")
		s.diagMu.Lock()
		s.lastClient = evt
		s.diagMu.Unlock()
		if rtt, ok := evt["rtt"].(float64); ok && rtt >= 0 {
			s.cs.Load().RTT(time.Duration(rtt * float64(time.Millisecond)))
		}
	case "client":
		vw, _ := evt["vw"].(float64)
		dpr, _ := evt["dpr"].(float64)
		s.adapt.setClientHints(int(vw), dpr)
	case "profile":
		profile, _ := evt["profile"].(string)
		s.adapt.setProfile(profile)
	case "pause":
		s.paused.Store(true)
	case "resume":
		if s.paused.Swap(false) {
			// The first post-background H.264 frame must be independently
			// decodable: mobile WebViews commonly discard decoder state while
			// suspended.
			s.forceKey.Store(true)
		}
	case "novideo":
		// The browser negotiated H.264 but failed to present a single frame.
		// Stop that producer and start JPEG on the already-open frames channel:
		// no reconnect, no second ICE handshake and no black-screen dead end.
		if s.videoMode.Swap(false) {
			if dc := s.framesDC.Load(); dc != nil &&
				dc.ReadyState() == webrtc.DataChannelStateOpen &&
				!s.jpegStarted.Swap(true) {
				go s.produce(dc)
			}
			s.sendCtrlJSON(map[string]any{"t": "video_mode", "mode": "jpeg"})
		}
	case "ping":
		s.sendCtrlJSON(map[string]any{"t": "pong", "ts": evt["ts"]})
	case "display":
		id, _ := evt["id"].(float64)
		did := int(id)
		if did >= 0 && did < s.numDisplays {
			b := screenshot.GetDisplayBounds(did)
			s.mu.Lock()
			s.bounds = b
			s.screenW, s.screenH = b.Dx(), b.Dy()
			displays := s.displays
			sw, sh := s.screenW, s.screenH
			s.mu.Unlock()
			s.sendCtrlJSON(map[string]any{"t": "info", "sw": sw, "sh": sh, "displays": displays})
		}
	case "clip_get":
		if !s.submitClip(func() {
			text, code := clipboardRead()
			if code == "clipboard_empty" {
				s.sendCtrlJSON(map[string]any{"t": "clip", "text": "", "empty": true})
			} else if code != "" {
				s.sendCtrlJSON(map[string]any{"t": "clip_error", "op": "read", "code": code})
			} else {
				s.sendCtrlJSON(map[string]any{"t": "clip", "text": text})
			}
		}) {
			s.sendCtrlJSON(map[string]any{"t": "clip_error", "op": "read", "code": "clipboard_busy"})
		}
	case "clip_set":
		if text, _ := evt["text"].(string); text != "" {
			if !s.submitClip(func() {
				if code := clipboardWrite(text); code != "" {
					s.sendCtrlJSON(map[string]any{"t": "clip_error", "op": "set_text", "code": code})
				} else {
					s.sendCtrlJSON(map[string]any{"t": "clip_result", "op": "set_text", "ok": true})
				}
			}) {
				s.sendCtrlJSON(map[string]any{"t": "clip_error", "op": "set_text", "code": "clipboard_busy"})
			}
		}
	case "clip_set_image":
		if d, _ := evt["data"].(string); d != "" {
			if !s.submitClip(func() {
				if code := clipboardWriteImage(d); code != "" {
					s.sendCtrlJSON(map[string]any{"t": "clip_error", "op": "set_image", "code": code})
				} else {
					s.sendCtrlJSON(map[string]any{"t": "clip_result", "op": "set_image", "ok": true})
				}
			}) {
				s.sendCtrlJSON(map[string]any{"t": "clip_error", "op": "set_image", "code": "clipboard_busy"})
			}
		}
	case "m", "s", "k", "txt", "tp", "paste":
		// "tp" — касания страницы. Его отсутствие здесь стоило владельцу
		// неработающей прокрутки: телефон в облаке ходит по этому каналу, и
		// каждое касание молча падало в неизвестные события, пока по
		// веб-сокету всё работало. Список типов теперь один на оба транспорта.
		//
		// Injection happens on the applier goroutine: clicks carry sleeps
		// (15–90ms) that must not stall this read-path, and move bursts
		// coalesce to the latest position instead of replaying a stale path.
		s.applier.Route(evt)
	}
}

func (s *rtcSession) submitClip(fn func()) bool {
	select {
	case s.clipCh <- fn:
		return true
	default: // worker busy — drop rather than pile up processes
		return false
	}
}

// produce is the capture→encode→send loop, the WebRTC analogue of the WS
// capture loop. Frames go out on the unreliable "frames" channel; stats on ctrl.
func (s *rtcSession) produce(dc *webrtc.DataChannel) {
	defer observability.RecoverPanic("rtc-produce")
	// Смотреть перестали — гасим поток кадров браузера: без зрителя он греет
	// процессор сервера впустую (само подключение остаётся для навигации).
	defer browserStreamStopped()
	defer subscribeBrowserNotices(func(v map[string]any) { s.sendCtrlJSON(v) })()
	log.Printf("[RTC] uid=%d frames channel open — producing", s.uid)

	// Ворота одинаковых кадров (по сырым пикселям, до кодирования) и номер
	// последнего кадра, взятого у самого браузера, — см. nextRemoteFrame.
	var gate frameGate
	var cdpSeq uint64
	var seq uint32
	buf := new(bytes.Buffer)
	seqBuf := make([]byte, 4)
	statsWindow := time.Now()
	lastFrameSent := time.Time{}
	var statsBytes int64
	var statsFrames, statsSkipped int
	var lastCaptureMs, lastEncodeMs int64
	var capFail captureFailStreak
	loopMark := time.Now()

	sendStats := func() {
		curFPS, curQ, curW, curRTT, profile := s.adapt.get()
		elapsed := time.Since(statsWindow).Seconds()
		sentFPS, kbps := 0.0, 0.0
		if elapsed > 0 {
			sentFPS = float64(statsFrames) / elapsed
			kbps = float64(statsBytes*8) / elapsed / 1000
		}
		s.sendCtrlJSON(map[string]any{
			"t": "stats", "rtt": curRTT.Milliseconds(), "fps": curFPS,
			"sentFps": math.Round(sentFPS*10) / 10, "quality": curQ, "width": curW,
			"kbps": math.Round(kbps), "skipped": statsSkipped,
			"captureMs": lastCaptureMs, "encodeMs": lastEncodeMs, "profile": profile,
			"transport": "webrtc",
		})
		statsWindow = time.Now()
		statsBytes, statsFrames, statsSkipped = 0, 0, 0
	}

	for {
		select {
		case <-s.done:
			return
		default:
		}
		if s.paused.Load() {
			time.Sleep(100 * time.Millisecond)
			loopMark = time.Now()
			continue
		}

		curFPS, curQ, curW, _, _ := s.adapt.get()
		curFPS, curQ = boostWhileInteracting(s.applier, curFPS, curQ)
		period := time.Second / time.Duration(curFPS)
		if d := period - time.Since(loopMark); d > 0 {
			time.Sleep(d)
		}
		loopMark = time.Now()

		select {
		case <-s.done:
			return
		default:
		}

		s.mu.Lock()
		curBounds := s.bounds
		s.mu.Unlock()

		keepAlive := time.Since(lastFrameSent) >= 2*time.Second
		capMs, encMs, changed, err := nextRemoteFrame(curBounds, curW, curQ, buf, &gate, &cdpSeq, keepAlive)
		if err != nil {
			if capFail.fail(time.Now()) {
				log.Printf("[RTC] uid=%d capture failing: %v", s.uid, err)
				s.sendCtrlJSON(map[string]any{
					"t": "error", "code": "capture_failed", "msg": err.Error(),
				})
			}
			continue
		}
		capFail.ok()
		lastCaptureMs, lastEncodeMs = capMs, encMs

		// Кадр не изменился (или новый ещё не пришёл от браузера) — отправлять
		// нечего, и работа по кодированию уже не делалась.
		if !changed {
			statsSkipped++
			if time.Since(statsWindow) > 2*time.Second {
				sendStats()
			}
			continue
		}

		// Backpressure / size guards: drop rather than wedge the SCTP buffer or
		// exceed the browser's DataChannel max-message-size.
		if dc.BufferedAmount() > rtcFramesBufLimit || buf.Len() > rtcMaxFrameBytes {
			statsSkipped++
			if time.Since(statsWindow) > 2*time.Second {
				sendStats()
			}
			continue
		}

		seq++
		s.adapt.onSend(seq)
		binary.BigEndian.PutUint32(seqBuf, seq)
		frame := make([]byte, 4+buf.Len())
		copy(frame[:4], seqBuf)
		copy(frame[4:], buf.Bytes())

		if err := dc.Send(frame); err != nil {
			log.Printf("[RTC] uid=%d frame send: %v", s.uid, err)
			return
		}
		lastFrameSent = time.Now()
		statsBytes += int64(len(frame))
		statsFrames++

		if time.Since(statsWindow) > 2*time.Second {
			sendStats()
		}
	}
}

// produceH264 encodes the screen as H.264 and writes RTP samples to the
// negotiated video track (used instead of the JPEG frames producer). The frame
// source is CDP-first, same as nextRemoteFrame: a live virtual-browser link
// hands over JPEG page frames (decoded here; 4:2:0 goes into the encoder
// straight as I420 planes), without a browser link it falls back to the X
// screen capture — and back, mid-stream, via encoder re-creation. fps/bitrate
// follow the client's stream profile, capped by the license (never above 60)
// and the RTCP-loss QoS ladder; retunes apply live via SetOption, only a
// resolution change (display or page viewport) recreates the encoder, and the
// first frame after that is a forced IDR carrying fresh SPS/PPS so the client
// decoder survives the size switch. PLIs from the client force a (debounced)
// IDR. Unchanged frames are skipped before encode (Seq gate for CDP,
// DAMAGE+crc32 for X) — sending nothing is both the cheapest and the
// lowest-latency thing to do on a static screen.
func (s *rtcSession) produceH264() {
	defer observability.RecoverPanic("rtc-produce-h264")
	// Поток кадров браузера гасим за собой: без зрителя он греет процессор
	// сервера впустую (само подключение остаётся для навигации — как у
	// JPEG-продюсера).
	defer browserStreamStopped()
	log.Printf("[RTC] uid=%d H.264 producer started (dll=%s)", s.uid, s.encDLL)

	var enc codec.Encoder
	var curW, curH int
	var curFPS, curBitrate int
	first := true
	defer func() {
		if enc != nil {
			enc.Close()
		}
	}()

	statsWindow := time.Now()
	var statsBytes int64
	var statsFrames, statsSkipped int
	var lastCaptureMs, lastEncodeMs int64
	var prevHash uint32
	var cdpSeq uint64      // Seq последнего взятого у браузера кадра — change-ворота CDP
	var lastSent time.Time // real send times → honest sample durations + keep-alive
	var lastIDR time.Time
	var capFail captureFailStreak
	// Источник кадров: на Windows берёт готовое у общего насоса DXGI, иначе
	// снимает экран сам (см. screenSource).
	s.mu.Lock()
	source := newScreenSource(s.bounds)
	s.mu.Unlock()
	loopMark := time.Now()

	emitStats := func(fps, encW int, profile string) {
		elapsed := time.Since(statsWindow).Seconds()
		sentFps, kbps := 0.0, 0.0
		if elapsed > 0 {
			sentFps = float64(statsFrames) / elapsed
			kbps = float64(statsBytes*8) / elapsed / 1000
		}
		m := map[string]any{
			"t": "stats", "transport": "webrtc-h264",
			"fps": fps, "sentFps": math.Round(sentFps*10) / 10,
			"width": encW, "kbps": math.Round(kbps), "skipped": statsSkipped,
			"captureMs": lastCaptureMs, "encodeMs": lastEncodeMs, "profile": profile,
			"qos": s.qos.level(), "inflight": s.sentN.Load() - s.ackedN.Load(),
		}
		s.sendCtrlJSON(m)
		delete(m, "t")
		s.diagMu.Lock()
		s.lastSrv = m
		s.diagMu.Unlock()
		statsWindow = time.Now()
		statsBytes, statsFrames, statsSkipped = 0, 0, 0
	}

	for {
		select {
		case <-s.done:
			return
		default:
		}
		if !s.videoMode.Load() {
			log.Printf("[RTC] uid=%d H.264 disabled by client — JPEG fallback active", s.uid)
			return
		}
		if s.paused.Load() {
			time.Sleep(100 * time.Millisecond)
			loopMark = time.Now()
			continue
		}

		_, curQ, curWa, _, profile := s.adapt.get()
		s.mu.Lock()
		b := s.bounds
		s.mu.Unlock()

		// fps не зависит от размера кадра, поэтому считается до захвата:
		// пейсинг идёт по нему. Потолок — лицензия (но не выше 60), ниже —
		// QoS-лесенка по потерям.
		fps := h264FPS(profile)
		if ceiling := s.adapt.fpsCeiling(); fps > ceiling {
			fps = ceiling
		}
		// Congestion ladder: on RTCP-reported loss cut fps (bits shrink
		// proportionally — h264Bitrate is linear in fps) but KEEP the
		// resolution — readable text beats motion.
		if lvl := s.qos.level(); lvl > 0 {
			if f := qosFPSLadder[min(lvl, len(qosFPSLadder)-1)]; f < fps {
				fps = f
			}
		}

		period := time.Second / time.Duration(fps)
		if d := period - time.Since(loopMark); d > 0 {
			time.Sleep(d)
		}
		loopMark = time.Now()

		select {
		case <-s.done:
			return
		default:
		}

		// Frame-ACK backpressure: never run more than rtcMaxInflight frames
		// ahead of what a vack-capable client has presented. This bounds the
		// invisible queue in the SRTP pacer/TURN relay when bandwidth dips —
		// otherwise latency drifts unboundedly into the past under congestion.
		if s.vackOn.Load() {
			for s.sentN.Load()-s.ackedN.Load() > rtcMaxInflight {
				if time.Since(time.Unix(0, s.lastVack.Load())) > rtcVackStall {
					s.sentN.Store(s.ackedN.Load()) // acks stalled — rebase, never deadlock
					break
				}
				select {
				case <-s.done:
					return
				case <-time.After(5 * time.Millisecond):
				}
			}
		}

		capStart := time.Now()
		// Источник кадра — как у JPEG-путей (nextRemoteFrame): живое
		// CDP-подключение отдаёт JPEG-кадры страницы, иначе захват X-экрана.
		// keep-alive/IDR пробивает change-ворота обоих источников.
		// paintDue — пора сделать качественный проход поверх статики: тоже
		// пробивает ворота, плюс ниже поднимает битрейт этого кадра.
		paintDue := time.Since(lastSent) >= rtcPaintOverInterval
		keepAlive := first || s.forceKey.Load() || paintDue || time.Since(lastSent) >= rtcStaticKeepAlive
		var img image.Image
		cdpImg, cdpOK, cdpChanged, cdpErr := cdpH264Frame(curQ, curWa, &cdpSeq, keepAlive)
		switch {
		case cdpErr != nil:
			if capFail.fail(time.Now()) {
				log.Printf("[RTC] uid=%d CDP frame decode failing: %v", s.uid, cdpErr)
			}
			continue
		case cdpOK:
			// Нового кадра от браузера нет: Chrome шлёт их только при смене
			// картинки — кодировать и слать нечего, декодер клиента держит
			// последний кадр.
			if !cdpChanged {
				statsSkipped++
				if time.Since(statsWindow) > 2*time.Second {
					emitStats(fps, curW, profile)
				}
				continue
			}
			capFail.ok()
			img = cdpImg
			prevHash = 0 // за время CDP экранный хэш протух — назад на экран вернёмся с чистого сравнения
		default:
			// Экран неподвижен — не трогаем ни X, ни кодек. Проверка стоит доли
			// миллисекунды (X DAMAGE), захват — копия всего кадра через сокет;
			// сравнение хэшей ниже спасало только от лишнего кодирования, но кадр
			// к тому моменту уже был вытащен. Keep-alive оставляем прежний.
			if !first && !s.forceKey.Load() && !paintDue &&
				time.Since(lastSent) < rtcStaticKeepAlive && !source.Changed(b) {
				statsSkipped++
				if time.Since(statsWindow) > 2*time.Second {
					emitStats(fps, curW, profile)
				}
				continue
			}

			rgba, err := source.Grab(b)
			if err != nil {
				// Без кадров экран не оживёт сам: сообщаем причину, иначе клиент
				// вечно висит на «Жду первый кадр».
				if capFail.fail(time.Now()) {
					log.Printf("[RTC] uid=%d capture failing: %v", s.uid, err)
					s.sendCtrlJSON(map[string]any{
						"t": "error", "code": "capture_failed", "msg": err.Error(),
					})
				}
				continue
			}
			capFail.ok()

			// Change detection BEFORE conversion/encode: identical pixels + no
			// pending IDR request → skip the frame entirely. A keep-alive frame
			// still flows every rtcStaticKeepAlive so the receiver track never
			// mutes and a lost PLI-recovery can't stall forever.
			hash := crc32.Checksum(rgba.Pix, castagnoli)
			if !first && !s.forceKey.Load() && !paintDue && hash == prevHash &&
				time.Since(lastSent) < rtcStaticKeepAlive {
				statsSkipped++
				if time.Since(statsWindow) > 2*time.Second {
					emitStats(fps, curW, profile)
				}
				continue
			}
			prevHash = hash
			img = rgba
		}
		lastCaptureMs = time.Since(capStart).Milliseconds()

		// Размеры кодека — по реальному кадру: у CDP это страница в пикселях
		// JPEG (не CSS-вьюпорт и не экран). Смена размера (поворот телефона,
		// переход CDP↔экран) пересоздаёт энкодер, и первый кадр после этого —
		// принудительный IDR со свежими SPS/PPS: декодер клиента переживает
		// смену разрешения посреди RTP-потока.
		sb := img.Bounds()
		encW, encH := codec.EncodeDims(sb.Dx(), sb.Dy(), s.adapt.videoWidthCap())
		bitrate := h264Bitrate(profile, encW, encH, fps)

		if enc == nil || encW != curW || encH != curH {
			if enc != nil {
				enc.Close()
				enc = nil
			}
			e, err := codec.NewH264(s.encDLL, encW, encH, fps, bitrate)
			if err != nil {
				log.Printf("[RTC] uid=%d H.264 encoder init failed: %v — no video track output", s.uid, err)
				return
			}
			enc = e
			curW, curH, curFPS, curBitrate = encW, encH, fps, bitrate
			first = true
			prevHash = 0
		} else if fps != curFPS || bitrate != curBitrate {
			// Profile/QoS retune: live SetOption when the encoder supports it —
			// re-creating costs an IDR + a visible stall.
			if lt, ok := enc.(codec.LiveTuner); ok {
				if err := lt.SetFrameRate(float64(fps)); err != nil {
					log.Printf("[RTC] uid=%d live retune failed (%v) — recreating encoder", s.uid, err)
					enc.Close()
					enc = nil
					continue
				}
				_ = lt.SetBitrate(bitrate)
				curFPS, curBitrate = fps, bitrate
			} else {
				enc.Close()
				enc = nil
				continue
			}
		}

		// A PLI means the decoder lost reference — force an IDR, but at most
		// one per rtcPLIDebounce: on a lossy link the IDR burst itself gets
		// lost, and an undebounced PLI→IDR→loss loop cascades into freezes.
		force := first
		if !force && s.forceKey.Load() && time.Since(lastIDR) >= rtcPLIDebounce {
			force = s.forceKey.Swap(false)
		}
		first = false

		// Paint-over: на статике один кадр с битрейтом повыше и IDR — мелкий
		// текст на странице, которую человек читает, становится идеальным.
		// Буст возвращаем сразу после кодирования, чтобы он не пережил кадр.
		if paintDue {
			force = true
			if lt, ok := enc.(codec.LiveTuner); ok {
				_ = lt.SetBitrate(max(bitrate*4, 6_000_000))
			}
		}

		encStart := time.Now()
		nal, key, err := encodeFrame(enc, img, force)
		lastEncodeMs = time.Since(encStart).Milliseconds()
		if paintDue {
			if lt, ok := enc.(codec.LiveTuner); ok {
				_ = lt.SetBitrate(bitrate)
			}
		}
		if err != nil {
			log.Printf("[RTC] uid=%d h264 encode: %v", s.uid, err)
			continue
		}
		if len(nal) == 0 {
			continue
		}
		if key {
			lastIDR = time.Now()
		}

		// Honest sample duration = the real gap since the previous SENT frame.
		// A fixed nominal period makes RTP timestamps lie whenever frames are
		// skipped, inflating the receiver's jitter estimate — the exact buffer
		// the client's jitterBufferTarget=0 is trying to keep empty.
		dur := period
		if !lastSent.IsZero() {
			dur = time.Since(lastSent)
			if dur <= 0 {
				dur = time.Millisecond
			} else if dur > 2*time.Second {
				dur = 2 * time.Second
			}
		}
		lastSent = time.Now()
		if err := s.videoTrack.WriteSample(media.Sample{Data: nal, Duration: dur}); err != nil {
			log.Printf("[RTC] uid=%d WriteSample: %v", s.uid, err)
			return
		}
		s.sentN.Add(1)
		statsBytes += int64(len(nal))
		statsFrames++

		if time.Since(statsWindow) > 2*time.Second {
			emitStats(fps, encW, profile)
		}
	}
}

// encodeFrame кодирует кадр в H.264. JPEG-кадр CDP-скринкаста Chrome — почти
// всегда YCbCr 4:2:0 ровно размера энкодера: его плоскости идут в кодек почти
// напрямую (I420 = Y, Cb→U, Cr→V; страйды учитываются), без попиксельной
// RGBA-конверсии. Иной формат или размер — обычный путь RGBA→I420, там же и
// масштабирование разового рассинхрона при смене viewport.
func encodeFrame(enc codec.Encoder, img image.Image, force bool) (nal []byte, key bool, err error) {
	if pe, ok := enc.(codec.PlaneEncoder); ok {
		if yc, ok := img.(*image.YCbCr); ok &&
			yc.SubsampleRatio == image.YCbCrSubsampleRatio420 &&
			yc.Rect.Min == (image.Point{}) &&
			yc.Rect.Dx() == enc.Width() && yc.Rect.Dy() == enc.Height() {
			return pe.EncodeI420(yc.Y, yc.Cb, yc.Cr, yc.YStride, yc.CStride, force)
		}
	}
	return enc.EncodeRGBA(img, force)
}

// h264FPS — целевая частота кадров профиля. auto/smooth просят потолок:
// период кадра — это латентность (16 мс при 60 против 66 при 15), а реальный
// максимум выставляют лицензия (fpsCeiling) и QoS-лесенка.
func h264FPS(profile string) int {
	switch profile {
	case "saver":
		return 8
	case "sharp":
		return 12
	default: // auto, smooth
		return 60
	}
}

// h264Bitrate — битрейт под размер кадра и fps: бит на кадр зависит от
// профиля (saver жёстче, sharp щедрее), в разумных границах.
func h264Bitrate(profile string, w, h, fps int) (bitrate int) {
	switch profile {
	case "saver":
		bitrate = w * h * fps / 26
	case "smooth":
		bitrate = w * h * fps / 16
	case "sharp":
		bitrate = w * h * fps / 8
	default: // auto
		bitrate = w * h * fps / 14
	}
	if bitrate < 350_000 {
		bitrate = 350_000
	}
	if bitrate > 8_000_000 {
		bitrate = 8_000_000
	}
	return bitrate
}

// qosFPSLadder maps rtcQoS level → fps cap. Resolution is deliberately kept:
// for screen content, blurry text is a worse failure than lower motion rate.
var qosFPSLadder = [...]int{0, 18, 10, 5}

// rtcQoS is the poor-man's congestion response until GCC/TWCC lands: RTCP
// receiver reports carry fractionLost (0..255 ≈ 0..100%); sustained loss steps
// the fps ladder down fast, clean reports climb back up slowly.
type rtcQoS struct {
	mu         sync.Mutex
	lvl        int
	lastChange time.Time
	cleanSince time.Time
}

func (q *rtcQoS) onLoss(frac uint8) {
	q.mu.Lock()
	defer q.mu.Unlock()
	now := time.Now()
	switch {
	case frac >= 13: // ≥ ~5% loss — congestion, step down (at most 1 per 2s)
		q.cleanSince = time.Time{}
		if q.lvl < len(qosFPSLadder)-1 && now.Sub(q.lastChange) > 2*time.Second {
			q.lvl++
			q.lastChange = now
		}
	case frac <= 2: // ≤ ~0.8% — clean; 10s of clean earns one step up
		if q.cleanSince.IsZero() {
			q.cleanSince = now
			return
		}
		if q.lvl > 0 && now.Sub(q.cleanSince) > 10*time.Second {
			q.lvl--
			q.lastChange = now
			q.cleanSince = now
		}
	default: // in-between — not clean, not bad enough to step
		q.cleanSince = time.Time{}
	}
}

func (q *rtcQoS) level() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.lvl
}

// readRTCP watches the video sender's RTCP feedback: a PLI/FIR (the client's
// decoder needs a fresh keyframe) forces an IDR on the next encoded frame, and
// receiver-report loss feeds the QoS fps ladder.
func (s *rtcSession) readRTCP(sender *webrtc.RTPSender) {
	defer observability.RecoverPanic("rtc-rtcp")
	buf := make([]byte, 1500)
	for {
		n, _, err := sender.Read(buf)
		if err != nil {
			return
		}
		pkts, perr := rtcp.Unmarshal(buf[:n])
		if perr != nil {
			continue
		}
		for _, p := range pkts {
			switch pkt := p.(type) {
			case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
				s.forceKey.Store(true)
			case *rtcp.ReceiverReport:
				for _, rr := range pkt.Reports {
					s.qos.onLoss(rr.FractionLost)
				}
			}
		}
	}
}

// rtcDiagSnapshot exposes per-live-session RTC diagnostics for
// GET /api/diag/connections: the agent's own producer stats plus the last
// client-reported receiver stats (jitter buffer, decode, ICE route, RTT).
func rtcDiagSnapshot() []map[string]any {
	rtcMu.Lock()
	sessions := make([]*rtcSession, 0, len(rtcReg))
	for s := range rtcReg {
		sessions = append(sessions, s)
	}
	rtcMu.Unlock()
	out := make([]map[string]any, 0, len(sessions))
	for _, s := range sessions {
		m := map[string]any{
			"uid":      s.uid,
			"video":    s.videoMode.Load(),
			"qos":      s.qos.level(),
			"inflight": s.sentN.Load() - s.ackedN.Load(),
		}
		s.diagMu.Lock()
		if s.lastSrv != nil {
			m["server"] = s.lastSrv
		}
		if s.lastClient != nil {
			m["client"] = s.lastClient
		}
		s.diagMu.Unlock()
		out = append(out, m)
	}
	return out
}

// ensureH264DLL downloads Cisco's openh264 into the app-data dir in the
// background when missing. The DLL is a runtime plugin (never shipped in the
// installer — Cisco's binary is only royalty-free when fetched from Cisco,
// same model as Firefox). Without it every session silently lands on the JPEG
// fallback — which is exactly what happened in prod. Sessions created before
// the download finishes still fall back; the next connect picks up H.264.
var h264EnsureOnce sync.Once

func ensureH264DLL() {
	// Cisco's prebuilt openh264 we fetch here is a Windows win64 DLL — on
	// Linux/macOS it's useless (a headless server would download it for nothing).
	// На Linux библиотеку ставит пакетный менеджер (libopenh264-*): своей
	// загрузки нет, скачивать неоткуда — поэтому просто проверяем систему и
	// пишем в журнал, чем всё кончилось. Нашлась — H.264 включится сам на
	// следующем сеансе; нет — работает JPEG-путь, как и раньше.
	if runtime.GOOS != "windows" {
		h264EnsureOnce.Do(func() {
			if p := codec.FindOpenH264DLL(paths.Base()); p != "" {
				log.Printf("[RTC] openh264 найден: %s — доступен H.264", p)
				return
			}
			log.Printf("[RTC] openh264 не установлен — видео идёт JPEG-путём (%s)", codec.OpenH264InstallHint)
		})
		return
	}
	h264EnsureOnce.Do(func() {
		go func() {
			defer observability.RecoverPanic("h264-dll-ensure")
			if codec.FindOpenH264DLL(paths.Base()) != "" {
				return
			}
			p, err := codec.EnsureOpenH264DLL(paths.Base(), "", "")
			if err != nil {
				log.Printf("[RTC] openh264 download failed: %v — JPEG fallback stays", err)
				return
			}
			log.Printf("[RTC] openh264 ready: %s", p)
		}()
	})
}

// produceAudio перекладывает Opus-пакеты из подписки vbrowser в аудиотрек.
// Кадр 20 мс = 960 сэмплов на канал при 48 кГц; RTP-метку pion считает из
// Duration по часам кодека (20 мс × 48000 = те самые 960 тиков на пакет).
// Пакеты до готовности DTLS pion молча роняет — как и у видео, это штатно.
func (s *rtcSession) produceAudio(ch <-chan []byte) {
	defer observability.RecoverPanic("rtc-audio")
	log.Printf("[RTC] uid=%d audio producer started (vbrowser)", s.uid)
	for {
		select {
		case <-s.done:
			return
		case pkt, ok := <-ch:
			if !ok {
				return
			}
			if err := s.audioTrack.WriteSample(media.Sample{
				Data: pkt, Duration: 20 * time.Millisecond,
			}); err != nil {
				log.Printf("[RTC] uid=%d audio WriteSample: %v", s.uid, err)
				return
			}
		}
	}
}

func (s *rtcSession) close() {
	s.closeOnce.Do(func() {
		close(s.done)
		s.mu.Lock()
		if s.audioUnsub != nil {
			s.audioUnsub() // пара к SubscribeAudio в negotiate; parec гаснет, когда уходит последний слушатель
			s.audioUnsub = nil
		}
		s.mu.Unlock()
		_ = s.pc.Close()
		s.cs.Load().Close("closed") // no-op when already closed with a real reason
		rtcUnregister(s)
		browserViewerLeft() // пара к browserViewerJoined в newRTCSession
		// Итог сессии — то, что телефон мерил у себя (cstats): маршрут, RTT,
		// кадры в секунду, потери, буфер джиттера, декодирование. Раньше это
		// жило только в памяти для /api/diag/connections и пропадало вместе с
		// сессией — разбирать «хуже AnyDesk» было нечем.
		s.diagMu.Lock()
		lc := s.lastClient
		s.diagMu.Unlock()
		if lc != nil {
			log.Printf("[RTC] uid=%d итог: маршрут=%v rtt=%vмс кадров/с=%v потери=%v%% jitter=%vмс decode=%vмс",
				s.uid, lc["route"], lc["rtt"], lc["rfps"], lc["loss"], lc["jb"], lc["dec"])
		}
		log.Printf("[RTC] uid=%d session closed", s.uid)
	})
}

// selectedPairSummary — выбранная ICE-пара человеческими словами: тип
// кандидатов (host = LAN/P2P, srflx = P2P через STUN, relay = TURN), протокол и
// адреса. Пустая строка, если пара ещё не выбрана.
func (s *rtcSession) selectedPairSummary() string {
	sctp := s.pc.SCTP()
	if sctp == nil {
		return "пара неизвестна (нет SCTP)"
	}
	dtls := sctp.Transport()
	if dtls == nil {
		return "пара неизвестна (нет DTLS)"
	}
	ice := dtls.ICETransport()
	if ice == nil {
		return "пара неизвестна (нет ICE)"
	}
	pair, err := ice.GetSelectedCandidatePair()
	if err != nil || pair == nil || pair.Local == nil || pair.Remote == nil {
		return "пара ещё не выбрана"
	}
	kind := "P2P"
	if pair.Local.Typ == webrtc.ICECandidateTypeRelay || pair.Remote.Typ == webrtc.ICECandidateTypeRelay {
		kind = "TURN"
	} else if pair.Local.Typ == webrtc.ICECandidateTypeHost && pair.Remote.Typ == webrtc.ICECandidateTypeHost {
		kind = "LAN"
	}
	return fmt.Sprintf("%s (наш %s %s %s:%d ↔ их %s %s %s:%d)", kind,
		pair.Local.Typ, pair.Local.Protocol, pair.Local.Address, pair.Local.Port,
		pair.Remote.Typ, pair.Remote.Protocol, pair.Remote.Address, pair.Remote.Port)
}

func toPionICEServers(in []rtcICEServer) []webrtc.ICEServer {
	out := make([]webrtc.ICEServer, 0, len(in))
	for _, s := range in {
		srv := webrtc.ICEServer{URLs: s.URLs}
		if s.Username != "" || s.Credential != "" {
			srv.Username = s.Username
			srv.Credential = s.Credential
		}
		out = append(out, srv)
	}
	return out
}
