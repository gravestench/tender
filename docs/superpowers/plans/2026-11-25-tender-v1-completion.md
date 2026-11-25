# Tender v1.0 Completion Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close the remaining locally testable financial, consumer and operational gaps while preserving the four-rail v1.0 target and the external sandbox and browser release gates.

**Architecture:** Keep provider wire and report parsing in their adapters, financial authority in signed events and trusted reports, and comparison-only reconciliation in the engine. The independent consumer exercises actual adapters over loopback HTTP and durable PostgreSQL. A host still owns report authentication, checkout policy, worker scheduling and merchant activation.

**Tech Stack:** Go 1.26+, `net/http`, `database/sql` with injected PostgreSQL driver, PostgreSQL 17, pinned Playwright/Chromium, provider-owned browser SDKs, GitHub Actions.

**Spec:** [Tender v1.0 design](../specs/2026-11-24-tender-v1-design.md), derived from `/Users/dylanknuth/Downloads/Tender_v1.0_Implementation_Handoff.pdf`, especially §§4–13. Read [the original plan](2026-11-24-tender-v1.md), [release gates](../../evidence/release-gates.md), and the provider source audit at `docs/provider-v1-completion-research.md` when available.

## Global Constraints

- Preserve `github.com/gravestench/tender`, Go 1.26+, 0BSD, a standalone importable module and no mandatory external Go payment SDK or server process.
- Keep Visa/Mastercard, PayPal, Klarna and US ACH in the v1.0 target. `v0.1.0-alpha.1` proves MOCK import only; no live or full-v1 claim follows from it.
- Use exact merchant, environment, provider account, market, payment and operation correlation before changing funds or issuing host review work. No browser callback, unverified report or aggregate GET is financial authority by itself.
- Keep MOCK, SANDBOX and LIVE separate; disable unqualified real methods and all prohibited top-level navigation or unapproved popup paths. Do not weaken a required authentication step.
- Signed adverse events and authenticated reports may arrive before or after HTTP results and after fulfillment. Retain prior gross history, use one durable review item per event and leave ambiguous evidence pending for investigation.
- Supported means documented for that provider, merchant product and market. Treat unavailable ACH void/refund, PayPal capture-intent void and any unqualified optional operation as **NOT APPLICABLE/ErrUnsupported**, never a simulated success.
- Use provider documentation and sanitized official fixtures for wire/report decisions. Do not invent fields or claim a sandbox PASS without merchant-generated transaction, webhook, report and browser evidence.
- Work in this checkout using exact-path staging or `git commit --only`; prior parallel staging once mixed two agents' changes. Each task needs a fresh implementation review before the next dependent task.

## Review Focus

- A payout line with a valid transaction sum but an unaccounted reserve or fee, missing report part, or mismatched provider section total must produce an open discrepancy, not a matched payout. Task 13.
- An original payment reference and modification reference that resolve to different scoped payments must not be accepted because their amounts happen to agree. Tasks 13, 15–17.
- An adverse event for another merchant, payment or modification must not freeze or reverse this order; a matching late event must create one durable review item. Tasks 14–16.
- Duplicate or reordered report/event delivery across a PostgreSQL restart must not double count money, history, review items or fulfillment. Tasks 13–16.
- A browser approval followed by abandonment or a lost mutation response must remain pending until server or signed evidence resolves it; the checkout must not resubmit with a new provider key. Tasks 18–20.
- Keyboard-only users must reach, understand and leave every synthetic action and error state without a focus trap or forbidden navigation. Task 20.

## Dependency and File Ownership

