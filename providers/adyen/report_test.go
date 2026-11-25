package adyen

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gravestench/tender"
)

func cardReportFields(kind string) map[string]string {
	return map[string]string{
		"Merchant Account": "TestMerchant", "Tender Merchant Account": "TestMerchant",
		"Psp Reference": "8813702551707653", "Merchant Reference": "capture-reference",
		"Payment Method": "visa", "Type": kind, "Modification Reference": "9913702551795048",
		"Gross Currency": "USD", "Net Currency": "USD", "Batch Number": "42",
		"Tender Source Part ID": "part-1", "Tender Source Section ID": "section-42",
	}
}

func TestProjectCardSettlement(t *testing.T) {
	now := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
	settled := cardReportFields("Settled")
	settled["Gross Credit (GC)"], settled["Net Credit (NC)"], settled["Commission (NC)"] = "100.00", "97.00", "3.00"
	line, err := ProjectCardSettlement(settled, now)
	if err != nil || line.Kind != tender.SettlementCaptured || line.OriginalReference != settled["Psp Reference"] || line.ModificationReference != settled["Modification Reference"] || line.ProviderReference != settled["Psp Reference"] || line.GrossMinor != 10000 || line.FeeMinor != 300 || line.NetMinor != 9700 || line.BatchID != "42" || line.SourcePartID != "part-1" || line.SourceSectionID != "section-42" || line.ID == "" {
		t.Fatalf("settled projection: %+v, %v", line, err)
	}
	if again, err := ProjectCardSettlement(settled, now.Add(time.Hour)); err != nil || again.ID != line.ID {
		t.Fatalf("same financial row changed identity: %+v, %v", again, err)
	}
	allFee := cardReportFields("Settled")
	allFee["Gross Credit (GC)"], allFee["Net Credit (NC)"], allFee["Commission (NC)"] = "0.01", "0.00", "0.01"
	if projected, err := ProjectCardSettlement(allFee, now); err != nil || projected.GrossMinor != 1 || projected.FeeMinor != 1 || projected.NetMinor != 0 {
		t.Fatalf("zero net settlement: %+v, %v", projected, err)
	}
	refunded := cardReportFields("Refunded")
	refunded["Gross Debit (GC)"], refunded["Net Debit (NC)"] = "25.00", "25.00"
	refunded["Commission (NC)"] = "0.00"
	line, err = ProjectCardSettlement(refunded, now)
	if err != nil || line.Kind != tender.SettlementRefunded || line.GrossMinor != -2500 || line.FeeMinor != 0 || line.NetMinor != -2500 {
		t.Fatalf("refund projection: %+v, %v", line, err)
	}
	chargeback := cardReportFields("Chargeback")
	chargeback["Gross Debit (GC)"], chargeback["Net Debit (NC)"] = "50.00", "57.50"
	chargeback["Commission (NC)"] = "7.50"
	line, err = ProjectCardSettlement(chargeback, now)
	if err != nil || line.Kind != tender.SettlementReturned || line.GrossMinor != -5000 || line.FeeMinor != 750 || line.NetMinor != -5750 {
		t.Fatalf("chargeback projection: %+v, %v", line, err)
	}
	for name, mutate := range map[string]func(map[string]string){
		"merchant":                     func(f map[string]string) { f["Merchant Account"] = "Other" },
		"currency":                     func(f map[string]string) { f["Net Currency"] = "EUR" },
		"missing modification":         func(f map[string]string) { delete(f, "Modification Reference") },
		"fee conflict":                 func(f map[string]string) { f["Commission (NC)"] = "2.00" },
		"fee split conflict":           func(f map[string]string) { f["Markup (NC)"] = "3.00" },
		"missing provenance":           func(f map[string]string) { delete(f, "Tender Source Part ID") },
		"oversized psp":                func(f map[string]string) { f["Psp Reference"] = "12345678901234567" },
		"oversized merchant reference": func(f map[string]string) { f["Merchant Reference"] = strings.Repeat("a", 81) },
		"wrong journal sign":           func(f map[string]string) { f["Net Debit (NC)"] = "97.00"; delete(f, "Net Credit (NC)") },
	} {
		t.Run(name, func(t *testing.T) {
			f := cardReportFields("Settled")
			f["Gross Credit (GC)"], f["Net Credit (NC)"], f["Commission (NC)"] = "100.00", "97.00", "3.00"
			mutate(f)
			if _, err := ProjectCardSettlement(f, now); err == nil {
				t.Fatal("accepted contradictory report row")
			}
		})
	}
	for _, typ := range []string{"Fee", "DepositCorrection", "SettledReversed", "ChargebackReversed"} {
		f := cardReportFields(typ)
		f["Gross Credit (GC)"], f["Net Credit (NC)"] = "100.00", "100.00"
		if _, err := ProjectCardSettlement(f, now); !errors.Is(err, tender.ErrUnsupported) {
			t.Errorf("%s: got %v, want unsupported", typ, err)
		}
	}
}

