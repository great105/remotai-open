//go:build windows

package pty

import "testing"

// Многокадровый снапшот собирается клиентом в один поток байтов: клиент читает
// frSnapshot тем же путём, что frOutput, поэтому кусков может быть сколько
// угодно (и старые версии клиента это тоже понимают).
func TestClientAssemblesChunkedSnapshot(t *testing.T) {
	wire, full := chunkedSnapshotWire(t)
	client := &pipeClient{}
	next := func() (frameType, []byte, error) { return readFrame(wire) }
	assertAssembled(t, func(buf []byte) (int, error) { return client.readDecoded(buf, next) }, full)
}
