package server

import "testing"

// Скрытая полка не должна попадать на витрину: «Проверка оплаты» за 10 ₽ нужна
// владельцу для живой проверки карт, а не покупателям.
func TestPublicPlansHideServiceTier(t *testing.T) {
	pub := publicPlans()
	for _, p := range pub {
		if p.ID == "test" {
			t.Fatalf("служебная полка попала на витрину: %+v", p)
		}
	}
	if len(pub) != 3 {
		t.Errorf("на витрине должны остаться три полки канона, получили %d", len(pub))
	}
	// А по идентификатору она находится — иначе оплатить её было бы нечем.
	if _, ok := planByID("test"); !ok {
		t.Error("скрытая полка обязана существовать для прямого обращения")
	}
}

// Цены витрины — из канона. Расхождение здесь означает, что сайт и касса
// назовут человеку разные суммы.
func TestCanonPrices(t *testing.T) {
	want := map[string]int{"local": 0, "pro": 90000, "fleet": 199000, "test": 1000}
	for id, sum := range want {
		p, ok := planByID(id)
		if !ok {
			t.Fatalf("нет полки %q", id)
		}
		if p.Monthly != sum {
			t.Errorf("%s: %d копеек, ожидали %d", id, p.Monthly, sum)
		}
	}
}

// ⚠ Проверочный платёж даёт СУТКИ, а не месяц: иначе «Про за десять рублей».
func TestTestTierGivesOneDay(t *testing.T) {
	if got := periodForTier("test"); got != testPeriod {
		t.Errorf("проверочная полка: %v, ожидали сутки", got)
	}
	if got := periodForTier("pro"); got != paidPeriod {
		t.Errorf("обычная полка: %v, ожидали месяц", got)
	}
}
