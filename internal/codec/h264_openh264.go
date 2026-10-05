//go:build windows || linux

package codec

import (
	"errors"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
	"unsafe"
)

var codecDebug = os.Getenv("CODEC_DEBUG") != ""

// openh264 enum/const values, verified against codec_def.h / codec_app_def.h.
const (
	usageScreenContentRealTime = 1  // EUsageType.SCREEN_CONTENT_REAL_TIME
	videoFormatI420            = 23 // EVideoFormatType.videoFormatI420
	rcBitrateMode              = 1  // RC_MODES.RC_BITRATE_MODE
	complexityLow              = 0  // ECOMPLEXITY_MODE.LOW_COMPLEXITY
	profileBaseline            = 66 // EProfileIdc.PRO_BASELINE (SDP fmtp 42001f)

	frameTypeInvalid = 0
	frameTypeIDR     = 1
	frameTypeI       = 2
	frameTypeP       = 3
	frameTypeSkip    = 4

	maxLayerNumOfFrame = 128

	// ISVCEncoder vtable ordinals (C++ declaration order).
	mInitialize       = 0
	mInitializeExt    = 1
	mGetDefaultParams = 2
	mUninitialize     = 3
	mEncodeFrame      = 4
	mEncodeParamSets  = 5
	mForceIntraFrame  = 6
	mSetOption        = 7
	mGetOption        = 8

	// ENCODER_OPTION ids (head of the enum, stable across 1.x–2.x).
	encoderOptionFrameRate  = 4 // float*  — range [1, 30.0] per header
	encoderOptionBitrate    = 5 // SBitrateInfo*
	encoderOptionMaxBitrate = 6 // SBitrateInfo*

	spatialLayerAll = 4 // LAYER_NUM.SPATIAL_LAYER_ALL
)

// ── openh264 struct layouts (amd64 natural alignment matches the C structs;
//    sizes are asserted in the test: 24 / 80 / 56 / 7192) ──

type encParamBase struct {
	usageType     int32
	picWidth      int32
	picHeight     int32
	targetBitrate int32
	rcMode        int32
	maxFrameRate  float32
}

// sliceArgument mirrors SSliceArgument (MAX_SLICES_NUM_TMP=35) — 152 bytes.
type sliceArgument struct {
	sliceMode           uint32
	sliceNum            uint32
	sliceMbNum          [35]uint32
	sliceSizeConstraint uint32
}

// spatialLayerConfig mirrors SSpatialLayerConfig (2.3–2.5) — 200 bytes.
type spatialLayerConfig struct {
	videoWidth              int32
	videoHeight             int32
	frameRate               float32
	spatialBitrate          int32
	maxSpatialBitrate       int32
	profileIdc              int32
	levelIdc                int32
	dLayerQp                int32
	sliceArgument           sliceArgument
	videoSignalTypePresent  bool
	videoFormat             uint8
	fullRange               bool
	colorDescriptionPresent bool
	colorPrimaries          uint8
	transferCharacteristics uint8
	colorMatrix             uint8
	aspectRatioPresent      bool
	aspectRatio             int32
	aspectRatioExtWidth     uint16
	aspectRatioExtHeight    uint16
}

