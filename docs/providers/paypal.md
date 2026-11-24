# PayPal US wallet

The direct merchant adapter uses OAuth 2.0 client credentials, Orders v2 with capture intent, Payments v2 refunds and Notifications Webhooks v1. The local US/USD fixture supports buyer approval, cancellation and full capture plus partial or full refunds. SANDBOX and LIVE checkout creation and confirmation are disabled. PayPal JS SDK v6 recommends a popup-first automatic presentation, limits the modal to WebView scenarios and recommends redirect for mobile. A supported, strictly in-page desktop or mobile web flow remains **BLOCKED**. [Presentation modes](https://developer.paypal.com/sdk/js/reference/) · [Orders API](https://developer.paypal.com/api/orders/v2)

## Configuration

Use provider name `paypal`, method `tender.MethodPayPal`, country `US` and currency `USD`. Pass the same host merchant scope and processor merchant ID to `paypal.New` and `tender.NewEngine`. Configure the PayPal REST app's client ID, secret and registered webhook ID separately for each environment. The default transport disables proxies and redirects. An optional host-supplied transport must protect credentials and enforce the configured destination.

| Environment | Exact API base |
| --- | --- |
| MOCK | A loopback IP literal, optional port and optional mount path |
| SANDBOX | `https://api-m.sandbox.paypal.com` |
| LIVE | `https://api-m.paypal.com`, with explicit `LiveActivated` after merchant approval |

The adapter exchanges Basic client credentials at `POST /v1/oauth2/token`, caches the bearer until shortly before expiry and retries one definite HTTP 401 with fresh credentials. It sends the engine's durable provider key in `PayPal-Request-Id` and requests full representations. Use the engine's default UUID generator or equivalent provider-safe operation IDs and keys. Keep credentials, transmission signatures, access tokens and complete provider responses out of logs and browser state. [OAuth](https://developer.paypal.com/api/rest/authentication/) · [Idempotency](https://developer.paypal.com/api/rest/reference/idempotency/)

## Operations

| Engine operation | Provider evidence and effect |
| --- | --- |
| Create | Posts one `CAPTURE` order with the server-priced purchase unit, intended payee and PayPal wallet source. Reads the order back and checks its amount, seller, payment ID and immutable attempt reference. MOCK returns the order ID as a ten-minute synthetic component action. |
| Confirm | Accepts an empty token. Reads the stored order ID and requires `APPROVED`, the same purchase unit and a PayPal buyer account. Posts to the bound order's capture endpoint and reads the order again. A single final `COMPLETED` capture of the exact amount establishes collection. A pending capture keeps the operation open. |
| Refund | Reads the bound order and single capture, posts the requested USD amount to that capture's refund endpoint, and matches the returned refund ID, amount, merchant-controlled operation reference and parent capture link. Only completed refunds increase the refunded amount. |
| Recover | Queries the stored order after an ambiguous confirmation. A bound pending refund can be read by its refund ID. A lost create or refund response without a bound resource ID requires operator correlation. While an outcome is unknown the engine blocks new conflicting operations. |

The purchase unit carries the host's opaque payment ID in `custom_id` and a digest of the scoped order, amount, account and attempt in `reference_id`. An approved browser callback does not authorize fulfillment. Closing or cancelling the approval component leaves the order uncollected. The adapter supports capture-on-confirm only. Separate authorizations, standalone captures, voids, multiple purchase units, separate settlements, disputes, reversals and report imports remain outside its supported operations. [Create order](https://developer.paypal.com/api/orders/v2/orders-create) · [Capture order](https://developer.paypal.com/api/orders/v2/orders-capture) · [Order details](https://developer.paypal.com/api/orders/v2/orders-get) · [Refund captured payment](https://developer.paypal.com/api/payments/v2/captures-refund)

## Verified notifications

Mount the receiver returned by `adapter.Webhook(store, engine, onStored)` under the registered route for that merchant. Subscribe to `PAYMENT.CAPTURE.COMPLETED`. The handler posts the received event, the configured webhook ID and PayPal's five transmission headers to `/v1/notifications/verify-webhook-signature` on the configured API base. It requires `verification_status == "SUCCESS"`, validates the certificate URL's environment and path, and persists only the event ID, event kind, payee, capture/order references and amount before returning HTTP 202. The handler never fetches the supplied certificate. A failed postback or failed inbox write leaves delivery unacknowledged. [Message verification and capture event example](https://developer.paypal.com/api/rest/webhooks/rest/)

Have a worker call `receiver.Process(ctx, eventID)` after notification or while draining the store's unprocessed inbox. It reads the authenticated order outside the transaction, binds the signed capture and seller to a scoped local confirmation, then atomically updates the payment, operation, receipt and fulfillment outbox. Unresolved receipts remain pending for retry. The fixture models the verify endpoint with a private synthetic signing key and runs over loopback HTTP. PayPal's public dashboard simulator emits mock events that cannot be verified by postback. External validation requires a webhook generated by a real sandbox transaction. [PayPal webhook simulator limitations](https://developer.paypal.com/api/rest/webhooks/simulator/)

## Verification gates

The memory and PostgreSQL contracts exercise the actual adapter against the embeddable fixture, including lost-response recovery, duplicate webhooks and a single fulfillment row. The SQL contract uses a disposable local database selected by `TENDER_TEST_DATABASE_URL`. A synthetic buyer is available through the loopback-only fixture routes `POST /fixture/orders/{id}/approve` and `POST /fixture/orders/{id}/cancel` with JSON content type.

**Sandbox transaction and browser verification are BLOCKED.** Required evidence includes a merchant-owned US sandbox seller and approved REST app, separate buyer, registered HTTPS listener and webhook ID, genuine order/capture/refund records, and a documented policy-compatible checkout surface. Verify the intended seller, buyer account, currency, amount, webhook receipt and current provider state for the same order. Record desktop/mobile browser, cookie restrictions, cancellation, top-level location and popup behavior before offering a wallet action. Production also requires separate merchant activation and settlement evidence. [Sandbox accounts](https://developer.paypal.com/tools/sandbox/accounts/) · [SDK v6](https://developer.paypal.com/sdk/js/set-up/)

For the read-only sandbox proof, set `PAYMENTS_ENV=sandbox`, `TENDER_PAYPAL_CLIENT_ID`, `TENDER_PAYPAL_SECRET`, `TENDER_PAYPAL_MERCHANT_ACCOUNT`, `TENDER_PAYPAL_WEBHOOK_ID` and `TENDER_PAYPAL_SANDBOX_PROOF_FILE`. Run `go test -count=1 -tags=sandbox ./test/sandbox -run TestPayPalSandboxTransaction`. The private JSON input contains `Payment` and `Confirm` in the engine's exported record shapes, `ExpectedBuyerID`, `ExpectedCaptureID`, `Headers` as a string-array header map, and `Event` as the original transaction webhook JSON object. Use the pre-collection payment and its uncertain confirmation with the original purchase-unit binding. The test compares the authenticated sandbox buyer and capture, submits the original event to PayPal for verification, then checks that applying the verified delivery twice produces one outbox row. This gate needs previously generated merchant transaction evidence and does not launch a checkout.
