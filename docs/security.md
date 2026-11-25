# Security boundary and threat model

Tender is a payment orchestration library. Source code, mock tests and hosted fields do not establish PCI certification or merchant eligibility. Before accepting live cards, the merchant and acquirer or qualified assessor must determine the applicable assessment against PCI DSS 4.0.1 and current revisions, provider tokenization rules, EMV 3-D Secure requirements and the approved checkout surface. Bank debit consent and Nacha duties likewise need merchant and originating-bank review. Follow the selected provider's current implementation guide when wire rules change.

## Assets and trust boundaries

| Boundary | Trusted input and control | Main failure to prevent |
| --- | --- | --- |
| Host order → Tender | The host supplies the authenticated merchant, opaque order ID, server-calculated amount, currency, market, environment and one stable operation key. | Browser price changes, cross-merchant payment claims and duplicate collection. |
| Browser → provider | Card numbers and CVV stay in provider hosted fields. ACH bank details use the provider's default Sessions submission; signed mandate handling is not yet approved. | PAN/CVV or bank account data entering Go, logs, storage or browser telemetry. |
| Tender → provider API | A dedicated server credential, pinned environment endpoint, TLS validation, 15-second API timeout and redirects disabled. | Credential leakage, SSRF, endpoint substitution and unknown financial POST retry. |
| Provider callback → verified inbox | Provider-specific HMAC or postback verification precedes a bounded, normalized durable receipt. A worker later binds it to the exact scoped payment and operation. | Forged, replayed, reordered or cross-account events changing funds or fulfillment. |
| Trusted report → reconciliation | The host authenticates and downloads the report. Tender ingests normalized fields under provider, merchant, environment and report identity. | A shopper-supplied report or duplicate/changed report silently rewriting the ledger. |
| Store → host fulfillment | A committed outbox entry identifies a verified collection; the host deduplicates fulfillment and separately processes capture-failure/return review. | Double delivery, delivery before collection, and missing compensation after an adverse event. |

## Controls required of a host

- Separate MOCK, SANDBOX and LIVE credentials, signing keys, accounts, databases, callback URLs, browser origins and logs. There is no automatic environment fallback. Mount `mockserver.Handler()` only on a private MOCK listener. The simulator's scenario controls and synthetic credentials must never be internet facing.
- Keep provider API keys, HMAC keys, PayPal OAuth secret, Klarna callback key and database credentials in a protected secret manager. Grant the runtime only the privileges it needs; use a separate migration role. Rotate keys with provider overlap where supported and retire old verification keys after delivery windows close. Do not include secrets in exceptions, traces, tickets or evidence files.
- Terminate TLS at a trusted endpoint, use TLS 1.3 where the provider supports or requires it, verify upstream certificates and constrain outbound destinations. Use registered HTTPS origins and callbacks for SANDBOX/LIVE. An injected HTTP transport must preserve the provider destination and TLS checks. Do not follow provider API redirects. Restrict callback methods, origins and body sizes at the reverse proxy as well as in the handlers.
- Redact request bodies, URL query strings and headers on checkout, webhook and report routes. The Adyen card confirmation contains transient JWE values; do not log, persist or replay it outside its reserved operation. Klarna callback tokens are sealed in the verified inbox and require the configured key for later processing. Treat that inbox as sensitive. Never collect or store CVV, raw PAN, bank login credentials or production secrets in Tender.
- Issue stable, unpredictable operation keys from the server. Persist a reservation before a financial POST, use only the recorded key for recovery, and hold unknown outcomes until a trusted query or signed event resolves them. Do not trigger a second provider mutation on an HTTP timeout. Use the scoped order claim and host fulfillment ledger to prevent double delivery.
- Schedule and monitor inbox drain, outbox delivery, capture-failure review and reconciliation. A successful callback acknowledgment means a verified receipt is durable, not that fulfillment is complete. ACH can return after settlement; a return requires host compensation even if goods were already delivered.
- Enforce a method disable switch at the host. Disable a route on credential compromise, provider outage, unexpected action/navigation, HMAC failure spikes, unresolved financial discrepancies or policy change. Existing pending operations still need recovery and reconciliation.

## Browser policy

The product requires approval and payment in the checkout page without a top-level redirect or unapproved popup. Browser guards, action allowlists and frame sandboxing are part of the reference consumer. They are not a substitute for testing the actual provider SDK, issuer, device and merchant settings. Ordinary PayPal and Klarna web actions stay disabled; Adyen card and ACH real surfaces stay unqualified until official browser evidence exists. If an issuer or provider requires a forbidden navigation, the host must stop and offer another eligible method.

## Incident response and retention

1. Disable the affected method and preserve the current scoped payment, operation, receipt, outbox, failure-review and reconciliation records. Record the provider incident and last successful settlement. Do not delete a reserved key to force a retry.
2. For an unknown mutation, query with the original key when supported, drain verified callbacks, compare the provider portal and settlement report, and record an operator decision before another charge or refund. For suspected forgery, rotate the signing secret and revalidate only against trusted provider records.
3. Identify affected merchant/environment scopes and customer orders. Compensate any fulfillment after capture failure or ACH return through a separate idempotent host workflow. Reconcile gross, fee, net payout and bank deposit before enabling broader traffic.
4. Retain only the sanitized evidence needed for dispute, accounting and legal obligations. The merchant defines retention and deletion schedules with counsel and processor rules. Keep raw webhook bodies and financial credentials out of the Tender store. Restrict access to payment records and audit each export.

Before a live pilot, review the current PCI DSS and applicable ACH requirements with the processor, confirm the provider account and signed mandate design, inspect TLS and secret handling, run the protected sandbox suite, approve low-value live transactions, and document actual payout/deposit matching. [Operations](operations.md) · [Release gates](evidence/release-gates.md).