| Sequence | Owner and exact files | Why it comes first |
| --- | --- | --- |
| 13 | `reconcile.go`, `store/postgres/reconcile.go`, `store/postgres/migrations/005_payout_batches.*.sql`, `test/contract/reconciliation_test.go`, `docs/storage.md` | Defines the batch/payout contract consumed by report mappers. |
| 14 | `adverse.go`, `store/memory.go`, `store/postgres/adverse.go`, `store/postgres/migrations/006_adverse_review.*.sql`, `store.go`, `test/contract/adverse_test.go` | Defines a generic durable adverse review record; no provider interprets one yet. |
| 15–17 | Provider-specific `report.go`, callback/fixture and contract files listed below | Can be delegated in parallel after 13–14 only when no shared file is staged. |
| 18–20 | Separate rail E2E files and consumer files listed below | Consume reviewed provider behavior and can run sequentially to avoid consumer-router overlap. |
| 21 | `observer.go`, `engine.go`, `reconcile.go`, callback wrappers, `observer_test.go` | Adds optional redacted signals after financial state behavior is stable. |
| 22 | Sandbox gate tests and evidence files listed below | Exercises the same reviewed adapter against official endpoints when access exists. |
| 23 | CI, release ledger, capability matrix, changelog and handoff report | Verifies all changed paths and preserves BLOCKED external gates. |

### Task 13: Reconcile payout batches without changing payment funds

**Files:** Modify `reconcile.go`, `store/postgres/reconcile.go`, `test/contract/reconciliation_test.go`, `docs/storage.md`. Create `store/postgres/migrations/005_payout_batches.up.sql` and `.down.sql`.

**Interfaces:** Extend `SettlementLine` with `BatchID`, `OriginalReference` and `ModificationReference` strings; keep `ProviderReference` for existing callers. Where a report supplies both references, resolve both and require the same scoped payment and the exact succeeded capture/refund operation before matching the line. Add `PayoutLine{ID, BatchID, Currency, PayoutReference string; AmountMinor int64; ObservedAt time.Time}` and `PayoutAdjustment{ID, BatchID, Currency, Type string; SignedMinor int64}` to `SettlementReport` as `Payouts` and `Adjustments`. `PayoutLine.AmountMinor` is **merchant-positive**: money delivered to the merchant bank is positive even when the provider's account journal records the payout as a debit; a debit from the merchant bank is negative. Never compare an unconverted provider sign.

Add `ReportPart{ID, SectionID, Digest string; Number, TotalParts, DeclaredRows int}` and `ReportSection{ID, BatchID, Currency string; DeclaredRows int; DeclaredNetMinor, DeclaredPayoutMinor *int64}` to `SettlementReport` as `Parts` and `Sections`. The host supplies authenticated artifact digests; a filename or marker alone is not authentication. Extend `ReconciliationRun` with `PayoutResults []PayoutReconciliationResult`, whose outcome distinguishes matched, missing/duplicate payout, incomplete artifact/section, provider-total mismatch, currency mismatch and amount mismatch. The report digest and SQL uniqueness include all sorted input collections. `Engine.Reconcile` accepts a bounded report with at least one financial line, adjustment or payout, but matches a payout only when every numbered part of every required account/section is present exactly once, row counts and provider-declared totals agree, and `sum(line.NetMinor)+sum(adjustment.SignedMinor)` equals the canonical payout for the exact provider/account/environment/batch/currency. Missing or unexplained sections remain open; an adjustment-only or payout-only report is ingested for audit and is unresolved unless independently complete. Reconciliation never changes `Payment.Funds` or the fulfillment outbox.

- [ ] Write `TestReconcilePayoutBatches` with two settled payments, one refund, a transaction fee, a separately reported batch adjustment, a provider-debit payout normalized merchant-positive, a payout mismatch, duplicate payout ID, cross-merchant batch reuse, an original/modification reference collision across payments, changed report identity and replay after reopening PostgreSQL. Add missing multipart file, duplicate part, missing section, wrong declared row count or net total, and bounded payout-only/adjustment-only cases. Assert no incomplete or mismatched batch is marked matched, one immutable result per state change, checked signed arithmetic and zero payment/outbox mutations.
- [ ] Run `go test -tags=integration ./test/contract/... -run '^TestReconcilePayoutBatches$' -count=1`; expect compilation or assertion failure.
- [ ] Add the types, deterministic digest, checked integer arithmetic, scoped SQL persistence and migration. Preserve the existing line-only `Reconcile` behavior for reports without a payout section, but mark their payout status unverified. Report mappers must identify which provider sections and totals are mandatory for their format; a host-provided `Complete=true` flag is insufficient.
- [ ] Run the named test twice under `-race` on native or disposable PostgreSQL 17, then `go test ./...` and `go vet ./...`; expect PASS. Review and commit only this task's paths.

