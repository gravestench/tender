# Release gate ledger

Updated 2026-11-25. The published `v0.1.0-alpha.1` MOCK import and CI evidence below are historical results from `main` at `0667258`. The current development candidate adds provider report projections, payout and adverse review records, durable consumer scenarios, protected gate checks and optional observations. **The four-rail v1.0 release is BLOCKED.** Green offline CI and local mock/PGlite results do not count as official provider sandbox proof. This ledger records the verification distinction and the evidence needed to change status.

## Current development candidate

The candidate on `codex/tender-v1` is untagged. G1 is **PENDING** for this candidate until a tag is published and independently imported without a local `replace`. G2 is **PASS for the offline candidate code at `a370861fa5d11f77ed0a513b078d488bf54d7460`**, with the local and remote evidence below. The older alpha G1/G2 results do not transfer to this code. G3 and G4 remain **BLOCKED** by merchant credentials, genuine provider-generated records, real browser SDK/issuer behavior and an approved ACH mandate. PayPal payout feed/correlation and refund/reversal settlement/event handling are explicit v1 gaps, rather than qualified or not-applicable rail features. A Blacktop integration and fee-saving live pilot have not run.

### Candidate offline verification at `a370861`

On 2026-11-25 UTC, a clean worktree at `a370861fa5d11f77ed0a513b078d488bf54d7460` used Go 1.26.7, Node 26.12.1, Playwright 1.62.1, installed Chromium 1255 and a fresh private PostgreSQL 17.5 WASM/PGlite Unix socket. `GOPROXY=off` was set for every local Go command. The SQL and browser commands used `TENDER_TEST_PGLITE=1` and `TENDER_TEST_DATABASE_URL='host=/private/tmp/tender-task23-pg user=postgres password=postgres dbname=postgres sslmode=disable default_query_exec_mode=exec'`. The browser command additionally used `TENDER_CHROMIUM='/Users/dylanknuth/Library/Caches/ms-playwright/chromium-1255/chrome-mac-arm64/Google Chrome for Testing.app/Contents/MacOS/Google Chrome for Testing'`. The password and paths are disposable local fixture values, not merchant credentials.

| Local command (with the environment above) | Exit | Result |
| --- | --- | --- |
| `go build ./...` | 0 | All packages build. |
| `go test -count=1 ./...` | 0 | All offline unit packages pass. |
| `go test -race -count=1 ./...` | 0 | All offline unit packages pass without a race report. |
| `go vet ./...` | 0 | No vet finding. |
| `go test -race -count=1 -timeout=8m -tags=integration -skip '^TestPostgresConflictingOperations' ./test/contract/...` | 0 | Full local SQL/provider contract suite except the two native multi-backend conflict cases; the new concurrent return-claim case self-skips when PostgreSQL `version()` has PGlite's `emcc` marker. PGlite package time: 7.047s. |
| `go test -race -count=1 -timeout=8m -tags=e2e,integration ./test/e2e/...` | 0 | Full durable consumer, loopback provider and Chromium suite; package time: 83.208s. |
| `(cd examples/consumer && GOWORK=off go build -o /private/tmp/tender-task23-consumer .)` | 0 | Local replacement build; this is not G1's external tagged import. |
| `go test -run '^$' -fuzz '^FuzzMoneyJSON$' -fuzztime=1s -parallel=1 .` | 0 | Money JSON fuzz target passes. |
| `go test -run '^$' -fuzz '^FuzzMoneyAdd$' -fuzztime=1s -parallel=1 .` | 0 | Money addition fuzz target passes. |
| `go test -run '^$' -fuzz '^FuzzPayPalMutationRejection$' -fuzztime=1s -parallel=1 ./providers/paypal` | 0 | PayPal mutation-error fuzz target passes. |
| `go test -run '^$' -fuzz '^FuzzACHSettlementRow$' -fuzztime=1s -parallel=1 ./providers/ach` | 0 | ACH report-row fuzz target passes. |
| `go test -run '^$' -fuzz '^FuzzAdyenStandardNotification$' -fuzztime=1s -parallel=1 ./webhook` | 0 | Adyen notification fuzz target passes. |

The first local browser attempt exited 1 because Playwright's default headless-shell executable was absent. Pointing `TENDER_CHROMIUM` to the installed Chromium binary and rerunning the full suite produced the pass above. An earlier local SQL attempt at `b4c7634` exposed a migration round-trip test that rolled back migration 006 while migration 007's foreign key still referenced it; the test was corrected in `a370861`, then the fresh PGlite contract suite passed. Migration 007's return-claim table must be rolled back before 006 and restored after 006. PGlite's one emulated backend cannot prove independent-session lock/serialization behavior; the native CI run below covers those cases.

