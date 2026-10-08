package transcription

import (
	"io"
	"strings"
	"testing"
	"time"

	"tgcontrol/internal/agentdesk"
)

type closingProgressWorker struct {
	*fakeWorker
	stderr io.Writer
}

func (w *closingProgressWorker) Close() error {
	_, _ = w.stderr.Write([]byte("Сохраняю транскрипт и субтитры…\n"))
	return w.fakeWorker.Close()
}

func TestSpeechStagesAreBoundedAndPrivate(t *testing.T) {
	p := &speechProgress{}
	p.reset()
	_, _ = p.Write([]byte("Извлекаю звук из файла secret.webm…\nЗагружаю локаль"))
	if p.snapshot().Stage != "preparing" {
		t.Fatal("partial stage published")
	}
	_, _ = p.Write([]byte("ную модель large-v3-turbo…\r\nprivate transcript\n"))
	if p.snapshot().Stage != "loading" {
		t.Fatal("chunked UTF-8 stage lost")
	}
	_, _ = p.Write([]byte(strings.Repeat("x", 100_000)))
	_, _ = p.Write([]byte("\nРаспознаю речь на CPU…\n"))
	if p.snapshot().Stage != "recognizing" || len(p.partial) > 4096 {
		t.Fatal("unbounded diagnostic or missing stage")
	}
	_, _ = p.Write([]byte("Сохраняю транскрипт и субтитры…\n"))
	if p.snapshot().Stage != "finishing" {
		t.Fatal("missing finishing stage")
	}
	if p.snapshot().ElapsedSeconds < 0 {
		t.Fatal("invalid elapsed time")
	}
}

func TestSpeechProgressOwnershipAndCloseDoNotBlock(t *testing.T) {
	s, worker, path := fixture(t)
	worker.release = make(chan struct{})
	s.newWorker = func(config agentdesk.Config) (Worker, error) {
		return &closingProgressWorker{worker, config.Stderr}, nil
	}
	s.RecordUpload(7, path)
	job, err := s.Start(7, path, "small", "ru", "")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = s.speech.Write([]byte("Загружаю локальную модель small…\n"))
	visible, err := s.Get(7, job.ID)
	if err != nil || visible.Progress.Stage != "loading" {
		t.Fatal("real stage not visible")
	}
	if s.Status(8).Active != nil {
		t.Fatal("foreign job visible")
	}
	done := make(chan struct{})
	go func() { s.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancel/close blocked by progress")
	}
}
