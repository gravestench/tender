# Adyen later adverse review design

## Intent and boundary

Tender needs durable operator notice when an Adyen card dispute is provisionally reversed, becomes a second chargeback, or an earlier refund is reversed. These events must not change captured, refunded, returned or fulfilled history from a webhook alone. Full financial correction and payout accounting still require authenticated merchant reports and sandbox evidence.

## Approaches

1. Apply the event directly to payment funds. This creates a false cash balance if a notification arrives before a report or has an ambiguous modification reference.
2. **Chosen: enqueue a notice-only adverse review.** Reuse the verified inbox transaction and existing `AdverseNotice`; require signed route/payment/amount fields plus a processed earlier notice or succeeded refund operation. Leave ambiguous and reordered events pending for replay.
3. Merely retain the unknown verified inbox row. This is safe but gives Blacktop no typed review item when correlation is available.

## Correlation rules

- `CHARGEBACK_REVERSED` requires a processed `chargeback` notice for the same scoped payment, original payment PSP, dispute PSP, currency and exact amount. This is a provisional review, never a settled credit.
- `SECOND_CHARGEBACK` requires the same original chargeback and a processed `chargeback_reversed` notice for that dispute and exact amount. A duplicate receipt cannot create a second notice.
- `REFUNDED_REVERSED` requires an existing succeeded refund operation on the same payment whose bound modification PSP and amount equal the signed event. Adyen documentation does not promise that every merchant's reversal event uses that PSP, so any different shape stays pending rather than being guessed from amount or unsigned metadata.
- The Standard webhook's HMAC-signed canonical fields, including merchant account, original PSP, modification/dispute PSP, merchant reference, amount, event code and success, are the only event authority. Missing original reference, conflicting route, PSP or amount fails closed. Keep all prior payment funds and fulfillment outbox unchanged.

## Verification and limits

Use the real Adyen HTTP receiver and mockserver's signed Standard fixture. Exercise memory and PostgreSQL contracts, replay, out-of-order event processing, wrong account/payment/PSP/amount and restart. Run focused race tests and vet. This is MOCK evidence only. Merchant configuration must include the original reference on dispute reversal/second-chargeback notifications, and actual refund-reversal identity needs genuine merchant evidence before claiming external compatibility.