[Candidate offline CI run 45539781851](https://github.com/gravestench/tender/actions/runs/45539781851) completed successfully at the exact code commit `a370861fa5d11f77ed0a513b078d488bf54d7460` on 2026-11-25 UTC. GitHub reports all four jobs successful: Go 1.26 and 1.27 build/unit/race/vet/consumer build, full unskipped provider contracts with a native PostgreSQL 17.11 service (including the two-session return-claim test), and full race-enabled Chromium durable consumer against native PostgreSQL. This is offline MOCK/SQL/browser evidence only. A later documentation-only commit needs its own branch CI check before a tag is proposed; no independent tagged import has been run for this candidate.

### Published MOCK alpha evidence

| Gate | Status | Evidence and missing work |
| --- | --- | --- |
| G1 — independent tagged import | **PASS for the MOCK alpha** | The published annotated [`v0.1.0-alpha.1` tag](https://github.com/gravestench/tender/tree/v0.1.0-alpha.1) points to `0667258`. A clean external consumer resolved it through the Go proxy with no `go.work` or `replace`, built, and completed a USD 25.99 MOCK capture smoke. This does not qualify provider sandbox or live checkout. |
| G2 — unit, race, contract and offline browser | **PASS for the offline alpha** | The [main branch CI run at `0667258`](https://github.com/gravestench/tender/actions/runs/45517581386) completed successfully on 2026-11-25. Go 1.26/1.27 unit, race, build and vet jobs, the full native PostgreSQL 17.11 integration contracts, and the Chromium durable-consumer job all passed. This does not qualify a real provider transaction. |
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

PGlite validates the SQL statements, migrations and restart paths but cannot prove independent backend lock/serialization behavior. The native CI result below supplies that missing multi-backend test evidence. Synthetic browser checks include a desktop Chromium viewport and a mobile-emulated Adyen card viewport; physical mobile browsers and real SDK/issuer navigation are still unverified.

## Remote offline CI

[Offline Go checks run 45516976421](https://github.com/gravestench/tender/actions/runs/45516976421) was triggered by a push to `main` at `ef4b55c9b4562caca6fc7b671bd3ad883ffd774b`. GitHub reports status **completed**, conclusion **success** (updated 2026-11-25 00:19:57 UTC). Its four jobs and required steps were all successful:

| Job | Verified step |
| --- | --- |
| `test (1.26.x)` | Build, unit tests, race detection, vet and independent consumer build. |
| `test (1.27.x)` | The same build, unit, race, vet and consumer checks. |
| `postgres` | Full `go test -race -tags=integration -count=1 -timeout=4m ./test/contract/...` against a native PostgreSQL 17.11 service, including the multi-backend conflict tests excluded from local PGlite. |
| `browser` | Pinned Playwright/Chromium installation and full `go test -race -count=1 -timeout=5m -tags=e2e,integration ./test/e2e/...` against the standalone consumer and native PostgreSQL service. |

The protected provider `sandbox` workflow has **not** run. No merchant credentials or genuine provider test transaction were used in this CI run. G3, G4 and merchant production activation remain blocked independently of G2.

[A second all-success CI run](https://github.com/gravestench/tender/actions/runs/45517581386) completed on `main` at the tagged commit `066725867a1fa5fe6c2b049134c77727a1e05e84` (updated 2026-11-25 00:24:09 UTC). GitHub reports success for all four jobs: Go 1.26, Go 1.27, native PostgreSQL contracts and Chromium durable consumer. The tagged source therefore has the same offline CI coverage as the earlier run.

## Tagged MOCK import

The annotated `v0.1.0-alpha.1` tag peels to `066725867a1fa5fe6c2b049134c77727a1e05e84` and is explicitly labeled for offline development. An independent copy of `examples/consumer` outside this checkout removed its local `replace` and had no `go.work`. `go get github.com/gravestench/tender@v0.1.0-alpha.1` through `proxy.golang.org`, `go mod tidy`, `go list -m -json` and a clean executable build all exited 0. Module metadata reported the exact tag and no `Replace`. A loopback MOCK guest order kept its server-owned USD 25.99 total and reached `captured` once; `Fulfilled=false` was expected because that memory-store smoke had no host fulfillment ledger. This verifies an importable alpha, not real card, wallet, financing or ACH processing.

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
