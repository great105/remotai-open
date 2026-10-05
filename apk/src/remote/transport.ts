/** Remote Desktop transport abstraction.
 *
 * RemoteView talks to one of two interchangeable transports:
 *
 *   - RtcTransport (preferred): a WebRTC PeerConnection. Video frames ride an
 *     unreliable "frames" DataChannel (UDP/SCTP, maxRetransmits:0) and the JSON
 *     control protocol rides a reliable "ctrl" DataChannel. ICE uses host
 *     candidates on LAN and relay-TURN over the cloud, so it survives lossy /
 *     high-latency links without TCP head-of-line blocking.
 *   - WsTransport (fallback): the original /ws/screen WebSocket — binary frames +
 *     JSON control on one TCP socket. Used where WebRTC is unsupported or fails
 *     (older iOS WKWebView, flaky Telegram WebView, UDP-blocked networks).
 *
 * Both expose the SAME event surface, so the rest of RemoteView (decode, gestures,
 * HUD, reconnect) is transport-agnostic. connectRemote() tries WebRTC first and
 * falls back to WebSocket automatically.
 */

import { isPcOffline } from "@tgcontrol/shared";
import { fetchIceServers, rtcSignalOffer, screenWSUrl } from "../api";
import { getMode } from "../config";

export type TransportKind = "webrtc" | "ws";
export type RemoteConnectionStage =
  | "route"
  | "offer"
  | "signaling"
  | "webrtc"
  | "websocket"
  | "waiting"
  | "frame";

/** Receiver-side WebRTC metrics sampled every ~2s from pc.getStats(). */
export interface RtcClientStats {
  /** Average jitter-buffer dwell per frame in the window, ms. */
  jbMs?: number;
  /** Average decode time per frame in the window, ms. */
  decMs?: number;
  /** Frames decoded per second in the window. */
  recvFps?: number;
  /** Packet loss in the window, %. */
  lossPct?: number;
  /** ICE round-trip time, ms. */
  rtt?: number;
  /** Selected ICE route: "host" (LAN/P2P) | "srflx" (P2P via STUN) | "relay" (TURN). */
  route?: string;
  /** Current receiver jitterBufferTarget, ms (0 = latency mode). */
  jbTarget?: number;
  /** Bytes received on the video RTP stream since this PeerConnection opened. */
  receivedBytes?: number;
}

export interface RemoteHandlers {
  /** Binary video frame: [seq:4 BE][jpeg]. */
  onFrame: (buf: ArrayBuffer) => void;
  /** Parsed JSON control message (info / stats / clip / pong / error). */
  onJSON: (msg: any) => void;
  /** Fired when an H.264 video track arrives (WebRTC video mode). The caller
   *  renders the MediaStream and stops using the JPEG frame callback. */
  onVideoTrack?: (stream: MediaStream) => void;
  /** Fired when an Opus audio track arrives (звук виртуального браузера; у
   *  обычного десктопа и у старых агентов трека просто нет). Отдельный
   *  MediaStream от видео — вешать на свой <audio>. */
  onAudioTrack?: (stream: MediaStream) => void;
  /** Periodic receiver-side WebRTC metrics (webrtc transport only). */
  onClientStats?: (s: RtcClientStats) => void;
  /** PeerConnection state changes (webrtc transport only). "connected" means
   *  ICE+DTLS are up — the only moment from which "no frames yet" is a real
   *  symptom rather than a handshake still in flight (N136). */
  onRtcState?: (state: RTCPeerConnectionState) => void;
  /** Fired once when the transport is live and ready to send/receive. */
  onOpen: (t: RemoteTransport) => void;
  /** Human-facing connection progress; keeps the first-frame path observable. */
  onStage?: (stage: RemoteConnectionStage) => void;
  /** Fired when the transport drops (triggers RemoteView's reconnect). */
  onClose: () => void;
  /** Fired on a transport-level error (non-fatal; usually followed by onClose). */
  onError: (e: unknown) => void;
}