func TestProjectPayout(t *testing.T) {
	f := cardReportFields("MerchantPayout")
	f["Modification Reference"] = "TX4313726XT batch 42, TestMerchant"
	f["Gross Currency"] = ""
	f["Net Debit (NC)"] = "94.50"
	line, err := ProjectPayout(f, time.Now())
	if err != nil || line.AmountMinor != 9450 || line.PayoutReference != "TX4313726XT" || line.BatchID != "42" || line.SourcePartID != "part-1" || line.SourceSectionID != "section-42" || line.ID == "" {
		t.Fatalf("payout projection: %+v, %v", line, err)
	}
	repeated := map[string]string{}
	for k, v := range f {
		repeated[k] = v
	}
	repeated["Modification Reference"] = "TX4313726XT batch 42, TestMerchant batch 42, TestMerchant"
	if got, err := ProjectPayout(repeated, time.Now()); err != nil || got.PayoutReference != line.PayoutReference {
		t.Fatalf("documented repeated account and batch: %+v, %v", got, err)
	}
	noComma := map[string]string{}
	for k, v := range f {
		noComma[k] = v
	}
	// Adyen's transaction-level reconciliation example omits the comma.
	noComma["Modification Reference"] = "TX4313726XT batch 42 TestMerchant"
	if got, err := ProjectPayout(noComma, time.Now()); err != nil || got.PayoutReference != line.PayoutReference || got.AmountMinor != line.AmountMinor {
		t.Fatalf("documented no-comma payout: %+v, %v", got, err)
	}
	for _, bad := range []func(map[string]string){
		func(m map[string]string) { m["Net Credit (NC)"] = "94.50"; delete(m, "Net Debit (NC)") },
		func(m map[string]string) { m["Modification Reference"] = "batch 42, TestMerchant" },
		func(m map[string]string) { m["Modification Reference"] = "TX4313726XT batch 43, TestMerchant" },
		func(m map[string]string) { m["Merchant Account"] = "Other" },
		func(m map[string]string) { m["Modification Reference"] = "TX4313726XT batch 42, TestMerchantEvil" },
		func(m map[string]string) { m["Modification Reference"] = "TX4313726XT batch 42 TestMerchantEvil" },
		func(m map[string]string) { m["Modification Reference"] = "TX4313726XT batch 43 TestMerchant" },
		func(m map[string]string) {
			m["Modification Reference"] = "TX4313726XT batch 42, TestMerchant batch 43, TestMerchant"
		},
		func(m map[string]string) {
			m["Modification Reference"] = "TX4313726XT batch 42, TestMerchant batch 42, OtherMerchant"
		},
	} {
		copy := map[string]string{}
		for k, v := range f {
			copy[k] = v
		}
		bad(copy)
		if _, err := ProjectPayout(copy, time.Now()); err == nil {
			t.Fatal("accepted ambiguous payout")
		}
	}
}

func TestProjectAdjustment(t *testing.T) {
	f := cardReportFields("Fee")
	f["Psp Reference"], f["Merchant Reference"], f["Payment Method"] = "", "", ""
	f["Modification Reference"] = "Transaction Fees May 2026"
	f["Gross Currency"] = ""
	f["Net Debit (NC)"] = "1.50"
	a, err := ProjectAdjustment(f, time.Now())
	if err != nil || a.Type != "Fee" || a.SignedMinor != -150 || a.BatchID != "42" || a.SourcePartID != "part-1" || a.SourceSectionID != "section-42" || a.ID == "" {
		t.Fatalf("fee adjustment: %+v, %v", a, err)
	}
	f["Net Credit (NC)"] = "1.50"
	delete(f, "Net Debit (NC)")
	if _, err := ProjectAdjustment(f, time.Now()); err == nil {
		t.Fatal("credited fee accepted")
	}
	f["Type"] = "DepositCorrection"
	if _, err := ProjectAdjustment(f, time.Now()); !errors.Is(err, tender.ErrUnsupported) {
		t.Fatalf("unqualified reserve: %v", err)
	}
}
