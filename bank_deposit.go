package tender

import (
	"context"
	"errors"
	"regexp"
	"time"
)

// BankDepositEvidence is a privacy-limited projection from a bank statement
// independently authenticated by the host. The bank account token is opaque;
// callers must never put an account number or raw statement in this record.
type BankDepositEvidence struct {
	Scope                                                                    Scope
	BankAccountToken, MovementID, StatementSHA256, PayoutReference, Currency string
	AmountMinor                                                              int64
	BookedAt                                                                 time.Time
}

type BankDepositOutcome string

const (
	BankDepositMatched             BankDepositOutcome = "matched"
	BankDepositProviderUnconfirmed BankDepositOutcome = "provider_unconfirmed"
	BankDepositReferenceMismatch   BankDepositOutcome = "reference_mismatch"
	BankDepositCurrencyMismatch    BankDepositOutcome = "currency_mismatch"
	BankDepositAmountMismatch      BankDepositOutcome = "amount_mismatch"
	BankDepositReused              BankDepositOutcome = "reused_bank_movement"
)

// BankDepositResult does not modify or replace the processor payout verdict.
type BankDepositResult struct {
	ReportID, PayoutID string
	Outcome            BankDepositOutcome
	Evidence           BankDepositEvidence
}

// BankDepositTx reads a persisted provider payout verdict and records a bank
// review in the same transaction as any unique movement/reference claim.
// A store without trusted payout history must return ErrUnsupported.
type BankDepositTx interface {
	PayoutResult(Scope, string, string, string, string) (PayoutReconciliationResult, error)
	StoreBankDepositResult(Scope, string, string, BankDepositResult) (BankDepositResult, error)
}

var bankDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)
var bankOpaque = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,200}$`)
var bankAccountToken = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9._:-]{0,199}$`)

func validBankEvidence(e BankDepositEvidence) bool {
	if !bankAccountToken.MatchString(e.BankAccountToken) || !bankOpaque.MatchString(e.MovementID) || !bankOpaque.MatchString(e.PayoutReference) || !bankDigest.MatchString(e.StatementSHA256) || e.AmountMinor <= 0 || e.BookedAt.IsZero() {
		return false
	}
	_, err := CurrencyExponent(e.Currency)
	return err == nil
}

// ReconcileBankDeposit compares one independent bank movement with a trusted
// provider payout result. It does not settle money, authorize fulfillment, or
// prove the host's statement authentication claim by itself.
func (e *Engine) ReconcileBankDeposit(ctx context.Context, reportID, payoutID string, evidence BankDepositEvidence) (BankDepositResult, error) {
	if e == nil || !validReportID(reportID) || !validReportID(payoutID) || evidence.Scope != e.config.Scope || !validBankEvidence(evidence) {
		return BankDepositResult{}, ErrValidation
	}
	evidence.BookedAt = evidence.BookedAt.UTC().Round(0)
	scope, provider, account := e.config.Scope, e.config.ProviderName, e.config.MerchantAccount
	var result BankDepositResult
	err := e.config.Store.Transact(ctx, func(base Tx) error {
		tx, ok := base.(BankDepositTx)
		if !ok {
			return ErrUnsupported
		}
		payout, err := tx.PayoutResult(scope, provider, account, reportID, payoutID)
		if err != nil {
			return err
		}
		result = BankDepositResult{ReportID: reportID, PayoutID: payoutID, Evidence: evidence, Outcome: BankDepositMatched}
		switch {
		case payout.Outcome != PayoutMatched:
			result.Outcome = BankDepositProviderUnconfirmed
		case payout.PayoutReference != evidence.PayoutReference:
			result.Outcome = BankDepositReferenceMismatch
		case payout.Currency != evidence.Currency:
			result.Outcome = BankDepositCurrencyMismatch
		case payout.ReportedMinor <= 0 || payout.ReportedMinor != evidence.AmountMinor:
			result.Outcome = BankDepositAmountMismatch
		}
		result, err = tx.StoreBankDepositResult(scope, provider, account, result)
		return err
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return BankDepositResult{}, ErrNotFound
		}
		return BankDepositResult{}, err
	}
	return result, nil
}
