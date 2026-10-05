package pty

import (
	"encoding/binary"
	"fmt"
	"io"
)

// Wire protocol between remotai (client) and a persistent pty-host process
// (server) over a per-session IPC channel (named pipe on Windows, unix socket on
// Linux). The host owns the PTY + shell and survives a full restart of remotai;
// the client reconnects to it. Platform-neutral — the transport address is in
// addr_windows.go / addr_linux.go (pipeName).
//
// ProtocolVersion is the pipe wire version. Frame type numbers are APPEND-ONLY:
// never reuse or repurpose a value, only add new ones. Receivers MUST skip
// unknown frame types (read len, discard payload) rather than error — so a newer
// remotai.exe can talk to an OLDER host left running across an auto-update (the
// on-disk binary is replaced but the host process keeps the old code in RAM).
const ProtocolVersion uint16 = 1

// maxFrame caps a single frame payload — matches the WS read limit in api_pty.go.
const maxFrame = 1 << 20

type frameType byte

const (
	// Client → Host
	// frClientHello — [ver:2][cols:2][rows:2] и append-only хвост:
	// [known:8 LE][epoch_len:1][known_epoch:epoch_len]. Весь хвост
	// необязательный; частичный хвост без epoch тоже валиден.
	//
	// known — сколько байт этой сессии клиент уже получил. Нужно при
	// восстановлении связи на живой сессии: без него хост отдаёт ВЕСЬ свой буфер
	// (до 4 МБ), клиент трубы не отличает историю от живого потока, и вся
	// история разворачивается на экране заново — а поскольку она оседает в
	// кольце сессии, дубль увидит и любой подключившийся позже.
	//
	// Расширение совместимо в обе стороны и БЕЗ бампа ProtocolVersion:
	// старый хост читает ровно payload[0:6] и лишнее игнорирует (см. serveClient),
	// а новый хост, получив короткий hello, считает known нулём и ведёт себя
	// как прежде. Проверять надо именно это: цена ошибки в протоколе к хосту —
	// не лишний трафик, а неоткрывающийся терминал.
	frClientHello frameType = 0x01
	frInput       frameType = 0x02 // raw keystrokes → ConPTY stdin
	frResize      frameType = 0x03 // [cols:2][rows:2] + optional [seq:4] для frResizeAck/frResizeNack
	frPing        frameType = 0x05 // keepalive
	frKill        frameType = 0x06 // user closed the terminal: kill shell, exit host
	frDetach      frameType = 0x07 // client leaving, shell keeps running
	// 0x04 reserved (frQuery) — CWD is resolved client-side via OpenProcess.

	// Host → Client
	frHello    frameType = 0x81 // [ver:2][json HelloMsg]
	frOutput   frameType = 0x82 // raw ANSI from ConPTY (live)
	frSnapshot frameType = 0x83 // current scrollback, sent once right after Hello
	frPong     frameType = 0x85
	frExit     frameType = 0x86 // [code:4] — shell process exited on its own
	// frReplayEnd — последний кадр переигровки буфера отправлен, дальше только
	// живой поток. Шлётся СРАЗУ после последнего frSnapshot (и при пустом
	// снапшоте тоже), а не «когда появится новый вывод»: у тихой сессии вывода
	// может не быть часами, и без явной границы клиент трубы не узнает, что
	// переигровка кончилась, — зеркало так и останется с мусором переигровки
	// в scrollback (внешний аудит 2.57.18, находка P0-03). Append-only: старый
	// клиент пропускает неизвестный тип (см. readDecoded), новый клиент против
	// старого хоста живёт на прежнем правиле «первый frOutput после снапшота».
	frReplayEnd  frameType = 0x87
	frResizeAck  frameType = 0x88 // [seq:4], host уже применил frResize
	frResizeNack frameType = 0x89 // [seq:4], host окончательно отклонил frResize
	// 0x84 reserved (frQueryResult).
)

