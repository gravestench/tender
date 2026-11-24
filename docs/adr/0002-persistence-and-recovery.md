# 0002. Host-owned persistence and recoverable operations

**Status** Accepted.

The engine uses a transactional store contract for payment state, attempts, history, a verified event inbox, settlement and reconciliation records, and a fulfillment outbox. The host chooses and configures the database and applies its migrations. Provide a memory implementation for tests and a PostgreSQL reference integration using `database/sql` with an injected driver.

Before a mutation leaves the process, commit its operation reservation, scoped idempotency key, canonical fingerprint, and stable provider key. Call the provider outside the storage transaction. Commit the resulting history and state changes atomically, along with any outbox record. Prevent conflicting mutations while a reservation or uncertain outcome is outstanding. Reusing a key with changed parameters is a conflict. An unanswered request leaves an uncertain outcome that requires provider-specific query or recovery under its original identity.

An in-memory lock alone would lose reservations when a process restarts or a second worker takes over. Keeping a SQL transaction open during a provider call would consume database connections without making the remote effect atomic. Persistence and a provider-supported recovery path are both needed for restart safety.

Scope identifiers by merchant and environment. Record settlement and returns independently of capture and refund totals. A verified webhook is durable before the server acknowledges it. The host must make outbox consumption idempotent by event ID before treating it as a fulfillment trigger.

Reserve one payment claim per scoped host order identity so separately generated request keys cannot create duplicate charges. Bind provider observations to the expected merchant account, payment UUID, attempt or operation references, and relevant amount and currency. Apply responses and verified events through the same transaction rules. Preserve authoritative success when a late transport failure arrives.
