# Klarna Payments in North America

**Ordinary web financing is unavailable.** The local US/USD simulation supports the one-time `pay_over_time` category through Klarna Payments v1 and Order Management v1. Both SANDBOX and LIVE advertise zero checkout capabilities. The server also rejects direct adapter calls in those environments. A merchant activation flag alone cannot enable the checkout.

Klarna's JavaScript SDK mounts an inline widget, then normally opens a popup when the shopper authorizes the purchase. Mobile guidance also describes redirecting to a hosted payment page. A merchant must qualify a provider-supported presentation, authentication, disclosures, recovery and fallback behavior for its supported browsers before the runtime gate can change. The synthetic approval button uses no external SDK. [SDK checkout behavior](https://docs.klarna.com/acquirer/klarna/web-payments/integrate-with-klarna-payments/integrate-via-sdk/step-2-checkout/)

## Wire protocol

| Purpose | Request | Bound evidence |
| --- | --- | --- |
| Start checkout | `POST /payments/v1/sessions` | US, USD, en-US, trusted order lines, gross amount, sales tax, merchant references and per-session callback URL. The synthetic component requires the returned category. |
| Place order | `POST /payments/v1/authorizations/{authorizationToken}/order` | Same server-priced cart and merchant references, with auto-capture disabled. Confirm requires the matching callback receipt. |
| Capture | `POST /ordermanagement/v1/orders/{order_id}/captures` | Amount, merchant operation reference, stable Klarna idempotency key and receipt capture ID. |
| Refund | `POST /ordermanagement/v1/orders/{order_id}/refunds` | Amount, merchant operation reference, stable Klarna idempotency key and receipt refund ID. |
| Cancel | `POST /ordermanagement/v1/orders/{order_id}/cancel` | Uncaptured authorization and HTTP 204. |
| Release balance | `POST /ordermanagement/v1/orders/{order_id}/release-remaining-authorization` | Previously captured principal survives release of the remainder. |
| Recover | `GET /ordermanagement/v1/orders/{order_id}` | Authenticated order record for a known provider order ID. |

The pinned playground and live hosts are `api-na.playground.klarna.com` and `api-na.klarna.com`. HTTP requests use Klarna's literal `Authorization: Basic <API key>` scheme. Configuration rejects environment-mismatched keys and hosts. MOCK requires a literal loopback IP for both the API and callback. Use a distinct, trusted store, account and key per route. [Regional URLs](https://docs.klarna.com/acquirer/klarna/get-started/integration-resilience/api-urls/) · [API key authentication](https://docs.klarna.com/acquirer/klarna/get-started/integration-resilience/authentication/)

