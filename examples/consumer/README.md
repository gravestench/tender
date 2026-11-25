# Standalone checkout

This module demonstrates how a host such as Blacktop can own checkout, authenticate an account or guest, price its order, and project a collected payment into fulfillment. The default mode uses a synthetic fixture in the MOCK environment. The Adyen example has explicit MOCK and SANDBOX configurations.

## Run locally

Use Go 1.26 or later. From this directory, run the following command.

```sh
GOWORK=off go run . -listen 127.0.0.1:8080
```

Open the one-use `Guest` or `Account` URL printed to the local terminal. Each grants access to a separate sample order. To use both at once, open separate browser profiles. A guest order costs USD 25.99 and an account order costs USD 48.95. Select pending collection, in-page confirmation, or immediate synthetic collection. Restarting the executable issues fresh tickets.

Each ticket exchanges for an HttpOnly, SameSite=Strict session cookie valid for one hour. The ticket response suppresses referrers. The checkout form requires the owning session, its CSRF token and the exact configured Origin. The consumer binds a loopback IP and checks the Host header. Keep the server and its terminal output private. A real host would supply its existing session middleware and order repository in place of the two local grants and fixed catalog.

## Synthetic host routes

| Method and route | Behavior |
| --- | --- |
| GET /session/{ticket} | Exchange a one-use local grant for a session, then redirect to its checkout. |
| GET /orders/{order} | Render the owned order and its current state. Expose the session CSRF token in the response header and active form. |
| POST /orders/{order} | Accept only urlencoded csrf and scenario. Look up the payment UUID, stable create key, currency and total on the server. |
| POST /orders/{order}/confirm | Accept only csrf. Complete an allowed, unexpired synthetic in-page action through the engine. |
| GET /orders/{order}/status | Return OrderID, PaymentID, Currency, Minor, Status and Fulfilled. Adyen also exposes cumulative Funds on a prepared payment. PaymentID is empty before reservation. |
| GET /simulator/scenarios | Read the mounted mockserver fixture's synthetic choices on the checkout listener. |

A successful form submission redirects back to the order. Missing authentication returns 401, a different owner receives 404, invalid origin or CSRF returns 403, and invalid form fields return 400. A changed replay conflicts with the engine claim. Exact replays use the server's original operation keys.

The synthetic provider uses the simulator's idempotent commit API in process. Its scenario controls belong exclusively to MOCK. The MOCK CSP permits same-origin forms and blocks scripts, frames and external resources. For Adyen card checkout, select the provider explicitly as described below.

## Durable fulfillment

Without a database, the default synthetic demo uses Tender's memory store and only reports payment state. Fulfilled remains false. To run the restart-safe version, provision a disposable PostgreSQL database and a schema owned by the example. Apply every Tender up migration in lexical order, then apply the host ledger migration in the same schema. Example commands below assume that the schema already exists and that the DSN points at your disposable database.

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

## Adyen card checkout

Apply the migrations above and set `TENDER_CONSUMER_PROVIDER=adyen`. Both card configurations run the actual v72 adapter, a verified Standard webhook endpoint at `POST /webhooks/adyen`, a SQL inbox worker and the host fulfillment projection. The fixed catalog issues two order grants per start. Use a fresh schema for a new checkout after spending both orders. Set `TENDER_ADYEN_BRAND=visa` or `mc` to choose the server-owned route for this instance.

| Mode | Run from this directory | Card entry |
| --- | --- | --- |
| MOCK | `GOWORK=off go run . -environment MOCK` | Click **Prepare card checkout**, then **Use synthetic card**. A form posts to the current version of the order. The adapter calls the embedded Checkout simulator over loopback HTTP. |
| SANDBOX | `GOWORK=off go run . -environment SANDBOX` | Open the registered public HTTPS origin. The order embeds Adyen Web 6.45.2 and its hosted secured fields in a restricted frame. Merchant verification is pending. |

In MOCK, `TENDER_ADYEN_MOCK_RESULT` selects `Authorised` (the default), `Refused`, `Pending`, `ChallengeShopper`, `IdentifyShopper`, `RedirectShopper` or `UnknownAction` on the host. The simulator duplicates its synthetic notifications. `TENDER_ADYEN_MOCK_LOST_RESPONSE=captures` drops the first committed capture response. The signed callback can still resolve the capture. Select `payments` to lose the authorization reply. An authorization webhook alone cannot verify card brand, so that checkout awaits reconciliation. Mock scenarios and the simulator mount exist only in MOCK.

