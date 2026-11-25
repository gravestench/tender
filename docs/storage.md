# PostgreSQL storage

Tender stores the host order claim, payment, attempt and operation reservation before dispatch. A later transaction commits the observed outcome, audit entry and full-collection notice together. Each payment has one immutable attempt, identified by `attempt_id` on the payment row. An operation references that attempt through a scoped foreign key.

## Install and configure

Use PostgreSQL 17 with a UTF-8 database. Choose a schema where only trusted roles can create objects. Set that schema as the search path for the migration session and **every** connection in the application pool. The example uses an existing schema named `tender`.

Apply outstanding `migrations/*.up.sql` files in lexicographic order before starting workers. Each script contains `BEGIN` and `COMMIT`. Record each successfully applied version in the host migration ledger. A fresh installation applies `001` through `006`; existing installations apply each version after their last recorded migration. The scripts are also available through `postgres.Migrations` in Go.

```sh
PGOPTIONS='-c search_path=tender' psql "$TENDER_MIGRATION_DSN" -X -v ON_ERROR_STOP=1 \
  -f store/postgres/migrations/001_initial.up.sql \
  -f store/postgres/migrations/002_reference_integrity.up.sql \
  -f store/postgres/migrations/003_capture_failures.up.sql \
  -f store/postgres/migrations/004_reconciliation_digest.up.sql \
  -f store/postgres/migrations/005_payout_batches.up.sql \
  -f store/postgres/migrations/006_adverse_review.up.sql
```

Grant the runtime role schema usage, table privileges for its operations, and sequence usage for inserts. Reserve DDL for the migration role. The host owns the SQL driver, pool, credentials, network settings and shutdown. With pgx v5, a PostgreSQL URL can include `search_path=tender` and `sslmode=verify-full` for a remote database. Configure a trusted CA for the server certificate. The `store/postgres` package uses the Go standard library and accepts a pool opened with any PostgreSQL `database/sql` driver that supports serializable transactions and SQLSTATE errors via `SQLState() string`.

```go
import (
    "context"
    "database/sql"

    "github.com/gravestench/tender/store/postgres"
    _ "github.com/jackc/pgx/v5/stdlib"
)

func openStore(ctx context.Context, dsn string) (*sql.DB, *postgres.Store, error) {
    db, err := sql.Open("pgx", dsn)
    if err != nil { return nil, nil, err }
    if err := db.PingContext(ctx); err != nil {
        db.Close()
        return nil, nil, err
    }
    return db, postgres.New(db), nil
}
```

Pass the returned store to `tender.EngineConfig.Store`. The host closes the pool after stopping its workers. Use separate merchant, environment and processor-account bindings for every engine.

## Recovery and host processing

- **Reservations.** Scoped unique indexes protect payment and order claims, operation IDs and keys, bound provider references and one active operation per payment. PostgreSQL serializable transactions protect reads of absent keys and version-checked updates across processes. The callback runs once. A uniqueness or serialization race returns `tender.ErrConflict`. After a commit error, replay the original request with its original key and fingerprint inputs to learn whether its reservation exists. An unknown or reserved result requires `Engine.Recover` or verified evidence before another monetary mutation can proceed. Recovery uses the recorded provider key. A definite no-effect rejection retains the retry deadline and one successor for `RetryCreate`.
- **History.** `History(ctx, scope, paymentID)` returns the committed reservation, dispatch and result changes. The JSON records retain typed values including int64 amounts, uint64 versions, nanosecond timestamps and operation retry metadata. Generated columns index the same identity that the Go decoder reads. Treat changes to exported field names, field types or index expressions as schema migrations.
- **Verified callbacks.** After authenticating the signed event, insert the verified, allowlisted evidence into `tender_provider_events` before acknowledging the delivery. Use the scoped provider event identity. On duplicate delivery, compare the stored evidence with the normalized incoming event before acknowledging it. Keep raw payloads, credentials and payment instruments out of this table. For each pending delivery, call `engine.ProcessVerifiedEvent(ctx, eventID, normalize)`. Tender reads the stored evidence under a row lock on the engine’s scope, provider, account and delivery ID. The adapter’s pure local normalizer derives `tender.VerifiedEvent` from that evidence. Preserve the authenticated delivery identity, configured provider and account in the evidence at ingress. Tender checks the normalized delivery ID, provider, scope, account, payment and operation bindings. It writes `processed_at`, the payment, audit and outbox in the same transaction. Replaying a committed delivery returns success even after a later capture or refund. A processing failure leaves the delivery pending. Run provider queries and other I/O outside the normalizer.

