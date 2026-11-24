# Adyen card qualification

Updated November 24, 2026. The verified checkout slice uses the v72 Advanced flow, SQL persistence and a loopback HTTP simulator for US/USD Visa and Mastercard routes. Official test merchant verification is **BLOCKED**. The card pre-release remains untagged.

| Check | Locally observed | Official merchant evidence |
| --- | --- | --- |
| Visa purchase and full refund | Authorized USD 25.99, recovered a lost capture acknowledgment from its signed Standard event, recorded one fulfillment, then applied the verified refund. Replaying capture and duplicate events kept one consumed outbox entry. | BLOCKED |
| Mastercard cancellation | Direct synthetic challenge survived refresh. A stale form could not advance the challenge. A fresh details request authorized the payment. Signed cancellation voided it with zero fulfillments. | BLOCKED |
| Decline and abandoned order | Refused payment displayed declined. An abandoned prepared order survived restart. Advancing its stored deadline disabled submission without creating a collection. | BLOCKED |
| Browser containment | Chromium on a 390 × 844 mobile-emulated viewport refreshed and submitted the synthetic Adyen checkout on one loopback origin. A provider redirect displayed the review state without a new page or external request. A 1200 × 900 desktop Chromium instance exercised the blocked unknown action. Desktop Chromium also submitted the original synthetic checkout. Both fixtures checked for credential inputs while the active form was visible. | Physical mobile browsers, hosted fields, issuer UI and desktop sandbox remain BLOCKED. |
| Native 3DS | The adapter, action gate and mock handle only direct native challenges. Unknown actions, redirects and fingerprint actions stop in checkout. A popup action mode fails the host policy. | BLOCKED for direct challenge. Adyen's fingerprint-dependent frictionless case is **unsupported**. |
| SQL and webhooks | pgx ran the Tender migrations and host ledger on PostgreSQL 17.5 WASM/PGlite. HMAC-verified HTTP deliveries drove the SQL inbox and fulfillment. | Native multi-session PostgreSQL, merchant-generated webhooks and Customer Area reconciliation are outstanding. |
| Independent module | The consumer builds with `GOWORK=off` and the development module replacement. | Published-tag import awaits a qualified tag. |

The sandbox integration pins both 6.45.2 test-CDN resources with SHA-384 integrity values. Card numbers and security codes belong in the provider's hosted fields. The host projects encrypted JWE fields and the allowed browser metadata into a transient confirmation. The SDK runs in an iframe with a response sandbox that omits top navigation and popup permissions. A direct challenge additionally needs a complete native token, the approved test ACS origin and Adyen's test notification origin. The SDK fingerprint component can dispatch a redirect itself. It is excluded from the supported browser actions. [Pinned Card source](https://github.com/Adyen/adyen-web/blob/v6.45.2/packages/lib/src/components/Card/Card.tsx) · [Pinned SDK](https://checkoutshopper-test.cdn.adyen.com/checkoutshopper/sdk/6.45.2/adyen.js) · [Official embedding guidance](https://docs.adyen.com/standard/integration/drop-in/best-practices)

## Reproduce the offline evidence

Supply a disposable local PostgreSQL instance in `TENDER_TEST_DATABASE_URL` with schema creation privileges. Install Chromium and Playwright before the offline run. `TENDER_PLAYWRIGHT` and `TENDER_CHROMIUM` can point to existing installations. Each SQL checkout case owns and removes a fresh schema. Follow the [consumer setup](../../examples/consumer/README.md).

```sh
GOPROXY=off go test -count=1 -tags=e2e ./test/e2e/... -run TestAdyenCheckout
GOPROXY=off go test -race -count=1 -tags=integration ./test/contract/... -run '^TestAdyen'
GOPROXY=off go test -race -count=1 -tags=e2e,integration ./test/e2e/...
(cd examples/consumer && node --test component.test.cjs && GOWORK=off go test ./...)
```

## Run a protected sandbox qualification

Configure the test merchant, registered HTTPS origin, callback and shopper described in the consumer README. The explicit command fails with **BLOCKED** when the API key, public test client key, merchant account, webhook HMAC key or registered origin is missing. No merchant credential, transaction, callback delivery or portal export was available for this record.

```sh
PAYMENTS_ENV=sandbox go test -count=1 -tags=sandbox ./test/sandbox/... -run TestAdyen
```

The browser cases can also run individually using Go's subtest filter. Provision a fresh order for each, using a separate schema when changing card route. Set the matching one-use HTTPS ticket variable only in the protected runner. The browser receives the public client key and test URL. It verifies the secured iframe origin before entering the published test values and monitors top-level navigation and popup creation. No recording or request-body logging is required.

| Browser subtest | Registered route and ticket | Expected observation |
| --- | --- | --- |
| `TestAdyenSandboxBrowser/visa_challenge` | Visa, `TENDER_ADYEN_VISA_CHALLENGE_TICKET` | Adyen's direct challenge test card opens the approved issuer frame, submits its test challenge, and returns an authorization on the original order. |
| `TestAdyenSandboxBrowser/mc_frictionless` | Mastercard, `TENDER_ADYEN_MC_FRICTIONLESS_TICKET` | Adyen's fingerprint-dependent test card reaches the disabled state. This case verifies the block and supplies no frictionless authorization evidence. |
| `TestAdyenSandboxBrowser/mc_purchase` | Mastercard, `TENDER_ADYEN_MC_PURCHASE_TICKET` | The ordinary test card authorizes when the test merchant's rules allow a direct payment. |

Set test Dynamic 3-D Secure rules to trigger the stated challenge or fingerprint case. Use the documented non-challenge test settings for the ordinary purchase. A different issuer origin, a forced redirect, or a merchant response lacking authoritative scheme brand requires investigating the observed result before enabling that route. [Official 3DS cases](https://docs.adyen.com/development-resources/testing/3d-secure-2-authentication/) · [Official test cards](https://docs.adyen.com/development-resources/test-cards-and-credentials/test-card-numbers/)

After a sandbox authorization, use the private merchant route to capture the order. Wait for its signed event and host fulfillment, then refund it and wait for the signed refund. Download the corresponding Payment accounting report from the **test** Customer Area with the additional **Modification Psp Reference** column. Run `TestAdyenSandboxPortal` separately once the report is ready. Set `TENDER_ADYEN_ACCOUNTING_CSV` to that local export, `TENDER_ADYEN_EVIDENCE_ORDER` to the order ID, and supply the matching consumer schema and read-access DSN. The read-only comparison requires the provider PSP, signed merchant reference, brand, currency, authorized amount and separately matched capture/refund modification PSPs. It also requires processed successful AUTHORISATION, CAPTURE and REFUND events and exactly one fulfillment. Run for each qualified brand and retain the dated result, API/SDK version, browser/device and redacted PSP references with the merchant. [Adyen Payment accounting report](https://docs.adyen.com/reporting/invoice-reconciliation/payment-accounting-report)

General report import, settlement, post-refund reversal and downstream delivery compensation remain separate capabilities. Credentialed verification and a clean independent import against a published version remain release gates. [Adapter coverage](../providers/adyen.md)
