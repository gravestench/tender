# Release gate ledger

Updated 2026-11-24 on `codex/tender-v1`. **The four-rail v1.0 release is BLOCKED.** Local mock and PGlite results do not count as official provider sandbox proof. This ledger records the verification distinction and the evidence needed to change status.

| Gate | Status | Evidence and missing work |
| --- | --- | --- |
| G1 — independent tagged import | **BLOCKED** | `examples/consumer` builds against the local module through a development `replace`. A published pre-release or v1 tag and clean external import without `replace` have not been run. |
| G2 — unit, race, contract and offline browser | **PARTIAL** | The combined local candidate passed the commands below. Native multi-connection PostgreSQL conflict tests and remote CI have not run; provider financial/security review must remain clean after packaging. |
| G3 — actual sandbox capability evidence | **BLOCKED** | Adyen card, PayPal, Klarna and ACH lack merchant credentials and genuine transaction/report evidence. [Card](adyen.md) · [PayPal](paypal.md) · [Klarna](klarna.md) · [ACH](ach.md). |
| G4 — strict on-page and sensitive-data policy | **BLOCKED** | Synthetic Chromium guards run, but supported mobile/desktop provider SDK and issuer flows are unverified. PayPal/Klarna ordinary web paths stay disabled; Adyen ACH still needs approved signed mandate handling. Security responsibilities are in [security.md](../security.md). |
| Production activation | **BLOCKED** | Merchant contracts, PCI/ACH assessment, pricing, provider approval, approved low-value live pilot, actual payout and bank-deposit comparison remain separate from a library release. |

## Combined local verification

On 2026-11-24, the worktree based on `59dfe04` used Go 1.26.7, Node 26.12.1, pinned Playwright 1.62.1, Chromium 159.0.8154.6 and an isolated Unix-socket PostgreSQL 17.5 WASM/PGlite instance. Local loopback listeners needed host permission. All of the following completed with exit code 0:

| Check | Observed result |
| --- | --- |
| `go build ./...`, `go test -count=1 ./...`, `go test -race -count=1 ./...`, `go vet ./...`, tagged vet | PASS. No failing package or race report. |
| `GOWORK=off go build ./...` in `examples/consumer` | PASS against the local module replacement; published-tag import remains untested. |
| `go test -race -tags=integration ./test/contract/... -skip '^TestPostgresConflictingOperations'` and the same plain test | PASS against the local SQL socket. Both native multi-backend `TestPostgresConflictingOperations*` cases were intentionally excluded because PGlite uses one backend; the PostgreSQL CI service must run them. |
| `go test -race -tags=e2e,integration ./test/e2e/...` and `go test -tags=e2e ./test/e2e/...` | PASS with the actual standalone consumer, loopback provider HTTP simulator, durable SQL and Chromium browser. Local runs took 36.544s and 30.025s. |
| Five one-second fuzz targets | PASS for money JSON validation/addition, PayPal mutation errors, ACH settlement rows and Adyen signed Standard notifications. The webhook 512 KiB body-limit test also passed. |
| `PAYMENTS_ENV=sandbox go test -tags=sandbox ./test/sandbox/... -run '^TestACHSandboxSessions$'` with no merchant secrets | **BLOCKED as required**, exit code 1 with an explicit missing test merchant/API/client key message. No official provider test was sent. |

PGlite validates the SQL statements, migrations and restart paths but cannot prove independent backend lock/serialization behavior. The native PostgreSQL 17.11 GitHub Actions jobs and the protected sandbox workflow have no remote run yet. Synthetic browser checks include a desktop Chromium viewport and a mobile-emulated Adyen card viewport; physical mobile browsers and real SDK/issuer navigation are still unverified.

## Offline commands

Run from the repository root, with Go 1.26 or newer. The integration and browser suites need a disposable PostgreSQL 17 database in `TENDER_TEST_DATABASE_URL`; browser checks need Node, pinned Playwright, Chromium and OpenSSL. Pull request CI supplies them. No provider credential belongs in these jobs.

```sh
go build ./...
go test ./...
go test -race ./...
go vet ./...
go test -tags=integration ./test/contract/...
go test -tags=e2e ./test/e2e/...
(cd examples/consumer && GOWORK=off go build -o /tmp/tender-consumer .)
```

The protected `.github/workflows/sandbox.yml` accepts one qualification at a time through `workflow_dispatch` on `refs/heads/main` only. Before adding secrets, protect `main` with a reviewed-pull-request rule and restrict the **`sandbox` environment** to that branch with required reviewers. The job has no provider secret at job scope; only the selected test step receives its own provider credentials and evidence. Its `PAYMENTS_ENV=sandbox` job never runs on a fork pull request and never selects LIVE. File evidence secrets are base64-encoded sanitized records; the selected step decodes them into private runner temporary files. For private staging, use a controlled runner with equivalent network access instead of exposing the database publicly. Missing credentials, tickets or records fail with **BLOCKED**; a successful job must still be reviewed against provider portal and browser evidence. Do not put raw card/bank fields or customer personal data into evidence secrets.

## External evidence record template

For each scenario, record: provider, pinned API and SDK version, commit/tag, environment, merchant account alias, UTC timestamp, market and currency, device/browser, expected amount/status/action, observed amount/status/action, sanitized provider reference and event IDs, redacted screenshots/portal/report links, reviewer and PASS/FAIL/BLOCKED reason. Store the actual protected artifacts outside this public repository and link only to access-controlled copies.
