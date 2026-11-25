package ach

import (
	"math/big"
	"testing"
)

// The host can hand the mapper an untrusted CSV export. A malformed decimal,
// direction or currency must not become a signed settlement amount.
func FuzzACHSettlementRow(f *testing.F) {
	for _, seed := range [][4]string{
		{"Settled", "12.34", "12.04", "USD"},
		{"Settled", "12.34", "13.00", "USD"},
		{"Chargeback", "12.34", "12.34", "USD"},
		{"Settled", "1e3", "1.00", "USD"},
		{"Settled", "12.34", "12.04", "EUR"},
	} {
		f.Add(seed[0], seed[1], seed[2], seed[3])
	}
	f.Fuzz(func(t *testing.T, kind, gross, net, currency string) {
		if len(kind)+len(gross)+len(net)+len(currency) > 512 {
			return
		}
		row := map[string]string{
			"Merchant Account": "merchant", "Psp Reference": "psp", "Merchant Reference": "order.aaaaaaaaaaaaaaaaaaaaaa",
			"Modification Reference": "mod", "Type": kind, "Batch Number": "batch", "Payment Method": "ach",
			"Gross Currency": currency, "Net Currency": currency,
		}
		if kind == "Chargeback" {
			row["Gross Debit (GC)"], row["Net Debit (NC)"] = gross, net
		} else {
			row["Gross Credit (GC)"], row["Net Credit (NC)"] = gross, net
		}
		line, err := MapSettlement(row)
		if err != nil {
			if kind == "Settled" && gross == "12.34" && net == "12.04" && currency == "USD" ||
				kind == "Chargeback" && gross == "12.34" && net == "12.34" && currency == "USD" {
				t.Fatalf("rejected a valid ACH report projection: %v", err)
			}
			return
		}
		if currency != "USD" || kind != "Settled" && kind != "Chargeback" {
			t.Fatalf("accepted unsupported ACH route: %+v", line)
		}
		for _, item := range []struct {
			raw string
			got int64
		}{{gross, line.Gross}, {net, line.Net}} {
			rat, ok := new(big.Rat).SetString(item.raw)
			if !ok {
				t.Fatalf("accepted malformed decimal %q", item.raw)
			}
			rat.Mul(rat, big.NewRat(100, 1))
			if !rat.IsInt() || !rat.Num().IsInt64() {
				t.Fatalf("accepted inexact or overflowing cents %q", item.raw)
			}
			want := rat.Num().Int64()
			if kind == "Chargeback" {
				want = -want
			}
			if item.got != want {
				t.Fatalf("projected %q as %d cents; want %d", item.raw, item.got, want)
			}
		}
		if kind == "Settled" && (line.Gross <= 0 || line.Net < 0 || line.Net > line.Gross) ||
			kind == "Chargeback" && (line.Gross >= 0 || line.Net > 0) {
			t.Fatalf("accepted impossible settlement direction: %+v", line)
		}
	})
}
