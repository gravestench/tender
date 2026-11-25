package klarna

import (
	"errors"
	"testing"
	"time"

	"github.com/gravestench/tender"
)

var reportTime = time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

// Removing operation identity, payout attribution, or fee direction must fail these fixtures.
func TestProjectSettlement(t *testing.T) {
	base := map[string]string{
		"type": "SALE", "detailed_type": "PURCHASE", "amount": "20000", "order_id": "order-1", "capture_id": "capture-1", "refund_id": "",
		"payment_reference": "payout-1", "currency_code": "USD", "merchant_reference1": "cart-1",
		"Tender Payout Merchant ID": "K123456", "Tender Expected Merchant ID": "K123456", "Tender Payout Reference": "payout-1", "Tender Payout Settlement Type": "NET", "Tender Source Part ID": "part-1", "Tender Source Section ID": "section-1",
	}
	clone := func() map[string]string {
		c := make(map[string]string)
		for k, v := range base {
			c[k] = v
		}
		return c
	}
	sale, err := ProjectSettlement(base, reportTime)
	if err != nil {
		t.Fatal(err)
	}
	if sale.Kind != tender.SettlementCaptured || sale.GrossMinor != 20000 || sale.FeeMinor != 0 || sale.NetMinor != 20000 || sale.ProviderReference != "order-1" || sale.OriginalReference != "order-1" || sale.ModificationReference != "capture-1" || sale.BatchID != "payout-1" || sale.SourcePartID != "part-1" || sale.SourceSectionID != "section-1" || sale.Currency != "USD" {
		t.Fatalf("sale: %+v", sale)
	}
	second := clone()
	second["capture_id"] = "capture-2"
	second["amount"] = "15000"
	partial, err := ProjectSettlement(second, reportTime)
	if err != nil || partial.ID == sale.ID || partial.ModificationReference != "capture-2" || partial.GrossMinor != 15000 {
		t.Fatalf("partial: %+v %v", partial, err)
	}
	refund := clone()
	refund["type"] = "RETURN"
	refund["detailed_type"] = "PURCHASE_RETURN"
	refund["capture_id"] = ""
	refund["refund_id"] = "refund-1"
	refund["amount"] = "2500"
	returned, err := ProjectSettlement(refund, reportTime)
	if err != nil || returned.Kind != tender.SettlementRefunded || returned.GrossMinor != -2500 || returned.NetMinor != -2500 || returned.ModificationReference != "refund-1" {
		t.Fatalf("refund: %+v %v", returned, err)
	}
	for name, mutate := range map[string]func(map[string]string){
		"missing capture": func(f map[string]string) { delete(f, "capture_id") },
		"other merchant":  func(f map[string]string) { f["Tender Payout Merchant ID"] = "K654321" },
		"other currency":  func(f map[string]string) { f["currency_code"] = "EUR" },
		"gross variant":   func(f map[string]string) { f["Tender Payout Settlement Type"] = "GROSS" },
		"wrong payout": func(f map[string]string) {
			f["payment_reference"] = "payout-2"
			f["Tender Payout Reference"] = "payout-1"
		},
		"default": func(f map[string]string) {
			f["type"] = "RETURN"
			f["detailed_type"] = "PAYMENT_DEFAULT"
			f["refund_id"] = "refund-1"
			f["capture_id"] = ""
		},
		"reversal": func(f map[string]string) { f["type"] = "REVERSAL"; f["detailed_type"] = "REVERSAL" },
	} {
		t.Run(name, func(t *testing.T) {
			f := clone()
			mutate(f)
			_, err := ProjectSettlement(f, reportTime)
			if err == nil {
				t.Fatal("accepted ambiguous row")
			}
		})
	}
	duplicate, err := ProjectSettlement(clone(), reportTime)
	if err != nil || duplicate.ID != sale.ID {
		t.Fatalf("duplicate identity: %+v %v", duplicate, err)
	}
}

func TestProjectSettlementFeeAndPayout(t *testing.T) {
	fee := map[string]string{"type": "FEE", "detailed_type": "PURCHASE_FEE_FIXED", "amount": "35", "order_id": "order-1", "capture_id": "capture-1", "payment_reference": "payout-1", "currency_code": "USD", "Tender Payout Merchant ID": "K123456", "Tender Expected Merchant ID": "K123456", "Tender Payout Reference": "payout-1", "Tender Payout Settlement Type": "NET", "Tender Source Part ID": "part-1", "Tender Source Section ID": "section-1"}
	a, err := ProjectFee(fee, reportTime)
	if err != nil || a.SignedMinor != -35 || a.Type != "PURCHASE_FEE_FIXED:order-1:capture-1" || a.BatchID != "payout-1" {
		t.Fatalf("fee: %+v %v", a, err)
	}
	fee["detailed_type"] = "PURCHASE_FEE_PERCENTAGE"
	fee["amount"] = "600"
	b, err := ProjectFee(fee, reportTime)
	if err != nil || b.ID == a.ID || b.SignedMinor != -600 {
		t.Fatalf("percentage fee: %+v %v", b, err)
	}
	fee["detailed_type"] = "DISPUTE_FEE"
	if _, err := ProjectFee(fee, reportTime); !errors.Is(err, tender.ErrUnsupported) {
		t.Fatalf("dispute fee: %v", err)
	}
	payout := map[string]string{"payment_reference": "payout-1", "merchant_id": "K123456", "currency_code": "USD", "merchant_settlement_type": "NET", "payout_date": "2026-11-25T12:00:00Z", "totals.sale_amount": "35000", "totals.return_amount": "2500", "totals.fee_amount": "635", "totals.settlement_amount": "31865", "Tender Expected Merchant ID": "K123456", "Tender Source Part ID": "part-1", "Tender Source Section ID": "section-1"}
	for _, k := range []string{"commission_amount", "commission_reversal_amount", "repay_amount", "holdback_amount", "tax_amount", "fee_correction_amount", "reversal_amount", "release_amount", "charge_amount", "credit_amount", "fee_refund_amount", "tax_refund_amount", "deposit_amount", "opening_debt_balance_amount", "closing_debt_balance_amount"} {
		payout["totals."+k] = "0"
	}
	p, err := ProjectPayout(payout, reportTime)
	if err != nil || p.AmountMinor != 31865 || p.BatchID != "payout-1" || p.PayoutReference != "payout-1" {
		t.Fatalf("payout: %+v %v", p, err)
	}
	payout["totals.holdback_amount"] = "100"
	if _, err := ProjectPayout(payout, reportTime); !errors.Is(err, tender.ErrUnsupported) {
		t.Fatalf("unsupported holdback: %v", err)
	}
	payout["totals.holdback_amount"] = "0"
	payout["totals.new_movement_amount"] = "1"
	if _, err := ProjectPayout(payout, reportTime); !errors.Is(err, tender.ErrUnsupported) {
		t.Fatalf("unknown payout movement: %v", err)
	}
}
