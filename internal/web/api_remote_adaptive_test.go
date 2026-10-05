package web

import (
	"testing"
	"time"
)

// ackWithRTT изображает подтверждение кадра, пришедшее через rtt: подделываем
// время отправки, потому что onACK меряет его сам (time.Since).
func ackWithRTT(a *adaptiveCtrl, seq uint32, rtt time.Duration, dropped int) {
	a.mu.Lock()
	a.sentTimes[seq] = time.Now().Add(-rtt)
	a.mu.Unlock()
	a.onACK(seq, dropped)
}

// Замер 23.08: зритель в Монголии, релей во Франкфурте — ping 263 мс, потерь
// нет. При абсолютном пороге «RTT > 200 мс = затор» такой маршрут обрушивал
// профиль «Авто» в пол (640 px / q35 / 5 fps) и не поднимался никогда, потому
// что порог подъёма (60 мс) физически недостижим. Здоровый дальний маршрут
// обязан удерживать читаемую картинку.
func TestAutoHoldsQualityOnStableDistantRoute(t *testing.T) {
	a := newAdaptiveCtrlWithLimits(30, 85, 1920)
	a.setClientHints(1500, 1) // окно ноутбука: cap ≈ 1500*1*1.15 = 1725

	const base = 263 * time.Millisecond
	for i := 0; i < 200; i++ {
		rtt := base + time.Duration(i%7)*time.Millisecond // естественный джиттер
		ackWithRTT(a, uint32(i), rtt, 0)
	}

	fps, quality, width, _, _ := a.get()
	if width < 1440 {
		t.Errorf("ширина съехала на стабильном дальнем маршруте: %d px (ждали ≥ 1440)", width)
	}
	if quality < 75 {
		t.Errorf("качество съехало на стабильном дальнем маршруте: %d (ждали ≥ 75)", quality)
	}
	if fps < 12 {
		t.Errorf("fps съехал на стабильном дальнем маршруте: %d (ждали ≥ 12)", fps)
	}
}

// Настоящий затор — это РОСТ RTT над базой маршрута, и отвечать на него надо
// плавностью: сначала fps, качество следом, ширина последней.
func TestCongestionSpendsFPSBeforeSharpness(t *testing.T) {
	a := newAdaptiveCtrlWithLimits(30, 85, 1920)
	a.setClientHints(1500, 1)

	const base = 263 * time.Millisecond
	for i := 0; i < 20; i++ { // набрали базу
		ackWithRTT(a, uint32(i), base, 0)
	}
	fps0, q0, w0, _, _ := a.get()

	// Затор: RTT втрое выше базы.
	for i := 100; i < 104; i++ {
		ackWithRTT(a, uint32(i), 3*base, 0)
	}
	fps1, q1, w1, _, _ := a.get()
	if fps1 >= fps0 {
		t.Errorf("fps не отдан первым при заторе: %d → %d", fps0, fps1)
	}
	if q1 != q0 || w1 != w0 {
		t.Errorf("чёткость отдана раньше плавности: quality %d→%d, width %d→%d", q0, q1, w0, w1)
	}

	// Затор держится — теперь очередь качества, ширина ещё держится.
	for i := 200; i < 210; i++ {
		ackWithRTT(a, uint32(i), 3*base, 0)
	}
	_, q2, w2, _, _ := a.get()
	if q2 >= q1 {
		t.Errorf("качество не снизилось при длительном заторе: %d", q2)
	}
	if w2 != w0 {
		t.Errorf("ширина ушла раньше качества: %d → %d", w0, w2)
	}
}

// База обязана уметь расти: маршрут ухудшился надолго (сменился VPN) — это
// новая норма, а не вечный затор.
func TestRTTBaselineFollowsWorseRoute(t *testing.T) {
	a := newAdaptiveCtrlWithLimits(30, 85, 1920)
	for i := 0; i < 20; i++ { // маршрут был быстрым
		ackWithRTT(a, uint32(i), 40*time.Millisecond, 0)
	}
	a.mu.Lock()
	if a.baseRTT > 45*time.Millisecond {
		t.Fatalf("база не поймала быстрый маршрут: %v", a.baseRTT)
	}
	a.mu.Unlock()

	// Маршрут стал дальним и остался таким. База догоняет за окно: первое
	// закрытие подводит итог окну, в котором быстрые замеры ещё были, второе —
	// окну, где их уже нет. Время в тесте двигаем сами, ждать 30 с нечестно.
	for window := 0; window < 2; window++ {
		a.mu.Lock()
		a.baseSince = time.Now().Add(-rttBaseWindow - time.Second)
		a.mu.Unlock()
		for i := 0; i < 5; i++ {
			ackWithRTT(a, uint32(100+window*10+i), 300*time.Millisecond, 0)
		}
	}

	a.mu.Lock()
	got := a.baseRTT
	a.mu.Unlock()
	if got < 250*time.Millisecond {
		t.Errorf("база застряла на старом маршруте: %v", got)
	}
}

// Потеря кадров — честный признак затора, реагируем сразу, не ожидая RTT.
func TestDroppedFramesDownshiftImmediately(t *testing.T) {
	a := newAdaptiveCtrlWithLimits(30, 85, 1920)
	a.setClientHints(1500, 1)
	for i := 0; i < 10; i++ {
		ackWithRTT(a, uint32(i), 50*time.Millisecond, 0)
	}
	fps0, _, _, _, _ := a.get()

	ackWithRTT(a, 100, 50*time.Millisecond, 2)
	fps1, _, _, _, _ := a.get()
	if fps1 >= fps0 {
		t.Errorf("потери кадров не снизили поток: %d → %d", fps0, fps1)
	}
}

// Потолок H.264 брался константой 1280 и резал картинку там, где тариф
// разрешает 1920.
func TestVideoWidthCapFollowsViewerAndTier(t *testing.T) {
	pro := newAdaptiveCtrlWithLimits(30, 85, 1920)
	pro.setClientHints(1500, 1)
	if got := pro.videoWidthCap(); got < 1700 {
		t.Errorf("Pro и окно 1500 px: потолок %d (ждали ≈1725)", got)
	}

	free := newAdaptiveCtrlWithLimits(12, 65, 1280)
	free.setClientHints(1500, 1)
	if got := free.videoWidthCap(); got != 1280 {
		t.Errorf("тариф с потолком 1280: получили %d", got)
	}

	phone := newAdaptiveCtrlWithLimits(30, 85, 1920)
	phone.setClientHints(390, 3)
	if got := phone.videoWidthCap(); got != 1280 {
		t.Errorf("узкое окно телефона должно оставлять 1280 для зума: %d", got)
	}
}
