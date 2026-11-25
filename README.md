# Tender

Tender is an independent Go payment library for applications that own their checkout and payment records. Its v1.0 target covers Visa and Mastercard acquiring, PayPal, Klarna and US ACH debit. The host owns trusted order totals, merchant eligibility, checkout policy, fulfillment, storage, operator review and deployment. The module path is `github.com/gravestench/tender`; Go 1.26 or newer is required.

**Development status:** the four adapter protocols, embeddable HTTP simulator, durable PostgreSQL store, independent consumer and offline checks are in development. Recent local work adds provider-specific settlement projections, scoped payout discrepancy checks, adverse review records, durable browser scenarios and optional redacted operational observations. These are synthetic or host-supplied checks. No provider is qualified for production, and full-scope `v1.0.0` is **not tagged**. Official sandbox access, genuine transaction and complete settlement proof, and mobile/desktop provider on-page checks are missing. See the [capability matrix](docs/capabilities.md) and [release ledger](docs/evidence/release-gates.md) before exposing a method.

Tender `v0.1.0-alpha.2` is available for **MOCK-only integration** in Blacktop and other Go hosts. In a separate consumer module, run `go get github.com/gravestench/tender@v0.1.0-alpha.2` and build without a local `replace`. Keep real payment methods disabled. [Tag](https://github.com/gravestench/tender/tree/v0.1.0-alpha.2) · [Independent import instructions](examples/consumer/README.md#verify-the-tagged-offline-alpha) · [Release evidence](docs/evidence/release-gates.md)

## Use in a host

A host imports `github.com/gravestench/tender`, chooses a scoped provider adapter and a durable `Store`, then creates an `Engine` for that merchant and environment. Keep provider API keys server-side. The host passes its authenticated order ID, server-calculated amount and stable idempotency key; a browser cannot choose the amount. Mount verified callback handlers and run inbox, recovery, fulfillment and reconciliation workers. The [standalone consumer](examples/consumer/README.md) is an executable example using MOCK and local PostgreSQL. The [Blacktop integration seam](docs/blacktop-integration.md) maps these boundaries to its existing order flow without changing Blacktop.

Use the [provider guides](docs/providers/adyen.md), [PayPal](docs/providers/paypal.md), [Klarna](docs/providers/klarna.md) and [ACH](docs/providers/ach.md) for exact scope and setup. Card entry belongs in Adyen hosted fields. ACH account entry belongs in the provider's Sessions browser path; real ACH remains disabled until a signed mandate design is approved. Ordinary PayPal and Klarna web approval is disabled under the strict no-redirect/no-unapproved-popup policy.

## Verify locally

The default unit suite is offline. A disposable PostgreSQL 17 database in `TENDER_TEST_DATABASE_URL` is needed for the tagged integration and browser suites. Install Node and the pinned Playwright runner with `npm ci`, then `npx playwright install chromium` for local Chromium checks. The browser suite also needs OpenSSL. Pull request CI provisions these dependencies.

```sh
go build ./...
go test ./...
go test -race ./...
go vet ./...
go test -tags=integration ./test/contract/...
go test -tags=e2e,integration ./test/e2e/...
(cd examples/consumer && GOWORK=off go build -o /tmp/tender-consumer .)
```

Official sandbox verification is a separate, protected manual workflow. Run only with the selected merchant's test credentials, registered HTTPS staging and sanitized provider records. Missing evidence produces a visible **BLOCKED** result. A local mock success never enables real traffic. [Evidence and workflow setup](docs/evidence/release-gates.md).

## Design, operations and release

- [PostgreSQL migrations and recovery](docs/storage.md)
- [Security boundary and threat model](docs/security.md)
- [Host operations and activation](docs/operations.md)
- [Architecture decisions](docs/adr/README.md)
- [v1.0 implementation plan](docs/superpowers/plans/2026-11-24-tender-v1.md)
- [v1.0 completion plan and external gates](docs/superpowers/plans/2026-11-25-tender-v1-completion.md)
- [Changelog](CHANGELOG.md)

Tender follows semantic versioning. APIs may change during development. From v1.0, compatible changes use minor or patch releases and breaking changes use a new Go module major version with migration guidance. A library release and each merchant's production activation are separate decisions. Tender can reduce an application software dependency; an acquirer, wallet, financing provider or ACH originating bank still charges under its own agreement. Measure any Blacktop cost reduction against actual merchant fees and payout records.

## License

[BSD Zero Clause License (0BSD)](LICENSE).