For SANDBOX, configure the merchant test account for US/USD scheme cards, manual capture and the payment method type/brand in the Checkout response. Register `TENDER_ADYEN_ORIGIN` as the client key's allowed HTTPS origin. Configure `TENDER_ADYEN_MERCHANT_ACCOUNT`, `TENDER_ADYEN_API_KEY`, `TENDER_ADYEN_CLIENT_KEY` (a public `test_` key), `TENDER_ADYEN_HMAC_KEY` (64 hex digits) and a server-owned `TENDER_ADYEN_TEST_SHOPPER_EMAIL`. Route the HTTPS origin to the loopback listener through a controlled TLS proxy. Preserve the registered Host, strip any incoming `X-Tender-Client-IP`, and set it to the IP observed by that proxy. Keep tickets and request bodies out of proxy/application logs. Issue the authenticated order grants privately. Expose the Standard webhook path to the test platform and configure the matching test HMAC key. The callback verifies the signed notification and persists it before acknowledgment. [Adyen client keys](https://docs.adyen.com/development-resources/client-side-authentication/) · [Webhook setup](https://docs.adyen.com/development-resources/webhooks/)

Only direct native `threeDS2/challenge` actions from Checkout are candidates for the interactive SDK path. The host checks the component mode, expiry, required challenge fields and the encoded destinations before passing an action to the SDK. `TENDER_ADYEN_TEST_ACS_ORIGIN` selects one approved test issuer HTTPS origin and defaults to `https://checkoutshopper-test.adyen.com`. The notification URL must use Adyen's test shopper origin. A refreshed order can remount a current challenge. Fingerprinting can cause an SDK-internal redirect and is disabled. Redirect, popup, unknown, expired and unapproved-issuer actions leave checkout on the order page. Credentialed browser verification of direct challenges and issuer-specific behavior is outstanding.

### Merchant operations

Set `TENDER_CONSUMER_OPERATOR_KEY` to a private random token of at least 32 characters to enable the sample merchant routes. From a trusted server process, send an empty `POST /operator/orders/{order}/{operation}` with an `Authorization: Bearer …` header. Supported operations are `capture`, `void` and `refund`. Each targets the full trusted order amount using its own durable key. The shopper session cannot authorize an operator request. HTTP 202 acknowledges acceptance. Read order status and the verified inbox for the resulting financial state. Keep the operator routes private at the TLS proxy.

The worker processes stored callbacks and checks fulfillment on startup, on new receipts and periodically. The example keeps one SQL connection for its two orders and retries unprocessed receipts after a restart. Before consuming a captured notice, the two-order example locks the verified inbox against new writers for the short host transaction. An active writer defers delivery until a subsequent pass. A signed CAPTURE_FAILED for this merchant, original payment PSP and currency holds delivery even when the worker has yet to process it. That conservative hold also covers unresolved modification correlation. A failure arriving after the delivery transaction commits creates a review or compensation obligation for the recorded delivery. Monitor `tender_capture_failures` and unprocessed `tender_provider_events` for the scoped merchant. The example contains no external shipment or compensation worker.

The offline E2E, sandbox cases and Customer Area comparison instructions are in the [Adyen evidence record](../../docs/evidence/adyen.md).

## Verify the example

Build the separate module from this directory with `GOWORK=off go build -o /tmp/tender-consumer .` so the executable stays outside the checkout. From the repository root, run the HTTP tests with `go test -tags=e2e ./test/e2e -run 'TestConsumerMountsOnOneListener|TestTrustedOrderAmount|TestConsumerSyntheticAction'`.

The browser test requires Node, Playwright (tested with 1.62.1), and Chromium installed before the offline test run. Set TENDER_PLAYWRIGHT to the absolute installed Playwright module path if it is outside Node's module search path. Set TENDER_CHROMIUM to the installed browser executable when using a separate Chromium installation. An ephemeral loopback proxy permits only the configured destinations, including every HTTPS tunnel and redirect hop. Chromium disables QUIC and HTTP/2 connection coalescing during these checks. The harness blocks service workers and WebSockets. OpenSSL supplies ephemeral test certificates for the local HTTPS policy regression. Set TENDER_TEST_DATABASE_URL to a local disposable PostgreSQL database for the Adyen flow. Run the whole offline consumer slice with `GOPROXY=off go test -count=1 -tags=e2e ./test/e2e` after populating the Go module cache.

The restart test creates a separate schema in a disposable database, injects a failing host ledger constraint, starts two independent processes and checks the final ledger and outbox. Use `TENDER_TEST_DATABASE_URL` for the test database. Run `go test -count=1 -tags=e2e,integration ./test/e2e -run TestConsumerDurableFulfillment`. The SQL test drops its own schema on completion.

## Verify the tagged offline alpha

The development go.mod requires the placeholder version v0.0.0 and replaces github.com/gravestench/tender with the local checkout. To verify the published **MOCK-only** alpha, copy this directory into a clean temporary directory outside the repository. Run the following commands inside the copy.

```sh
export GOWORK=off
export TENDER_TAG=v0.1.0-alpha.1
go mod edit -dropreplace=github.com/gravestench/tender
go get "github.com/gravestench/tender@$TENDER_TAG"
go mod tidy
go list -m -json github.com/gravestench/tender
go build -o /tmp/tender-consumer-tag .
```

Check that the selected Version is `v0.1.0-alpha.1` and that Replace is absent. A clean copy completed this import, build and a USD 25.99 loopback MOCK capture. The alpha is suitable for offline integration tests only; the card, PayPal, Klarna and ACH provider paths still need genuine sandbox evidence and merchant activation before real use.
