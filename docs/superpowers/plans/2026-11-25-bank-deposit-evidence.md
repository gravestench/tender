# Bank Deposit Evidence Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Persist an independent bank-deposit comparison against a trusted, previously matched provider payout.

**Architecture:** Add a scoped bank evidence API separate from `SettlementReport`; PostgreSQL reads existing payout history and stores immutable evidence/review/unique claims transactionally. Memory stores claim semantics but explicitly lacks trusted payout history.

**Tech Stack:** Go 1.26, database/sql PostgreSQL, standard library, current Tender contracts.

**Spec:** `docs/superpowers/specs/2026-11-25-bank-deposit-evidence-design.md`

## Global Constraints

- Keep `PayoutMatched` as processor report versus ledger only.
- Never ingest raw bank account numbers or a raw statement.
- No claim of genuine provider or bank evidence from offline tests.

## Review Focus

- Reused bank movement or provider reference must not match another payout.
- Changed evidence under one identity must conflict, even after restart.
- Different merchant/environment scopes must remain isolated.
- A pending or mismatched processor payout must not produce a matched deposit.
- Concurrent claims must have one winner under serializable PostgreSQL transactions.

---

### Task 1: Bank evidence contract

**Files:** Create `bank_deposit.go`, `bank_deposit_test.go`; modify `store/memory.go`.

**Interfaces:** `Engine.ReconcileBankDeposit(ctx, reportID, payoutID, BankDepositEvidence) (BankDepositResult, error)`; optional `BankDepositTx` store extension.

- [ ] Write tests for validation, read-only bank verdict separation, memory unsupported boundary and immutable claim replay.
- [ ] Run focused tests and observe RED.
- [ ] Implement the minimal contract and memory claim storage.
- [ ] Run focused tests and observe GREEN.

### Task 2: PostgreSQL durability

**Files:** Create `store/postgres/bank_deposit.go`, `store/postgres/migrations/008_bank_deposits.up.sql`, `008_bank_deposits.down.sql`, `test/contract/bank_deposit_test.go`.

- [ ] Write SQL contract for exact match, mismatches, replay, conflicting/reused movement/reference, cross-scope isolation, restart and concurrent claim.
- [ ] Run focused SQL test and observe RED.
- [ ] Implement the migration and store extension.
- [ ] Run focused SQL test and observe GREEN.

### Task 3: Documentation and verification

**Files:** Modify `docs/storage.md`, `docs/operations.md`, `docs/evidence/adyen.md`.

- [ ] Document host authentication boundary, distinct verdicts, and unresolved real evidence.
- [ ] Run `go test -race ./...`, `go vet ./...`, native PostgreSQL contract when available, and inspect output.
- [ ] Inspect diff and commit the isolated branch locally.

## Verification record

- `go test -race -count=1 ./...` and `go vet ./...` passed in the isolated worktree.
- `go vet -tags=integration ./test/contract/...` passed.
- Focused bank-deposit SQL contracts passed with `-race` against an isolated PGlite socket server, using pgx `default_query_exec_mode=exec` to avoid prepared statement collisions across PGlite connections. The native PostgreSQL concurrency subtest is explicitly skipped for this single-client emulator.
- Native PostgreSQL could not start because the host exhausted macOS SysV shared-memory identifiers. The full integration suite cannot run on PGlite: the existing multi-connection `TestPostgresConflictingOperations` held for 60 seconds and the single-client socket server crashed on `ECONNRESET`. Native PostgreSQL CI remains the remaining test gate for concurrent claim serialization.
- These results are offline contract evidence only. No real bank statement or provider sandbox artifact was used.
