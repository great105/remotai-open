package web

import (
	"testing"
	"time"
)

// Прогрев запускается по возрасту САМОГО СВЕЖЕГО замера: пустой кэш (агент
// только поднялся) и кэш десятиминутной давности одинаково непригодны — по
// такому замеру «загрузка сейчас» превратилась бы в среднее за десять минут.
func TestCPUSnapFreshDrivesWarmup(t *testing.T) {
	cpuSnap = map[int32]cpuSample{}
	if got := cpuSnapFresh(); !got.IsZero() {
		t.Fatalf("на пустом кэше ожидали нулевое время, получили %v", got)
	}

	now := time.Now()
	cpuSnap = map[int32]cpuSample{
		11: {total: 1, at: now.Add(-30 * time.Minute)},
		12: {total: 2, at: now.Add(-2 * time.Minute)},
	}
	fresh := cpuSnapFresh()
	if !fresh.Equal(now.Add(-2 * time.Minute)) {
		t.Fatalf("ожидали время самого свежего замера, получили %v", fresh)
	}
	if now.Sub(fresh) <= cpuSnapMaxAge {
		t.Fatal("замер двухминутной давности обязан считаться устаревшим")
	}

	cpuSnap = map[int32]cpuSample{13: {total: 3, at: now.Add(-time.Second)}}
	if fresh := cpuSnapFresh(); now.Sub(fresh) > cpuSnapMaxAge {
		t.Fatal("свежий замер не должен запускать повторный прогрев")
	}
	cpuSnap = map[int32]cpuSample{}
}
