# Standalone mock checkout

This module demonstrates how a host such as Blacktop can own checkout, authenticate an account or guest, price its order, and project a collected payment into fulfillment. The executable uses a synthetic fixture in the MOCK environment. It sends no payment requests to a processor.

## Run locally

Use Go 1.26 or later. From this directory, run the following command.

```sh
GOWORK=off go run . -listen 127.0.0.1:8080
```

Open the one-use `Guest` or `Account` URL printed to the local terminal. Each grants access to a separate sample order. To use both at once, open separate browser profiles. A guest order costs USD 25.99 and an account order costs USD 48.95. Select pending collection, in-page confirmation, or immediate synthetic collection. Restarting the executable issues fresh tickets.

Each ticket exchanges for an HttpOnly, SameSite=Strict session cookie valid for one hour. The ticket response suppresses referrers. The checkout form requires the owning session, its CSRF token and the exact configured Origin. The consumer binds a loopback IP and checks the Host header. Keep the server and its terminal output private. A real host would supply its existing session middleware and order repository in place of the two local grants and fixed catalog.

## Host routes

| Method and route | Behavior |
| --- | --- |
| GET /session/{ticket} | Exchange a one-use local grant for a session, then redirect to its checkout. |
| GET /orders/{order} | Render the owned order and its current state. Expose the session CSRF token in the response header and active form. |
| POST /orders/{order} | Accept only urlencoded csrf and scenario. Look up the payment UUID, stable create key, currency and total on the server. |
| POST /orders/{order}/confirm | Accept only csrf. Complete an allowed, unexpired synthetic in-page action through the engine. |
| GET /orders/{order}/status | Return OrderID, PaymentID, Currency, Minor, Status and Fulfilled. PaymentID is empty before reservation. |
| GET /simulator/scenarios | Read the mounted mockserver fixture's synthetic choices on the checkout listener. |

A successful form submission redirects back to the order. Missing authentication returns 401, a different owner receives 404, invalid origin or CSRF returns 403, and invalid form fields return 400. A changed replay conflicts with the engine claim. Exact replays use the server's original operation keys.

The synthetic provider uses the simulator's idempotent commit API in process. Its scenario controls belong exclusively to MOCK. The MOCK CSP permits same-origin forms and blocks scripts, frames and external resources. SANDBOX and LIVE fail during startup. Their browser components, official origin policies, provider protocol routes and verified webhook handlers must be supplied with a qualified adapter. This example exposes no public callback route.

## Durable fulfillment

Without a database, the demo uses Tender's memory store and only reports payment state. Fulfilled remains false. To run the restart-safe version, provision a disposable PostgreSQL database and a schema owned by the example. Apply every Tender up migration in lexical order, then apply the host ledger migration in the same schema. Example commands below assume that the schema already exists and that the DSN points at your disposable database.

```sh
export TENDER_CONSUMER_DATABASE_URL='postgres://localhost/tender_demo?sslmode=disable'
export TENDER_CONSUMER_SCHEMA=demo
export PGOPTIONS='-c search_path=demo'
for migration in ../../store/postgres/migrations/*.up.sql; do
  psql "$TENDER_CONSUMER_DATABASE_URL" -X -v ON_ERROR_STOP=1 -f "$migration" || exit 1
done
psql "$TENDER_CONSUMER_DATABASE_URL" -X -v ON_ERROR_STOP=1 -f fulfillment.sql
GOWORK=off go run .
```

The host supplies a loopback or Unix-socket pool to postgres.New. Every database failover host must also be local. After a successful checkout mutation and at process startup, the host locks the committed Tender collection notice and current payment. It checks the order's amount, routing, captured principal and adverse funds. A single database transaction inserts one host fulfillment per scoped order and marks the corresponding outbox entry consumed. Rollback permits retry. A refunded, disputed or returned payment holds the notice for host review. The ledger row represents the entire example delivery. Actual shipment or another external side effect requires an idempotent downstream consumer.

## Verify the example

Build the separate module from this directory with `GOWORK=off go build ./...`. From the repository root, run the HTTP tests with `go test -tags=e2e ./test/e2e -run 'TestConsumerMountsOnOneListener|TestTrustedOrderAmount|TestConsumerSyntheticAction'`.

The browser test requires Node, Playwright (tested with 1.62.1), and Chromium installed before the offline test run. Set TENDER_PLAYWRIGHT to the absolute installed Playwright module path if it is outside Node's module search path. Set TENDER_CHROMIUM to the installed browser executable when using a separate Chromium installation. The harness blocks browser requests outside the one loopback origin, disables service workers, closes WebSockets, disables proxies and disables external DNS resolution. Run the whole offline consumer slice with `GOPROXY=off go test -count=1 -tags=e2e ./test/e2e` after populating the Go module cache.

The restart test creates a separate schema in a disposable database, injects a failing host ledger constraint, starts two independent processes and checks the final ledger and outbox. Use `TENDER_TEST_DATABASE_URL` for the test database. Run `go test -count=1 -tags=e2e,integration ./test/e2e -run TestConsumerDurableFulfillment`. The SQL test drops its own schema on completion.

## Build against a future release

The development go.mod requires the placeholder version v0.0.0 and replaces github.com/gravestench/tender with the local checkout. To verify a published tag, copy this directory into a clean temporary directory outside the repository. Set TENDER_TAG to the actual published version and run the following commands inside the copy.

```sh
export GOWORK=off
go mod edit -dropreplace=github.com/gravestench/tender
go get "github.com/gravestench/tender@$TENDER_TAG"
go mod tidy
go list -m -json github.com/gravestench/tender
go build ./...
```

Check that the selected Version is the published tag and that Replace is absent. This verification needs the published module in the chosen Go proxy or module cache.
