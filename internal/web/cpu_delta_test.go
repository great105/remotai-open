package web

import (
	"testing"
	"time"
)

// Первый замер не может ничего знать о загрузке — и обязан сказать это честно,
// а не выдать 0% за «процессор свободен».
func TestCPUDeltaFirstSampleIsWarmup(t *testing.T) {
	cpuSnap = map[int32]cpuSample{}
	now := time.Now()
	pct, ok := cpuDelta(1, 10, now, 4)
	if ok {
		t.Fatalf("первый замер обязан быть warmup, получили ok=true")
	}
	if pct != 0 {
		t.Fatalf("warmup обязан отдавать 0, получили %v", pct)
	}
}

// Ядро, занятое целиком, на 4-ядерной машине — это 25%% общей загрузки:
// gopsutil-подобное «среднее за жизнь» тут дало бы совсем другое число.
func TestCPUDeltaOneBusyCore(t *testing.T) {
	cpuSnap = map[int32]cpuSample{}
	start := time.Now()
	cpuDelta(2, 0, start, 4)

	pct, ok := cpuDelta(2, 2, start.Add(2*time.Second), 4)
	if !ok {
		t.Fatalf("второй замер обязан дать значение")
	}
	if pct < 24.5 || pct > 25.5 {
		t.Fatalf("ожидали ~25%%, получили %v", pct)
	}
}

// PID переиспользуются: «отрицательная» разница означает, что под тем же
// номером живёт уже другой процесс. Показать мусор хуже, чем промолчать.
func TestCPUDeltaHandlesPidReuse(t *testing.T) {
	cpuSnap = map[int32]cpuSample{}
	start := time.Now()
	cpuDelta(3, 100, start, 2)

	pct, ok := cpuDelta(3, 1, start.Add(time.Second), 2)
	if ok || pct != 0 {
		t.Fatalf("после переиспользования PID ждали (0,false), получили (%v,%v)", pct, ok)
	}
}

// Загрузка клампится в 0..100: рывок счётчика не должен рисовать «380%»,
// которые и были в отчёте аудита.
func TestCPUDeltaClamps(t *testing.T) {
	cpuSnap = map[int32]cpuSample{}
	start := time.Now()
	cpuDelta(4, 0, start, 1)

	pct, ok := cpuDelta(4, 50, start.Add(time.Second), 1)
	if !ok {
		t.Fatalf("ожидали значение")
	}
	if pct != 100 {
		t.Fatalf("ожидали кламп до 100, получили %v", pct)
	}
}

// Мёртвые процессы не должны копиться в кэше всё время работы агента.
func TestPruneCPUSnapDropsDeadProcesses(t *testing.T) {
	cpuSnap = map[int32]cpuSample{5: {total: 1, at: time.Now()}, 6: {total: 2, at: time.Now()}}
	pruneCPUSnap(map[int32]bool{5: true})
	if _, ok := cpuSnap[6]; ok {
		t.Fatalf("исчезнувший процесс остался в кэше")
	}
	if _, ok := cpuSnap[5]; !ok {
		t.Fatalf("живой процесс удалён из кэша")
	}
}