The host supplies an immutable, itemized cart. The adapter checks line arithmetic in minor units, including discounts and separate US sales-tax lines. It hashes the original cart so substitution at the same price also blocks order creation. Order creation omits the merchant confirmation URL. An unexpected redirect or unrecognized financing method leaves the order outcome unresolved without exposing the URL. The simulator uses `fixed_amount`. Merchant-specific category and method identifiers require verification. [Payments API](https://docs.klarna.com/acquirer/klarna/api/payments/) · [US tax handling](https://docs.klarna.com/acquirer/klarna/web-payments/additional-resources/error-handling-and-validations/tax-handling/) · [Order placement](https://docs.klarna.com/acquirer/klarna/web-payments/integrate-with-klarna-payments/integrate-via-sdk/step-3-create-an-order/)

## Callbacks and recovery

Mount `Adapter.CallbackHandler()` at the configured callback URL. Supply a persistent, random 32-byte route secret and use the same Tender store in the adapter and engine. Each callback URL includes its create operation ID and a MAC bound to its reservation, merchant, attempt, order and amount. The handler checks the MAC and returned session ID, then AES-GCM seals the authorization token and saves a deduplicated receipt before acknowledging it. Redact request queries and bodies in host and proxy logs. Retain the route secret for pending receipts. Memory storage is for local simulation. [Callback security, expiry and acknowledgement](https://docs.klarna.com/acquirer/klarna/web-payments/integrate-with-klarna-payments/other-actions/authorization-callback/)

A host worker calls `Authorizations(ctx, paymentID)` and `ProcessAuthorization(ctx, engine, paymentID, receiptID)`. Reprocessing an identical receipt uses the original engine key. Reauthorization can produce a distinct receipt. Refresh uses `CheckoutAction` to check the scope, market, method, status and session expiry before exposing synthetic browser data. The local session ceiling is 48 hours and the receipt ceiling is 60 minutes from receipt. Klarna can reject a token earlier. A session with no eligible category or an inactive merchant returns `ErrUnsupported`. Klarna's explicit `REJECTED` order decision records a decline. A pending fraud decision keeps funds unconfirmed until an authoritative read resolves it. [Session lifetime](https://docs.klarna.com/acquirer/klarna/web-payments/integrate-with-klarna-payments/integrate-via-sdk/step-2-checkout/) · [Payments error codes](https://docs.klarna.com/acquirer/klarna/resources/developer-tools/error-handling/error-codes-and-messages-for-klarna-payments/)

Partial and full captures and refunds use independent, persisted `Klarna-Idempotency-Key` values. Recovery reads a known order and checks its host reference, binding, currency, original and current amount, fraud and principal state, operation reference, resource ID and operation amount. Conflicting evidence holds the reservation. A lost session or order response without a bound provider order ID, or a lost cancel/release response without attributable operation evidence, remains `ErrUnknownOutcome` for reconciliation. The adapter does not redispatch those writes automatically. [Order Management API](https://docs.klarna.com/acquirer/klarna/api/ordermanagement/) · [Capture receipts](https://docs.klarna.com/acquirer/klarna/after-payments/order-management/manage-orders-with-the-api/capture-and-track-orders/) · [Refunds and releases](https://docs.klarna.com/acquirer/klarna/after-payments/order-management/manage-orders-with-the-api/refund-orders-and-manage-authorizations/) · [Retry policy](https://docs.klarna.com/acquirer/klarna/get-started/integration-resilience/escalation-and-retry-policy/)

## Running the example and gates

Set `TENDER_CONSUMER_PROVIDER=klarna`, select MOCK and configure the consumer's migrated SQL database. Run the independent module with `go run . -listen 127.0.0.1:8080` from `examples/consumer`. Open a generated guest or account ticket. Start the synthetic session, approve in place, then refresh to read the order authorization. The example stops at authorization. Capture and refund require a separate trusted host operation. Set `TENDER_KLARNA_MOCK_CATEGORY=pay_now` to exercise unavailable financing.

The offline suites run against a loopback-only simulator. Set `TENDER_TEST_DATABASE_URL` to a disposable local database and configure the repository's Playwright and Chromium paths. Run the following from the module root.

```sh
go test -race -tags=integration ./test/contract/... -run TestKlarna
go test -race -tags=e2e ./test/e2e/... -run TestKlarna
```

**Playground and production evidence are BLOCKED.** The scoped playground order-record check requires `PAYMENTS_ENV=sandbox`, a merchant-issued `TENDER_KLARNA_API_KEY`, and `TENDER_KLARNA_HOST_EVIDENCE` pointing to a private JSON export from a previously captured, partially refunded host order. The expected fields are `OrderID`, `MerchantReference1`, `MerchantReference2`, `CaptureID`, `RefundID`, `OrderAmount`, `CapturedAmount` and `RefundedAmount`. Amounts are USD minor units. The named sandbox test performs one authenticated GET at the pinned playground host and checks the independent references and totals. No credentialed order, portal comparison, merchant callback delivery or provider-controlled browser approval has been observed. A read-only order-record match would establish only that sample's financial record. [Merchant integration checklist](https://docs.klarna.com/acquirer/klarna/get-started/integration-checklist/)

```sh
go test -tags=sandbox ./test/sandbox/... -run TestKlarnaSandboxOrderRecord
```
