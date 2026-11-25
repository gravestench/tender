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

## Blacktop-owned changes for the first adapter

Blacktop's current store-order checkout route calls `StartStripeCheckout`, its checkout response contains Stripe client fields, and `CustomerOrderView.vue` mounts `StripeConnectedCheckout`. A Tender route needs its own response type for the allowed `CustomerAction`, a server-side provider choice made before the financial claim, and a browser component for the chosen Adyen action. Keep that choice fixed through retries and recovery.

Add a Blacktop migration before writing an Adyen payment: existing provider `CHECK` constraints on `customer_order_payments`, `payment_provider_configs` and `payment_provider_events` admit only the current providers. The migration also needs a tenant-scoped, unique mapping from Blacktop order/payment IDs to Tender's scoped payment and operation IDs. Commit the trusted amount, currency, idempotency key and provider choice with Blacktop's order claim before calling Tender. Run Tender's versioned PostgreSQL migrations in a separately owned schema or an explicitly coordinated shared schema.

Mount the verified provider callback and run Tender's inbox and recovery workers. A Blacktop worker must consume each full-collection outbox entry through an idempotent transaction that updates the mapped payment, order and inventory/fulfillment record once. It must preserve Blacktop's existing review holds, refunds and historical Stripe reconciliation; a successful browser response alone cannot mark an order paid. Test duplicate submission, callback replay, timeout, restart, cancellation, refund and a late adverse event across both ledgers. Blacktop's existing direct-charge path accepts Stripe/Square source tokens and cannot substitute for the hosted-field action flow.

Keep the initial route restricted to a single merchant-owned direct account. Blacktop's current Stripe Connect checkout, connected-account payouts, invoicing and other platform flows rely on multi-merchant capabilities that this reference slice does not replace. Avoid dual submission: a given order must select one provider before reserving a financial operation and keep that choice through recovery. Existing Stripe payments should finish and reconcile on their original provider.

## Qualification sequence

1. Import the published **MOCK-only** `v0.1.0-alpha.1` tag into a clean Blacktop branch and build without a local `replace`. Keep the provider switch off. Follow the [tagged import instructions](../examples/consumer/README.md#verify-the-tagged-offline-alpha).
2. Wire the order claim, trusted total, callback inbox and fulfillment outbox in MOCK. Run Blacktop's purchase, cancellation, refund, timeout, restart and duplicate-submit tests against the embedded simulator. Compare Blacktop's payment row and fulfillment ledger to Tender's records.
3. Obtain a merchant-owned Adyen test account, registered origin and HMAC keys. Run the protected [card qualification](evidence/adyen.md), including genuine provider portal and browser evidence. Resolve any navigation or issuer path that violates checkout policy before exposing the method.
4. Obtain the acquirer's commercial terms, assessment decision and live approval. Run an approved low-value pilot, then compare actual captured amount, processor fee, net payout, refund cost and bank deposit to equivalent Blacktop transactions on the existing stack. Expand only if effective cost and reliability meet the business target.

Tender removes a hosted software dependency at the application boundary; it cannot remove the card acquirer, network, PayPal, Klarna or ACH originating bank. No fee saving is established by an offline test. [Security](security.md) · [Operations](operations.md).
