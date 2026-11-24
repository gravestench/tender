# Provider capability matrix

The initial evaluation scope is US/USD. The rows below are candidates for implementation and sandbox verification. Nothing in this matrix authorizes production traffic.

| Required rail | Candidate | Candidate checkout path | Operations to verify | External evidence |
| --- | --- | --- | --- | --- |
| Visa and Mastercard | Adyen Checkout | Hosted card fields and an in-page 3-D Secure challenge where documented and verified | Session, authorization, capture, cancel, refund, status, verified events, settlement reports, partial operations and disputes where supported | BLOCKED pending test merchant, API and client keys, allowed origins, HMAC setup, and a controlled HTTPS callback |
| PayPal wallet | Direct PayPal Orders | BLOCKED pending validation of a documented in-page path in each supported browser context. PayPal discourages the v6 modal on desktop, where its recommended popup needs separate approval. | Approval, authorization and capture where supported, cancel, refund, status, verified transaction events, settlement and disputes where supported | BLOCKED pending sandbox app, seller and buyer accounts, webhook configuration, and a controlled HTTPS callback |
| Klarna financing | Regional Klarna product | BLOCKED pending verification of strict in-page approval. Klarna's component can invoke a popup or redirect, depending on the flow. | Session, approval, order, capture, cancel, refund, callbacks, and settlement where supported | BLOCKED pending product and region validation, playground merchant credentials, fixture customers, and a controlled HTTPS callback |
| ACH bank payments | Partner selection pending | Adyen Web v6.45.2 Sessions-only submission is a candidate pending credentialed browser verification that bank details go directly to Adyen. Advanced first-use is blocked because its submission callback exposes plaintext account and routing data to host JavaScript. | Initiation, pending, settlement, returns, reports, and cancellation or refund where supported | BLOCKED pending partner selection, sandbox access, and verification of the consent flow |

Eligibility depends on the merchant account, regional product, currency, browser, device, funding source, and authentication flow. Record pinned API versions and precise supported modes with the actual evidence. Disable contexts that require prohibited navigation or unapproved popups.

See the [US provider research](provider-research.md) for official provider guidance and the evidence needed to enable each checkout path.

## Evidence records

For each scenario, record provider and API version, commit, environment, UTC timestamp, region and currency, expected and actual behavior, sanitized transaction and event IDs, and redacted browser or portal evidence where relevant. Use **PASS**, **FAIL**, **BLOCKED**, or **NOT APPLICABLE** with a rationale. Record mock results separately from sandbox observations. Keep credentials and raw customer data out of evidence artifacts.

The [release backlog](backlog.md) tracks library release and merchant activation independently.
