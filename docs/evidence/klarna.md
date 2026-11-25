# Klarna financing qualification

Updated 2026-11-24. The reference adapter models a US/USD regional financing candidate with the Klarna Payments and Order Management APIs. Its authorization callback, order creation, capture, cancel and refund are exercised against a loopback fixture. This is **MOCK evidence**. Official playground and live eligibility remain **BLOCKED**; ordinary web financing checkout is disabled under the strict on-page policy.

| Scenario | Local observation | Official playground observation |
| --- | --- | --- |
| Financing eligibility and authorization | Synthetic eligible and ineligible customer paths, callback binding, expiry, reauthorization and changed-cart rejection are in the contract. | **BLOCKED:** approved merchant product, region, fixture customer and observed authorization. |
| Order, capture, cancel and partial refund | Loopback contract checks authenticated order/modification references, no-effect rejections, duplicate callbacks and durable state. | **BLOCKED:** actual order/callback and portal records. |
| Browser containment | Chromium MOCK renders in-page approval; popup and redirect modes stop before collection. | **BLOCKED:** actual SDK and mobile/desktop approval behavior. |
| Settlement and fees | A host-authenticated Settlements API v1 JSON projection has offline US/USD NET tests for sale, partial capture, merchant return, purchase fees, payout arithmetic, completeness and SQL restart replay. Automatic report download remains unsupported. | **BLOCKED:** a real merchant payout details response, every transaction page, fee contract, bank statement reference and deposit comparison. |

`TestKlarnaSandboxOrderRecord` compares a previously captured/refunded US/USD order against an authenticated NA playground read. It does not launch an approval or establish browser compatibility. Follow [the Klarna setup guide](../providers/klarna.md). Record timestamp, product, region, API/SDK version, expected/observed amount and status, sanitized order/capture/refund IDs and browser trace before claiming support.

The report projector accepts only the documented [Settlements API payout details and paginated transaction JSON](https://docs.klarna.com/acquirer/klarna/api/settlements/). It has no authenticated merchant export or bank statement in this workspace. Its local rows are synthetic; the report projection and payout match are offline checks, not a Klarna sandbox qualification.
