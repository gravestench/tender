package tender

import (
	"encoding/json"
	"math"
	"math/big"
	"testing"
)

// A broken purchase validator would allow a decoded zero, negative or unknown
// currency amount to become a financial operation.
func FuzzMoneyJSON(f *testing.F) {
	for _, raw := range []string{
		`{"Minor":2599,"Currency":"USD"}`,
		`{"Minor":0,"Currency":"USD"}`,
		`{"Minor":-1,"Currency":"USD"}`,
		`{"Minor":1,"Currency":"ZZZ"}`,
		`{"Minor":9223372036854775808,"Currency":"USD"}`,
		`{"Minor":"2599","Currency":"USD"}`,
	} {
		f.Add([]byte(raw))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 4096 {
			return
		}
		var m Money
		if json.Unmarshal(raw, &m) != nil {
			return
		}
		validation := m.ValidatePurchase()
		if string(raw) == `{"Minor":2599,"Currency":"USD"}` && validation != nil {
			t.Fatalf("rejected a valid USD purchase: %v", validation)
		}
		if validation == nil {
			if m.Minor <= 0 {
				t.Fatalf("accepted a nonpositive purchase: %d", m.Minor)
			}
			if _, err := CurrencyExponent(m.Currency); err != nil {
				t.Fatalf("accepted unknown currency: %q", m.Currency)
			}
		}
	})
}

// Use arbitrary precision as an independent oracle for checked int64 addition.
func FuzzMoneyAdd(f *testing.F) {
	for _, pair := range [][2]int64{{1, 2}, {math.MaxInt64, 1}, {math.MinInt64, -1}, {math.MinInt64, math.MaxInt64}} {
		f.Add(pair[0], pair[1])
	}
	f.Fuzz(func(t *testing.T, a, b int64) {
		got, err := (Money{Minor: a, Currency: "USD"}).Add(Money{Minor: b, Currency: "USD"})
		want := new(big.Int).Add(big.NewInt(a), big.NewInt(b))
		if !want.IsInt64() {
			if err == nil {
				t.Fatalf("accepted int64 overflow for %d + %d", a, b)
			}
			return
		}
		if err != nil || got.Minor != want.Int64() || got.Currency != "USD" {
			t.Fatalf("Add(%d,%d) = %+v, %v; want %d USD", a, b, got, err, want.Int64())
		}
	})
}