// encParamExt mirrors SEncParamExt of the 2.4.x release headers (920 bytes,
// incl. the trailing bFixRCOverShoot/iIdrBitrateRatio added in 2.1). The Go
// struct carries extra reserved tail so a DLL with a slightly LARGER struct
// (future fields appended) still reads/writes inside our memory. Offsets are
// sanity-checked at runtime against GetDefaultParams output (see initializeExt)
// and sizes are asserted in h264_windows_test.go — same discipline as the
// 40-byte layerBSInfo gotcha.
type encParamExt struct {
	usageType     int32
	picWidth      int32
	picHeight     int32
	targetBitrate int32
	rcMode        int32
	maxFrameRate  float32

	temporalLayerNum int32
	spatialLayerNum  int32
	spatialLayers    [4]spatialLayerConfig

	complexityMode        int32
	intraPeriod           uint32
	numRefFrame           int32
	spsPpsIdStrategy      int32
	prefixNalAddingCtrl   bool
	enableSSEI            bool
	simulcastAVC          bool
	_pad0                 [1]byte
	paddingFlag           int32
	entropyCodingModeFlag int32

	enableFrameSkip bool
	_pad1           [3]byte
	maxBitrate      int32
	maxQp           int32
	minQp           int32
	maxNalSize      uint32

	enableLongTermReference bool
	_pad2                   [3]byte
	ltrRefNum               int32
	ltrMarkPeriod           uint32

	multipleThreadIdc uint16
	useLoadBalancing  bool
	_pad3             [1]byte

	loopFilterDisableIdc    int32
	loopFilterAlphaC0Offset int32
	loopFilterBetaOffset    int32

	enableDenoise             bool
	enableBackgroundDetection bool
	enableAdaptiveQuant       bool
	enableFrameCroppingFlag   bool
	enableSceneChangeDetect   bool

	isLosslessLink  bool
	fixRCOverShoot  bool
	_pad4           [1]byte
	idrBitrateRatio int32

	reserved [64]byte // slack for future appended fields in newer DLLs
}

// bitrateInfo mirrors SBitrateInfo.
type bitrateInfo struct {
	layer   int32
	bitrate int32
}

type sourcePicture struct {
	colorFormat int32
	stride      [4]int32
	pData       [4]uintptr
	picWidth    int32
	picHeight   int32
	timeStamp   int64
	psnrY       bool
	psnrU       bool
	psnrV       bool
}

// layerBSInfo mirrors the SLayerBSInfo in the SHIPPING openh264 binary, which is
// 40 bytes and has NO trailing rPsnr[3] field (present in the master header but
// not the 2.4.x release DLL — verified by raw-memory inspection: a 56-byte stride
// misreads layer[1], dropping the IDR slice and leaving the browser undecodable).
type layerBSInfo struct {
	temporalId       uint8
	spatialId        uint8
	qualityId        uint8
	frameType        int32
	layerType        uint8
	subSeqId         int32
	nalCount         int32
	pNalLengthInByte unsafe.Pointer // int*  (C memory written by openh264)
	pBsBuf           unsafe.Pointer // unsigned char*
}

type frameBSInfo struct {
	layerNum  int32
	layerInfo [maxLayerNumOfFrame]layerBSInfo
	frameType int32
	frameSize int32
	timeStamp int64
}

type h264Encoder struct {
	lib         *dynLib
	destroyProc uintptr
	enc         unsafe.Pointer // ISVCEncoder*
	vtbl        unsafe.Pointer // its vtable
	w, h        int
	buf         *i420Buffer
	bsInfo      *frameBSInfo
	start       time.Time // monotonic base for honest frame timestamps
	extInit     bool      // InitializeExt succeeded (SEncParamExt layout verified)
	mu          sync.Mutex
	closed      bool
}

// FindOpenH264DLL locates openh264.dll in the given dirs (plus next to the exe).
// Returns "" if not found — the caller then falls back to the JPEG path.
func FindOpenH264DLL(dirs ...string) string {
	search := append([]string{}, dirs...)
	if exe, err := os.Executable(); err == nil {
		search = append(search, filepath.Dir(exe))
	}
	// На Linux библиотеку ставит пакетный менеджер (libopenh264-8 и родня), и
	// лежит она в системных каталогах, а не рядом с программой. Скачивать её
	// самим неоткуда: официальный CDN Cisco отдаёт .so только по отдельному
	// каналу и на прямые ссылки отвечает отказом — зато в дистрибутивах пакет
	// есть, с подписями и обновлениями.
	search = append(search, systemLibDirs...)
	for _, d := range search {
		if d == "" {
			continue
		}
		for _, name := range openh264Names {
			p := filepath.Join(d, name)
			if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
				return p
			}
		}
	}
	return ""
}

