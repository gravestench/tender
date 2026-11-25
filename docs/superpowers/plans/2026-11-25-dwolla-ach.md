# Dwolla US ACH Adapter Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a safe Dwolla US/USD ACH provider candidate with real-wire offline transfer and signed receipt coverage, while preserving unqualified SANDBOX/LIVE gates.

**Architecture:** The `providers/dwolla` package owns endpoint validation, host authorization binding and Dwolla HAL transfer calls. `webhook/dwolla.go` verifies and persists signed notifications. Settlement and bank payout projection remain a distinct host-authenticated task because a transfer callback is not a deposit.

**Tech Stack:** Go 1.26 standard library, Tender Provider and Store contracts, Dwolla HAL v1, OAuth bearer tokens supplied by the host.

**Spec:** [Dwolla ACH design](../specs/2026-11-25-dwolla-ach-design.md)

**Current branch:** Task 1 transfer initiation, source preflight and the signed receipt portion of Task 3 are implemented with memory tests. Task 2's authenticated lost-reply recovery and Task 3's SQL restart contract remain open; Task 4 is blocked on merchant artifacts. The checkboxes below describe the complete qualification path, not a claim that this branch satisfies it.

## Global Constraints

- MOCK endpoint is loopback; SANDBOX and LIVE are fixed official hosts and advertise zero ACH capabilities until qualification.
- USD cents are rendered as exact decimal strings; no bank/account/routing number enters Tender.
- A Create ambiguity is held for review; no blind POST replay or early collection.
- HMAC uses the original raw body and a distinct subscription secret.

## Review Focus

- A shopper-controlled source ID must fail the host authorization check before POST.
- Cross-host or malformed funding-source and transfer links must fail closed.
- Timeout after POST must leave the operation unknown without another mutation.
- A duplicate or changed signed event ID must not alter a stored receipt.
- A pending or processed Dwolla transfer must never enqueue a collection.

---

### Task 1: Configuration and transfer initiation

**Files:** Create `providers/dwolla/dwolla.go`, `providers/dwolla/dwolla_test.go`.

**Interfaces:** `New(Config) (*Adapter,error)` and `Adapter.Execute/Query/Capabilities`; `Config.AuthorizeSource(ctx,payment,source)` is a required host callback, and `Config.BearerToken(ctx)` supplies a fresh OAuth token.

- [ ] Write constructor/market/endpoint and authorized-transfer request tests.
- [ ] Run `go test ./providers/dwolla -run 'TestConfig|TestCreate'` and observe expected failures.
- [ ] Implement exact MOCK HAL `POST /transfers` with `Idempotency-Key`, `correlationId`, fixed destination and no bank numbers; verify `Location` and authenticated GET transfer identity before returning pending with zero funds.
- [ ] Run focused tests and full `go test ./...` with loopback permission; commit.

### Task 2: Unknown outcome and recovery guard

**Files:** Modify `providers/dwolla/dwolla.go`; test `providers/dwolla/dwolla_test.go`.

- [ ] Write tests for lost reply, malformed Location, wrong amount/source/destination/correlation ID, and no second POST.
- [ ] Run focused tests to red, implement unknown-outcome handling and read-only `Query` only for a previously bound transfer reference; otherwise `ErrUnsupported`.
- [ ] Run focused tests, full suite and race package; commit.

### Task 3: Signed durable receipt

**Files:** Create `webhook/dwolla.go`, `webhook/dwolla_test.go`; extend `mockserver` only for Dwolla's documented event shape.

- [ ] Write failing raw-body HMAC, wrong-account, duplicate, changed-event and restart tests against memory and SQL stores.
- [ ] Implement bounded POST handler that validates all signed fields and durably inserts before 2xx. Keep receipt processing notice-only/pending until authenticated transfer correlation is proven.
- [ ] Run full tests, race contracts and vet; commit.

### Task 4: Settlement and qualification

**Files:** Create `providers/dwolla/report.go`, host-authenticated contract tests, `docs/providers/dwolla.md` and capability/evidence updates.

- [ ] Obtain actual merchant statement/report format and define exact gross, fee, return and payout identities before writing financial projection tests.
- [ ] Write red tests for delayed return, two transfer legs and payout/bank-deposit mismatch; implement only mappings proven by authenticated artifacts.
- [ ] Run native PostgreSQL and protected sandbox tests with merchant credentials and browser proof; keep qualification BLOCKED until this evidence exists.
