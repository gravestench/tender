// Package store provides Tender's in-memory reference store for tests and
// local simulations. Its contents last only for the lifetime of the process.
package store

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"time"

	"github.com/gravestench/tender"
	"github.com/gravestench/tender/store/internal/binding"
)

type key struct {
	scope tender.Scope
	id    string
}
type inboxEvent struct {
	evidence   json.RawMessage
	processed  bool
	receivedAt time.Time
}
type returnClaimKey struct {
	scope                      tender.Scope
	provider, account, eventID string
}
type returnClaimOwner struct{ reportID, lineID string }

type memoryTx struct {
	processing      *tender.VerifiedEventKey
	events          map[tender.VerifiedEventKey]inboxEvent
	payments        map[key]tender.Payment
	operations      map[key]tender.OperationRecord
	history         []tender.AuditEntry
	outbox          []tender.OutboxEntry
	captureFailures []tender.CaptureFailureNotice
	adverse         map[tender.AdverseKey]tender.AdverseNotice
	returnClaims    map[returnClaimKey]returnClaimOwner
	bankReviews     map[bankReviewKey]tender.BankDepositResult
	bankClaims      map[bankClaimKey]bankClaimOwner
	bankReferences  map[bankReferenceKey]bankClaimOwner
}

// Memory serializes transactions and publishes a new snapshot only on commit.
type Memory struct {
	lock  chan struct{}
	state memoryTx
}

func NewMemory() *Memory {
	return &Memory{lock: make(chan struct{}, 1), state: memoryTx{events: make(map[tender.VerifiedEventKey]inboxEvent), payments: make(map[key]tender.Payment), operations: make(map[key]tender.OperationRecord), adverse: make(map[tender.AdverseKey]tender.AdverseNotice), returnClaims: make(map[returnClaimKey]returnClaimOwner), bankReviews: make(map[bankReviewKey]tender.BankDepositResult), bankClaims: make(map[bankClaimKey]bankClaimOwner), bankReferences: make(map[bankReferenceKey]bankClaimOwner)}}
}

func (m *Memory) acquire(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case m.lock <- struct{}{}:
		return nil
	}
}

func (m *Memory) Transact(ctx context.Context, fn func(tender.Tx) error) error {
	if err := m.acquire(ctx); err != nil {
		return err
	}
	defer func() { <-m.lock }()
	if err := ctx.Err(); err != nil {
		return err
	}
	tx := memoryTx{
		events:   maps.Clone(m.state.events),
		payments: maps.Clone(m.state.payments), operations: maps.Clone(m.state.operations),
		history: slices.Clone(m.state.history), outbox: slices.Clone(m.state.outbox), captureFailures: slices.Clone(m.state.captureFailures),
		adverse:      maps.Clone(m.state.adverse),
		returnClaims: maps.Clone(m.state.returnClaims),
		bankReviews:  maps.Clone(m.state.bankReviews), bankClaims: maps.Clone(m.state.bankClaims), bankReferences: maps.Clone(m.state.bankReferences),
	}
	if err := fn(&tx); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	m.state = tx
	return nil
}

func (t *memoryTx) Payment(scope tender.Scope, id string) (tender.Payment, error) {
	if p, ok := t.payments[key{scope, id}]; ok {
		return p, nil
	}
	return tender.Payment{}, tender.ErrNotFound
}

func (t *memoryTx) PaymentByOrder(scope tender.Scope, id string) (tender.Payment, error) {
	for _, p := range t.payments {
		if p.Scope == scope && p.OrderID == id {
			return p, nil
		}
	}
	return tender.Payment{}, tender.ErrNotFound
}

func (t *memoryTx) Operation(scope tender.Scope, id string) (tender.OperationRecord, error) {
	if op, ok := t.operations[key{scope, id}]; ok {
		return op, nil
	}
	return tender.OperationRecord{}, tender.ErrNotFound
}

func (t *memoryTx) OperationByKey(scope tender.Scope, id string) (tender.OperationRecord, error) {
	for _, o := range t.operations {
		if o.Scope == scope && o.Key == id {
			return o, nil
		}
	}
	return tender.OperationRecord{}, tender.ErrNotFound
}