// NewH264 creates an openh264-backed H.264 encoder of fixed size (w,h must be
// even). dllPath must point at a loadable openh264 DLL (see FindOpenH264DLL /
// EnsureOpenH264DLL). Returns an error if the DLL/encoder can't be initialized,
// so the caller can fall back gracefully.
func NewH264(dllPath string, w, h, fps, bitrate int) (Encoder, error) {
	if dllPath == "" {
		return nil, errors.New("openh264 dll not available")
	}
	if w < 16 || h < 16 || w%2 != 0 || h%2 != 0 {
		return nil, fmt.Errorf("invalid encode size %dx%d", w, h)
	}
	if fps <= 0 {
		fps = 15
	}
	if bitrate <= 0 {
		bitrate = 2_000_000
	}

	lib, err := openDynLib(dllPath)
	if err != nil {
		return nil, err
	}
	createProc, err := lib.proc("WelsCreateSVCEncoder")
	if err != nil {
		lib.close()
		return nil, err
	}
	destroyProc, err := lib.proc("WelsDestroySVCEncoder")
	if err != nil {
		lib.close()
		return nil, err
	}

	e := &h264Encoder{
		lib:         lib,
		destroyProc: destroyProc,
		w:           w,
		h:           h,
		buf:         newI420Buffer(w, h),
		bsInfo:      &frameBSInfo{},
		start:       time.Now(),
	}

	// WelsCreateSVCEncoder(ISVCEncoder** ppEncoder) writes the object pointer into
	// e.enc (an unsafe.Pointer holding C memory). Reading the vtable is then a
	// plain pointer deref — no uintptr→Pointer round-trip for vet to flag.
	r := callAddr(createProc, uintptr(unsafe.Pointer(&e.enc)))
	if int32(r) != 0 || e.enc == nil {
		lib.close()
		return nil, fmt.Errorf("WelsCreateSVCEncoder rc=%d", int32(r))
	}
	e.vtbl = *(*unsafe.Pointer)(e.enc)

	if err := e.initializeExt(w, h, fps, bitrate); err != nil {
		// Layout mismatch or InitializeExt failure → the encoder object may be
		// half-initialized; recreate it and fall back to the base Initialize
		// (pre-2.17 behaviour: DLL derives its own low-latency defaults).
		if codecDebug {
			fmt.Printf("[codec-dbg] InitializeExt unavailable (%v), falling back to Initialize\n", err)
		}
		callAddr(e.destroyProc, uintptr(e.enc))
		e.enc = nil
		r := callAddr(createProc, uintptr(unsafe.Pointer(&e.enc)))
		if int32(r) != 0 || e.enc == nil {
			lib.close()
			return nil, fmt.Errorf("WelsCreateSVCEncoder(retry) rc=%d", int32(r))
		}
		e.vtbl = *(*unsafe.Pointer)(e.enc)
		param := encParamBase{
			usageType:     usageScreenContentRealTime,
			picWidth:      int32(w),
			picHeight:     int32(h),
			targetBitrate: int32(bitrate),
			rcMode:        rcBitrateMode,
			maxFrameRate:  float32(fps),
		}
		if rc := e.call(mInitialize, uintptr(unsafe.Pointer(&param))); int32(rc) != 0 {
			callAddr(e.destroyProc, uintptr(e.enc))
			lib.close()
			return nil, fmt.Errorf("openh264 Initialize rc=%d", int32(rc))
		}
	}
	return e, nil
}

