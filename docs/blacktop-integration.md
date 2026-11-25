# Blacktop integration seam and cost pilot

The fastest useful Tender slice for Blacktop is a **single merchant, US/USD card checkout** behind a Blacktop method switch. This repository supplies the independent Go module and a durable example consumer; it has not changed Blacktop or processed a real payment. Adyen test-merchant qualification and a browser-safe hosted card path remain prerequisites for a genuine pilot.

Blacktop currently begins store-order checkout in `backend/internal/website/storefront/customerOrderStripeCheckout.go` through `StartStripeCheckout`. It authenticates the guest/account, locks the pending order, reads server-owned `total_cents` and `currency`, and commits a checkout claim before the Stripe request. Its `customer_order_payments` row carries the opaque public payment ID, order link, idempotency key and review state. These are the seam for a Blacktop-owned Tender adapter. The standalone [consumer](../examples/consumer/README.md) demonstrates the same trust and lifecycle shape without importing Blacktop.

## Proposed adapter boundary

| Blacktop value or action | Tender boundary |
| --- | --- |
| Tenant, merchant configuration and environment | Stable `tender.Scope` and one provider account; use a separate merchant-owned Adyen configuration. |
| `customer_orders.public_id`, server `total_cents`, `currency` | Opaque Tender order claim, `Money` and exact US/USD card market; browser form cannot supply the price. |
| `customer_order_payments.public_id` and idempotency key | Stable Tender payment ID and create key; map both directions in a Blacktop-owned table with a uniqueness constraint. |
| Customer session and CSRF | Blacktop's existing authenticated routes. Project only an allowed, unexpired provider `CustomerAction` to the checkout page. |
| Provider callback | Mount the Adyen Standard receiver under Blacktop's HTTPS routes; process its durable inbox asynchronously. |
| Completed order | Consume a Tender full-collection outbox entry through a Blacktop idempotent fulfillment transaction. Update Blacktop order/payment status once per scoped entry ID. |
| Capture failure, refund, ACH return or discrepancy | Blacktop operator review and compensation workflow; never infer settlement from the browser. |

Keep the initial route restricted to a single merchant-owned direct account. Blacktop's current Stripe Connect checkout, connected-account payouts, invoicing and other platform flows rely on multi-merchant capabilities that this reference slice does not replace. Avoid dual submission: a given order must select one provider before reserving a financial operation and keep that choice through recovery. Existing Stripe payments should finish and reconcile on their original provider. The Blacktop adapter should own migrations for its mapping and ensure no duplicate order fulfillment across provider paths.

## Qualification sequence

1. Import Tender into a clean Blacktop branch and build it with the local development module or a published **pre-release**. Keep the provider switch off. The [independent import instructions](../examples/consumer/README.md#build-against-a-future-release) apply after a tag exists.
2. Wire the order claim, trusted total, callback inbox and fulfillment outbox in MOCK. Run Blacktop's purchase, cancellation, refund, timeout, restart and duplicate-submit tests against the embedded simulator. Compare Blacktop's payment row and fulfillment ledger to Tender's records.
3. Obtain a merchant-owned Adyen test account, registered origin and HMAC keys. Run the protected [card qualification](evidence/adyen.md), including genuine provider portal and browser evidence. Resolve any navigation or issuer path that violates checkout policy before exposing the method.
4. Obtain the acquirer's commercial terms, assessment decision and live approval. Run an approved low-value pilot, then compare actual captured amount, processor fee, net payout, refund cost and bank deposit to equivalent Blacktop transactions on the existing stack. Expand only if effective cost and reliability meet the business target.

Tender removes a hosted software dependency at the application boundary; it cannot remove the card acquirer, network, PayPal, Klarna or ACH originating bank. No fee saving is established by an offline test. [Security](security.md) · [Operations](operations.md).