func (t *memoryTx) OperationByReference(scope tender.Scope, paymentID, ref string) (tender.OperationRecord, error) {
	for _, o := range t.operations {
		if ref != "" && o.Scope == scope && o.PaymentID == paymentID && o.ProviderReference == ref {
			return o, nil
		}
	}
	return tender.OperationRecord{}, tender.ErrNotFound
}

func active(o tender.OperationRecord) bool {
	return o.Status != tender.OperationSucceeded && o.Status != tender.OperationFailed
}

func (t *memoryTx) ActiveOperation(scope tender.Scope, paymentID string) (tender.OperationRecord, error) {
	for _, o := range t.operations {
		if o.Scope == scope && o.PaymentID == paymentID && active(o) {
			return o, nil
		}
	}
	return tender.OperationRecord{}, tender.ErrNotFound
}

func (t *memoryTx) InsertPayment(p tender.Payment) error {
	if _, ok := t.payments[key{p.Scope, p.ID}]; ok {
		return tender.ErrConflict
	}
	if _, err := t.PaymentByOrder(p.Scope, p.OrderID); err == nil {
		return tender.ErrConflict
	}
	return t.savePayment(p)
}

func (t *memoryTx) UpdatePayment(p tender.Payment) error {
	old, err := t.Payment(p.Scope, p.ID)
	if err != nil {
		return err
	}
	if err := binding.PaymentUpdate(old, p); err != nil {
		return err
	}
	return t.savePayment(p)
}

func (t *memoryTx) savePayment(p tender.Payment) error {
	for _, other := range t.payments {
		if p.ProviderReference != "" && other.Scope == p.Scope && other.ID != p.ID &&
			other.Provider == p.Provider && other.MerchantAccount == p.MerchantAccount &&
			other.ProviderReference == p.ProviderReference {
			return tender.ErrConflict
		}
	}
	t.payments[key{p.Scope, p.ID}] = p
	return nil
}

func (t *memoryTx) InsertOperation(o tender.OperationRecord) error {
	if _, err := t.Payment(o.Scope, o.PaymentID); err != nil {
		return err
	}
	if _, err := t.Operation(o.Scope, o.ID); err == nil {
		return tender.ErrConflict
	}
	if _, err := t.OperationByKey(o.Scope, o.Key); err == nil {
		return tender.ErrConflict
	}
	return t.saveOperation(o)
}

func (t *memoryTx) UpdateOperation(o tender.OperationRecord) error {
	old, err := t.Operation(o.Scope, o.ID)
	if err != nil {
		return err
	}
	if err := binding.OperationUpdate(old, o); err != nil {
		return err
	}
	return t.saveOperation(o)
}

func (t *memoryTx) saveOperation(o tender.OperationRecord) error {
	if active(o) {
		if other, err := t.ActiveOperation(o.Scope, o.PaymentID); err == nil && other.ID != o.ID {
			return tender.ErrConflict
		}
	}
	if other, err := t.OperationByReference(o.Scope, o.PaymentID, o.ProviderReference); err == nil && other.ID != o.ID {
		return tender.ErrConflict
	}
	t.operations[key{o.Scope, o.ID}] = o
	return nil
}

func (t *memoryTx) AppendAudit(a tender.AuditEntry) error {
	t.history = append(t.history, a)
	return nil
}

func (t *memoryTx) Enqueue(entry tender.OutboxEntry) error {
	for _, v := range t.outbox {
		if v.Scope == entry.Scope && v.ID == entry.ID {
			return nil
		}
	}
	t.outbox = append(t.outbox, entry)
	return nil
}

// History returns the chronological history for one scoped payment.
func (m *Memory) History(ctx context.Context, scope tender.Scope, paymentID string) ([]tender.AuditEntry, error) {
	if err := m.acquire(ctx); err != nil {
		return nil, err
	}
	defer func() { <-m.lock }()
	var rows []tender.AuditEntry
	for _, r := range m.state.history {
		if r.Scope == scope && r.PaymentID == paymentID {
			rows = append(rows, r)
		}
	}
	return rows, nil
}

