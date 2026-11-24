# Tender

Tender is a Go module for applications that own their checkout and payment records. Its v1.0 target covers Visa and Mastercard acquiring, PayPal, Klarna, and ACH. The host owns order totals, checkout policy, fulfillment, storage configuration, and deployment.

Tender is under development. Check the [capability matrix](docs/capabilities.md) before planning a provider integration. It records the target scope and which sandbox checks still need evidence.

## Development

The module path is `github.com/gravestench/tender`. Use Go 1.26 or newer.

```sh
go build ./...
go test ./...
go test -race ./...
go vet ./...
```

The default checks are offline. The [milestone backlog](docs/backlog.md) tracks the tagged contract, end-to-end, and sandbox suites. Sandbox verification uses a separate opt-in job with official test credentials.

## Design and release

- [Architecture decisions](docs/adr/README.md)
- [v1.0 implementation plan](docs/superpowers/plans/2026-11-24-tender-v1.md)
- [Changelog](CHANGELOG.md)

Tender follows semantic versioning. APIs may change during development. From v1.0, compatible changes use minor or patch releases, and breaking changes use a new Go module major version with migration guidance. Production activation requires separate approval for each merchant and provider.

## License

[BSD Zero Clause License (0BSD)](LICENSE).
