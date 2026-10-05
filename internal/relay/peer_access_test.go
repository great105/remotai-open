package relay

import "testing"

// Граница между «сосед по аккаунту» и «владелец» существует ровно в одном
// месте — в этой функции. Тест держит её на месте.
func TestРежимПоУмолчаниюПускаетТолькоДиагностику(t *testing.T) {
	allow := [][2]string{
		{"GET", "/api/health"},
		{"GET", "/api/system/network"},
		{"GET", "/api/system/boot-report"},
		{"GET", "/api/system/logs?tail=500"},
		{"POST", "/api/system/vpn"}, // ради этого доступ и заводился
	}
	for _, c := range allow {
		if ok, why := PeerAllowed("", c[0], c[1]); !ok {
			t.Errorf("%s %s должно быть можно в diag: %s", c[0], c[1], why)
		}
	}

	deny := [][2]string{
		{"GET", "/api/files?path=C:\\"},
		{"POST", "/api/pty"},
		{"GET", "/api/pty/abc/export"}, // содержимое чужой работы
		{"POST", "/api/system/power"},
		{"POST", "/api/settings/apply"},
		{"POST", "/api/system/kill"},
		{"GET", "/api/system/screenshot"},
	}
	for _, c := range deny {
		if ok, _ := PeerAllowed("diag", c[0], c[1]); ok {
			t.Errorf("%s %s не должно быть доступно соседу в режиме diag", c[0], c[1])
		}
	}
}

func TestOffЗакрываетВсё(t *testing.T) {
	if ok, why := PeerAllowed("off", "GET", "/api/health"); ok || why == "" {
		t.Fatalf("off обязан закрывать даже health и объяснять почему: ok=%v why=%q", ok, why)
	}
}

func TestFullПускаетВсё(t *testing.T) {
	if ok, _ := PeerAllowed("full", "POST", "/api/pty"); !ok {
		t.Fatal("full — это доступ как у владельца")
	}
}

// Метод не должен быть лазейкой: тот же путь на запись в diag закрыт.
func TestМетодУчитывается(t *testing.T) {
	if ok, _ := PeerAllowed("diag", "POST", "/api/system/logs"); ok {
		t.Fatal("POST на читающий путь не должен проходить")
	}
	if ok, _ := PeerAllowed("diag", "DELETE", "/api/system/network"); ok {
		t.Fatal("DELETE в diag недопустим")
	}
}

// Путь приходит от чужой стороны: хвостовой слэш и параметры не должны
// превращать закрытый путь в открытый (и наоборот).
func TestПутьНормализуется(t *testing.T) {
	if ok, _ := PeerAllowed("diag", "GET", "/api/system/network/"); !ok {
		t.Fatal("хвостовой слэш не должен закрывать разрешённый путь")
	}
	if ok, _ := PeerAllowed("diag", "GET", "/api/system/network/../files"); ok {
		t.Fatal("подмешанный путь не должен проходить по префиксу")
	}
}

func TestНормализацияРежима(t *testing.T) {
	cases := map[string]string{"": PeerAccessDiag, "  DIAG ": PeerAccessDiag, "full": PeerAccessFull,
		"OFF": PeerAccessOff, "мусор": PeerAccessDiag}
	for in, want := range cases {
		if got := NormalizePeerAccess(in); got != want {
			t.Errorf("NormalizePeerAccess(%q) = %q, ожидалось %q", in, got, want)
		}
	}
}