export interface RemoteTransport {
  readonly kind: TransportKind;
  /** Send a JSON control/input message. No-op if not open. */
  send: (json: any) => void;
  /** Send a pointer-move on the unreliable low-latency lane. Returns false when
   *  the lane is unavailable (WS transport / channel not open) — the caller
   *  falls back to send(). */
  sendMove: (json: any) => boolean;
  close: () => void;
}

export interface RemoteConnection {
  close: () => void;
}

// How long to give WebRTC (ICE + signaling + channel open) before falling back.
const WEBRTC_CONNECT_TIMEOUT = 8000;
const ICE_GATHER_TIMEOUT = 4000;

// ── WebSocket transport (fallback / legacy) ─────────────────────

class WsTransport implements RemoteTransport {
  readonly kind = "ws" as const;
  private ws: WebSocket;

  constructor(url: string, private h: RemoteHandlers) {
    const ws = new WebSocket(url);
    ws.binaryType = "arraybuffer";
    this.ws = ws;
    ws.onopen = () => this.h.onOpen(this);
    ws.onclose = () => this.h.onClose();
    ws.onerror = (e) => this.h.onError(e);
    ws.onmessage = (ev) => {
      if (typeof ev.data === "string") {
        try { this.h.onJSON(JSON.parse(ev.data)); } catch { /* malformed */ }
      } else if (ev.data instanceof ArrayBuffer) {
        this.h.onFrame(ev.data);
      }
    };
  }

  send(json: any) {
    if (this.ws.readyState === WebSocket.OPEN) this.ws.send(JSON.stringify(json));
  }

  sendMove(_json: any): boolean {
    return false; // no unreliable lane on a WebSocket — caller uses send()
  }

  close() {
    try { this.ws.close(); } catch { /* ignore */ }
  }
}

// ── WebRTC transport (preferred) ────────────────────────────────

class RtcTransport implements RemoteTransport {
  readonly kind = "webrtc" as const;
  private pc: RTCPeerConnection | null = null;
  private ctrl: RTCDataChannel | null = null;
  private frames: RTCDataChannel | null = null;
  private input: RTCDataChannel | null = null;
  private receiver: RTCRtpReceiver | null = null;
  private statsTimer: ReturnType<typeof setInterval> | null = null;
  private opened = false;

  constructor(private h: RemoteHandlers, private preferH264: boolean) {}