// HelloMsg is the JSON body of the host's frHello frame.
type HelloMsg struct {
	ShellPID uint32 `json:"shell_pid"`
	CWD      string `json:"cwd"`
	Shell    string `json:"shell"`
	Cols     int    `json:"cols"`
	Rows     int    `json:"rows"`
	Created  int64  `json:"created"` // unix ms
	// Modes — активные DEC private-режимы (alt-screen/mouse/…) по наблюдению
	// хоста. Хост живёт дольше сервера и видит ВСЕ байты с рождения сессии —
	// авторитетный источник для reset-resync после рестарта remotai.exe
	// (включающая ?1049h у долгоживущего TUI давно вытеснена из 512 КБ
	// scrollback-снапшота). JSON-поле: старый сервер молча игнорирует, старый
	// хост его не шлёт (тогда сервер берёт набор из pty.json) — бамп
	// ProtocolVersion не нужен.
	Modes []int `json:"modes,omitempty"`
	// StreamEpoch/ReplayStart/Produced — абсолютная шкала
	// долгоживущего pty-host. Remotai может перезапуститься и
	// создать новый Session, но offset не имеет права снова стать
	// относительным: иначе второй reconnect повторит кольцо.
	// Поля optional: старый агент их игнорирует, новый агент против
	// старого хоста видит пустой epoch и сохраняет legacy path.
	StreamEpoch string `json:"stream_epoch,omitempty"`
	ReplayStart uint64 `json:"replay_start,omitempty"`
	Produced    uint64 `json:"produced,omitempty"`
	// ResizeAck объявляет append-only resize completion protocol:
	// frResizeAck on success and (for a new host) frResizeNack on rejection.
	// Без флага клиент не ждёт ответ от старого хоста.
	ResizeAck bool `json:"resize_ack,omitempty"`
}

// hostStreamState — атомарно снятая вместе с snapshot шкала.
type hostStreamState struct {
	Epoch       string
	ReplayStart uint64
	Produced    uint64
}

// writeFrame encodes [type:1][len:4 LE][payload]. Callers serialize concurrent
// writes (pipeConn.writeFrame holds a mutex).
func writeFrame(w io.Writer, t frameType, payload []byte) error {
	if len(payload) > maxFrame {
		return fmt.Errorf("pty frame too large: %d", len(payload))
	}
	var hdr [5]byte
	hdr[0] = byte(t)
	binary.LittleEndian.PutUint32(hdr[1:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := w.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

// readFrame reads one frame. An oversized length is a protocol error. Callers
// treat an unknown frameType by ignoring its payload (already consumed here).
func readFrame(r io.Reader) (frameType, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.LittleEndian.Uint32(hdr[1:])
	if n > maxFrame {
		return 0, nil, fmt.Errorf("pty frame too large: %d", n)
	}
	var payload []byte
	if n > 0 {
		payload = make([]byte, n)
		if _, err := io.ReadFull(r, payload); err != nil {
			return 0, nil, err
		}
	}
	return frameType(hdr[0]), payload, nil
}

// helloPayload собирает тело frClientHello. known>0 добавляет 8 байт
// «столько я уже получил», а непустой knownEpoch — длину+строку.
// Старый хост весь хвост просто игнорирует.
func helloPayload(cols, rows int, known uint64, knownEpoch ...string) []byte {
	n := 6
	if known > 0 {
		n = 14
	}
	epoch := ""
	if len(knownEpoch) > 0 && known > 0 {
		epoch = knownEpoch[0]
		if len(epoch) > 255 {
			epoch = epoch[:255]
		}
		if epoch != "" {
			n = 15 + len(epoch)
		}
	}
	hb := make([]byte, n)
	binary.LittleEndian.PutUint16(hb[0:], ProtocolVersion)
	binary.LittleEndian.PutUint16(hb[2:], uint16(cols))
	binary.LittleEndian.PutUint16(hb[4:], uint16(rows))
	if known > 0 {
		binary.LittleEndian.PutUint64(hb[6:], known)
	}
	if epoch != "" {
		hb[14] = byte(len(epoch))
		copy(hb[15:], epoch)
	}
	return hb
}

func clientHelloKnownEpoch(payload []byte) string {
	if len(payload) < 15 {
		return ""
	}
	n := int(payload[14])
	if n == 0 || len(payload) < 15+n {
		return ""
	}
	return string(payload[15 : 15+n])
}
