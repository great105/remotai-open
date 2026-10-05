package license

import "testing"

// Цены в коде однажды уже разошлись с витриной: на сайте 599 ₽, в приложении
// $9. Этот тест пришпиливает их к канону (`ПРОДВИЖЕНИЕ/КАНОН-ПРОДУКТА.md`),
// чтобы расхождение ловилось прогоном, а не жалобой человека.
func TestPricingMatchesCanon(t *testing.T) {
	cases := []struct {
		tier       Tier
		name       string
		monthlyRUB int
		annualRUB  int
		monthlyUSD int
		annualUSD  int
		cloudDevs  int
	}{
		{TierFree, "Локально", 0, 0, 0, 0, 1},
		{TierPro, "Про", 90000, 900000, 1800, 18000, 5},
		{TierTeam, "Флит", 199000, 1990000, 3900, 39000, 25},
	}
	for _, c := range cases {
		t.Run(string(c.tier), func(t *testing.T) {
			if got := PlanNames[c.tier]; got != c.name {
				t.Errorf("название полки = %q, канон: %q", got, c.name)
			}
			if got := PricingRUB[c.tier]; got != c.monthlyRUB {
				t.Errorf("цена в месяц = %d коп., канон: %d", got, c.monthlyRUB)
			}
			if got := PricingAnnualRUB[c.tier]; got != c.annualRUB {
				t.Errorf("цена в год = %d коп., канон: %d", got, c.annualRUB)
			}
			if got := Pricing[c.tier]; got != c.monthlyUSD {
				t.Errorf("цена в месяц = %d центов, канон: %d", got, c.monthlyUSD)
			}
			if got := PricingAnnual[c.tier]; got != c.annualUSD {
				t.Errorf("цена в год = %d центов, канон: %d", got, c.annualUSD)
			}
			if got := Limits[c.tier].MaxDevices; got != c.cloudDevs {
				t.Errorf("устройств = %d, канон: %d", got, c.cloudDevs)
			}
		})
	}
}

// «Год = десять месяцев (два в подарок)» — правило канона, а не случайные числа.
func TestAnnualIsTenMonths(t *testing.T) {
	for _, tier := range []Tier{TierPro, TierTeam} {
		if want := PricingRUB[tier] * 10; PricingAnnualRUB[tier] != want {
			t.Errorf("%s: год = %d коп., ожидалось десять месяцев = %d",
				tier, PricingAnnualRUB[tier], want)
		}
		if want := Pricing[tier] * 10; PricingAnnual[tier] != want {
			t.Errorf("%s: год = %d центов, ожидалось десять месяцев = %d",
				tier, PricingAnnual[tier], want)
		}
	}
}