// initializeExt configures the encoder through SEncParamExt for low latency:
// frame-skip OFF (skips showed as freezes), QP clamped for readable text,
// tight VBV (maxBitrate=target), scene-change detect for window switches.
// Returns an error if the DLL's struct layout doesn't match ours — the caller
// then falls back to the base Initialize.
func (e *h264Encoder) initializeExt(w, h, fps, bitrate int) error {
	if os.Getenv("REMOTAI_H264_BASEINIT") != "" {
		return errors.New("forced off via REMOTAI_H264_BASEINIT")
	}
	var p encParamExt
	if rc := e.call(mGetDefaultParams, uintptr(unsafe.Pointer(&p))); int32(rc) != 0 {
		return fmt.Errorf("GetDefaultParams rc=%d", int32(rc))
	}
	// Layout sanity probe: FillDefault() in every supported DLL (2.3–2.5) sets
	// fMaxFrameRate=30, iMaxQp=51, iMinQp=0. If any lands elsewhere through our
	// struct lens, offsets are wrong and poking fields would corrupt the config.
	if p.maxFrameRate <= 0 || p.maxFrameRate > 240 || p.maxQp != 51 || p.minQp != 0 {
		return fmt.Errorf("SEncParamExt layout mismatch (fps=%v maxQp=%d minQp=%d)",
			p.maxFrameRate, p.maxQp, p.minQp)
	}

	p.usageType = usageScreenContentRealTime
	p.picWidth = int32(w)
	p.picHeight = int32(h)
	p.targetBitrate = int32(bitrate)
	p.rcMode = rcBitrateMode
	p.maxFrameRate = float32(fps)
	p.temporalLayerNum = 1
	p.spatialLayerNum = 1
	p.complexityMode = complexityLow
	p.intraPeriod = 0             // IDR only on demand (first frame / PLI) — no periodic GOP
	p.entropyCodingModeFlag = 0   // CAVLC (baseline)
	p.enableFrameSkip = false     // skips render as freezes; server paces instead
	p.maxBitrate = int32(bitrate) // tight VBV = short encoder queue
	p.maxQp = 38                  // floor for text readability
	p.minQp = 10
	p.multipleThreadIdc = 1 // >1 degrades the screen-content path (A/B later)
	p.enableDenoise = false
	p.enableAdaptiveQuant = false // AQ smears static text
	p.enableSceneChangeDetect = true

	sl := &p.spatialLayers[0]
	sl.videoWidth = int32(w)
	sl.videoHeight = int32(h)
	sl.frameRate = float32(fps)
	sl.spatialBitrate = int32(bitrate)
	sl.maxSpatialBitrate = int32(bitrate)
	sl.profileIdc = profileBaseline
	sl.sliceArgument.sliceMode = 0 // SM_SINGLE_SLICE
	sl.sliceArgument.sliceNum = 1

	if rc := e.call(mInitializeExt, uintptr(unsafe.Pointer(&p))); int32(rc) != 0 {
		return fmt.Errorf("InitializeExt rc=%d", int32(rc))
	}
	e.extInit = true
	return nil
}

// SetBitrate retunes target+max bitrate live, without re-creating the encoder
// (re-creation costs an IDR + a visible stall). Implements codec.LiveTuner.
func (e *h264Encoder) SetBitrate(bps int) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return errors.New("encoder closed")
	}
	bi := bitrateInfo{layer: spatialLayerAll, bitrate: int32(bps)}
	// Max first: SetOption(BITRATE) clamps target against the current max.
	if rc := e.call(mSetOption, encoderOptionMaxBitrate, uintptr(unsafe.Pointer(&bi))); int32(rc) != 0 {
		return fmt.Errorf("SetOption(MAX_BITRATE) rc=%d", int32(rc))
	}
	bi = bitrateInfo{layer: spatialLayerAll, bitrate: int32(bps)}
	if rc := e.call(mSetOption, encoderOptionBitrate, uintptr(unsafe.Pointer(&bi))); int32(rc) != 0 {
		return fmt.Errorf("SetOption(BITRATE) rc=%d", int32(rc))
	}
	return nil
}

