# Tender v1.0 design

Tender lets an application own its checkout, fulfillment decisions, and payment records while using acquiring processors, wallets, financing, and bank payments. The source specification is the 24 November 2026 Tender v1.0 implementation handoff. Build an independent module under `github.com/gravestench/tender` with an embeddable simulator and tested direct provider adapters. The full v1.0 scope covers Visa and Mastercard acquiring, PayPal, Klarna, and ACH.

## Approach

Build the core engine and persistence boundary once, then use a Go consumer with a browser checkout to prove a direct card integration over real HTTP. Prioritize this path so Blacktop has an early integration target. A pre-release can identify the capabilities that have passed actual sandbox verification while the remaining rails progress toward v1.0.

Implementing four adapters in parallel before the first consumer journey would delay feedback on the provider and store interfaces. Shipping Tender as a separate service would add a required runtime and duplicate infrastructure the host already owns. The library plus one vertical slice provides an independent integration point and exercises those interfaces early.

## Boundaries

The root module owns domain types, state transitions, provider contracts, and orchestration. It accepts contextual, typed operations and injects clocks, IDs, logging, storage, and HTTP clients. Provider packages own documented wire formats, authentication, status mapping, and verified callback parsing. Optional operations use capabilities and typed unsupported errors. Provider references remain opaque to consumers and scoped by merchant and environment. Each adapter documents the versions, markets, operations, and customer action modes it can verify.

The host decides order totals, owns checkout pages and authenticates access, configures storage and migrations, mounts webhook handlers, and schedules reconciliation and fulfillment. Hosts can use a transactional store contract with reference PostgreSQL queries and an injected SQL driver. A memory store serves unit tests. Keep the application dependency graph pointing toward Tender.

## Transaction lifecycle

Use signed integer minor units and validated currency exponents. Purchase amounts must be positive. Reservation checks atomically prevent capture beyond authorization and refunds beyond captured funds, including while another operation is in progress. Track authorized, captured, settled, refunded, and returned amounts independently. Append every operation and verified event to history.

An operation first persists its canonical request fingerprint, stable provider key, and conflict reservation. A scoped host order identity claims the payment independently of its operation keys. Each external adapter call runs outside a database transaction. Result application updates state and the fulfillment outbox atomically. A timeout after dispatch leaves the outcome uncertain until an authoritative result is known. Recover using provider queries or provider-defined recovery under the same key. Process restarts, out-of-order events, and duplicate requests can all occur after a provider has committed. Keep history and reconcile authoritatively before accepting conflicting requests.

Validate provider callbacks over the required original body or canonical signed fields. Persist a verified, deduplicated inbox record before acknowledgement, then process it asynchronously. Bind each observation to the merchant account, host payment UUID, attempt and operation references when present, and relevant amount and currency. Apply events and HTTP results using consistent authority and version checks. Query if an event is out of order or its reference is unresolved. Settlement reports are matched to internal records with observed gross, net, fees, and payout references. Record discrepancies and their resolution in an audit trail.

## Checkout and environment policy

Use provider-hosted card fields and documented in-page components, frames, or an approved in-page modal. Advertise methods only in contexts that satisfy strict on-page checkout. Require separate approval for popups and preserve required authentication. Return typed, expiring customer actions with opaque provider data. The browser only supplies an opaque result. Server-side records and provider verification determine whether fulfillment can proceed.

Separate `MOCK`, `SANDBOX`, and `LIVE` environments with explicit endpoint, data, and credential policies. Mocks are local-only and depend on no outside network. Sandbox uses official test hosts and credentials. Live requires approved merchant activation, hosts, and credentials. Protect endpoints, signing keys, logs, and records. The host must prevent raw sensitive credentials from entering its data path.

## Verification and release

Pin each simulator fixture to provider documentation or sanitized observations of official sandbox behavior. Offline contract tests exercise real adapters and compare provider and host records. An independent consumer mounts checkout, simulators, and webhook routes on its own listener, uses durable test persistence, and verifies status and one-time fulfillment. Offline CI runs unit, race, vet, contract, and browser checks as their suites land. Use a separate trusted workflow for opted-in sandbox checks and mark missing access BLOCKED.

Before v1.0, collect actual sandbox evidence for all required rails, verify the strict navigation policy, run applicable security checks, and validate that an independent module builds against the tagged version. Tag a pre-release if the full scope remains blocked. A production pilot requires separate merchant approval, applicable provider certification, assessment of compliance responsibilities, and controlled live transaction and settlement checks.

## Initial choices

Use Go 1.26+, 0BSD, and the standard library for the root runtime. Evaluate US/USD, Adyen Checkout for cards, direct PayPal, and the region-appropriate Klarna product. Select an ACH provider only after verifying its consent, status, returns, and reporting interfaces. Account access, eligibility, commercial terms, and supported action modes are tracked in the [capability matrix](../../capabilities.md).

Blacktop integration begins with a standalone consumer shaped around its server-owned order UUID, trusted totals, and fulfillment contracts. A Blacktop application change requires authorization for that repository. The single-merchant card slice leaves its existing platform payout and Stripe Connect paths available.