```sql
INSERT INTO tender_provider_events
    (merchant_id, environment, provider, merchant_account, event_id, evidence)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (merchant_id, environment, provider, merchant_account, event_id) DO NOTHING;
```

- **Fulfillment.** `Outbox(ctx, scope)` lists committed collection notices. For a worker, claim a pending row with the following query inside a host transaction. Check current payment state and host policy. Record the scoped `entry_id` in the host's unique fulfillment ledger, enqueue host work and update `consumed_at` in that same transaction. A host using another database or external service must make that destination idempotent using the same scoped ID. Identity sequence numbers establish insertion order and can have gaps or commit out of order. Select by pending status when polling.

```sql
SELECT seq, entry_id, record FROM tender_outbox
WHERE merchant_id = $1 AND environment = $2 AND consumed_at IS NULL
ORDER BY seq LIMIT 1 FOR UPDATE SKIP LOCKED;

UPDATE tender_outbox SET consumed_at = CURRENT_TIMESTAMP
WHERE merchant_id = $1 AND environment = $2 AND seq = $3;
```

- **Capture failure review.** A verified capture failure puts the payment on hold and atomically adds one durable review item per scoped operation, including failures reported after fulfillment. `CaptureFailures(ctx, scope)` lists pending PostgreSQL records. A host worker can claim one as shown below. Record compensation or follow-up keyed by merchant, environment and operation ID and acknowledge the review in the same transaction. The earlier gross capture and its original outbox entry remain available for audit.

```sql
SELECT seq, operation_id, record FROM tender_capture_failures
WHERE merchant_id = $1 AND environment = $2 AND reviewed_at IS NULL
ORDER BY seq LIMIT 1 FOR UPDATE SKIP LOCKED;

UPDATE tender_capture_failures SET reviewed_at = CURRENT_TIMESTAMP
WHERE merchant_id = $1 AND environment = $2 AND seq = $3;
```

- **Settlement and reconciliation.** Call `Engine.Reconcile` only with a report downloaded and authenticated by trusted host code. Its normalized transaction rows, source parts, sections, payout rows and adjustments are inserted with the run and input digest in one serializable transaction. The route key is merchant, environment, provider and merchant account. A repeated identical report is compared with current ledger facts; changed content under the same report ID appends a conflict audit entry and returns `ErrConflict`. Earlier unmatched results remain in history when later verified facts resolve them. `005_payout_batches` adds source and dual-reference columns to `tender_settlements`, `tender_reconciliation_inputs` for the redacted payout/part/section projection, and `tender_reconciliation_payout_history` for immutable batch results. This does not store a raw provider report or shopper data.

The payout amount uses a **merchant-positive** sign: money delivered to the merchant bank is positive even if the provider journal records its own payout as a debit. A payout matches only when every authenticated numbered part and section is present, attributed row counts and provider-declared totals agree, capture/refund lines match exact successful operations, and the batch-wide sum of matched transaction net plus separately reported adjustments equals the payout. Any missing, mismatched or unsupported evidence stays open. A report alone cannot confirm a return or dispute; matching requires a processed verified adverse receipt with the original payment and modification references. The host still matches the processor payout to the actual bank deposit as a separate production gate.

When matching an opaque reference, scope both the digest lookup and the exact comparison to the provider route. The full value remains in the record. A digest collision in a unique index produces a conflict that requires investigation.

```sql
SELECT payment_id FROM tender_payments
WHERE (merchant_id, environment, provider, merchant_account) = ($1,$2,$3,$4)
  AND tender_reference_digest(provider_reference) = tender_reference_digest($5)
  AND provider_reference = $5;
```

