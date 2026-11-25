# Adyen card qualification

Updated November 24, 2026. The verified checkout slice uses the v72 Advanced flow, SQL persistence and a loopback HTTP simulator for US/USD Visa and Mastercard routes. Official test merchant verification is **BLOCKED**. The published `v0.1.0-alpha.1` is a MOCK preview; the current expanded candidate remains untagged.

| Check | Locally observed | Official merchant evidence |
| --- | --- | --- |
| Visa purchase and full refund | Authorized USD 25.99, recovered a lost capture acknowledgment from its signed Standard event, recorded one fulfillment, then applied the verified refund. Replaying capture and duplicate events kept one consumed outbox entry. | BLOCKED |
| Mastercard cancellation | Direct synthetic challenge survived refresh. A stale form could not advance the challenge. A fresh details request authorized the payment. Signed cancellation voided it with zero fulfillments. | BLOCKED |
| Decline and abandoned order | Refused payment displayed declined. An abandoned prepared order survived restart. Advancing its stored deadline disabled submission without creating a collection. | BLOCKED |
| Browser containment | Chromium on a 390 × 844 mobile-emulated viewport refreshed and submitted the synthetic Adyen checkout on one loopback origin. A provider redirect displayed the review state without a new page or external request. A 1200 × 900 desktop Chromium instance exercised the blocked unknown action. Desktop Chromium also submitted the original synthetic checkout. Both fixtures checked for credential inputs while the active form was visible. | Physical mobile browsers, hosted fields, issuer UI and desktop sandbox remain BLOCKED. |
| Native 3DS | The adapter, action gate and mock handle only direct native challenges. The protected runner requires the exact `FingerprintUnavailable` provider observation when it checks a blocked fingerprint. Unknown actions, redirects and fingerprint actions stop in checkout. A popup action mode fails the host policy. | BLOCKED for direct challenge. Adyen's fingerprint-dependent frictionless case is **unsupported**. |
| SQL and webhooks | pgx ran the Tender migrations and host ledger on PostgreSQL 17.5 WASM/PGlite. HMAC-verified deliveries drove the SQL inbox and fulfillment. A restart with an unprocessed, verified capture failure held the collection notice. A failure arriving after delivery kept the single delivery and created a compensation review item. | Native multi-session PostgreSQL, merchant-generated webhooks and Customer Area reconciliation are outstanding. |
| Settlement details projection | Synthetic, host-scoped US/USD card `Settled`, `Refunded`, `Chargeback`, `MerchantPayout`, and monthly `Fee` rows project original and modification PSPs, signed gross/net/fee, batch and source provenance. A provider-debit payout becomes merchant-positive. Contradictory fees, wrong merchant or currency, unsupported journals and ambiguous payout references are rejected. | A complete authenticated merchant Settlement details export, all report sections and parts, provider totals, and a matching bank statement remain BLOCKED. |
| Adverse card events | Synthetic independently HMAC-signed `CHARGEBACK` and `REFUND_FAILED` create one durable review notice only after exact account, payment, PSP, operation, amount and currency correlation. A failure received before its refund acknowledgement stays pending until recovery. A reordered positive refund cannot apply over an already received matching failure. Previously captured/refunded principal and fulfillment history remain unchanged. | Merchant-generated disputes and failed refunds, actual webhook ordering and downstream compensation remain BLOCKED. |
| Independent module | The older `v0.1.0-alpha.1` MOCK tag passed a clean external import. The current expanded candidate has only local import checks pending a new tag. | A published tag does not qualify genuine Adyen transactions; current candidate G1 awaits its own tag and independent import. |

