# Milestone backlog

The milestone numbers follow the Tender v1.0 implementation handoff. Exit criteria apply in addition to the [implementation plan](superpowers/plans/2026-11-24-tender-v1.md).

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

1. Validate the host integration boundary with a standalone consumer and agree on the order, verified-event, and fulfillment contracts.
2. Build the M3 offline card purchase, refund, and recovery paths as soon as the relevant M1 and M2 dependencies are ready. Use that slice to test importing Tender into Blacktop once a change to Blacktop is authorized.
3. Produce an explicitly labeled card pre-release after verification for its advertised scope. For real payments, get merchant approval, verify the strict checkout policy in the sandbox, and complete production activation together with the acquiring partner.
4. Continue M4 through M6 for the four-rail v1.0 release.

## External dependencies

- Adyen access and commercial onboarding for the reference cards slice.
- PayPal sandbox seller and buyer credentials, plus wallet browser-flow validation.
- Klarna regional product selection and playground access, plus embedded-flow validation.
- ACH partner selection, contracting and sandbox access, plus checkout-compatible consent and return fixtures.
- Protected sandbox secrets and a controlled HTTPS callback endpoint for each provider.
- A merchant and provider-approved production pilot, including assessment scope, certification where applicable, and approval for narrowly scoped live transactions.

See the [capability matrix](capabilities.md) for evidence status.
