//go:build linux || darwin

package pty

import "testing"

// То же, что на Windows, но через unix-клиент: на Linux и macOS транспорт —
// unix-сокет, и декодер у него свой. Тест держит обе реализации в согласии —
// иначе чанкованный снапшот собирался бы правильно только на одной платформе,
// а на другой терминал молча приходил бы пустым.
func TestUnixClientAssemblesChunkedSnapshot(t *testing.T) {
	wire, full := chunkedSnapshotWire(t)
	client := &sockClient{}
	next := func() (frameType, []byte, error) { return readFrame(wire) }
	assertAssembled(t, func(buf []byte) (int, error) { return client.readDecoded(buf, next) }, full)
}
