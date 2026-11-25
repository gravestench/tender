# Changelog

## Unreleased — v1.0 development

- Added MOCK-only review notices for exactly correlated Adyen chargeback reversals, second chargebacks and refund reversals. No webhook-derived money movement, report-reversal projection or provider qualification is claimed.
- Official provider sandbox and production qualification remain BLOCKED. The four-rail `v1.0.0` release awaits G3 and G4 evidence.

## v0.1.0-alpha.2 — MOCK preview (2026-11-25 UTC)

- Adds bounded Adyen card Settlement details, PayPal version 001 Settlement SFTP capture, and Klarna NET Settlements API projections. Scoped payout batch checks retain open discrepancies for missing parts, inconsistent totals and unmatched ledger movements. PayPal refund/reversal projection and payout feed/correlation remain open v1 work.
- Adds durable review notices for correlated Adyen chargebacks and failed refunds, PayPal capture denial after collection and ACH returns. A unique reconciliation claim ties a verified adverse event to one settlement row.
- Expands independent MOCK consumer coverage for PayPal, Klarna and ACH restart/recovery, settlement and adverse scenarios. Adds keyboard accessibility, navigation guards and optional redacted operational observations.
- Adds PostgreSQL migrations `005` through `007`. Apply them in order when upgrading from alpha.1. [Storage guide](docs/storage.md)
- Passed an independent tagged import and a durable loopback MOCK capture with duplicate submission and restart. [Release evidence](docs/evidence/release-gates.md)

## v0.1.0-alpha.1 — MOCK preview

- Initialized the independent `github.com/gravestench/tender` module under the 0BSD license, with a public API, durable operation reservations, idempotent state transitions, audit history, verified inbox and fulfillment outbox.
- Added a PostgreSQL store, versioned migrations, embeddable deterministic HTTP simulator and standalone checkout consumer.
- Added US/USD reference adapters and synthetic contracts for Adyen Visa/Mastercard, direct PayPal, Klarna financing and Adyen ACH with giact. Added signed callback processing, bounded recovery and capture-failure/ACH return review.
- Added settlement report projection and scoped reconciliation history and discrepancy handling.
- Added offline browser, race and contract checks, protected manual sandbox workflow, security/operations guides, capability matrix and Blacktop integration seam.
- A clean external consumer imported the tagged source and completed a USD 25.99 loopback capture. [Historical release evidence](docs/evidence/release-gates.md)
