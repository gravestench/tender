# Changelog

## Unreleased — v1.0 development

- Initialized the independent `github.com/gravestench/tender` module under the 0BSD license, with a public API, durable operation reservations, idempotent state transitions, audit history, verified inbox and fulfillment outbox.
- Added a PostgreSQL store, versioned migrations, embeddable deterministic HTTP simulator and standalone checkout consumer.
- Added US/USD reference adapters and synthetic contracts for Adyen Visa/Mastercard, direct PayPal, Klarna financing and Adyen ACH with giact. Added signed callback processing, bounded recovery and capture-failure/ACH return review.
- Added settlement report projection and scoped reconciliation history/discrepancy handling. Automatic provider report downloads and production settlement closeout remain unsupported.
- Added offline browser, race and contract checks, protected manual sandbox workflow, security/operations guides, capability matrix and Blacktop integration seam.
- Official provider sandbox and production qualification remain BLOCKED. No v1.0.0 release or production claim is made.