  /** Establish the PeerConnection. Resolves true once the ctrl channel is open. */
  async connect(timeoutMs: number): Promise<boolean> {
    this.h.onStage?.("route");
    const iceServers = await fetchIceServers();
    const pc = new RTCPeerConnection({ iceServers });
    this.pc = pc;

    // Звук виртуального браузера: просим m=audio recvonly всегда. Агент с
    // поднятым sink'ом добавит Opus-трек; старые агенты и обычный десктоп
    // отвечают без audio m-line — pion и браузер переваривают это штатно.
    pc.addTransceiver("audio", { direction: "recvonly" });
    pc.ontrack = (ev) => {
      if (ev.track.kind === "video") {
        this.receiver = ev.receiver;
        // Latency mode: ask the jitter buffer to hold ~nothing. This is THE
        // single biggest cut on the cloud path (the default buffer sits at
        // 50–150ms). Trade-off: on loss we prefer a brief freeze (NACK/PLI
        // recovers) over smooth-but-late video — right for remote control.
        // Units: jitterBufferTarget is MILLISECONDS, playoutDelayHint SECONDS.
        setJitterTarget(ev.receiver, 0);
        const stream = ev.streams[0] || new MediaStream([ev.track]);
        this.h.onVideoTrack?.(stream);
      } else if (ev.track.kind === "audio") {
        const stream = ev.streams[0] || new MediaStream([ev.track]);
        this.h.onAudioTrack?.(stream);
      }
    };

    // Request an H.264 video track when the browser can decode it. The agent
    // attaches a sendonly H.264 track if it has an encoder; otherwise it streams
    // JPEG over the frames channel below (universal fallback).
    if (this.preferH264 && supportsH264Receive()) {
      pc.addTransceiver("video", { direction: "recvonly" });
    }

    // Client creates the channels (it makes the offer); the agent picks them up
    // via OnDataChannel. ctrl = reliable+ordered JSON; frames = unreliable binary;
    // input = unreliable/unordered pointer-move lane (agents ≥2.17 consume it,
    // older ones just ignore the unknown label — moves then ride ctrl).
    const ctrl = pc.createDataChannel("ctrl", { ordered: true });
    const frames = pc.createDataChannel("frames", { ordered: false, maxRetransmits: 0 });
    const input = pc.createDataChannel("input", { ordered: false, maxRetransmits: 0 });
    frames.binaryType = "arraybuffer";
    this.ctrl = ctrl;
    this.frames = frames;
    this.input = input;

    ctrl.onmessage = (ev) => {
      try { this.h.onJSON(JSON.parse(ev.data)); } catch { /* malformed */ }
    };
    frames.onmessage = (ev) => {
      if (ev.data instanceof ArrayBuffer) this.h.onFrame(ev.data);
    };

    pc.onconnectionstatechange = () => {
      const st = pc.connectionState;
      this.h.onRtcState?.(st);
      if (st === "failed" || st === "closed" || st === "disconnected") {
        if (this.opened) this.h.onClose();
      }
    };

    // Non-trickle: gather all candidates, then exchange complete SDP once.
    this.h.onStage?.("offer");
    const offer = await pc.createOffer();
    await pc.setLocalDescription(offer);
    await waitIceGathering(pc, ICE_GATHER_TIMEOUT);

    this.h.onStage?.("signaling");
    const answer = await rtcSignalOffer({
      type: "offer",
      sdp: pc.localDescription?.sdp ?? offer.sdp ?? "",
      iceServers,
    });
    if (!answer?.sdp) return false;
    await pc.setRemoteDescription({ type: "answer", sdp: answer.sdp });

    this.h.onStage?.("webrtc");
    const ok = await waitChannelOpen(ctrl, timeoutMs);
    if (ok) {
      this.opened = true;
      this.startStats();
      this.h.onOpen(this);
    }
    return ok;
  }

  send(json: any) {
    if (this.ctrl?.readyState === "open") this.ctrl.send(JSON.stringify(json));
  }

  sendMove(json: any): boolean {
    const dc = this.input;
    // bufferedAmount guard: if the unreliable lane is somehow backed up, let
    // the caller coalesce instead of queuing stale positions.
    if (!dc || dc.readyState !== "open" || dc.bufferedAmount > 4096) return false;
    try { dc.send(JSON.stringify(json)); return true; } catch { return false; }
  }

  // ── Receiver metrics + adaptive jitter target ──
  //
  // Every 2s: decompose latency (jitter buffer / decode), read loss and the
  // selected ICE route. On sustained loss the jitter target is raised to 80ms
  // (smoothness), back to 0 once the link is clean — freezes are worse than
  // +80ms only while packets are actually dying.
  private prev: Record<string, number> = {};
  private lossyWins = 0;
  private cleanWins = 0;
  private jbTarget = 0;

  private startStats() {
    this.statsTimer = setInterval(() => {
      this.sampleStats().catch(() => { /* stats are best-effort */ });
    }, 2000);
  }