The sandbox integration pins both 6.45.2 test-CDN resources with SHA-384 integrity values. Card numbers and security codes belong in the provider's hosted fields. The host projects encrypted JWE fields and the allowed browser metadata into a transient confirmation. The SDK runs in an iframe with a response sandbox that omits top navigation and popup permissions. A direct challenge additionally needs a complete native token, the approved test ACS origin and Adyen's test notification origin. The SDK fingerprint component can dispatch a redirect itself. It is excluded from the supported browser actions. [Pinned Card source](https://github.com/Adyen/adyen-web/blob/v6.45.2/packages/lib/src/components/Card/Card.tsx) · [Pinned SDK](https://checkoutshopper-test.cdn.adyen.com/checkoutshopper/sdk/6.45.2/adyen.js) · [Official embedding guidance](https://docs.adyen.com/standard/integration/drop-in/best-practices)

## Reproduce the offline evidence

Supply a disposable local PostgreSQL instance in `TENDER_TEST_DATABASE_URL` with schema creation privileges. For a dedicated single-session PGlite server, set `TENDER_TEST_PGLITE=1` so the test harness rolls back an interrupted child's transaction on reconnect. Install Chromium, Playwright and OpenSSL before the offline run. OpenSSL generates temporary certificates for the local HTTPS redirect tests. `TENDER_PLAYWRIGHT` and `TENDER_CHROMIUM` can point to existing installations. Each SQL checkout case owns and removes a fresh schema. Follow the [consumer setup](../../examples/consumer/README.md).

```sh
GOPROXY=off go test -count=1 -tags=e2e ./test/e2e/... -run TestAdyenCheckout
GOPROXY=off go test -race -count=1 -tags=integration ./test/contract/... -run '^TestAdyen'
GOPROXY=off go test -race -count=1 -tags=e2e,integration ./test/e2e/...
(cd examples/consumer && node --test component.test.cjs && GOWORK=off go test ./...)
```

## Run a protected sandbox qualification

Configure the test merchant, controlled HTTPS origins, callbacks and shopper described in the consumer README. The first phase exercises the browser and the second reads accounting evidence after the operator captures and refunds. The browser ticket must use a public registered HTTPS merchant origin; loopback and reserved mock domains are rejected. The portal probe requires the merchant/evidence/DB settings only; the browser API, client and HMAC secrets are not passed to its workflow step. Its processed signed events must have a host receipt time within 30 days, and every matched Customer Area accounting row must carry a `Booking Date (AMS)` within 30 days. A freshly decoded report file does not establish freshness. Each phase ends with visible **BLOCKED** even if its partial probe succeeds: a complete qualification must correlate the same host-issued order across browser result, signed event and authenticated report. Official merchant evidence remains outstanding.

```sh
PAYMENTS_ENV=sandbox go test -count=1 -tags=sandbox ./test/sandbox/... -run '^TestAdyenSandboxBrowser$'
```

Run two consumer instances for the browser phase. Give each its own migrated schema and registered HTTPS origin. The same public test client key must allow both origins. Point each origin's reverse proxy and signed Standard callback at the corresponding listener. Start the following commands in separate terminals with the protected merchant settings from the consumer README.

```sh
TENDER_CONSUMER_PROVIDER=adyen TENDER_ADYEN_BRAND=visa TENDER_CONSUMER_SCHEMA=visa_demo \
  TENDER_ADYEN_ORIGIN="$TENDER_ADYEN_VISA_ORIGIN" go run . -environment SANDBOX -listen 127.0.0.1:8081
TENDER_CONSUMER_PROVIDER=adyen TENDER_ADYEN_BRAND=mc TENDER_CONSUMER_SCHEMA=mc_demo \
  TENDER_ADYEN_ORIGIN="$TENDER_ADYEN_MC_ORIGIN" go run . -environment SANDBOX -listen 127.0.0.1:8082
```

In the protected runner, set `TENDER_ADYEN_VISA_ORIGIN` and `TENDER_ADYEN_MC_ORIGIN` to those two explicitly registered HTTPS origins. Assign the Visa guest URL to `TENDER_ADYEN_VISA_CHALLENGE_TICKET`, the Mastercard guest URL to `TENDER_ADYEN_MC_FRICTIONLESS_TICKET`, and its account URL to `TENDER_ADYEN_MC_PURCHASE_TICKET`. The browser tests consume those three grants on separate orders. The runner reads the component's own state response, verifies the secured iframe origin, and restricts each outbound destination and every top-level navigation. Each case can also run separately with Go's subtest filter.

| Browser subtest | Registered route and ticket | Expected observation |
| --- | --- | --- |
| `TestAdyenSandboxBrowser/visa_challenge` | Visa, `TENDER_ADYEN_VISA_CHALLENGE_TICKET` | Adyen's direct challenge test card opens the approved issuer frame, submits its test challenge, and returns an authorization on the original order. |
| `TestAdyenSandboxBrowser/mc_frictionless` | Mastercard, `TENDER_ADYEN_MC_FRICTIONLESS_TICKET` | Adyen's fingerprint-dependent test card reaches the disabled state. This case verifies the block and supplies no frictionless authorization evidence. |
| `TestAdyenSandboxBrowser/mc_purchase` | Mastercard, `TENDER_ADYEN_MC_PURCHASE_TICKET` | The ordinary test card authorizes when the test merchant's rules allow a direct payment. |

Set test Dynamic 3-D Secure rules to trigger the stated challenge or fingerprint case. Use the documented non-challenge test settings for the ordinary purchase. A different issuer origin, a forced redirect, or a merchant response lacking authoritative scheme brand requires investigating the observed result before enabling that route. [Official 3DS cases](https://docs.adyen.com/development-resources/testing/3d-secure-2-authentication/) · [Official test cards](https://docs.adyen.com/development-resources/test-cards-and-credentials/test-card-numbers/)

After a sandbox authorization, use the private merchant route to capture the order. Wait for its signed event and host fulfillment, then refund it and wait for the signed refund. Download the corresponding Payment accounting report from the **test** Customer Area with the additional **Modification Psp Reference** and **Booking Date (AMS)** columns. Run `TestAdyenSandboxPortal` separately once the report is ready. Set `TENDER_ADYEN_ACCOUNTING_CSV` to that local export, `TENDER_ADYEN_EVIDENCE_ORDER` to the order ID, and supply the matching consumer schema and read-access DSN. The read-only comparison requires the provider PSP, signed merchant reference, brand, currency, authorized amount and separately matched capture/refund modification PSPs. It requires processed successful AUTHORISATION, CAPTURE and REFUND events and exactly one fulfillment. Export the entire payment history including adverse entries. In USD payment currency, the Authorised row credits the authorized register, SentForSettle debits that register and credits the captured register, and SentForRefund debits the captured register. Amounts must equal the trusted order total. Settled and Refunded entries, when present, must clear the corresponding captured register movement. An adverse, duplicate, stale or contradictory row fails the comparison. Run for each qualified brand and retain the dated result, API/SDK version, browser/device and redacted PSP references with the merchant. [Adyen Payment accounting report](https://docs.adyen.com/reporting/invoice-reconciliation/payment-accounting-report)

```sh
PAYMENTS_ENV=sandbox go test -count=1 -tags=sandbox ./test/sandbox/... -run '^TestAdyenSandboxPortal$'
```

A combined `-run TestAdyen` run requires three fresh browser grants plus a previously captured and refunded order in the configured evidence schema with its completed report. The current two phases are diagnostic probes and cannot return a qualification PASS. The fingerprint case is an expected block, not a successful authorization.

Automated report download, adjustment import, post-refund reversal, bank payout confirmation and downstream delivery compensation remain separate capabilities. Credentialed verification and a clean independent import against a published version remain release gates. [Adapter coverage](../providers/adyen.md)
