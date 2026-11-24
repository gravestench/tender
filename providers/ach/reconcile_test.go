package ach_test

import (
	"testing"
	"time"

	"github.com/gravestench/tender"
	"github.com/gravestench/tender/providers/ach"
)

func TestProjectSettlementForLedger(t *testing.T) {
	at := time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC)
	settled := ach.SettlementLine{Account: "acct", PSP: "psp", MerchantReference: "order.abcdefghijklmnopqrstuv", Modification: "mod", Type: "Settled", Batch: "batch", Gross: 10000, Net: 9950}
	line, err := ach.ProjectSettlement(settled, at)
	if err != nil {
		t.Fatal(err)
	}
	if line.ID == "" || line.Kind != tender.SettlementSettled || line.GrossMinor != 10000 || line.FeeMinor != 50 || line.NetMinor != 9950 || line.Currency != "USD" || line.ProviderReference != "psp" {
		t.Fatalf("projection: %+v", line)
	}
	again, err := ach.ProjectSettlement(settled, at)
	if err != nil || again != line {
		t.Fatalf("nondeterministic: %+v %v", again, err)
	}
	returned := settled
	returned.Type, returned.Gross, returned.Net = "Chargeback", -10000, -10000
	r, err := ach.ProjectSettlement(returned, at)
	if err != nil {
		t.Fatal(err)
	}
	if r.ID == line.ID || r.Kind != tender.SettlementReturned || r.GrossMinor != -10000 || r.FeeMinor != 0 {
		t.Fatalf("return: %+v", r)
	}
	returned.PSP = "bad account no"
	if _, err := ach.ProjectSettlement(returned, at); err == nil {
		t.Fatal("accepted invalid projected row")
	}
}
