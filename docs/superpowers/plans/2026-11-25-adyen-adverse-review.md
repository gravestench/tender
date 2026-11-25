# Adyen Later Adverse Review Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Record three later signed Adyen card exceptions as durable, notice-only operator work when prior scoped evidence makes their identity unambiguous.

**Architecture:** Extend the existing Adyen verified-receipt projection and mock signed fixture. Correlate dispute events to processed adverse notices and a refund reversal to one succeeded refund operation. Keep the core store schema and payment funds unchanged.

**Tech Stack:** Go 1.26, `net/http`, Tender's verified inbox/adverse review store, memory and PostgreSQL contracts.

**Spec:** [Adyen later adverse review design](../specs/2026-11-25-adyen-adverse-review-design.md); [Tender v1 handoff](/Users/dylanknuth/Downloads/Tender_v1.0_Implementation_Handoff.pdf) R04/R05/R06.

## Global Constraints

- MOCK tests cannot qualify SANDBOX or LIVE.
- No principal, settlement, payout or fulfillment movement from these notices.
- Missing signed original PSP or ambiguous disputed/refund PSP remains pending.
- Do not use unsigned `additionalData`, reason or event timestamp as authority.

## Review Focus

- Cross-payment dispute PSP reuse must not authorize a notice on another payment.
- Missing `originalReference` on dispute reversal must remain pending.
- Reordered second chargeback must remain pending until the prior reversal is processed.
- Partial refunds of equal amount must not correlate by amount alone.
- Replay after restart must leave one notice and unchanged funds/outbox.

---

### Task 1: Signed later adverse notices

**Files:** Modify `mockserver/adyen.go`, `webhook/adyen.go`, `docs/providers/adyen.md`, `docs/evidence/adyen.md`, `docs/capabilities.md`, `CHANGELOG.md`; add `test/contract/adyen_later_adverse_test.go`.

**Interfaces:** `Adyen.Process(ctx, eventID)` already calls `Engine.ProcessVerifiedAdverseEvent`; `AdverseEvidenceReader.AdverseEvidence(scope, provider, account, paymentID)` returns processed notices; `EventRecords.OperationByReference(scope, paymentID, PSP)` returns a bound operation. `Server.AdyenAdverseNotification` emits locally signed Standard events.

- [ ] Add memory and PostgreSQL contract tests for CHARGEBACK → CHARGEBACK_REVERSED → SECOND_CHARGEBACK, requiring distinct notices with the same dispute PSP, exact signed original PSP, account, amount and currency, while payment funds/outbox remain unchanged.
- [ ] Add reversal-before-chargeback and second-before-reversal tests: processing returns a conflict, leaves the verified receipt pending, and succeeds once prerequisite notices are processed; replay adds no notice. Test wrong payment/account/PSP/amount and missing original reference.
- [ ] Add REFUNDED_REVERSED test with two equal partial refunds: only the exact succeeded refund modification PSP and amount may create one notice; unknown or cross-payment PSP remains pending. Verify unchanged refunded principal and outbox after restart.
- [ ] Run `go test -tags=integration -run '^TestAdyenLaterAdverse' ./test/contract/...`; expect the assertions to fail because the new event codes are unsupported.
- [ ] Extend the mock fixture allowlist and Adyen Process dispatch. In `adverse`, use only canonical signed fields, the processed prior adverse notice or exact succeeded refund operation. Keep `AdverseNotice` notice-only and do not change store schema.
- [ ] Run focused contract tests, then `go test -race ./...`, race-enabled integration contracts against disposable PostgreSQL, `go vet ./...`; expect PASS. Update provider/evidence docs with MOCK scope and merchant identity limitations; request independent review, fix findings and commit exact paths.
