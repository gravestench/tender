package paypal

import (
	"errors"
	"testing"
	"time"

	"github.com/gravestench/tender"
)

func paypalReportRow() map[string]string {
	return map[string]string{
		"column_type": "SB", "transaction_id": "C123", "paypal_reference_id": "O123", "paypal_reference_id_type": "ODR",
		"transaction_event_code": "T0006", "transaction_debit_or_credit": "CR", "gross_transaction_amount": "1000",
		"gross_transaction_currency": "USD", "fee_debit_or_credit": "DR", "fee_amount": "59", "fee_currency": "USD",
		"Tender Expected Merchant Account": "SELLER123", "Tender RH Account ID": "SELLER123", "Tender SH Account ID": "SELLER123",
		"Tender Report Version": "001", "Tender Report Complete": "true", "Tender Source Part ID": "STL-20261124.01.001.CSV",
		"Tender Source Section ID": "SELLER123-1", "Tender Batch ID": "SELLER123-20261124",
	}
}

func TestProjectSettlementCapture(t *testing.T) {
	at := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	f := paypalReportRow()
	l, err := ProjectSettlement(f, at)
	if err != nil {
		t.Fatal(err)
	}
	if l.Kind != tender.SettlementCaptured || l.GrossMinor != 1000 || l.FeeMinor != 59 || l.NetMinor != 941 || l.ProviderReference != "O123" || l.OriginalReference != "O123" || l.ModificationReference != "C123" || l.SourcePartID != f["Tender Source Part ID"] || l.SourceSectionID != f["Tender Source Section ID"] {
		t.Fatalf("capture projection: %+v", l)
	}
}

func TestProjectSettlementRefundNeedsAuthenticatedCaptureOrderJoin(t *testing.T) {
	at := time.Now().UTC()
	for _, amount := range []string{"400", "1000"} { // partial and full refunds
		for _, relatedType := range []string{"ODR", "TXN"} {
			f := paypalReportRow()
			f["transaction_id"] = "R123"
			f["transaction_event_code"] = "T1107"
			f["transaction_debit_or_credit"] = "DR"
			f["gross_transaction_amount"] = amount
			f["fee_debit_or_credit"] = "CR"
			f["fee_amount"] = "10"
			f["paypal_reference_id_type"] = relatedType
			if relatedType == "TXN" {
				f["paypal_reference_id"] = "C123"
			}
			if _, err := ProjectSettlement(f, at); !errors.Is(err, tender.ErrUnsupported) {
				t.Fatalf("%s refund of %s without capture to order join = %v", relatedType, amount, err)
			}
		}
	}
}

func TestProjectSettlementRejectsUnprovenRows(t *testing.T) {
	at := time.Now().UTC()
	for name, mutate := range map[string]func(map[string]string){
		"incomplete file set": func(f map[string]string) { f["Tender Report Complete"] = "false" },
		"wrong account":       func(f map[string]string) { f["Tender SH Account ID"] = "OTHER" },
		"missing section":     func(f map[string]string) { delete(f, "Tender Source Section ID") },
		"unknown version":     func(f map[string]string) { f["Tender Report Version"] = "099" },
		"other reference":     func(f map[string]string) { f["paypal_reference_id_type"] = "TXN" },
		"wrong direction":     func(f map[string]string) { f["transaction_debit_or_credit"] = "DR" },
		"cross currency fee":  func(f map[string]string) { f["fee_currency"] = "EUR" },
		"withdrawal":          func(f map[string]string) { f["transaction_event_code"] = "T0400" },
		"chargeback":          func(f map[string]string) { f["transaction_event_code"] = "T1201" },
	} {
		t.Run(name, func(t *testing.T) {
			f := paypalReportRow()
			mutate(f)
			if _, err := ProjectSettlement(f, at); err == nil {
				t.Fatal("ambiguous row projected")
			}
		})
	}
	f := paypalReportRow()
	f["transaction_event_code"] = "T0400"
	f["bank_reference_id"] = "BANK123"
	if _, err := ProjectPayout(f, at); !errors.Is(err, tender.ErrUnsupported) {
		t.Fatalf("payout = %v", err)
	}
}
