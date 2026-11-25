# Dwolla US ACH adapter design

Tender's next US ACH route uses Dwolla's documented pay-in transfer API without taking bank account or routing numbers into Go. This is an additional processor candidate beside Adyen/giact, not a claim of merchant eligibility, production support, or lower fees. The host owns customer onboarding, verified funding-source selection, the approved one-time authorization and durable proof, the shopper interface, and reconciliation with the merchant bank statement.

## Approaches

- **Direct pay-in transfer API (selected):** Create one USD transfer from a host-approved, verified customer funding source to the configured merchant bank funding source. Use the operation key as `Idempotency-Key` and the operation ID as `correlationId`. A `201 Location` is checked by an authenticated read. The reply creates a pending ACH payment with zero funds. Signed webhooks are durably received, but an event alone cannot establish settlement or a bank deposit.
- **Dwolla Drop-in-only facade:** Easier browser wiring, but does not implement Tender's payment and recovery contracts; rejected.
- **Plaid Transfer:** Another credible route, but its authorization, ledger and sweep model needs a separate design; deferred.

## Boundaries and safety

The adapter accepts only opaque Dwolla customer and funding-source resource IDs plus a host-held authorization-proof digest. It requires an injected host authorization check before any transfer POST. It never accepts account or routing numbers. Its HTTP request uses Dwolla HAL v1, a bearer-token callback owned by the host, a pinned endpoint for each environment, exact source/destination URLs, decimal USD, and a stable correlation ID. MOCK uses loopback and the same wire format. Ordinary SANDBOX and LIVE capabilities remain disabled until protected merchant qualification; the adapter never silently switches endpoints.

Network failure or malformed/ambiguous response after a financial POST produces an unknown outcome. It is not automatically replayed. The adapter reads the proposed source funding resource and requires verified, nonremoved bank status, ACH channel and checking or savings type before POST; the host separately proves customer ownership and consent. A transfer lookup establishes the immediate response identity only when its source, destination, correlation ID and amount all match. Transfer `pending` and even `processed` are not enough for Tender collection: Dwolla has two-sided legs and delayed ACH failures. Host-authenticated settlement and payout/bank-deposit evidence must be added before money can enter Tender's `Settled` total or fulfillment outbox.

The webhook listener validates the original body against Dwolla's `X-Request-Signature-SHA-256` HMAC, checks the account and resource links, then durably deduplicates by event ID before a 2xx response. Receipt processing remains pending until authenticated transfer reads, return evidence, and report correlation are implemented. Unknown topics remain reviewable; no webhook alone changes funds.

## Qualification gates

MOCK tests exercise exact transfer JSON, authorization binding, malformed and lost replies, HMAC tampering/replay and durable inbox behavior. Protected SANDBOX requires a merchant test account, approved pay-in funds flow, customer onboarding and bank verification, signed mandate/proof, actual transfer/return events, matching payout report and bank statement, and browser data-boundary evidence. LIVE and fee-savings claims additionally require merchant contracts, compliance approval and a controlled pilot.

Sources: [Dwolla receive money](https://developers.dwolla.com/docs/receive-money), [transfer lifecycle](https://developers.dwolla.com/docs/transfer-lifecycle), [transfer API](https://developers.dwolla.com/docs/api-reference/transfers/initiate-a-transfer), [webhook verification](https://developers.dwolla.com/docs/working-with-webhooks), [bank verification](https://developers.dwolla.com/docs/bank-funding-source).