// Outbox returns committed full-collection notices in insertion order.
func (m *Memory) Outbox(ctx context.Context, scope tender.Scope) ([]tender.OutboxEntry, error) {
	if err := m.acquire(ctx); err != nil {
		return nil, err
	}
	defer func() { <-m.lock }()
	var rows []tender.OutboxEntry
	for _, r := range m.state.outbox {
		if r.Scope == scope {
			rows = append(rows, r)
		}
	}
	return rows, nil
}

func (t *memoryTx) InsertVerifiedEvent(key tender.VerifiedEventKey, evidence json.RawMessage) error {
	if !json.Valid(evidence) {
		return tender.ErrValidation
	}
	if _, found := t.events[key]; found {
		return tender.ErrConflict
	}
	t.events[key] = inboxEvent{evidence: slices.Clone(evidence), receivedAt: time.Now().UTC()}
	return nil
}

func (t *memoryTx) VerifiedEventReceivedAt(key tender.VerifiedEventKey) (time.Time, error) {
	event, ok := t.events[key]
	if !ok {
		return time.Time{}, tender.ErrNotFound
	}
	return event.receivedAt, nil
}

func (t *memoryTx) ProcessVerifiedEvent(key tender.VerifiedEventKey, _ time.Time, apply func(json.RawMessage) error) error {
	if t.processing != nil {
		return tender.ErrConflict
	}
	event, found := t.events[key]
	if !found {
		return tender.ErrNotFound
	}
	if event.processed {
		return nil
	}
	t.processing = &key
	defer func() { t.processing = nil }()
	if err := apply(slices.Clone(event.evidence)); err != nil {
		return err
	}
	event.processed = true
	t.events[key] = event
	return nil
}

func (t *memoryTx) EnqueueCaptureFailure(n tender.CaptureFailureNotice) error {
	for _, old := range t.captureFailures {
		if old.Scope == n.Scope && old.OperationID == n.OperationID {
			return nil
		}
	}
	t.captureFailures = append(t.captureFailures, n)
	return nil
}

func (m *Memory) CaptureFailures(ctx context.Context, scope tender.Scope) ([]tender.CaptureFailureNotice, error) {
	if err := m.acquire(ctx); err != nil {
		return nil, err
	}
	defer func() { <-m.lock }()
	var notices []tender.CaptureFailureNotice
	for _, n := range m.state.captureFailures {
		if n.Scope == scope {
			notices = append(notices, n)
		}
	}
	return notices, nil
}

func (t *memoryTx) EnqueueAdverse(n tender.AdverseNotice, claim tender.AdverseClaim) error {
	if t.processing == nil || !claim.ValidFor(*t.processing, n) {
		return tender.ErrConflict
	}
	if err := n.Validate(); err != nil {
		return err
	}
	n.ObservedAt = n.ObservedAt.Round(0).UTC()
	k := n.Key()
	if _, found := t.events[tender.VerifiedEventKey{Scope: k.Scope, Provider: k.Provider, MerchantAccount: k.MerchantAccount, ID: k.ProviderEventID}]; !found {
		return tender.ErrNotFound
	}
	if old, found := t.adverse[k]; found {
		old.Decision = nil
		if old != n {
			return tender.ErrConflict
		}
		return nil
	}
	t.adverse[k] = n
	return nil
}

