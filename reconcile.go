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
	OriginalReference, ModificationReference         string
	BatchID, SourcePartID, SourceSectionID           string
	Kind                                             SettlementEntryKind
	GrossMinor, FeeMinor, NetMinor                   int64
	ObservedAt                                       time.Time
}

type SettlementReport struct {
	ID          string
	Feed        ReportFeed
	Lines       []SettlementLine
	Payouts     []PayoutLine
	Adjustments []PayoutAdjustment
	Parts       []ReportPart
	Sections    []ReportSection
}

// PayoutLine is a host-authenticated provider payout projected into a
// merchant-positive sign convention. It is not bank-deposit proof.
type PayoutLine struct {
	ID, BatchID, SourcePartID, SourceSectionID, Currency, PayoutReference string
	AmountMinor                                                           int64
	ObservedAt                                                            time.Time
}

// PayoutAdjustment is a signed, non-transactional batch movement such as a
// reserve. It must remain visible separately from captured/refunded principal.
type PayoutAdjustment struct {
	ID, BatchID, SourcePartID, SourceSectionID, Currency, Type string
	SignedMinor                                                int64
}

// ReportPart and ReportSection carry authenticated provider completeness
// claims. Counts and totals are checked against attributed financial rows.
type ReportPart struct {
	ID, SectionID, Digest            string
	Number, TotalParts, DeclaredRows int
}

type ReportSection struct {
	ID, BatchID, Currency                 string
	Number, TotalSections, DeclaredRows   int
	DeclaredNetMinor, DeclaredPayoutMinor *int64
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
	LineID, PaymentID, ExpectedCurrency, AttemptDigest, AdverseEventID string
	Outcome                                                            ReconciliationOutcome
	ExpectedMinor                                                      int64
	Line                                                               SettlementLine
}

type ReconciliationRun struct {
	ID, Digest    string
	Results       []ReconciliationResult
	PayoutResults []PayoutReconciliationResult
}

type PayoutOutcome string

const (
	PayoutMatched               PayoutOutcome = "matched"
	PayoutMissing               PayoutOutcome = "missing_payout"
	PayoutDuplicate             PayoutOutcome = "duplicate_payout"
	PayoutIncomplete            PayoutOutcome = "incomplete_artifact"
	PayoutProviderTotalMismatch PayoutOutcome = "provider_total_mismatch"
	PayoutCurrencyMismatch      PayoutOutcome = "currency_mismatch"
	PayoutAmountMismatch        PayoutOutcome = "amount_mismatch"
	PayoutUnconfirmed           PayoutOutcome = "ledger_unconfirmed"
)

type PayoutReconciliationResult struct {
	PayoutID, BatchID, Currency, PayoutReference     string
	Outcome                                          PayoutOutcome
	ExpectedMinor, ReportedMinor, BatchReportedMinor int64
	Payout                                           PayoutLine
}

// PayoutReconciliationDiscrepancy is the latest unresolved batch result for
// one scoped report, retained separately from transaction-line discrepancies.
type PayoutReconciliationDiscrepancy struct {
	ReportID string
	Result   PayoutReconciliationResult
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
	OperationByReference(Scope, string, string) (OperationRecord, error)
	AppendReconciliationResult(Scope, string, string, string, ReconciliationResult) error
	FinishReconciliationRun(Scope, string, string, string) error
	ReconciliationResults(Scope, string, string, string) ([]ReconciliationResult, error)
	InsertReconciliationPayoutInput(Scope, string, string, string, SettlementReport) error
	AppendPayoutReconciliationResult(Scope, string, string, string, PayoutReconciliationResult) error
	PayoutReconciliationResults(Scope, string, string, string) ([]PayoutReconciliationResult, error)
}

// ReturnEvidenceClaim binds one processed adverse receipt to one report row.
// A provider return must not match again under a different report identity.
type ReturnEvidenceClaim struct {
	ProviderEventID, LineID string
}

