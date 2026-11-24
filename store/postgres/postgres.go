// Package postgres implements Tender's transactional store with PostgreSQL.
// The host configures its database/sql driver, owns the connection pool, and
// applies the versioned SQL in Migrations before constructing the store.
package postgres

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"

	"github.com/gravestench/tender"
	"github.com/gravestench/tender/store/internal/binding"
)

// Migrations contains the host-applied migrations/*.up.sql and *.down.sql files.
//
//go:embed migrations/*.sql
var Migrations embed.FS

// Store uses the host's pool for short, serializable transactions.
type Store struct{ db *sql.DB }

var _ tender.Store = (*Store)(nil)

// New uses an existing pool with a PostgreSQL database/sql driver. All
// connections must use the same trusted search_path as the migrations.
func New(db *sql.DB) *Store { return &Store{db: db} }

type transaction struct {
	ctx context.Context
	tx  *sql.Tx
}

// Transact calls fn once. PostgreSQL's serializable isolation covers absent
// keys as well as existing rows. A worker losing a race gets ErrConflict.
func (s *Store) Transact(ctx context.Context, fn func(tender.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return dbError(err)
	}
	defer tx.Rollback()
	if err = fn(&transaction{ctx: ctx, tx: tx}); err != nil {
		return err
	}
	return dbError(tx.Commit())
}

func dbError(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return tender.ErrNotFound
	}
	var state interface{ SQLState() string }
	if errors.As(err, &state) {
		switch state.SQLState() {
		case "23505", "40001", "40P01": // uniqueness, serialization failure, deadlock
			return tender.ErrConflict
		case "23503": // missing referenced record
			return tender.ErrNotFound
		}
	}
	return err
}

// The records are value types. Decoding into the destination Go type preserves
// integer precision, retry durations and nanosecond timestamps across restarts.
func read[T any](row interface{ Scan(...any) error }) (T, error) {
	var value T
	var raw []byte
	if err := row.Scan(&raw); err != nil {
		return value, dbError(err)
	}
	err := json.Unmarshal(raw, &value)
	return value, err
}

func (t *transaction) Payment(scope tender.Scope, id string) (tender.Payment, error) {
	return read[tender.Payment](t.tx.QueryRowContext(t.ctx, `SELECT record FROM tender_payments WHERE merchant_id=$1 AND environment=$2 AND payment_id=$3`, scope.MerchantID, scope.Environment, id))
}
func (t *transaction) PaymentByOrder(scope tender.Scope, order string) (tender.Payment, error) {
	return read[tender.Payment](t.tx.QueryRowContext(t.ctx, `SELECT record FROM tender_payments WHERE merchant_id=$1 AND environment=$2 AND order_id=$3`, scope.MerchantID, scope.Environment, order))
}
func (t *transaction) Operation(scope tender.Scope, id string) (tender.OperationRecord, error) {
	return read[tender.OperationRecord](t.tx.QueryRowContext(t.ctx, `SELECT record FROM tender_operations WHERE merchant_id=$1 AND environment=$2 AND operation_id=$3`, scope.MerchantID, scope.Environment, id))
}
func (t *transaction) OperationByKey(scope tender.Scope, key string) (tender.OperationRecord, error) {
	return read[tender.OperationRecord](t.tx.QueryRowContext(t.ctx, `SELECT record FROM tender_operations WHERE merchant_id=$1 AND environment=$2 AND operation_key=$3`, scope.MerchantID, scope.Environment, key))
}
func (t *transaction) OperationByReference(scope tender.Scope, paymentID, ref string) (tender.OperationRecord, error) {
	return read[tender.OperationRecord](t.tx.QueryRowContext(t.ctx, `SELECT record FROM tender_operations WHERE merchant_id=$1 AND environment=$2 AND payment_id=$3 AND provider_reference=$4`, scope.MerchantID, scope.Environment, paymentID, ref))
}
func (t *transaction) ActiveOperation(scope tender.Scope, paymentID string) (tender.OperationRecord, error) {
	return read[tender.OperationRecord](t.tx.QueryRowContext(t.ctx, `SELECT record FROM tender_operations WHERE merchant_id=$1 AND environment=$2 AND payment_id=$3 AND status NOT IN ('succeeded','failed')`, scope.MerchantID, scope.Environment, paymentID))
}

func (t *transaction) write(query string, record any, keys ...any) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	args := append([]any{string(raw)}, keys...)
	_, err = t.tx.ExecContext(t.ctx, query, args...)
	return dbError(err)
}

func (t *transaction) InsertPayment(p tender.Payment) error {
	return t.write(`INSERT INTO tender_payments(record) VALUES ($1)`, p)
}
func (t *transaction) UpdatePayment(p tender.Payment) error {
	previous, err := t.Payment(p.Scope, p.ID)
	if err != nil {
		return err
	}
	if err = binding.PaymentUpdate(previous, p); err != nil {
		return err
	}
	return t.write(`UPDATE tender_payments SET record=$1 WHERE merchant_id=$2 AND environment=$3 AND payment_id=$4`, p, p.Scope.MerchantID, p.Scope.Environment, p.ID)
}
func (t *transaction) InsertOperation(o tender.OperationRecord) error {
	return t.write(`INSERT INTO tender_operations(record) VALUES ($1)`, o)
}
func (t *transaction) UpdateOperation(o tender.OperationRecord) error {
	previous, err := t.Operation(o.Scope, o.ID)
	if err != nil {
		return err
	}
	if err = binding.OperationUpdate(previous, o); err != nil {
		return err
	}
	return t.write(`UPDATE tender_operations SET record=$1 WHERE merchant_id=$2 AND environment=$3 AND operation_id=$4`, o, o.Scope.MerchantID, o.Scope.Environment, o.ID)
}
func (t *transaction) AppendAudit(a tender.AuditEntry) error {
	return t.write(`INSERT INTO tender_audit(record) VALUES ($1)`, a)
}
func (t *transaction) Enqueue(e tender.OutboxEntry) error {
	return t.write(`INSERT INTO tender_outbox(record) VALUES ($1) ON CONFLICT (merchant_id, environment, entry_id) DO NOTHING`, e)
}

func list[T any](ctx context.Context, db *sql.DB, query string, args ...any) ([]T, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, dbError(err)
	}
	defer rows.Close()
	var result []T
	for rows.Next() {
		v, err := read[T](rows)
		if err != nil {
			return nil, err
		}
		result = append(result, v)
	}
	return result, dbError(rows.Err())
}

// History returns committed entries in insertion order for one scoped payment.
func (s *Store) History(ctx context.Context, scope tender.Scope, paymentID string) ([]tender.AuditEntry, error) {
	return list[tender.AuditEntry](ctx, s.db, `SELECT record FROM tender_audit WHERE merchant_id=$1 AND environment=$2 AND payment_id=$3 ORDER BY seq`, scope.MerchantID, scope.Environment, paymentID)
}

// Outbox returns all committed full-collection notices for a scope in insertion
// order. Hosts can claim and acknowledge pending notices using docs/storage.md.
func (s *Store) Outbox(ctx context.Context, scope tender.Scope) ([]tender.OutboxEntry, error) {
	return list[tender.OutboxEntry](ctx, s.db, `SELECT record FROM tender_outbox WHERE merchant_id=$1 AND environment=$2 ORDER BY seq`, scope.MerchantID, scope.Environment)
}