### Task 14: Persist adverse financial review work

**Files:** Create `adverse.go`, `store/postgres/adverse.go`, `store/postgres/migrations/006_adverse_review.up.sql`, `.down.sql`, and `test/contract/adverse_test.go`. Modify `store.go`, `store/memory.go`.

**Interfaces:** Add `AdverseNotice` with `Scope Scope`, `Amount Money`, `ObservedAt time.Time`, and string fields `Provider`, `MerchantAccount`, `PaymentID`, `ProviderEventID`, `Kind`, `ProviderReference`. Add `AdverseReviewTx.EnqueueAdverse(AdverseNotice) error`, `AdverseReviews(context.Context, Scope) ([]AdverseNotice, error)` and `ReviewAdverse(context.Context, AdverseKey, AdverseDecision) error` on memory and PostgreSQL stores. `AdverseKey` includes merchant/environment/provider/account/event ID; `AdverseDecision` carries a bounded disposition, opaque reviewer ID, UTC review time and redacted rationale. Review is an idempotent, audited transition, never deletion. The queue is separate from `CaptureFailureNotice` and does not infer refund, dispute or return principal.

Add `Engine.ProcessVerifiedAdverseEvent(ctx context.Context, id string, project func(EventRecords, json.RawMessage) (AdverseNotice, error)) error`. In one store transaction, it reads an **already verified** inbox receipt, checks the provider-specific projection, inserts the scoped notice and marks that same receipt processed; it does not call `ProcessVerifiedEventWithRecords` or apply an `Observation`. Providers that also have an authoritative funds change must use an equally atomic receipt/funds/notice path, rather than acknowledging a receipt before one of those writes commits.

- [ ] Write `TestAdverseReviewDurability` for duplicate event delivery, conflicting event payload, cross-merchant isolation, restart, concurrent enqueue, a late event after fulfillment, reviewer transition and replay after review. Inject failure on notice insert and on processed-marker write: each must roll back the whole verified-receipt processing transaction and leave the receipt retryable. Assert one unresolved notice and unchanged payment/outbox for notice-only events.
- [ ] Run `go test -tags=integration ./test/contract/... -run '^TestAdverseReviewDurability$' -count=1`; expect failure.
- [ ] Implement atomic insert, scoped listing and reviewed-state update in the existing store transaction model, with bounded redacted fields and additive migration. Keep the no-observation processor separate from the existing observation-applying API.
- [ ] Run the named test under `-race`, migrations up/down on a disposable database, root tests and vet; expect PASS. Review and commit only these paths.

### Task 15: Map Adyen card reports and verified adverse events

**Files:** Create `providers/adyen/report.go`, `providers/adyen/report_test.go`, `test/contract/adyen_adverse_test.go`. Modify `webhook/adyen.go`, `mockserver/adyen.go`, `docs/providers/adyen.md`, `docs/evidence/adyen.md`.

