package klarna

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gravestench/tender"
)

// Report rows are from the JSON Settlements API payout details and paginated
// transaction endpoints. The Tender keys are host-authenticated provenance and
// scope assertions, not Klarna response fields. Amounts are integer minor units.
const (
	klarnaPartKey             = "Tender Source Part ID"
	klarnaSectionKey          = "Tender Source Section ID"
	klarnaExpectedMerchantKey = "Tender Expected Merchant ID"
	klarnaPayoutMerchantKey   = "Tender Payout Merchant ID"
	klarnaPayoutReferenceKey  = "Tender Payout Reference"
	klarnaSettlementTypeKey   = "Tender Payout Settlement Type"
)

var klarnaReportID = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,200}$`)

func transactionID(v string) bool { return len(v) <= 64 && klarnaReportID.MatchString(v) }

func reportID(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "klarna-" + hex.EncodeToString(h[:])
}

func minorInteger(s string, positive bool) (int64, error) {
	if s == "" || strings.Trim(s, "0123456789") != "" {
		return 0, tender.ErrValidation
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || positive && n <= 0 {
		return 0, tender.ErrValidation
	}
	return n, nil
}

func reportProvenance(f map[string]string, observedAt time.Time, detail bool) (part, section, payout string, err error) {
	part, section, payout = f[klarnaPartKey], f[klarnaSectionKey], f["payment_reference"]
	if observedAt.IsZero() || !klarnaReportID.MatchString(part) || !klarnaReportID.MatchString(section) || !klarnaReportID.MatchString(payout) || !klarnaReportID.MatchString(f[klarnaExpectedMerchantKey]) {
		return "", "", "", tender.ErrValidation
	}
	if detail {
		if f[klarnaPayoutMerchantKey] != f[klarnaExpectedMerchantKey] || f[klarnaPayoutReferenceKey] != payout || f[klarnaSettlementTypeKey] != "NET" {
			return "", "", "", tender.ErrUnsupported
		}
	} else if f["merchant_id"] != f[klarnaExpectedMerchantKey] {
		return "", "", "", tender.ErrUnsupported
	}
	if f["currency_code"] != "USD" {
		return "", "", "", tender.ErrUnsupported
	}
	return part, section, payout, nil
}

// ProjectSettlement maps a SALE/PURCHASE or merchant RETURN/PURCHASE_RETURN
// transaction from a fully paginated, host-authenticated US/USD NET payout.
// The order ID and capture/refund ID remain distinct so reconciliation can
// require the succeeded local modification operation.
func ProjectSettlement(f map[string]string, observedAt time.Time) (tender.SettlementLine, error) {
	part, section, payout, err := reportProvenance(f, observedAt, true)
	if err != nil {
		return tender.SettlementLine{}, err
	}
	order := f["order_id"]
	if !transactionID(order) {
		return tender.SettlementLine{}, tender.ErrValidation
	}
	var kind tender.SettlementEntryKind
	var mod string
	var sign int64 = 1
	switch {
	case f["type"] == "SALE" && f["detailed_type"] == "PURCHASE":
		kind, mod = tender.SettlementCaptured, f["capture_id"]
		if f["refund_id"] != "" {
			return tender.SettlementLine{}, tender.ErrValidation
		}
	case f["type"] == "RETURN" && f["detailed_type"] == "PURCHASE_RETURN":
		kind, mod, sign = tender.SettlementRefunded, f["refund_id"], -1
		if f["capture_id"] != "" {
			return tender.SettlementLine{}, tender.ErrValidation
		}
	default:
		return tender.SettlementLine{}, tender.ErrUnsupported
	}
	if !transactionID(mod) || mod == order {
		return tender.SettlementLine{}, tender.ErrValidation
	}
	amount, err := minorInteger(f["amount"], true)
	if err != nil {
		return tender.SettlementLine{}, err
	}
	gross := sign * amount
	return tender.SettlementLine{
		ID:                reportID("transaction", payout, part, section, f["type"], order, mod, f["amount"]),
		ProviderReference: order, OriginalReference: order, ModificationReference: mod,
		BatchID: payout, SourcePartID: part, SourceSectionID: section, PayoutReference: payout,
		Kind: kind, Currency: "USD", GrossMinor: gross, NetMinor: gross, ObservedAt: observedAt.UTC(),
	}, nil
}

// ProjectFee keeps each purchase fee as a separate negative batch adjustment.
// A fee is not payment principal and cannot be represented as a second capture.
func ProjectFee(f map[string]string, observedAt time.Time) (tender.PayoutAdjustment, error) {
	part, section, payout, err := reportProvenance(f, observedAt, true)
	if err != nil {
		return tender.PayoutAdjustment{}, err
	}
	if f["type"] != "FEE" || f["detailed_type"] != "PURCHASE_FEE_FIXED" && f["detailed_type"] != "PURCHASE_FEE_PERCENTAGE" {
		return tender.PayoutAdjustment{}, tender.ErrUnsupported
	}
	order, capture := f["order_id"], f["capture_id"]
	if !transactionID(order) || !transactionID(capture) || order == capture || f["refund_id"] != "" {
		return tender.PayoutAdjustment{}, tender.ErrValidation
	}
	amount, err := minorInteger(f["amount"], true)
	if err != nil {
		return tender.PayoutAdjustment{}, err
	}
	return tender.PayoutAdjustment{
		ID:      reportID("fee", payout, part, section, f["detailed_type"], order, capture, f["amount"]),
		BatchID: payout, SourcePartID: part, SourceSectionID: section, Currency: "USD",
		Type: f["detailed_type"] + ":" + order + ":" + capture, SignedMinor: -amount,
	}, nil
}

// ProjectPayout maps one NET payout-details response. Unsupported account
// movements make a payout non-provable by this narrow projector.
func ProjectPayout(f map[string]string, observedAt time.Time) (tender.PayoutLine, error) {
	part, section, payout, err := reportProvenance(f, observedAt, false)
	if err != nil {
		return tender.PayoutLine{}, err
	}
	if f["merchant_settlement_type"] != "NET" {
		return tender.PayoutLine{}, tender.ErrUnsupported
	}
	if _, err := time.Parse(time.RFC3339Nano, f["payout_date"]); err != nil {
		return tender.PayoutLine{}, tender.ErrValidation
	}
	zeroTotals := []string{"commission_amount", "commission_reversal_amount", "repay_amount", "holdback_amount", "tax_amount", "fee_correction_amount", "reversal_amount", "release_amount", "charge_amount", "credit_amount", "fee_refund_amount", "tax_refund_amount", "deposit_amount", "opening_debt_balance_amount", "closing_debt_balance_amount"}
	allowedTotals := map[string]bool{"totals.sale_amount": true, "totals.return_amount": true, "totals.fee_amount": true, "totals.settlement_amount": true}
	for _, name := range zeroTotals {
		allowedTotals["totals."+name] = true
	}
	for key := range f {
		if strings.HasPrefix(key, "totals.") && !allowedTotals[key] {
			return tender.PayoutLine{}, tender.ErrUnsupported
		}
	}
	for _, name := range zeroTotals {
		n, err := minorInteger(f["totals."+name], false)
		if err != nil {
			return tender.PayoutLine{}, err
		}
		if n != 0 {
			return tender.PayoutLine{}, tender.ErrUnsupported
		}
	}
	sale, err := minorInteger(f["totals.sale_amount"], false)
	if err != nil {
		return tender.PayoutLine{}, err
	}
	ret, err := minorInteger(f["totals.return_amount"], false)
	if err != nil {
		return tender.PayoutLine{}, err
	}
	fee, err := minorInteger(f["totals.fee_amount"], false)
	if err != nil {
		return tender.PayoutLine{}, err
	}
	settlement, err := minorInteger(f["totals.settlement_amount"], true)
	if err != nil {
		return tender.PayoutLine{}, err
	}
	if sale < ret || sale-ret < fee || sale-ret-fee != settlement {
		return tender.PayoutLine{}, tender.ErrValidation
	}
	return tender.PayoutLine{
		ID:      reportID("payout", f["merchant_id"], payout, part, section, f["totals.settlement_amount"]),
		BatchID: payout, SourcePartID: part, SourceSectionID: section, Currency: "USD",
		PayoutReference: payout, AmountMinor: settlement, ObservedAt: observedAt.UTC(),
	}, nil
}
