package tender

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
)

// ReportFeed names the provenance of a normalized settlement report. The host
// authenticates and downloads the source before requesting reconciliation.
// Adapters do not currently claim an automatic report download capability.
type ReportFeed string

const FeedHostVerified ReportFeed = "host_verified"

type SettlementEntryKind string

const (
	SettlementCaptured SettlementEntryKind = "captured"
	SettlementRefunded SettlementEntryKind = "refunded"
	SettlementSettled  SettlementEntryKind = "settled"
	SettlementReturned SettlementEntryKind = "returned"
)

// SettlementLine is a privacy-limited report projection. FeeMinor is the
// signed processor fee in the same currency; NetMinor must equal gross-fee.
// Negative gross represents money leaving the merchant.
type SettlementLine struct {
	ID, ProviderReference, Currency, PayoutReference string
	Kind                                             SettlementEntryKind
	GrossMinor, FeeMinor, NetMinor                   int64
	ObservedAt                                       time.Time
}

type SettlementReport struct {
	ID    string
	Feed  ReportFeed
	Lines []SettlementLine
}

type ReconciliationOutcome string

const (
	ReconciliationMatched          ReconciliationOutcome = "matched"
	ReconciliationUnmatched        ReconciliationOutcome = "unmatched_reference"
	ReconciliationCurrencyMismatch ReconciliationOutcome = "currency_mismatch"
	ReconciliationAmountMismatch   ReconciliationOutcome = "amount_mismatch"
	ReconciliationFeeMismatch      ReconciliationOutcome = "fee_mismatch"
	ReconciliationUnconfirmed      ReconciliationOutcome = "ledger_unconfirmed"
	ReconciliationReportConflict   ReconciliationOutcome = "report_conflict"
)

type ReconciliationResult struct {
	LineID, PaymentID, ExpectedCurrency, AttemptDigest string
	Outcome                                            ReconciliationOutcome
	ExpectedMinor                                      int64
	Line                                               SettlementLine
}

type ReconciliationRun struct {
	ID, Digest string
	Results    []ReconciliationResult
}

// ReconciliationDiscrepancy identifies the latest unresolved report result
// for host review, including a changed report identity.
type ReconciliationDiscrepancy struct {
	ReportID string
	Result   ReconciliationResult
}

// ReconciliationTx is an optional store extension. Each method participates
// in the Store transaction, including the immutable report and result writes.
type ReconciliationTx interface {
	ReconciliationRun(Scope, string, string, string) (ReconciliationRun, error)
	InsertReconciliationRun(Scope, string, string, ReconciliationRun) error
	InsertSettlementLine(Scope, string, string, string, SettlementLine) error
	ResolveSettlementReference(Scope, string, string, string) (Payment, OperationRecord, error)
	AppendReconciliationResult(Scope, string, string, string, ReconciliationResult) error
	FinishReconciliationRun(Scope, string, string, string) error
	ReconciliationResults(Scope, string, string, string) ([]ReconciliationResult, error)
}

func validSettlementLine(l SettlementLine) error {
	if strings.TrimSpace(l.ID) == "" || len(l.ID) > 200 || strings.TrimSpace(l.ProviderReference) == "" || len(l.ProviderReference) > 5000 || l.ObservedAt.IsZero() {
		return ErrValidation
	}
	if _, err := CurrencyExponent(l.Currency); err != nil {
		return err
	}
	switch l.Kind {
	case SettlementCaptured, SettlementSettled:
		if l.GrossMinor <= 0 {
			return ErrValidation
		}
	case SettlementRefunded, SettlementReturned:
		if l.GrossMinor >= 0 || l.GrossMinor == math.MinInt64 {
			return ErrValidation
		}
	default:
		return ErrValidation
	}
	return nil
}

func preparedReport(r SettlementReport) (string, error) {
	if strings.TrimSpace(r.ID) == "" || len(r.ID) > 200 || len(r.Lines) == 0 || len(r.Lines) > 10000 {
		return "", ErrValidation
	}
	ordered := slices.Clone(r.Lines)
	slices.SortFunc(ordered, func(a, b SettlementLine) int { return strings.Compare(a.ID, b.ID) })
	for i, l := range ordered {
		if err := validSettlementLine(l); err != nil {
			return "", err
		}
		if i > 0 && ordered[i-1].ID == l.ID {
			return "", ErrConflict
		}
	}
	r.Lines = ordered
	raw, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}

