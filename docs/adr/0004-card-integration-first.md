# 0004. Card integration first

**Status** Accepted for implementation planning.

Prioritize a narrow importable card pre-release so Blacktop can test a direct acquiring integration early. The reference design targets US/USD and evaluates Adyen Checkout with Visa and Mastercard. A minimal slice includes server-owned order totals, hosted card fields, an allowed 3-D Secure action, idempotent authorization and capture, a verified event inbox, durable payment and fulfillment records, void and refund where supported, and operator recovery for uncertain outcomes. Gate every advertised capability on official sandbox evidence. Enable a live Blacktop pilot only after separate merchant and provider approval.

Keep the integration in a Blacktop-owned adapter when its repository is separately in scope. That adapter translates Blacktop order IDs, trusted totals and fulfillment policy to Tender, then makes callback and outbox processing idempotent. The Tender module and sample must build outside Blacktop.

Blacktop currently builds customer checkout in `backend/internal/website/storefront/customerOrderStripeCheckout.go`, called by customer order and account routes in `backend/internal/app/httpapi`. Use that code's server-side order pricing and order UUID as the integration seam. Exercise the seam first with a standalone mock consumer that uses the same shapes. Scope the direct Adyen path to a single merchant and preserve existing platform payout and Stripe Connect flows, which rely on multi-merchant payment facilitation.

Completing every rail before the first consumer integration would delay feedback on the purchase path. Launching early as full v1.0 would promise a scope and evidence level the early slice cannot meet. An independently tested card pre-release gives Blacktop an earlier integration target while PayPal, Klarna, and ACH stay in the full release backlog.

Direct integration can remove a software intermediary. Processor or acquirer contracts, bank or network fees, merchant onboarding, PCI assessment, and provider approval still apply. Evaluate total cost against actual commercial terms and operational ownership.
