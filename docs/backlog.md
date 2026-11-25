# Milestone backlog

The milestone numbers follow the Tender v1.0 implementation handoff. Exit criteria apply in addition to the [implementation plan](superpowers/plans/2026-11-24-tender-v1.md) and [completion plan](superpowers/plans/2026-11-25-tender-v1-completion.md). Local MOCK and host-supplied evidence work has advanced across all four rails; official qualification remains open.

| Milestone | Deliverable | Exit criteria |
| --- | --- | --- |
| M0 | Independent repository, license, CI, ADRs, capability matrix, and backlog | Module path matches the origin. A clean checkout builds without a host dependency. Provider access requirements are recorded. |
| M1 | Money, payment model, provider interfaces, typed errors, transaction store, and durable PostgreSQL integration | State, amount, concurrency, and restart tests pass. Uncertain operations retain their provider keys and prevent conflicting mutations. |
| M2 | Embeddable mockserver, deterministic scenarios, callback lifecycle, independent consumer, and offline Go contract runner | The host serves checkout, provider routes, and callbacks on one listener. A real adapter can exercise the provider contract over local HTTP. |
| M3 | Adyen card slice with hosted fields, 3-D Secure, lifecycle operations, verified events, and reconciliation | Offline browser and Go scenarios pass. Official sandbox evidence covers each advertised card capability. Prohibited navigation is disabled. |
| M4 | Direct PayPal and Klarna adapters, checkout components, and simulators | Contracts, browser coverage, and actual sandbox verification are complete for both providers. |
| M5 | Selected ACH provider, simulator, and cross-provider settlement/return reconciliation | Actual ACH sandbox evidence and durable discrepancy tests pass. Unsupported bank flows stay disabled. |
| M6 | Documentation, compatibility and compliance review, evidence, release candidate, and gated tag | Applicable checks and all four rail gates pass. Scope changes require explicit approval. Production activation remains a separate merchant gate. |

## Early Blacktop integration

1. Publish and independently verify the `v0.1.0-alpha.2` MOCK tag with a clean external import. The [release ledger](evidence/release-gates.md) records alpha.1 evidence and tracks the new tag's verification.
2. Use the verified MOCK tag and the independent consumer to map Blacktop's order totals, merchant scope, callback verification, durable store, fulfillment outbox and refund workflow. A direct Blacktop code integration and production pilot have yet to run. Follow the [Blacktop integration guide](blacktop-integration.md).
3. Obtain each merchant entitlement and run genuine sandbox browser, mutation, signed delivery and complete report checks. For ACH, approve and retain the one-time WEB debit mandate before real checkout. PayPal payout movement and refund/reversal report correlation remain v1 gaps.
4. Complete an approved, low-value live pilot with actual fee and bank-deposit comparisons before claiming a Blacktop cost reduction. Continue G3/G4 for the four-rail v1.0 release.

## External dependencies

- Adyen access and commercial onboarding for the reference cards slice.
- PayPal sandbox seller and buyer credentials, plus wallet browser-flow validation.
- Klarna regional product selection and playground access, plus embedded-flow validation.
- Adyen ACH/giact merchant contracting and sandbox access, plus approved signed-mandate handling, checkout-compatible consent and genuine return/report fixtures.
- Protected sandbox secrets and a controlled HTTPS callback endpoint for each provider.
- A merchant and provider-approved production pilot, including assessment scope, certification where applicable, and approval for narrowly scoped live transactions.

See the [capability matrix](capabilities.md) for evidence status.