// Reconcile compares an authenticated host-projected report with the local
// ledger. It never changes payment funds or fulfillment notices. The host owns
// report authentication, scheduling, discrepancy review and fee accounting.
func (e *Engine) Reconcile(ctx context.Context, report SettlementReport) (ReconciliationRun, error) {
	if e == nil {
		return ReconciliationRun{}, ErrValidation
	}
	if report.Feed != FeedHostVerified {
		return ReconciliationRun{}, ErrUnsupported
	}
	digest, err := preparedReport(report)
	if err != nil {
		return ReconciliationRun{}, err
	}
	scope, provider, account := e.config.Scope, e.config.ProviderName, e.config.MerchantAccount
	var run ReconciliationRun
	var conflicted bool
	err = e.config.Store.Transact(ctx, func(base Tx) error {
		tx, ok := base.(ReconciliationTx)
		if !ok {
			return ErrUnsupported
		}
		prior, err := tx.ReconciliationRun(scope, provider, account, report.ID)
		if err == nil {
			if prior.Digest != digest {
				conflicted = true
				return tx.AppendReconciliationResult(scope, provider, account, report.ID, ReconciliationResult{LineID: "__report__", Outcome: ReconciliationReportConflict, AttemptDigest: digest})
			}
			history, err := tx.ReconciliationResults(scope, provider, account, report.ID)
			if err != nil {
				return err
			}
			latest := make(map[string]ReconciliationResult, len(history))
			for _, r := range history {
				latest[r.LineID] = r
			}
			for _, line := range report.Lines {
				result, err := e.matchSettlementLine(tx, line)
				if err != nil {
					return err
				}
				if result != latest[line.ID] {
					if err := tx.AppendReconciliationResult(scope, provider, account, report.ID, result); err != nil {
						return err
					}
				}
				prior.Results = append(prior.Results, result)
			}
			run = prior
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		run = ReconciliationRun{ID: report.ID, Digest: digest}
		if err := tx.InsertReconciliationRun(scope, provider, account, run); err != nil {
			return err
		}
		for _, line := range report.Lines {
			if err := tx.InsertSettlementLine(scope, provider, account, report.ID, line); err != nil {
				return err
			}
			result, err := e.matchSettlementLine(tx, line)
			if err != nil {
				return err
			}
			if err := tx.AppendReconciliationResult(scope, provider, account, report.ID, result); err != nil {
				return err
			}
			run.Results = append(run.Results, result)
		}
		return tx.FinishReconciliationRun(scope, provider, account, report.ID)
	})
	if err != nil {
		return ReconciliationRun{}, err
	}
	if conflicted {
		return ReconciliationRun{}, fmt.Errorf("%w: report identity was reused with changed contents", ErrConflict)
	}
	return run, nil
}

func (e *Engine) matchSettlementLine(tx ReconciliationTx, line SettlementLine) (ReconciliationResult, error) {
	r := ReconciliationResult{LineID: line.ID, Outcome: ReconciliationUnmatched, Line: line}
	p, op, err := tx.ResolveSettlementReference(e.config.Scope, e.config.ProviderName, e.config.MerchantAccount, line.ProviderReference)
	if errors.Is(err, ErrNotFound) {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	if p.Market != e.config.Market {
		return r, nil
	}
	r.PaymentID = p.ID
	r.ExpectedCurrency = p.Amount.Currency
	if line.Currency != p.Amount.Currency {
		r.Outcome = ReconciliationCurrencyMismatch
		return r, nil
	}
	if p.Status == PaymentCaptureFailed {
		r.Outcome = ReconciliationUnconfirmed
		return r, nil
	}
	var expected int64
	switch line.Kind {
	case SettlementCaptured:
		expected = p.Funds.Captured
	case SettlementRefunded:
		expected = p.Funds.Refunded
	case SettlementSettled:
		expected = p.Funds.Settled
	case SettlementReturned:
		expected = p.Funds.Returned
	}
	if op.ID != "" {
		kind := map[SettlementEntryKind]OperationKind{SettlementCaptured: OpCapture, SettlementRefunded: OpRefund}[line.Kind]
		if kind == "" || op.Kind != kind || op.Status != OperationSucceeded || expected < op.Amount.Minor {
			r.Outcome = ReconciliationUnconfirmed
			return r, nil
		}
		expected = op.Amount.Minor
	}
	r.ExpectedMinor = expected
	if expected == 0 {
		r.Outcome = ReconciliationUnconfirmed
		return r, nil
	}
	actual := line.GrossMinor
	if actual < 0 {
		actual = -actual
	}
	if actual != expected {
		r.Outcome = ReconciliationAmountMismatch
		return r, nil
	}
	if line.FeeMinor > 0 && line.GrossMinor < math.MinInt64+line.FeeMinor || line.FeeMinor < 0 && line.GrossMinor > math.MaxInt64+line.FeeMinor || line.GrossMinor-line.FeeMinor != line.NetMinor {
		r.Outcome = ReconciliationFeeMismatch
		return r, nil
	}
	r.Outcome = ReconciliationMatched
	return r, nil
}
