package tender_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gravestench/tender"
	"github.com/gravestench/tender/store"
)

func testBankEvidence() tender.BankDepositEvidence {
	return tender.BankDepositEvidence{Scope: shop, BankAccountToken: "opaque-bank-token", MovementID: "movement-1", StatementSHA256: strings.Repeat("a", 64), PayoutReference: "TX123XT", Currency: "USD", AmountMinor: 7100, BookedAt: time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)}
}

func TestBankDepositRequiresTrustedPayoutHistory(t *testing.T) {
	e := openEngine(t, store.NewMemory(), &processor{}, shop)
	evidence := testBankEvidence()
	if _, err := e.ReconcileBankDeposit(t.Context(), "report-1", "payout-1", evidence); !errors.Is(err, tender.ErrUnsupported) {
		t.Fatalf("memory store has no trusted payout history: %v", err)
	}
	evidence.Scope.MerchantID = "other-shop"
	if _, err := e.ReconcileBankDeposit(t.Context(), "report-1", "payout-1", evidence); !errors.Is(err, tender.ErrValidation) {
		t.Fatalf("cross-scope bank evidence: %v", err)
	}
}
