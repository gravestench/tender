// Package store provides Tender's in-memory reference store for tests and
// local simulations. Its contents last only for the lifetime of the process.
package store

import (
	"context"
	"maps"
	"slices"

	"github.com/gravestench/tender"
)

type key struct {
	scope tender.Scope
	id    string
}
type memoryTx struct {
	payments   map[key]tender.Payment
	operations map[key]tender.OperationRecord
	history    []tender.AuditEntry
	outbox     []tender.OutboxEntry
}

// Memory serializes transactions and publishes a new snapshot only on commit.
type Memory struct {
	lock  chan struct{}
	state memoryTx
}

func NewMemory() *Memory {
	return &Memory{lock: make(chan struct{}, 1), state: memoryTx{payments: make(map[key]tender.Payment), operations: make(map[key]tender.OperationRecord)}}
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
		payments: maps.Clone(m.state.payments), operations: maps.Clone(m.state.operations),
		history: slices.Clone(m.state.history), outbox: slices.Clone(m.state.outbox),
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
	if p.OrderID != old.OrderID || p.Amount != old.Amount || p.Version != old.Version+1 ||
		p.AttemptID != old.AttemptID || p.Market != old.Market ||
		p.Provider != old.Provider || p.MerchantAccount != old.MerchantAccount ||
		old.ProviderReference != "" && old.ProviderReference != p.ProviderReference {
		return tender.ErrConflict
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
	if old.SuccessorID != "" && o.SuccessorID != old.SuccessorID ||
		old.ProviderReference != "" && o.ProviderReference != old.ProviderReference {
		return tender.ErrConflict
	}
	// Only the result and its binding may change after reservation.
	old.Status, old.ProviderReference = o.Status, o.ProviderReference
	if o.Operation != old.Operation {
		return tender.ErrConflict
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
