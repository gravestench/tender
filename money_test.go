package tender

import (
	"errors"
	"math"
	"testing"
)

func TestMoneyValidation(t *testing.T) {
	for _, c := range []struct {
		code string
		exp  int
	}{{"USD", 2}, {"JPY", 0}, {"KWD", 3}, {"EUR", 2}, {"CAD", 2}, {"BHD", 3}, {"CLF", 4}} {
		m, err := NewMoney(1234, c.code)
		if err != nil || m.Minor != 1234 || m.Currency != c.code {
			t.Fatalf("NewMoney(%s) = %+v, %v", c.code, m, err)
		}
		if exp, err := CurrencyExponent(c.code); err != nil || exp != c.exp {
			t.Errorf("exponent(%s) = %d, %v", c.code, exp, err)
		}
	}
	for _, code := range []string{"", "usd", "ZZZ", "XAU", "XXX", "BGN"} {
		if _, err := NewMoney(1, code); !errors.Is(err, ErrValidation) {
			t.Errorf("accepted currency %q: %v", code, err)
		}
	}
	for _, n := range []int64{math.MinInt64, -1, 0} {
		m, err := NewMoney(n, "USD")
		if err != nil {
			t.Fatal("signed ledger amount:", err)
		}
		if err := m.ValidatePurchase(); !errors.Is(err, ErrValidation) {
			t.Errorf("purchase %d: %v", n, err)
		}
	}
	for _, c := range []struct {
		a, b, want int64
		ok         bool
	}{
		{math.MaxInt64 - 1, 1, math.MaxInt64, true}, {math.MinInt64 + 1, -1, math.MinInt64, true},
		{math.MaxInt64, 1, 0, false}, {math.MinInt64, -1, 0, false}, {math.MaxInt64, math.MinInt64, -1, true},
	} {
		got, err := (Money{c.a, "USD"}).Add(Money{c.b, "USD"})
		if c.ok && (err != nil || got != (Money{c.want, "USD"})) || !c.ok && !errors.Is(err, ErrValidation) {
			t.Errorf("%d + %d = %+v, %v", c.a, c.b, got, err)
		}
	}
	if _, err := (Money{200, "USD"}).Add(Money{100, "JPY"}); !errors.Is(err, ErrValidation) {
		t.Fatalf("currency mismatch: %v", err)
	}
}
