package store

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gravestench/tender"
)

func memoryDeposit(scope tender.Scope, payout, movement, reference string) tender.BankDepositResult {
	return tender.BankDepositResult{ReportID: "report", PayoutID: payout, Outcome: tender.BankDepositMatched, Evidence: tender.BankDepositEvidence{Scope: scope, BankAccountToken: "bank-token", MovementID: movement, StatementSHA256: strings.Repeat("a", 64), PayoutReference: reference, Currency: "USD", AmountMinor: 100, BookedAt: time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)}}
}

func TestMemoryBankDepositClaims(t *testing.T) {
	m := NewMemory()
	scope := tender.Scope{MerchantID: "shop", Environment: tender.EnvironmentMock}
	claim := func(scope tender.Scope, r tender.BankDepositResult) (tender.BankDepositResult, error) {
		var got tender.BankDepositResult
		err := m.Transact(t.Context(), func(tx tender.Tx) error {
			var err error
			got, err = tx.(tender.BankDepositTx).StoreBankDepositResult(scope, "adyen", "acct", r)
			return err
		})
		return got, err
	}
	first := memoryDeposit(scope, "payout-a", "movement-a", "reference-a")
	if got, err := claim(scope, first); err != nil || got.Outcome != tender.BankDepositMatched {
		t.Fatalf("first claim: %+v, %v", got, err)
	}
	if got, err := claim(scope, first); err != nil || got != first {
		t.Fatalf("replay: %+v, %v", got, err)
	}
	changed := first
	changed.Evidence.StatementSHA256 = strings.Repeat("b", 64)
	if _, err := claim(scope, changed); !errors.Is(err, tender.ErrConflict) {
		t.Fatalf("changed evidence: %v", err)
	}
	reused := memoryDeposit(scope, "payout-b", "movement-a", "reference-b")
	if got, err := claim(scope, reused); err != nil || got.Outcome != tender.BankDepositReused {
		t.Fatalf("reused movement: %+v, %v", got, err)
	}
	other := scope
	other.MerchantID = "other-shop"
	independent := memoryDeposit(other, "payout-a", "movement-b", "reference-a")
	if got, err := claim(other, independent); err != nil || got.Outcome != tender.BankDepositMatched {
		t.Fatalf("cross-scope claim: %+v, %v", got, err)
	}
}

func TestMemoryConcurrentBankMovementClaim(t *testing.T) {
	m := NewMemory()
	scope := tender.Scope{MerchantID: "shop", Environment: tender.EnvironmentMock}
	var wg sync.WaitGroup
	outcomes := make(chan tender.BankDepositOutcome, 2)
	errors := make(chan error, 2)
	for _, payout := range []string{"payout-a", "payout-b"} {
		wg.Add(1)
		go func(payout string) {
			defer wg.Done()
			r := memoryDeposit(scope, payout, "shared-movement", "ref-"+payout)
			err := m.Transact(t.Context(), func(tx tender.Tx) error {
				var err error
				r, err = tx.(tender.BankDepositTx).StoreBankDepositResult(scope, "adyen", "acct", r)
				return err
			})
			if err != nil {
				errors <- err
				return
			}
			outcomes <- r.Outcome
		}(payout)
	}
	wg.Wait()
	close(outcomes)
	close(errors)
	for err := range errors {
		t.Fatal(err)
	}
	counts := map[tender.BankDepositOutcome]int{}
	for outcome := range outcomes {
		counts[outcome]++
	}
	if counts[tender.BankDepositMatched] != 1 || counts[tender.BankDepositReused] != 1 {
		t.Fatalf("concurrent claim outcomes: %+v", counts)
	}
}