**Interfaces:** `adyen.ProjectCardSettlement(fields map[string]string, observedAt time.Time) (tender.SettlementLine, error)` and `adyen.ProjectPayout(fields map[string]string, observedAt time.Time) (tender.PayoutLine, error)` accept a host-authenticated Settlement details row. Pin header names, journal types, batch/section metadata and signed debit/credit/fee direction to [Adyen's official report](https://docs.adyen.com/reporting/settlement-reconciliation/transaction-level/settlement-details-report/). Populate original PSP and modification PSP separately; require both to resolve to the same scoped payment/operation where present. Map `Settled`, `Refunded`, documented dispute rows and `MerchantPayout`, converting its provider-debit journal to canonical merchant-positive payout; unsupported or ambiguous rows return typed errors. `Adyen.Process` handles independently signed, correlated `CHARGEBACK` and `REFUND_FAILED` at minimum, creating Task 14 review notices and preserving prior capture/refund history. Other documented adverse codes remain explicitly unsupported until their amount/reference meaning is pinned.

- [ ] Write failing mapper and signed-webhook tests: valid card capture/refund plus fee and payout batch; wrong merchant/currency/reference, original PSP from payment A with a modification PSP from payment B, fee sign, duplicate payout, incomplete batch, `CHARGEBACK` before/after fulfillment, `REFUND_FAILED` before/after HTTP acknowledgment, replay and unrelated modification. Assert no unverified principal or duplicate notice.
- [ ] Run `go test ./providers/adyen -run 'TestProjectCardSettlement|TestProjectPayout'` and `go test -tags=integration ./test/contract/... -run '^TestAdyenAdverse'`; expect failure.
- [ ] Implement projection, simulator fixtures and verified inbox processing using the core ledger and Task 14 queue. Do not treat an informational dispute notice or a report alone as a refund reversal or final chargeback.
- [ ] Run focused tests with `-race`, root tests, vet and card browser E2E; expect PASS. Review and commit only these paths.

### Task 16: Map PayPal settlement and verified adverse events

**Files:** Create `providers/paypal/report.go`, `providers/paypal/report_test.go`, `test/contract/paypal_adverse_test.go`. Modify `providers/paypal/webhook.go`, `mockserver/paypal.go`, `docs/providers/paypal.md`, `docs/evidence/paypal.md`.

**Interfaces:** `paypal.ProjectSettlement(fields map[string]string, observedAt time.Time) (tender.SettlementLine, error)` and `paypal.ProjectPayout(fields map[string]string, observedAt time.Time) (tender.PayoutLine, error)` consume bounded, host-authenticated rows from the [official Settlement SFTP report](https://developer.paypal.com/reports/settlement/) or a separately pinned disbursement feed; expose only a redacted projection, never buyer IDs. Pin `RH`/`SH` account and report version, every multipart file number, `SB` transaction/event/reference and available `SF`/`RF` count/currency-total semantics before a payout can match. An `RF` balance footer is not bank payout evidence. Pin a real merchant payout movement such as documented bank-withdrawal T-code `T0400` and its bank reference, normalize its debit to merchant-positive, and keep PayPal payout reconciliation an **open v1 gap** until a merchant report proves the selected feed and correlation. Unsupported report variants return `ErrUnsupported`, not `NOT APPLICABLE`. Where available, populate original order/capture and modification/refund references separately and require same scoped payment/operation. The verified webhook path accepts [documented](https://developer.paypal.com/payment-methods/webhooks/) `PAYMENT.CAPTURE.PENDING` and `PAYMENT.CAPTURE.DENIED` with exact seller/order/capture/amount correlation, and processes refund/reversal events only after their official resource IDs and totals are independently corroborated. A post-capture adverse event creates Task 14 review work without erasing a prior fulfillment.

- [ ] Write failing tests from official sanitized rows and webhook shapes for credit/debit, optional fee, `T0400` withdrawal, missing multipart file or section total, wrong related ID, buyer or merchant, pending-then-completed, denied-after-reservation, refund/chargeback replay and a late adverse event. Assert no collection on PENDING/DENIED, one notice, no cross-order effect and no matched payout without a complete report set.
- [ ] Run `go test ./providers/paypal -run '^TestProjectSettlement'` and `go test -tags=integration ./test/contract/... -run '^TestPayPalAdverse'`; expect failure.
- [ ] Implement only the report T-codes and webhook event types whose signed/postback-verified semantics are pinned. Mark unknown or ambiguous T-codes/events unsupported and retain the verified inbox record for operator review.
- [ ] Run focused tests under `-race`, root tests, vet and PayPal E2E; expect PASS. Review and commit only these paths.

### Task 17: Map Klarna settlement records

**Files:** Create `providers/klarna/report.go`, `providers/klarna/report_test.go`, `test/contract/klarna_report_test.go`. Modify `docs/providers/klarna.md`, `docs/evidence/klarna.md`.

**Interfaces:** `klarna.ProjectSettlement(fields map[string]string, observedAt time.Time) (tender.SettlementLine, error)` and `klarna.ProjectPayout(fields map[string]string, observedAt time.Time) (tender.PayoutLine, error)` map the selected merchant's [official settlement-file format](https://docs.klarna.com/acquirer/klarna/after-payments/settlements/settlement-files/) after pinning the product/version. Bind `order_id` and `capture_id` or `refund_id` as separate original/modification references to the same scoped payment and operation. Keep separate transaction, fee and payout rows; do not fold a fee row into a payment amount or claim a generic combined file is ordinary CSV.

- [ ] Write failing report tests for SALE, RETURN, fee, payout, partial captures, duplicate reference, missing modification ID, another merchant/currency and unsupported journal type; check projected batch totals against Task 13.
- [ ] Run `go test ./providers/klarna -run '^TestProjectSettlement'` and `go test -tags=integration ./test/contract/... -run '^TestKlarnaReport'`; expect failure.
- [ ] Implement only documented fields for the chosen merchant format and return `ErrUnsupported` when the report variant cannot be attributed safely. Keep provider download/authentication host-owned.
- [ ] Run focused tests under `-race`, root tests and vet; expect PASS. Review and commit only these paths.

### Task 18: Exercise PayPal's durable consumer lifecycle

**Files:** Modify `test/e2e/paypal_test.go`, `examples/consumer/paypal.go`, `examples/consumer/paypal.js`, `mockserver/paypal.go` only for missing synthetic routes. Do not change ordinary web availability.

**Interfaces:** The existing consumer routes and real PayPal adapter run over the same loopback listener and PostgreSQL store. Synthetic approval is never evidence of a permitted real browser presentation.

- [ ] Add failing `TestPayPalDurableLifecycle` subcases for buyer cancellation, decline/denial, pending, partial refund, refresh, abandonment, lost capture reply and restart. Inspect both mock provider state and SQL payment/operation/inbox/outbox/host fulfillment records; concurrent submits must yield one capture and one fulfillment.
- [ ] Run `go test -tags=e2e,integration ./test/e2e/... -run '^TestPayPalDurableLifecycle$'`; expect failure.
- [ ] Add only the consumer/fixture behavior needed to exercise those cases, using stable provider keys and verified callbacks; prohibited popup/redirect modes remain disabled.
- [ ] Run the named test under `-race` with PostgreSQL/Chromium and the existing PayPal contract; expect PASS. Review and commit these paths.

### Task 19: Exercise Klarna's durable consumer lifecycle

**Files:** Modify `test/e2e/klarna_test.go`, `examples/consumer/klarna.go`, `mockserver/klarna.go` only for missing synthetic routes.

**Interfaces:** Use the existing Klarna order and modification APIs and the consumer's trusted price and fulfillment ledger. A browser approval alone must never fulfill.

- [ ] Add failing `TestKlarnaDurableLifecycle` subcases for partial capture, refund, cancel/release, pending fraud, ineligible product, expiry, abandonment, lost order/capture response and restart. Inspect provider fixture and durable host records; one full capture produces one fulfillment, no approval or pending status does.
- [ ] Run `go test -tags=e2e,integration ./test/e2e/... -run '^TestKlarnaDurableLifecycle$'`; expect failure.
- [ ] Add the missing example routes/fixture controls without enabling ordinary SANDBOX/LIVE web approval.
- [ ] Run the named test under `-race` with PostgreSQL/Chromium and existing Klarna contracts; expect PASS. Review and commit these paths.

### Task 20: Exercise ACH settlement, return and accessible checkout

**Files:** Modify `test/e2e/ach_test.go`, `test/e2e/consumer_test.go`, `webhook/adyen_ach.go`, `examples/consumer/checkout.html`, `examples/consumer/adyen.html`, `examples/consumer/paypal.html`, `examples/consumer/component.html`, `examples/consumer/order.js` only where an accessibility assertion proves a gap.

**Interfaces:** Use the Adyen ACH simulator, signed Standard callback and authenticated settlement projection from the existing API; leave ACH void/refund `ErrUnsupported`. Browser tests cover synthetic UI only; real Adyen SDK and mandate accessibility remain G4 BLOCKED.

- [ ] Add failing `TestACHDurableReturn` for consent denial, validation refusal, pending, delayed signed settlement, return after settled/fulfilled, duplicate/reordered callbacks, restart and one compensation notice. Add `TestCheckoutKeyboardAccess` for tab order, focus after error, accessible button names/status announcements, refresh, no focus trap and no popup/navigation in the mock surfaces.
- [ ] Run `go test -tags=e2e,integration ./test/e2e/... -run 'TestACHDurableReturn|TestCheckoutKeyboardAccess'`; expect failure.
- [ ] Wire missing synthetic display or worker behavior and enqueue a Task 14 adverse notice for a verified post-fulfillment return; never collect bank account fields in the host. Keep real ACH disabled pending approved mandate and credentialed browser evidence.
- [ ] Run named E2E tests under `-race` with PostgreSQL/Chromium, existing ACH contracts, root tests and vet; expect PASS. Review and commit only these paths.

### Task 21: Expose optional redacted operational observations

**Files:** Create `observer.go`, `observer_test.go`. Modify `engine.go`, `reconcile.go`, `webhook/adyen.go`, `providers/paypal/webhook.go`, `providers/klarna/callback.go`, `docs/operations.md`.

**Interfaces:** `EngineConfig.Observer Observer` is optional; `Observer.Observe(context.Context, OperationalEvent)` receives bounded event kind, UTC time, scoped opaque payment/operation IDs, provider, merchant/environment, duration and typed error kind. No token, key, raw provider body, browser data, report row, buyer or bank/card fields may enter the event. Callback wrappers and reconciliation emit receipt lag and discrepancy counts. Observer failures/panics must not change payment outcomes; an implementation may attach `slog`/metrics at the host boundary.

- [ ] Write failing `TestOperationalObserver` for success, timeout/unknown, rate limit, webhook lag, reconciliation discrepancy, context cancellation and a hostile observer. Assert exactly one event per stage, redaction and unchanged funds/outbox if the observer fails.
- [ ] Run `go test ./... -run '^TestOperationalObserver$'`; expect failure.
- [ ] Add the nonblocking hook at committed stage boundaries, avoiding network calls or logging inside store transactions. Document correlation and aggregation semantics.
- [ ] Run focused tests under `-race`, provider contracts, root tests and vet; expect PASS. Review and commit these paths.

### Task 22: Make official sandbox gates capable of proving the same adapter

**Files:** Modify `test/sandbox/adyen_test.go`, `test/sandbox/paypal_test.go`, `test/sandbox/klarna_test.go`, `test/sandbox/ach_test.go`, `.github/workflows/sandbox.yml`, and rail-specific `docs/evidence/*.md` only as required by the test. Do not add secrets.

**Interfaces:** Each named test runs the same adapter package against its official test endpoint and correlates host records with actual provider-generated transactions, signed deliveries and report/portal records. An existing-order read may supplement, but cannot substitute for, the mutation and browser path. Keep the main-only protected workflow and per-provider step secrets.

- [ ] Add local no-credential tests/assertions that every requested qualification returns visible `BLOCKED`, never `Skip`/PASS. Include a fixture proving the sandbox harness rejects mock endpoints, synthetic webhook verification, mismatched merchant/product, stale evidence and a forbidden navigation.
- [ ] Run `PAYMENTS_ENV=sandbox go test -tags=sandbox ./test/sandbox/...`; without protected merchant access expect an explicit nonzero BLOCKED result. Run the new harness validation offline and expect PASS.
- [ ] Extend PayPal and Klarna gates from post-hoc reads to adapter mutation/event scenarios only after a documented policy-compatible on-page path is available. Until then keep real checkout disabled and the relevant test `BLOCKED`. For Adyen card and ACH, require the original host-issued payment, provider event, report and registered-origin browser proof; no mock fallback.
- [ ] Review security and evidence documents. With protected merchant access, run one qualification per provider on `main` and record sanitized IDs, API/SDK version, merchant market, expected/observed amounts/status, browser/device and report link. Do not mark a scenario PASS from a mock or from a read-only sample. Commit the honest gate code/docs only; external tests remain BLOCKED until actually run.

### Task 23: Reverify, publish evidence and audit the full v1.0 gate

**Files:** Modify `.github/workflows/ci.yml`, `docs/evidence/release-gates.md`, `docs/capabilities.md`, `docs/backlog.md`, `CHANGELOG.md`, `README.md` and provider evidence files as needed. No code-only commit may silently change a capability claim.

**Interfaces:** G1 is an independently imported tagged module; G2 covers full unit/race/native PostgreSQL/real-adapter HTTP/Chromium suites for all four rails; G3 requires actual merchant sandbox records for every advertised capability; G4 requires supported-device on-page and sensitive-data evidence. Production activation remains a separate merchant gate.

- [ ] Run `go test ./...`, `go test -race ./...`, `go vet ./...`, `go test -tags=integration ./test/contract/...`, `go test -tags=e2e,integration ./test/e2e/...` and representative fuzz targets. Use native PostgreSQL 17 for the full conflict suite; record exact commit, commands, exit codes and CI run links.
- [ ] Re-run clean external `go get`/build at the proposed tag with no `replace`. Run selected `PAYMENTS_ENV=sandbox go test -tags=sandbox ./test/sandbox/...` only through protected merchant access. Record unavailable official scenarios as BLOCKED and unsupported provider operations as NOT APPLICABLE with reasons.
- [ ] Obtain independent task reviews and a whole-branch financial/security review. Reconcile the capability matrix with actually verified markets, action modes and reports; list PayPal payout feed/correlation as a v1 gap until a real merchant report pins it. Publish a concise handoff with host setup and migration changes.
- [ ] Tag `v1.0.0` only if every applicable G1–G4 item has direct evidence and no unresolved critical correctness/security issue remains. Otherwise retain an explicitly labeled pre-release and the full four-rail goal; do not call any merchant production-ready without separate account approval, live pilot and bank-deposit matching.

## External blockers that code cannot turn into PASS

- **Adyen card:** real test merchant, API/client keys, registered HTTPS origin, HMAC, official Visa/Mastercard transactions including permitted 3DS, genuine modification/webhook/report records and supported-device browser proof.
- **PayPal:** seller/buyer app, actual transaction webhook, complete Settlement SFTP file set and a verified bank-withdrawal/payout feed and reference, plus a documented desktop/mobile in-page approval path or explicit product approval for a separate popup. The payout feed is a v1 gap, not `NOT APPLICABLE`; current ordinary web method stays disabled.
- **Klarna:** eligible US/USD financing merchant/product/customers, actual order and callback/report, plus an approved on-page financing presentation. Current ordinary web method stays disabled.
- **ACH:** enabled Adyen/giact test merchant, approved electronic mandate design, credentialed Sessions browser transport, real debit/return/signed notification and settlement report. Eligibility at major US banks is not universal provider or account acceptance.
- **Production:** merchant commercial terms and security/PCI/ACH assessment, provider certification where applicable, approved small live pilot, fees, payout and bank-deposit comparison. These are not library-release test substitutes.
