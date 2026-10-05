package input

import "testing"

type scrollOnly struct{ Controller }

type scrollBoth struct {
	Controller
	got []int
}

func (s *scrollBoth) ScrollH(dx int) error { s.got = append(s.got, dx); return nil }

// Горизонтальное колесо — необязательное умение: контроллер без него молчит,
// с ним получает dx как есть; ноль не доходит ни до кого.
func TestScrollHIsOptional(t *testing.T) {
	if err := ScrollH(scrollOnly{}, 3); err != nil {
		t.Fatalf("контроллер без ScrollH должен молчать, а не падать: %v", err)
	}
	both := &scrollBoth{}
	if err := ScrollH(both, -2); err != nil {
		t.Fatal(err)
	}
	if err := ScrollH(both, 0); err != nil {
		t.Fatal(err)
	}
	if len(both.got) != 1 || both.got[0] != -2 {
		t.Fatalf("ожидался один вызов с -2, получено %v", both.got)
	}
}
