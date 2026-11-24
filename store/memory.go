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
	evidence  json.RawMessage
	processed bool
}

type memoryTx struct {
	events          map[tender.VerifiedEventKey]inboxEvent
	payments        map[key]tender.Payment
	operations      map[key]tender.OperationRecord
	history         []tender.AuditEntry
	outbox          []tender.OutboxEntry
	captureFailures []tender.CaptureFailureNotice
}

// Memory serializes transactions and publishes a new snapshot only on commit.
type Memory struct {
	lock  chan struct{}
	state memoryTx
}

func NewMemory() *Memory {
	return &Memory{lock: make(chan struct{}, 1), state: memoryTx{events: make(map[tender.VerifiedEventKey]inboxEvent), payments: make(map[key]tender.Payment), operations: make(map[key]tender.OperationRecord)}}
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
	t.events[key] = inboxEvent{evidence: slices.Clone(evidence)}
	return nil
}

func (t *memoryTx) ProcessVerifiedEvent(key tender.VerifiedEventKey, _ time.Time, apply func(json.RawMessage) error) error {
	event, found := t.events[key]
	if !found {
		return tender.ErrNotFound
	}
	if event.processed {
		return nil
	}
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