func (t *memoryTx) ClaimReturnEvidenceSet(scope tender.Scope, provider, account, reportID string, claims []tender.ReturnEvidenceClaim) (bool, error) {
	if scope.Validate() != nil || provider == "" || account == "" || reportID == "" || len(claims) == 0 {
		return false, tender.ErrValidation
	}
	seen := make(map[string]bool, len(claims))
	lines := make(map[string]bool, len(claims))
	for _, claim := range claims {
		if claim.ProviderEventID == "" || claim.LineID == "" || seen[claim.ProviderEventID] || lines[claim.LineID] {
			return false, tender.ErrValidation
		}
		seen[claim.ProviderEventID] = true
		lines[claim.LineID] = true
		key := returnClaimKey{scope, provider, account, claim.ProviderEventID}
		event := t.events[tender.VerifiedEventKey{Scope: scope, Provider: provider, MerchantAccount: account, ID: claim.ProviderEventID}]
		if !event.processed {
			return false, tender.ErrConflict
		}
		if _, ok := t.adverse[tender.AdverseKey{Scope: scope, Provider: provider, MerchantAccount: account, ProviderEventID: claim.ProviderEventID}]; !ok {
			return false, tender.ErrConflict
		}
		if prior, ok := t.returnClaims[key]; ok && prior != (returnClaimOwner{reportID, claim.LineID}) {
			return false, nil
		}
		for priorKey, owner := range t.returnClaims {
			if priorKey.scope == scope && priorKey.provider == provider && priorKey.account == account && priorKey.eventID != claim.ProviderEventID && owner == (returnClaimOwner{reportID, claim.LineID}) {
				return false, nil
			}
		}
	}
	for _, claim := range claims {
		t.returnClaims[returnClaimKey{scope, provider, account, claim.ProviderEventID}] = returnClaimOwner{reportID, claim.LineID}
	}
	return true, nil
}

func (t *memoryTx) ReviewAdverse(k tender.AdverseKey, d tender.AdverseDecision) error {
	if err := k.Validate(); err != nil {
		return err
	}
	var err error
	d, err = d.Normalize()
	if err != nil {
		return err
	}
	n, found := t.adverse[k]
	if !found {
		return tender.ErrNotFound
	}
	if !t.events[tender.VerifiedEventKey{Scope: k.Scope, Provider: k.Provider, MerchantAccount: k.MerchantAccount, ID: k.ProviderEventID}].processed {
		return tender.ErrConflict
	}
	if n.Decision != nil {
		if *n.Decision == d {
			return nil
		}
		return tender.ErrConflict
	}
	n.Decision = &d
	t.adverse[k] = n
	return nil
}

// AdverseReviews lists durable notices, including reviewed decisions, for one
// merchant/environment. The host filters Decision == nil for its work queue.
func (m *Memory) AdverseReviews(ctx context.Context, scope tender.Scope) ([]tender.AdverseNotice, error) {
	if err := m.acquire(ctx); err != nil {
		return nil, err
	}
	defer func() { <-m.lock }()
	var out []tender.AdverseNotice
	for _, n := range m.state.adverse {
		k := n.Key()
		if n.Scope == scope && m.state.events[tender.VerifiedEventKey{Scope: k.Scope, Provider: k.Provider, MerchantAccount: k.MerchantAccount, ID: k.ProviderEventID}].processed {
			if n.Decision != nil {
				d := *n.Decision
				n.Decision = &d
			}
			out = append(out, n)
		}
	}
	return out, nil
}

func (m *Memory) ReviewAdverse(ctx context.Context, k tender.AdverseKey, d tender.AdverseDecision) error {
	return m.Transact(ctx, func(tx tender.Tx) error { return tx.(tender.AdverseReviewTx).ReviewAdverse(k, d) })
}

func (t *memoryTx) AdverseEvidence(scope tender.Scope, provider, account, paymentID string) ([]tender.AdverseNotice, error) {
	var out []tender.AdverseNotice
	for k, n := range t.adverse {
		if k.Scope == scope && k.Provider == provider && k.MerchantAccount == account && n.PaymentID == paymentID &&
			t.events[tender.VerifiedEventKey{Scope: k.Scope, Provider: k.Provider, MerchantAccount: k.MerchantAccount, ID: k.ProviderEventID}].processed {
			out = append(out, n)
		}
	}
	return out, nil
}

func (t *memoryTx) MatchingVerifiedEvidence(scope tender.Scope, provider, account string, fields map[string]string) ([]json.RawMessage, error) {
	var result []json.RawMessage
	for k, row := range t.events {
		if k.Scope != scope || k.Provider != provider || k.MerchantAccount != account {
			continue
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(row.evidence, &obj); err != nil {
			return nil, err
		}
		matches := true
		for field, want := range fields {
			var s string
			if json.Unmarshal(obj[field], &s) != nil || s != want {
				matches = false
				break
			}
		}
		if matches {
			result = append(result, slices.Clone(row.evidence))
		}
	}
	return result, nil
}