// SetFrameRate retunes the encoder's max frame rate live. The header documents
// a [1,30] range, but the DLL accepts more; we cap at 60 because that's where
// the CDP screencast source tops out — no browser decodes screen content
// faster with any benefit. Implements codec.LiveTuner.
func (e *h264Encoder) SetFrameRate(fps float64) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return errors.New("encoder closed")
	}
	f := float32(min(max(fps, 1), 60))
	if rc := e.call(mSetOption, encoderOptionFrameRate, uintptr(unsafe.Pointer(&f))); int32(rc) != 0 {
		return fmt.Errorf("SetOption(FRAME_RATE) rc=%d", int32(rc))
	}
	return nil
}

func (e *h264Encoder) call(ord int, args ...uintptr) uintptr {
	slot := unsafe.Add(e.vtbl, uintptr(ord)*unsafe.Sizeof(uintptr(0)))
	fn := *(*uintptr)(slot)
	all := make([]uintptr, 0, len(args)+1)
	all = append(all, uintptr(e.enc)) // `this` pointer
	all = append(all, args...)
	return callAddr(fn, all...)
}

func (e *h264Encoder) Width() int  { return e.w }
func (e *h264Encoder) Height() int { return e.h }

func (e *h264Encoder) EncodeRGBA(img image.Image, forceKeyframe bool) ([]byte, bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil, false, errors.New("encoder closed")
	}

	e.buf.fill(img)

	if forceKeyframe {
		// ForceIntraFrame(bool bIDR, int iLayerId=-1) — both args required at ABI.
		e.call(mForceIntraFrame, 1, ^uintptr(0))
	}

	var src sourcePicture
	src.colorFormat = videoFormatI420
	src.picWidth = int32(e.w)
	src.picHeight = int32(e.h)
	src.stride[0] = int32(e.w)
	src.stride[1] = int32(e.w / 2)
	src.stride[2] = int32(e.w / 2)
	src.pData[0] = uintptr(unsafe.Pointer(&e.buf.y[0]))
	src.pData[1] = uintptr(unsafe.Pointer(&e.buf.u[0]))
	src.pData[2] = uintptr(unsafe.Pointer(&e.buf.v[0]))
	// Honest monotonic timestamp: with variable capture pacing (skipped dup
	// frames, load) a synthetic ts+=1000/fps drifts from reality and misleads
	// the rate control.
	src.timeStamp = time.Since(e.start).Milliseconds()

	*e.bsInfo = frameBSInfo{}
	rc := e.call(mEncodeFrame, uintptr(unsafe.Pointer(&src)), uintptr(unsafe.Pointer(e.bsInfo)))
	runtime.KeepAlive(e.buf)

	if int32(rc) != 0 {
		return nil, false, fmt.Errorf("EncodeFrame rc=%d", int32(rc))
	}
	return e.collectBitstream()
}

// EncodeI420 encodes one frame supplied as I420 planes with explicit strides —
// a decoded CDP screencast JPEG (YCbCr 4:2:0: Y→Y, Cb→U, Cr→V) goes straight
// into openh264, skipping the per-pixel RGBA→I420 conversion entirely. The
// frame MUST match the encoder's fixed dimensions (the caller checks); the
// planes are only read during the synchronous EncodeFrame call.
// Implements codec.PlaneEncoder.
func (e *h264Encoder) EncodeI420(y, u, v []byte, yStride, cStride int, forceKeyframe bool) ([]byte, bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil, false, errors.New("encoder closed")
	}
	cw, ch := e.w/2, e.h/2
	if yStride < e.w || cStride < cw ||
		len(y) < yStride*(e.h-1)+e.w ||
		len(u) < cStride*(ch-1)+cw ||
		len(v) < cStride*(ch-1)+cw {
		return nil, false, fmt.Errorf("i420 planes too small for %dx%d (strides %d/%d, lens %d/%d/%d)",
			e.w, e.h, yStride, cStride, len(y), len(u), len(v))
	}

	if forceKeyframe {
		// ForceIntraFrame(bool bIDR, int iLayerId=-1) — both args required at ABI.
		e.call(mForceIntraFrame, 1, ^uintptr(0))
	}

	var src sourcePicture
	src.colorFormat = videoFormatI420
	src.picWidth = int32(e.w)
	src.picHeight = int32(e.h)
	src.stride[0] = int32(yStride)
	src.stride[1] = int32(cStride)
	src.stride[2] = int32(cStride)
	src.pData[0] = uintptr(unsafe.Pointer(&y[0]))
	src.pData[1] = uintptr(unsafe.Pointer(&u[0]))
	src.pData[2] = uintptr(unsafe.Pointer(&v[0]))
	src.timeStamp = time.Since(e.start).Milliseconds()

	*e.bsInfo = frameBSInfo{}
	rc := e.call(mEncodeFrame, uintptr(unsafe.Pointer(&src)), uintptr(unsafe.Pointer(e.bsInfo)))
	runtime.KeepAlive(y)
	runtime.KeepAlive(u)
	runtime.KeepAlive(v)

	if int32(rc) != 0 {
		return nil, false, fmt.Errorf("EncodeFrame rc=%d", int32(rc))
	}
	return e.collectBitstream()
}