  private async sampleStats() {
    const pc = this.pc;
    if (!pc || pc.connectionState !== "connected") return;
    const report = await pc.getStats();
    const out: RtcClientStats = { jbTarget: this.jbTarget };
    let selectedPairId = "";
    const pairs: Record<string, any> = {};
    const cands: Record<string, any> = {};
    report.forEach((st: any) => {
      if (st.type === "inbound-rtp" && st.kind === "video") {
        if (typeof st.bytesReceived === "number") out.receivedBytes = st.bytesReceived;
        const d = (k: string) => {
          const v = (st[k] as number) ?? 0;
          const dv = v - (this.prev[k] ?? 0);
          this.prev[k] = v;
          return dv;
        };
        const emitted = d("jitterBufferEmittedCount");
        const jbDelay = d("jitterBufferDelay"); // seconds, cumulative
        if (emitted > 0) out.jbMs = Math.round((jbDelay / emitted) * 1000);
        const decoded = d("framesDecoded");
        const decTime = d("totalDecodeTime"); // seconds, cumulative
        if (decoded > 0) {
          out.decMs = Math.round((decTime / decoded) * 1000 * 10) / 10;
          out.recvFps = Math.round(decoded / 2);
        }
        const lost = d("packetsLost");
        const recv = d("packetsReceived");
        if (recv + lost > 0) out.lossPct = Math.round((lost / (recv + lost)) * 1000) / 10;
      } else if (st.type === "transport" && st.selectedCandidatePairId) {
        selectedPairId = st.selectedCandidatePairId;
      } else if (st.type === "candidate-pair") {
        pairs[st.id] = st;
        if (!selectedPairId && st.selected) selectedPairId = st.id; // Firefox
      } else if (st.type === "local-candidate" || st.type === "remote-candidate") {
        cands[st.id] = st;
      }
    });
    const pair = pairs[selectedPairId];
    if (pair) {
      if (typeof pair.currentRoundTripTime === "number") {
        out.rtt = Math.round(pair.currentRoundTripTime * 1000);
      }
      // Route: relay on EITHER side means the media crosses the TURN server.
      const lc = cands[pair.localCandidateId];
      const rc = cands[pair.remoteCandidateId];
      const types = [lc?.candidateType, rc?.candidateType].filter(Boolean);
      out.route = types.includes("relay") ? "relay" : (lc?.candidateType || types[0] || "");
    }

    // Adaptive jitter target: 2 consecutive lossy windows → 80ms; 3 clean → 0.
    if ((out.lossPct ?? 0) > 2) {
      this.lossyWins++;
      this.cleanWins = 0;
      if (this.lossyWins >= 2 && this.jbTarget === 0) {
        this.jbTarget = 80;
        if (this.receiver) setJitterTarget(this.receiver, 80);
      }
    } else {
      this.cleanWins++;
      this.lossyWins = 0;
      if (this.cleanWins >= 3 && this.jbTarget !== 0) {
        this.jbTarget = 0;
        if (this.receiver) setJitterTarget(this.receiver, 0);
      }
    }
    out.jbTarget = this.jbTarget;
    this.h.onClientStats?.(out);
  }

  close() {
    if (this.statsTimer) { clearInterval(this.statsTimer); this.statsTimer = null; }
    try { this.input?.close(); } catch { /* ignore */ }
    try { this.frames?.close(); } catch { /* ignore */ }
    try { this.ctrl?.close(); } catch { /* ignore */ }
    try { this.pc?.close(); } catch { /* ignore */ }
  }
}

// setJitterTarget asks the receiver to hold targetMs of jitter buffer.
// jitterBufferTarget (standard, ms) with playoutDelayHint (legacy, SECONDS)
// as fallback; older WebViews support neither — silent no-op.
function setJitterTarget(receiver: RTCRtpReceiver, targetMs: number) {
  const r = receiver as any;
  try { r.jitterBufferTarget = targetMs; } catch { /* unsupported */ }
  try { r.playoutDelayHint = targetMs / 1000; } catch { /* unsupported */ }
}

// supportsH264Receive reports whether this engine advertises an H.264 video
// receive codec. Browsers/WebViews without it (some older iOS WKWebView /
// Telegram in-app) skip video and use the JPEG frames path.
function supportsH264Receive(): boolean {
  try {
    const getCaps = (RTCRtpReceiver as any).getCapabilities;
    if (!getCaps) return false;
    const caps = getCaps("video");
    return !!caps && Array.isArray(caps.codecs) &&
      caps.codecs.some((c: any) => /h264/i.test(c.mimeType || ""));
  } catch {
    return false;
  }
}

function waitIceGathering(pc: RTCPeerConnection, timeoutMs: number): Promise<void> {
  if (pc.iceGatheringState === "complete") return Promise.resolve();
  return new Promise((resolve) => {
    const finish = () => {
      pc.removeEventListener("icegatheringstatechange", check);
      clearTimeout(timer);
      resolve();
    };
    const check = () => { if (pc.iceGatheringState === "complete") finish(); };
    pc.addEventListener("icegatheringstatechange", check);
    const timer = setTimeout(finish, timeoutMs);
  });
}