// ReturnEvidenceClaimTx atomically claims a complete set in the surrounding
// store transaction. A conflicting owner returns false without inserting any
// claims. The same report and line may replay its prior claim.
type ReturnEvidenceClaimTx interface {
	ClaimReturnEvidenceSet(Scope, string, string, string, []ReturnEvidenceClaim) (bool, error)
}

func validSettlementLine(l SettlementLine) error {
	if !validReportID(l.ID) || strings.TrimSpace(l.ProviderReference) == "" || len(l.ProviderReference) > 5000 || len(l.OriginalReference) > 5000 || len(l.ModificationReference) > 5000 || !validOptionalReportID(l.BatchID) || !validOptionalReportID(l.SourcePartID) || !validOptionalReportID(l.SourceSectionID) || l.ObservedAt.IsZero() {
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

func validReportID(v string) bool         { return strings.TrimSpace(v) != "" && len(v) <= 200 }
func validOptionalReportID(v string) bool { return v == "" || validReportID(v) }

func validPayoutLine(p PayoutLine) error {
	if !validReportID(p.ID) || !validOptionalReportID(p.BatchID) || !validOptionalReportID(p.SourcePartID) || !validOptionalReportID(p.SourceSectionID) || !validReportID(p.PayoutReference) || p.ObservedAt.IsZero() {
		return ErrValidation
	}
	_, err := CurrencyExponent(p.Currency)
	return err
}

func validPayoutAdjustment(a PayoutAdjustment) error {
	if !validReportID(a.ID) || !validOptionalReportID(a.BatchID) || !validOptionalReportID(a.SourcePartID) || !validOptionalReportID(a.SourceSectionID) || !validReportID(a.Type) {
		return ErrValidation
	}
	_, err := CurrencyExponent(a.Currency)
	return err
}

func validReportPart(p ReportPart) bool {
	return validReportID(p.ID) && validReportID(p.SectionID) && validReportID(p.Digest) && p.Number > 0 && p.TotalParts > 0 && p.Number <= p.TotalParts && p.TotalParts <= 10000 && p.DeclaredRows >= 0 && p.DeclaredRows <= 10000
}

func validReportSection(s ReportSection) bool {
	if !validReportID(s.ID) || !validReportID(s.BatchID) || s.DeclaredRows < 0 || s.DeclaredRows > 10000 || s.Number < 0 || s.TotalSections < 0 || s.Number > s.TotalSections || s.TotalSections > 10000 {
		return false
	}
	_, err := CurrencyExponent(s.Currency)
	return err == nil
}

func preparedReport(r SettlementReport) (string, error) {
	if !validReportID(r.ID) || len(r.Lines)+len(r.Payouts)+len(r.Adjustments) == 0 || len(r.Lines) > 10000 || len(r.Payouts) > 10000 || len(r.Adjustments) > 10000 || len(r.Parts) > 10000 || len(r.Sections) > 10000 {
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
	r.Payouts = slices.Clone(r.Payouts)
	slices.SortFunc(r.Payouts, func(a, b PayoutLine) int { return strings.Compare(a.ID, b.ID) })
	for i, p := range r.Payouts {
		if err := validPayoutLine(p); err != nil {
			return "", err
		}
		if i > 0 && r.Payouts[i-1].ID == p.ID {
			return "", ErrConflict
		}
	}
	r.Adjustments = slices.Clone(r.Adjustments)
	slices.SortFunc(r.Adjustments, func(a, b PayoutAdjustment) int { return strings.Compare(a.ID, b.ID) })
	for i, a := range r.Adjustments {
		if err := validPayoutAdjustment(a); err != nil {
			return "", err
		}
		if i > 0 && r.Adjustments[i-1].ID == a.ID {
			return "", ErrConflict
		}
	}
	r.Parts = slices.Clone(r.Parts)
	slices.SortFunc(r.Parts, func(a, b ReportPart) int { return strings.Compare(a.ID, b.ID) })
	for i, p := range r.Parts {
		if !validReportPart(p) {
			return "", ErrValidation
		}
		if i > 0 && r.Parts[i-1].ID == p.ID {
			return "", ErrConflict
		}
	}
	r.Sections = slices.Clone(r.Sections)
	slices.SortFunc(r.Sections, func(a, b ReportSection) int { return strings.Compare(a.ID, b.ID) })
	for i, s := range r.Sections {
		if !validReportSection(s) {
			return "", ErrValidation
		}
		if i > 0 && r.Sections[i-1].ID == s.ID {
			return "", ErrConflict
		}
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	if len(raw) > 16<<20 {
		return "", ErrValidation
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
	started := time.Now()
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
			results, payoutResults, err := e.reportResults(base, tx, report)
			if err != nil {
				return err
			}
			for _, result := range results {
				if !sameReconciliationResult(result, latest[result.LineID]) {
					if err := tx.AppendReconciliationResult(scope, provider, account, report.ID, result); err != nil {
						return err
					}
				}
				prior.Results = append(prior.Results, result)
			}
			priorPayouts, err := tx.PayoutReconciliationResults(scope, provider, account, report.ID)
			if err != nil {
				return err
			}
			latestPayouts := make(map[string]PayoutReconciliationResult, len(priorPayouts))
			for _, result := range priorPayouts {
				latestPayouts[result.PayoutID] = result
			}
			for _, result := range payoutResults {
				if !samePayoutResult(result, latestPayouts[result.PayoutID]) {
					if err := tx.AppendPayoutReconciliationResult(scope, provider, account, report.ID, result); err != nil {
						return err
					}
				}
				prior.PayoutResults = append(prior.PayoutResults, result)
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
		if err := tx.InsertReconciliationPayoutInput(scope, provider, account, report.ID, report); err != nil {
			return err
		}
		for _, line := range report.Lines {
			if err := tx.InsertSettlementLine(scope, provider, account, report.ID, line); err != nil {
				return err
			}
		}
		results, payoutResults, err := e.reportResults(base, tx, report)
		if err != nil {
			return err
		}
		for _, result := range results {
			if err := tx.AppendReconciliationResult(scope, provider, account, report.ID, result); err != nil {
				return err
			}
			run.Results = append(run.Results, result)
		}
		for _, result := range payoutResults {
			if err := tx.AppendPayoutReconciliationResult(scope, provider, account, report.ID, result); err != nil {
				return err
			}
			run.PayoutResults = append(run.PayoutResults, result)
		}
		return tx.FinishReconciliationRun(scope, provider, account, report.ID)
	})
	if err != nil {
		return ReconciliationRun{}, err
	}
	if conflicted {
		e.observe(ctx, OperationalEvent{Kind: OperationalReconciliation, Duration: time.Since(started), ErrorKind: ErrConflict, Discrepancies: 1})
		return ReconciliationRun{}, fmt.Errorf("%w: report identity was reused with changed contents", ErrConflict)
	}
	lines, payouts := operationalDiscrepancyCounts(run)
	e.observe(ctx, OperationalEvent{Kind: OperationalReconciliation, Duration: time.Since(started), Discrepancies: lines, PayoutDiscrepancies: payouts})
	return run, nil
}

func operationalDiscrepancyCounts(run ReconciliationRun) (int, int) {
	var lines, payouts int
	for _, result := range run.Results {
		if result.Outcome != ReconciliationMatched {
			lines++
		}
	}
	for _, result := range run.PayoutResults {
		if result.Outcome != PayoutMatched {
			payouts++
		}
	}
	return lines, payouts
}

// reportResults claims each verified return identity only after the entire
// projected report is internally matched. A conflicting claim demotes the
// returns and any affected payout in this same transaction.
func (e *Engine) reportResults(base Tx, tx ReconciliationTx, report SettlementReport) ([]ReconciliationResult, []PayoutReconciliationResult, error) {
	results, err := e.matchSettlementLines(base, tx, report.Lines)
	if err != nil {
		return nil, nil, err
	}
	payouts, err := reconcilePayouts(report, results)
	if err != nil {
		return nil, nil, err
	}
	claims := make([]ReturnEvidenceClaim, 0)
	complete := true
	for _, result := range results {
		if result.Outcome != ReconciliationMatched {
			complete = false
		}
		if result.Line.Kind == SettlementReturned && result.Outcome == ReconciliationMatched {
			if !validReportID(result.AdverseEventID) {
				complete = false
			}
			claims = append(claims, ReturnEvidenceClaim{ProviderEventID: result.AdverseEventID, LineID: result.LineID})
		}
	}
	for _, payout := range payouts {
		if payout.Outcome != PayoutMatched {
			complete = false
		}
	}
	if len(claims) == 0 {
		return results, payouts, nil
	}
	if complete {
		claimTx, ok := tx.(ReturnEvidenceClaimTx)
		if !ok {
			return nil, nil, ErrUnsupported
		}
		claimed, err := claimTx.ClaimReturnEvidenceSet(e.config.Scope, e.config.ProviderName, e.config.MerchantAccount, report.ID, claims)
		if err != nil {
			return nil, nil, err
		}
		if claimed {
			return results, payouts, nil
		}
	}
	for i := range results {
		if results[i].Line.Kind == SettlementReturned && results[i].Outcome == ReconciliationMatched {
			results[i].Outcome = ReconciliationUnconfirmed
		}
	}
	payouts, err = reconcilePayouts(report, results)
	return results, payouts, err
}

// Each successful modification operation or verified adverse identity can
// contribute principal once to a report. Split provider rows need a separate,
// proven aggregate contract.
func (e *Engine) matchSettlementLines(base Tx, tx ReconciliationTx, lines []SettlementLine) ([]ReconciliationResult, error) {
	type movementKey struct{ paymentID, modification string }
	seen := make(map[movementKey]int)
	results := make([]ReconciliationResult, len(lines))
	for i, line := range lines {
		result, err := e.matchSettlementLine(tx, line)
		if err != nil {
			return nil, err
		}
		results[i] = result
		principalMovement := line.Kind == SettlementCaptured || line.Kind == SettlementRefunded || line.Kind == SettlementReturned
		if !principalMovement || result.Outcome != ReconciliationMatched || (line.BatchID == "" && line.Kind != SettlementReturned) {
			continue
		}
		key := movementKey{result.PaymentID, line.ModificationReference}
		if prior, duplicate := seen[key]; duplicate {
			results[prior].Outcome = ReconciliationUnconfirmed
			results[i].Outcome = ReconciliationUnconfirmed
		} else {
			seen[key] = i
		}
	}
	// A partial report can cover fewer returns than the cumulative ledger, but
	// cannot claim more than its nonzero Returned total or collected principal.
	// With zero Returned, an exact verified notice is comparison evidence for
	// a review-only return; it still cannot exceed collected principal.
	returned := make(map[string][]int)
	for i, result := range results {
		if result.Line.Kind == SettlementReturned && result.Outcome == ReconciliationMatched {
			returned[result.PaymentID] = append(returned[result.PaymentID], i)
		}
	}
	for paymentID, indices := range returned {
		payment, err := base.Payment(e.config.Scope, paymentID)
		if err != nil {
			return nil, err
		}
		limit := max(payment.Funds.Captured, payment.Funds.Settled)
		if payment.Funds.Returned > 0 && payment.Funds.Returned < limit {
			limit = payment.Funds.Returned
		}
		var reported int64
		for _, i := range indices {
			reported, err = checkedReportSum(reported, results[i].ExpectedMinor)
			if err != nil {
				return nil, err
			}
		}
		if reported > limit {
			for _, i := range indices {
				results[i].Outcome = ReconciliationUnconfirmed
			}
		}
	}
	return results, nil
}

// JSONB stores the instant but not Go's monotonic reading or location.
func sameReconciliationResult(a, b ReconciliationResult) bool {
	aAt, bAt := a.Line.ObservedAt, b.Line.ObservedAt
	a.Line.ObservedAt, b.Line.ObservedAt = time.Time{}, time.Time{}
	return a == b && aAt.Equal(bAt)
}

func samePayoutResult(a, b PayoutReconciliationResult) bool {
	aAt, bAt := a.Payout.ObservedAt, b.Payout.ObservedAt
	a.Payout.ObservedAt, b.Payout.ObservedAt = time.Time{}, time.Time{}
	return a == b && aAt.Equal(bAt)
}

func checkedReportSum(a, b int64) (int64, error) {
	if b > 0 && a > math.MaxInt64-b || b < 0 && a < math.MinInt64-b {
		return 0, ErrValidation
	}
	return a + b, nil
}

// reconcilePayouts checks a provider's complete artifact claims and compares
// its merchant-positive batch payout with independently matched ledger rows.
// It only emits discrepancies; report rows never grant financial authority.
func reconcilePayouts(report SettlementReport, lines []ReconciliationResult) ([]PayoutReconciliationResult, error) {
	if len(report.Payouts)+len(report.Adjustments)+len(report.Parts)+len(report.Sections) == 0 {
		return nil, nil
	}
	parts := make(map[string]ReportPart, len(report.Parts))
	sections := make(map[string]ReportSection, len(report.Sections))
	partRows := make(map[string]int)
	sectionRows := make(map[string]int)
	sectionNet := make(map[string]int64)
	sectionPayouts := make(map[string][]PayoutLine)
	sectionUnconfirmed := make(map[string]bool)
	sectionIncomplete := make(map[string]bool)
	sectionCurrencyMismatch := make(map[string]bool)
	payoutReferences := make(map[string]int, len(report.Payouts))
	for _, payout := range report.Payouts {
		payoutReferences[payout.PayoutReference]++
	}
	for _, part := range report.Parts {
		if _, exists := parts[part.ID]; exists {
			return nil, ErrConflict
		}
		parts[part.ID] = part
	}
	for _, section := range report.Sections {
		if _, exists := sections[section.ID]; exists {
			return nil, ErrConflict
		}
		sections[section.ID] = section
	}
	for _, part := range report.Parts {
		if _, ok := sections[part.SectionID]; !ok {
			return nil, ErrValidation
		}
	}
	structured := len(report.Parts)+len(report.Sections) > 0
	attribute := func(partID, sectionID, batch, currency string) (string, bool, error) {
		if partID == "" || sectionID == "" || batch == "" {
			if structured {
				return "", false, ErrValidation
			}
			return "", false, nil
		}
		part, partOK := parts[partID]
		section, sectionOK := sections[sectionID]
		if !partOK || !sectionOK || part.SectionID != sectionID || section.BatchID != batch {
			return "", false, ErrValidation
		}
		partRows[partID]++
		sectionRows[sectionID]++
		return sectionID, section.Currency != currency, nil
	}
	for i, line := range report.Lines {
		sectionID, wrongCurrency, err := attribute(line.SourcePartID, line.SourceSectionID, line.BatchID, line.Currency)
		if err != nil {
			return nil, err
		}
		if sectionID == "" {
			continue
		}
		if wrongCurrency {
			sectionCurrencyMismatch[sectionID] = true
			continue
		}
		sectionNet[sectionID], err = checkedReportSum(sectionNet[sectionID], line.NetMinor)
		if err != nil {
			return nil, err
		}
		if i >= len(lines) || lines[i].LineID != line.ID || lines[i].Outcome != ReconciliationMatched {
			sectionUnconfirmed[sectionID] = true
		}
	}
	for _, adjustment := range report.Adjustments {
		sectionID, wrongCurrency, err := attribute(adjustment.SourcePartID, adjustment.SourceSectionID, adjustment.BatchID, adjustment.Currency)
		if err != nil {
			return nil, err
		}
		if sectionID == "" {
			continue
		}
		if wrongCurrency {
			sectionCurrencyMismatch[sectionID] = true
			continue
		}
		sectionNet[sectionID], err = checkedReportSum(sectionNet[sectionID], adjustment.SignedMinor)
		if err != nil {
			return nil, err
		}
	}
	for _, payout := range report.Payouts {
		sectionID, wrongCurrency, err := attribute(payout.SourcePartID, payout.SourceSectionID, payout.BatchID, payout.Currency)
		if err != nil {
			return nil, err
		}
		if wrongCurrency {
			sectionCurrencyMismatch[sectionID] = true
		}
		sectionPayouts[sectionID] = append(sectionPayouts[sectionID], payout)
	}
	// Numbered sections are complete only if all sections for each exact batch
	// and currency are present, with the same provider-declared total.
	type batchKey struct{ batch, currency string }
	sectionNumbers := make(map[batchKey]map[int]bool)
	sectionTotals := make(map[batchKey]int)
	for _, section := range report.Sections {
		key := batchKey{section.BatchID, section.Currency}
		if sectionNumbers[key] == nil {
			sectionNumbers[key] = make(map[int]bool)
		}
		if section.Number > 0 {
			if sectionNumbers[key][section.Number] {
				return nil, ErrConflict
			}
			sectionNumbers[key][section.Number] = true
		}
		if old := sectionTotals[key]; old != 0 && old != section.TotalSections {
			return nil, ErrConflict
		}
		sectionTotals[key] = section.TotalSections
	}
	for _, section := range report.Sections {
		key := batchKey{section.BatchID, section.Currency}
		if section.TotalSections == 0 || len(sectionNumbers[key]) != section.TotalSections {
			sectionIncomplete[section.ID] = true
		}
		if sectionRows[section.ID] != section.DeclaredRows {
			sectionIncomplete[section.ID] = true
		}
		partNumbers := make(map[int]bool)
		partDigests := make(map[string]bool)
		var totalParts int
		for _, part := range report.Parts {
			if part.SectionID != section.ID {
				continue
			}
			if partNumbers[part.Number] {
				return nil, ErrConflict
			}
			if partDigests[part.Digest] {
				return nil, ErrConflict
			}
			partDigests[part.Digest] = true
			partNumbers[part.Number] = true
			if totalParts != 0 && totalParts != part.TotalParts {
				return nil, ErrConflict
			}
			totalParts = part.TotalParts
			if partRows[part.ID] != part.DeclaredRows {
				sectionIncomplete[section.ID] = true
			}
		}
		if totalParts == 0 || len(partNumbers) != totalParts {
			sectionIncomplete[section.ID] = true
		}
		if section.DeclaredNetMinor == nil || section.DeclaredPayoutMinor == nil {
			sectionIncomplete[section.ID] = true
		}
	}
	type batchState struct {
		expected, reported                                                     int64
		payouts                                                                []PayoutLine
		incomplete, unconfirmed, providerMismatch, duplicate, currencyMismatch bool
	}
	batches := make(map[batchKey]*batchState)
	for _, section := range report.Sections {
		key := batchKey{section.BatchID, section.Currency}
		state := batches[key]
		if state == nil {
			state = &batchState{}
			batches[key] = state
		}
		var err error
		state.expected, err = checkedReportSum(state.expected, sectionNet[section.ID])
		if err != nil {
			return nil, err
		}
		state.incomplete = state.incomplete || sectionIncomplete[section.ID]
		state.unconfirmed = state.unconfirmed || sectionUnconfirmed[section.ID]
		state.currencyMismatch = state.currencyMismatch || sectionCurrencyMismatch[section.ID]
		if section.DeclaredNetMinor != nil && *section.DeclaredNetMinor != sectionNet[section.ID] {
			state.providerMismatch = true
		}
		var sectionPayoutTotal int64
		for _, payout := range sectionPayouts[section.ID] {
			state.reported, err = checkedReportSum(state.reported, payout.AmountMinor)
			if err != nil {
				return nil, err
			}
			sectionPayoutTotal, err = checkedReportSum(sectionPayoutTotal, payout.AmountMinor)
			if err != nil {
				return nil, err
			}
			if payoutReferences[payout.PayoutReference] > 1 {
				state.duplicate = true
			}
			state.payouts = append(state.payouts, payout)
		}
		if section.DeclaredPayoutMinor != nil && *section.DeclaredPayoutMinor != sectionPayoutTotal {
			state.providerMismatch = true
		}
	}
	var out []PayoutReconciliationResult
	for key, state := range batches {
		if len(state.payouts) == 0 {
			out = append(out, PayoutReconciliationResult{PayoutID: "__missing__:" + key.batch + ":" + key.currency, BatchID: key.batch, Currency: key.currency, Outcome: PayoutMissing, ExpectedMinor: state.expected})
			continue
		}
		for _, payout := range state.payouts {
			r := PayoutReconciliationResult{PayoutID: payout.ID, BatchID: payout.BatchID, Currency: payout.Currency, PayoutReference: payout.PayoutReference, ExpectedMinor: state.expected, ReportedMinor: payout.AmountMinor, BatchReportedMinor: state.reported, Payout: payout, Outcome: PayoutMatched}
			switch {
			case state.duplicate:
				r.Outcome = PayoutDuplicate
			case state.incomplete:
				r.Outcome = PayoutIncomplete
			case state.currencyMismatch:
				r.Outcome = PayoutCurrencyMismatch
			case state.unconfirmed:
				r.Outcome = PayoutUnconfirmed
			case state.providerMismatch:
				r.Outcome = PayoutProviderTotalMismatch
			case state.reported != state.expected:
				r.Outcome = PayoutAmountMismatch
			}
			out = append(out, r)
		}
	}
	for _, payout := range sectionPayouts[""] {
		out = append(out, PayoutReconciliationResult{PayoutID: payout.ID, BatchID: payout.BatchID, Currency: payout.Currency, PayoutReference: payout.PayoutReference, ReportedMinor: payout.AmountMinor, Payout: payout, Outcome: PayoutIncomplete})
	}
	if len(report.Adjustments) > 0 && len(out) == 0 {
		out = append(out, PayoutReconciliationResult{PayoutID: "__missing__:" + report.ID, Outcome: PayoutMissing})
	}
	slices.SortFunc(out, func(a, b PayoutReconciliationResult) int { return strings.Compare(a.PayoutID, b.PayoutID) })
	return out, nil
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
	if line.BatchID != "" && (line.Kind == SettlementCaptured || line.Kind == SettlementRefunded) && (line.OriginalReference == "" || line.ModificationReference == "") {
		r.Outcome = ReconciliationUnconfirmed
		return r, nil
	}
	if line.OriginalReference != "" {
		original, _, err := tx.ResolveSettlementReference(e.config.Scope, e.config.ProviderName, e.config.MerchantAccount, line.OriginalReference)
		if err == nil && (original.ID != p.ID || original.ProviderReference != line.OriginalReference) {
			r.Outcome = ReconciliationUnconfirmed
			return r, nil
		}
		if errors.Is(err, ErrNotFound) {
			r.Outcome = ReconciliationUnconfirmed
			return r, nil
		}
		if err != nil {
			return r, err
		}
	}
	if line.ModificationReference != "" && line.Kind != SettlementReturned {
		modified, _, err := tx.ResolveSettlementReference(e.config.Scope, e.config.ProviderName, e.config.MerchantAccount, line.ModificationReference)
		if err == nil && modified.ID != p.ID {
			r.Outcome = ReconciliationUnconfirmed
			return r, nil
		}
		if errors.Is(err, ErrNotFound) {
			r.Outcome = ReconciliationUnconfirmed
			return r, nil
		}
		if err != nil {
			return r, err
		}
		if line.Kind == SettlementCaptured || line.Kind == SettlementRefunded {
			op, err = tx.OperationByReference(e.config.Scope, p.ID, line.ModificationReference)
			if errors.Is(err, ErrNotFound) {
				r.Outcome = ReconciliationUnconfirmed
				return r, nil
			}
			if err != nil {
				return r, err
			}
			if op.PaymentID != p.ID || op.AttemptID != p.AttemptID {
				r.Outcome = ReconciliationUnconfirmed
				return r, nil
			}
		}
	}
	if line.Currency != p.Amount.Currency {
		r.Outcome = ReconciliationCurrencyMismatch
		return r, nil
	}
	if p.Status == PaymentCaptureFailed {
		r.Outcome = ReconciliationUnconfirmed
		return r, nil
	}
	var adverseExpected int64
	if line.Kind == SettlementReturned {
		// A report cannot corroborate its own return. A provider projector must
		// first process a scoped verified receipt and persist its exact adverse
		// identity. No provider-specific JSON is parsed in this generic core.
		if line.OriginalReference == "" || line.ModificationReference == "" || p.ProviderReference != line.OriginalReference {
			r.Outcome = ReconciliationUnconfirmed
			return r, nil
		}
		// Adyen sometimes puts the capture PSP in a Chargeback report row.
		// Require a full succeeded capture on this payment before considering
		// a separately verified dispute for that variant.
		captureVariant := false
		_, modification, resolveErr := tx.ResolveSettlementReference(e.config.Scope, e.config.ProviderName, e.config.MerchantAccount, line.ModificationReference)
		if resolveErr == nil {
			if e.config.ProviderName != "adyen" || (p.Market.Method != MethodVisa && p.Market.Method != MethodMastercard) ||
				modification.ID == "" || modification.PaymentID != p.ID || modification.AttemptID != p.AttemptID ||
				modification.Kind != OpCapture || modification.Status != OperationSucceeded || modification.Amount.Currency != line.Currency ||
				modification.Amount.Minor != -line.GrossMinor || p.Funds.Captured != modification.Amount.Minor {
				r.Outcome = ReconciliationUnconfirmed
				return r, nil
			}
			captureVariant = true
		} else if !errors.Is(resolveErr, ErrNotFound) {
			return r, resolveErr
		}
		reader, ok := tx.(AdverseEvidenceReader)
		if !ok {
			r.Outcome = ReconciliationUnconfirmed
			return r, nil
		}
		notices, err := reader.AdverseEvidence(e.config.Scope, e.config.ProviderName, e.config.MerchantAccount, p.ID)
		if err != nil {
			return r, err
		}
		matches := 0
		var adverseEventID string
		for _, notice := range notices {
			if notice.Scope != p.Scope || notice.Provider != p.Provider || notice.MerchantAccount != p.MerchantAccount || notice.PaymentID != p.ID || notice.OriginalReference != line.OriginalReference || notice.Amount.Currency != line.Currency || notice.Amount.Minor != -line.GrossMinor {
				continue
			}
			if captureVariant {
				if notice.Kind != "chargeback" || notice.ModificationReference == line.ModificationReference || notice.ModificationReference == p.ProviderReference || notice.ModificationReference == "" || notice.ProviderReference != notice.ModificationReference {
					continue
				}
			} else if notice.ModificationReference != line.ModificationReference || notice.Kind != "returned" && notice.Kind != "chargeback" && notice.Kind != "reversal" {
				continue
			}
			matches++
			adverseExpected = notice.Amount.Minor
			adverseEventID = notice.ProviderEventID
			if matches > 1 {
				break
			}
		}
		if matches != 1 {
			r.Outcome = ReconciliationUnconfirmed
			return r, nil
		}
		r.AdverseEventID = adverseEventID
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
		expected = adverseExpected
	}
	if op.ID != "" && line.Kind != SettlementReturned {
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
