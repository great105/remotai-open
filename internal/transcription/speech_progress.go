package transcription

import (
	"bytes"
	"strings"
	"sync"
	"time"
)

// Never lock Service.mu here: worker.Close waits for stderr to drain.
// Only known stage names are retained; paths and transcript text are discarded.
type speechProgress struct {
	mu      sync.Mutex
	partial []byte
	stage   string
	started time.Time
}

func (p *speechProgress) reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.partial = nil
	p.stage, p.started = "preparing", time.Now()
}

func (p *speechProgress) Write(data []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := len(data)
	for len(data) > 0 {
		end := bytes.IndexByte(data, '\n')
		if end < 0 {
			if len(p.partial)+len(data) <= 4096 {
				p.partial = append(p.partial, data...)
			} else {
				p.partial = nil
			}
			break
		}
		if len(p.partial)+end <= 4096 {
			line := strings.TrimSpace(string(append(p.partial, data[:end]...)))
			switch {
			case strings.HasPrefix(line, "Извлекаю звук"):
				p.stage = "preparing"
			case strings.HasPrefix(line, "Загружаю локальную модель"), strings.HasPrefix(line, "Возвращаю модель"), strings.HasPrefix(line, "Запускаю "):
				p.stage = "loading"
			case strings.HasPrefix(line, "Распознаю речь"):
				p.stage = "recognizing"
			case strings.HasPrefix(line, "Сохраняю транскрипт"):
				p.stage = "finishing"
			}
		}
		p.partial = nil
		data = data[end+1:]
	}
	return n, nil
}

func (p *speechProgress) snapshot() *ModuleProgress {
	p.mu.Lock()
	defer p.mu.Unlock()
	return &ModuleProgress{Stage: p.stage, ElapsedSeconds: int64(time.Since(p.started) / time.Second)}
}

func (s *Service) snapshotJobLocked(job *Job) Job {
	copy := *job
	if copy.State == "running" && job.speech != nil {
		copy.Progress = job.speech.snapshot()
	}
	return copy
}