function waitChannelOpen(dc: RTCDataChannel, timeoutMs: number): Promise<boolean> {
  if (dc.readyState === "open") return Promise.resolve(true);
  return new Promise((resolve) => {
    let settled = false;
    const done = (ok: boolean) => {
      if (settled) return;
      settled = true;
      dc.removeEventListener("open", onOpen);
      dc.removeEventListener("error", onFail);
      dc.removeEventListener("close", onFail);
      clearTimeout(timer);
      resolve(ok);
    };
    const onOpen = () => done(true);
    const onFail = () => done(false);
    dc.addEventListener("open", onOpen);
    dc.addEventListener("error", onFail);
    dc.addEventListener("close", onFail);
    const timer = setTimeout(() => done(false), timeoutMs);
  });
}

// ── Orchestration: WebRTC first, WebSocket fallback ─────────────

export interface ConnectOptions {
  /** Set false to skip WebRTC and go straight to WebSocket. */
  preferWebRTC?: boolean;
  /** Set false after a decoder failure to negotiate JPEG DataChannel mode. */
  preferH264?: boolean;
}

export function connectRemote(h: RemoteHandlers, opts: ConnectOptions = {}): RemoteConnection {
  let closed = false;
  let current: RemoteTransport | null = null;

  // Guard onOpen so a transport that opens AFTER the caller disposed is closed.
  const wrapped: RemoteHandlers = {
    ...h,
    onOpen: (t) => {
      if (closed) { t.close(); return; }
      h.onOpen(t);
    },
  };

  const rtcSupported = typeof RTCPeerConnection !== "undefined";

  (async () => {
    if (opts.preferWebRTC !== false && rtcSupported) {
      const rtc = new RtcTransport(wrapped, opts.preferH264 !== false);
      current = rtc;
      try {
        const ok = await rtc.connect(WEBRTC_CONNECT_TIMEOUT);
        if (closed) { rtc.close(); return; }
        if (ok) return; // onOpen already fired
        rtc.close();
      } catch (e) {
        rtc.close();
        if (isFatalSignalError(e) || isRemoteOffline(e)) {
          h.onError(e);
          h.onClose();
          return;
        }
      }
      if (closed) return;
    }
    // Fallback: WebSocket.
    try {
      h.onStage?.("websocket");
      current = new WsTransport(screenWSUrl(), wrapped);
    } catch (e) {
      h.onError(e);
      h.onClose();
    }
  })();

  return {
    close() {
      closed = true;
      current?.close();
      current = null;
    },
  };
}

// Auth/agent capacity/display refusals are not transport failures. Falling back
// to WS would only hide their machine code and then start a reconnect storm.
function isFatalSignalError(e: unknown): boolean {
  const status = Number((e as { status?: number } | null)?.status || 0);
  return status === 401 || status === 403 || status === 409 ||
    status === 429 || status === 503;
}

/**
 * «Компьютер не в сети» на этапе сигналинга.
 *
 * Релей отдаёт машинный код (`pc_offline` / `pc_timeout`, статус 502/504) — его
 * распознаёт общий isPcOffline. В LAN выключенный ПК не отвечает вовсе: fetch
 * падает без HTTP-статуса, поэтому статус 0 там тоже офлайн (в облаке статус 0
 * — скорее сеть телефона, и общий клиент офлайном его осознанно не считает).
 * Проверяем только настоящие сетевые сбои (`instanceof Error`): у `{t:"error"}`
 * от агента статуса нет вовсе, и без этой оговорки любой отказ агента в LAN
 * выглядел бы как выключенный компьютер.
 *
 * Фолбэк на WebSocket при таком отказе бесполезен — он тоже не дозвонится,
 * зато прячет причину и запускает лестницу «Переподключение… (7/10)» на минуту,
 * пока баннер сверху уже пишет «Компьютер не в сети».
 */
export function isRemoteOffline(e: unknown): boolean {
  if (isPcOffline(e)) return true;
  if (!(e instanceof Error) || getMode() === "cloud") return false;
  const status = Number((e as { status?: number }).status || 0);
  const code = String((e as { code?: string }).code || "");
  return status === 0 && code !== "timeout";
}
