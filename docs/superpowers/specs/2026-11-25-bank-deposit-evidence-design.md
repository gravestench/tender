# Bank deposit evidence design

## Intent

Tender must help a merchant compare a processor payout with an independent bank statement before a low-value live pilot is accepted. The existing `PayoutMatched` verdict means only that a complete provider report agrees with the payment ledger; it must remain unchanged.

## Boundaries

The host authenticates the bank statement independently, chooses an opaque bank-account token, and supplies a stable movement ID, statement SHA-256 digest, canonical payout reference, booking time, positive deposit amount and currency. Tender stores no bank account number, statement text, or personal data. The bank source is separate from the processor report and may arrive later. The host remains responsible for proving that its canonical reference is actually present on the statement.

## Approaches

1. Extend `SettlementReport` with bank rows. This couples independent artifacts and forces a report replay when a statement arrives. Rejected.
2. Accept a caller-provided `PayoutMatched` result and compare it in memory. This allows fabricated provider verdicts. Rejected.
3. Add a separate, durable deposit review keyed to a persisted scoped payout result. Chosen. PostgreSQL can read its existing reconciliation history. Memory can retain unique claims but cannot issue a match until it supports trusted provider payout history.

## Contract

An engine method accepts a report ID, payout ID and host-authenticated bank evidence. It validates scope, account token, SHA-256 digest, movement ID, reference, amount, currency and booking time. It reads the latest persisted provider payout verdict and produces a separate result: matched, provider unconfirmed, reference/currency/amount mismatch, or reused movement/reference. A bank movement and a payout reference can be claimed by at most one payout within their merchant/environment scope. Replaying identical evidence is idempotent; changing an existing claim is a conflict. The result and claim are transactional. No bank result mutates payment funds or fulfillment.

The host must still furnish a real authenticated provider report, linked signed events, a real independent bank statement, and a fee comparison before any G3 or production claim. Synthetic tests prove only the contract.

## Verdict transitions

A current bank verdict is an operational projection; each first result and changed verdict is also appended to immutable history with its own timestamp and unchanged bank evidence. Appending a non-matched provider payout result automatically changes any current bank `matched` verdict to `provider_unconfirmed` in the same transaction and appends that transition. A later provider restoration does not automatically re-match bank evidence. Only an explicit replay against the latest matched provider payout may do that; the original movement claim remains reserved throughout.
