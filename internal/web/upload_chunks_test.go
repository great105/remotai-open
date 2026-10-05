package web

import (
	"bytes"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseChunkParams(t *testing.T) {
	q := url.Values{}
	if _, ok, err := parseChunkParams(q); ok || err != nil {
		t.Fatalf("no params: want (false, nil), got ok=%v err=%v", ok, err)
	}
	q.Set("upload_id", "abc12345")
	if _, _, err := parseChunkParams(q); err == nil {
		t.Fatal("missing chunk/chunks: want error")
	}
	q.Set("chunk", "0")
	q.Set("chunks", "3")
	q.Set("offset", "12")
	q.Set("total_size", "36")
	cp, ok, err := parseChunkParams(q)
	if err != nil || !ok {
		t.Fatalf("valid: got ok=%v err=%v", ok, err)
	}
	if cp.id != "abc12345" || cp.index != 0 || cp.total != 3 ||
		!cp.hasOffset || cp.offset != 12 || cp.totalSize != 36 {
		t.Fatalf("parsed: %+v", cp)
	}
	q.Set("chunk", "3") // index >= total
	if _, _, err := parseChunkParams(q); err == nil {
		t.Fatal("chunk out of range: want error")
	}
	q.Set("chunk", "0")
	q.Set("upload_id", "../../etc")
	if _, _, err := parseChunkParams(q); err == nil {
		t.Fatal("path traversal in upload_id: want error")
	}
}

func TestChunkResumeFromOffset(t *testing.T) {
	id := "test-upload-resume"
	defer abortChunkUpload(id)

	first := chunkParams{
		id: id, index: 0, total: 2,
		offset: 0, totalSize: 11, hasOffset: true,
	}
	if _, final, err := appendChunk(first, bytes.NewReader([]byte("hello "))); err != nil || final {
		t.Fatalf("first: final=%v err=%v", final, err)
	}
	size, _, exists, err := chunkUploadStatus(id)
	if err != nil || !exists || size != 6 {
		t.Fatalf("status: exists=%v size=%d err=%v", exists, size, err)
	}

	second := chunkParams{
		id: id, index: 1, total: 2,
		offset: size, totalSize: 11, hasOffset: true,
	}
	part, final, err := appendChunk(second, bytes.NewReader([]byte("world")))
	if err != nil || !final {
		t.Fatalf("second: final=%v err=%v", final, err)
	}
	got, err := os.ReadFile(part)
	if err != nil || string(got) != "hello world" {
		t.Fatalf("assembled=%q err=%v", got, err)
	}
}

func TestChunkResumeRejectsGap(t *testing.T) {
	id := "test-upload-gap"
	defer abortChunkUpload(id)
	if err := os.WriteFile(chunkPartPath(id), []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := chunkParams{
		id: id, index: 1, total: 2,
		offset: 5, totalSize: 8, hasOffset: true,
	}
	if _, _, err := appendChunk(p, bytes.NewReader([]byte("def"))); err == nil {
		t.Fatal("want offset mismatch")
	} else if _, ok := err.(*chunkOffsetError); !ok {
		t.Fatalf("want *chunkOffsetError, got %T (%v)", err, err)
	}
}

func TestChunkCleanupRemovesOnlyExpiredParts(t *testing.T) {
	oldID := "test-upload-oldpart"
	freshID := "test-upload-fresh"
	defer abortChunkUpload(oldID)
	defer abortChunkUpload(freshID)
	if err := os.WriteFile(chunkPartPath(oldID), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(chunkPartPath(freshID), []byte("fresh"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(chunkPartPath(oldID), old, old); err != nil {
		t.Fatal(err)
	}
	lastChunkCleanup.Store(0)
	maybeCleanupChunkUploads(time.Now())
	if _, err := os.Stat(chunkPartPath(oldID)); !os.IsNotExist(err) {
		t.Fatalf("expired part still exists: %v", err)
	}
	if _, err := os.Stat(chunkPartPath(freshID)); err != nil {
		t.Fatalf("fresh part was removed: %v", err)
	}
}

func TestChunkAssembly(t *testing.T) {
	id := "test-upload-1"
	defer abortChunkUpload(id)
	partPath := chunkPartPath(id)
	defer os.Remove(partPath)

	parts := []string{"hello ", "chunked ", "world"}
	for i, data := range parts {
		cp := chunkParams{id: id, index: i, total: len(parts)}
		part, final, err := appendChunk(cp, bytes.NewReader([]byte(data)))
		if err != nil {
			t.Fatalf("chunk %d: %v", i, err)
		}
		if part != partPath {
			t.Fatalf("chunk %d: part path %q, want %q", i, part, partPath)
		}
		if want := i == len(parts)-1; final != want {
			t.Fatalf("chunk %d: final=%v, want %v", i, final, want)
		}
	}

	dst := filepath.Join(os.TempDir(), "tgc-test-assembled.bin")
	defer os.Remove(dst)
	if err := finishChunkUpload(partPath, dst, false); err != nil {
		t.Fatalf("finish: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "hello chunked world" {
		t.Fatalf("assembled %q", got)
	}
	// .part должен исчезнуть после rename
	if _, err := os.Stat(partPath); !os.IsNotExist(err) {
		t.Fatalf("part file still exists: %v", err)
	}
}

func TestChunkRetransmitTruncates(t *testing.T) {
	id := "test-upload-2"
	defer abortChunkUpload(id)

	first := chunkParams{id: id, index: 0, total: 2}
	if _, _, err := appendChunk(first, bytes.NewReader([]byte("garbage-attempt"))); err != nil {
		t.Fatal(err)
	}
	// Повторная попытка с тем же id: chunk=0 обязан обрезать старое содержимое.
	if _, _, err := appendChunk(first, bytes.NewReader([]byte("clean-"))); err != nil {
		t.Fatal(err)
	}
	second := chunkParams{id: id, index: 1, total: 2}
	part, final, err := appendChunk(second, bytes.NewReader([]byte("data")))
	if err != nil || !final {
		t.Fatalf("final chunk: final=%v err=%v", final, err)
	}
	got, err := os.ReadFile(part)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "clean-data" {
		t.Fatalf("retransmit: got %q", got)
	}
}