The reconciliation history foreign key checks the matched payment's scope, provider and account against the run. Applying `002` validates this constraint against existing rows and rolls back the migration if a historical payment belongs to a different route. Review the report and affected history before correcting a prior mismatch.

`Engine.Reconcile(ctx, SettlementReport)` accepts a host-authenticated normalized feed. The `FeedHostVerified` marker is an assertion by trusted host code, not an authentication mechanism for shopper input. Provider auto-download feeds return `ErrUnsupported` until implemented. One transaction records the report, its input digest, projected lines, payout evidence, run and result histories; an exact replay does not duplicate results. Reusing a report ID with changed contents records `report_conflict` and returns `ErrConflict`. Reprocessing after an unmatched line becomes resolvable appends a resolution without rewriting its earlier discrepancy. Reconciliation does not modify payment funds or the fulfillment outbox. For ACH, apply the independently authenticated Adyen report through the ACH signed-report path before interpreting ledger comparison as settlement authority. Query `ReconciliationHistory` and `OpenReconciliationDiscrepancies` for transaction lines, and `OpenPayoutDiscrepancies` for the latest unresolved batch results. The host owns download authentication, scheduling, alerting and operator review.

The underlying run/history SQL can also support host inspection. Preserve the original report identity and projected input digest on every replay.

```sql
INSERT INTO tender_reconciliation_runs
    (merchant_id, environment, provider, merchant_account, run_id, report_id, input_digest)
VALUES ($1,$2,$3,$4,$5,$6,$7);

INSERT INTO tender_reconciliation_history
    (merchant_id, environment, provider, merchant_account, run_id, line_id,
     payment_id, outcome, evidence)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9);

UPDATE tender_reconciliation_runs SET finished_at = CURRENT_TIMESTAMP
WHERE (merchant_id, environment, provider, merchant_account, run_id) = ($1,$2,$3,$4,$5);
```

## Rollback

Before deployment, back up existing Tender data and verify restore access. A code rollback can keep a compatible schema. Before removing the initial schema, stop all Tender writers and workers, identify pending operations and outbox work, and preserve the full database for recovery. On a disposable database or after the host has explicitly approved deleting the records, run the down migrations in reverse order using the same role and search path. Downgrading `006` removes adverse review records; `005` removes payout source projections and result history. Retain both histories before either rollback. Downgrading `004` removes the reconciliation run input digest and weakens same-ID content conflict detection; retain a backup and do not process reports with older code. Downgrading `003` removes the capture failure review queue and its evidence lookup index. Preserve its review history first. Downgrading `002` restores full-width reference indexes and fails atomically if a stored reference exceeds the old B-tree limit. Preserve the references and the current schema in that case.

```sh
PGOPTIONS='-c search_path=tender' psql "$TENDER_MIGRATION_DSN" -X -v ON_ERROR_STOP=1 \
  -f store/postgres/migrations/006_adverse_review.down.sql \
  -f store/postgres/migrations/005_payout_batches.down.sql \
  -f store/postgres/migrations/004_reconciliation_digest.down.sql \
  -f store/postgres/migrations/003_capture_failures.down.sql \
  -f store/postgres/migrations/002_reference_integrity.down.sql \
  -f store/postgres/migrations/001_initial.down.sql
```

The `001` down migration drops the eight Tender tables and their indexes and sequences. Restore the backup and reconcile outstanding external operations before resuming payment traffic. Recreating an empty schema erases the durable keys needed for recovery.

## Test the storage contract

Provide a disposable database using a role with `CREATE` on that database. Each test creates a random schema, applies the versioned migrations, reconnects as needed, and drops only its own schema during cleanup. `TENDER_TEST_DATABASE_URL` is required. The CI service uses PostgreSQL 17.11. The pgx driver is imported by the tagged tests only.

```sh
TENDER_TEST_DATABASE_URL='postgres://tender_test@localhost/tender_test?sslmode=disable' \
  go test -race -count=1 -tags=integration ./test/contract/... -run '^TestPostgres'
```