// collectBitstream gathers the per-layer NAL output of the last EncodeFrame
// call into one Annex-B buffer and reports whether it carries a keyframe.
// Caller must hold e.mu.
func (e *h264Encoder) collectBitstream() ([]byte, bool, error) {
	// NOTE: openh264 (2.4.x) leaves the frame-level eFrameType/iFrameSizeInBytes
	// at 0 and reports real data per-layer, so we gather from the layers and
	// detect keyframes by scanning NAL unit types rather than trusting the flag.
	if e.bsInfo.layerNum <= 0 {
		return nil, false, nil
	}
	if codecDebug {
		ln := int(e.bsInfo.layerNum)
		if ln > maxLayerNumOfFrame {
			ln = maxLayerNumOfFrame
		}
		fmt.Printf("[codec-dbg] layerNum=%d\n", e.bsInfo.layerNum)
		for li := 0; li < ln; li++ {
			l := &e.bsInfo.layerInfo[li]
			fmt.Printf("[codec-dbg]   layer[%d] type=%d nalCount=%d pBsBuf=%p\n", li, l.frameType, l.nalCount, l.pBsBuf)
		}
	}

	var out []byte
	n := int(e.bsInfo.layerNum)
	if n > maxLayerNumOfFrame {
		n = maxLayerNumOfFrame
	}
	for li := 0; li < n; li++ {
		layer := &e.bsInfo.layerInfo[li]
		if layer.pBsBuf == nil || layer.pNalLengthInByte == nil || layer.nalCount <= 0 {
			continue
		}
		lens := unsafe.Slice((*int32)(layer.pNalLengthInByte), int(layer.nalCount))
		total := 0
		for _, l := range lens {
			total += int(l)
		}
		if total <= 0 {
			continue
		}
		data := unsafe.Slice((*byte)(layer.pBsBuf), total)
		out = append(out, data...)
	}
	if len(out) == 0 {
		return nil, false, nil
	}
	return out, annexBHasKeyframe(out), nil
}

// annexBHasKeyframe scans an Annex-B bitstream for an SPS (NAL type 7) or IDR
// slice (NAL type 5) — either marks the access unit as a decodable keyframe.
func annexBHasKeyframe(b []byte) bool {
	i := 0
	for i+3 < len(b) {
		// find start code 00 00 01 (3-byte) — 4-byte form has an extra leading 0.
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			nalType := b[i+3] & 0x1F
			if nalType == 5 || nalType == 7 {
				return true
			}
			i += 3
			continue
		}
		i++
	}
	return false
}

func (e *h264Encoder) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return
	}
	e.closed = true
	if e.enc != nil {
		e.call(mUninitialize)
		callAddr(e.destroyProc, uintptr(e.enc))
		e.enc = nil
	}
	if e.lib != nil {
		e.lib.close()
		e.lib = nil
	}
}
