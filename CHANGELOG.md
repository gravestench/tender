# Changelog

## Unreleased — v1.0 development

- Initialized the independent `github.com/gravestench/tender` module under the 0BSD license, with a public API, durable operation reservations, idempotent state transitions, audit history, verified inbox and fulfillment outbox.
- Added a PostgreSQL store, versioned migrations, embeddable deterministic HTTP simulator and standalone checkout consumer.
- Added US/USD reference adapters and synthetic contracts for Adyen Visa/Mastercard, direct PayPal, Klarna financing and Adyen ACH with giact. Added signed callback processing, bounded recovery and capture-failure/ACH return review.
- Added bounded Adyen card Settlement details, PayPal version 001 Settlement SFTP capture, and Klarna NET Settlements API projections. Scoped payout batch checks retain open discrepancies for missing parts, inconsistent totals and unmatched ledger movements. PayPal refund/reversal projection and payout feed/correlation remain open v1 work; automatic provider report downloads and production bank-deposit closeout remain unsupported.
- Added durable review notices for correlated Adyen chargebacks and failed refunds, PayPal capture denial after collection and ACH returns. Verified receipts and adverse notices preserve the original collection and fulfillment history.
- Expanded independent MOCK consumer coverage for PayPal, Klarna and ACH restart/recovery, settlement and adverse scenarios, plus keyboard accessibility and navigation guards. Added optional redacted operational observations.
- Added offline browser, race and contract checks, protected manual sandbox workflow, security/operations guides, capability matrix and Blacktop integration seam.
- Official provider sandbox and production qualification remain BLOCKED. No v1.0.0 release or production claim is made.
