package paypal

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gravestench/tender"
)

// Tender metadata is supplied by the host after authenticating the complete
// SFTP artifact. It is not part of an SB row. ProjectSettlement cannot verify
// file counts or SF/RF totals in isolation; the ingesting host must do so.
const (
	payPalExpectedAccount = "Tender Expected Merchant Account"
	payPalRHAccount       = "Tender RH Account ID"
	payPalSHAccount       = "Tender SH Account ID"
	payPalVersion         = "Tender Report Version"
	payPalComplete        = "Tender Report Complete"
	payPalPart            = "Tender Source Part ID"
	payPalSection         = "Tender Source Section ID"
	payPalBatch           = "Tender Batch ID"
)

var paypalReportOpaque = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,200}$`)
var paypalPartV001 = regexp.MustCompile(`^STL-[0-9]{8}\.[0-9]{2}\.001\.(CSV|TAB)$`)

func paypalUnsignedMinor(raw string, positive bool) (int64, error) {
	if raw == "" || len(raw) > 26 || strings.Trim(raw, "0123456789") != "" {
		return 0, tender.ErrValidation
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || positive && n == 0 {
		return 0, tender.ErrValidation
	}
	return n, nil
}

func paypalRowID(f map[string]string) string {
	h := sha256.Sum256([]byte(strings.Join([]string{
		f[payPalBatch], f[payPalPart], f[payPalSection], f["transaction_id"],
		f["transaction_event_code"], f["paypal_reference_id"], f["transaction_completion_date"],
	}, "\x00")))
	return "paypal-" + hex.EncodeToString(h[:])
}

// ProjectSettlement maps only a merchant-credit Express Checkout capture or
// merchant-debit refund whose SB row directly relates to the original order.
// PayPal's money fields are unsigned integer minor units, not decimal dollars.
// A related TXN capture, unknown T-code, or report variant needs a separately
// corroborated reference mapping and is deliberately left for review.
func ProjectSettlement(f map[string]string, observedAt time.Time) (tender.SettlementLine, error) {
	var empty tender.SettlementLine
	if observedAt.IsZero() || f["column_type"] != "SB" {
		return empty, tender.ErrValidation
	}
	account := f[payPalExpectedAccount]
	if !paypalReportOpaque.MatchString(account) || account != f[payPalRHAccount] || account != f[payPalSHAccount] ||
		!paypalReportOpaque.MatchString(f[payPalBatch]) || !paypalReportOpaque.MatchString(f[payPalSection]) ||
		!paypalPartV001.MatchString(f[payPalPart]) || f[payPalVersion] != "001" || f[payPalComplete] != "true" {
		return empty, tender.ErrUnsupported
	}
	if f["paypal_reference_id_type"] != "ODR" {
		return empty, tender.ErrUnsupported
	}
	order, movement := f["paypal_reference_id"], f["transaction_id"]
	if !idPattern.MatchString(order) || !idPattern.MatchString(movement) || order == movement || len(order) > 24 || len(movement) > 24 {
		return empty, tender.ErrValidation
	}
	var kind tender.SettlementEntryKind
	var sign int64
	switch f["transaction_event_code"] {
	case "T0006":
		kind, sign = tender.SettlementCaptured, 1
		if f["transaction_debit_or_credit"] != "CR" {
			return empty, tender.ErrUnsupported
		}
	case "T1107":
		kind, sign = tender.SettlementRefunded, -1
		if f["transaction_debit_or_credit"] != "DR" {
			return empty, tender.ErrUnsupported
		}
	default:
		return empty, tender.ErrUnsupported
	}
	if f["gross_transaction_currency"] != "USD" {
		return empty, tender.ErrUnsupported
	}
	gross, err := paypalUnsignedMinor(f["gross_transaction_amount"], true)
	if err != nil {
		return empty, err
	}
	gross *= sign
	var feeMovement int64
	if f["fee_amount"] != "" || f["fee_debit_or_credit"] != "" || f["fee_currency"] != "" {
		if f["fee_currency"] != "USD" {
			return empty, tender.ErrUnsupported
		}
		feeMovement, err = paypalUnsignedMinor(f["fee_amount"], false)
		if err != nil {
			return empty, err
		}
		switch f["fee_debit_or_credit"] {
		case "DR":
			feeMovement = -feeMovement
		case "CR":
		default:
			return empty, tender.ErrValidation
		}
	}
	if feeMovement > 0 && gross > math.MaxInt64-feeMovement || feeMovement < 0 && gross < math.MinInt64-feeMovement {
		return empty, tender.ErrValidation
	}
	l := tender.SettlementLine{
		ID: paypalRowID(f), ProviderReference: order, OriginalReference: order, ModificationReference: movement,
		BatchID: f[payPalBatch], SourcePartID: f[payPalPart], SourceSectionID: f[payPalSection],
		Kind: kind, Currency: "USD", GrossMinor: gross, FeeMinor: -feeMovement, NetMinor: gross + feeMovement,
		ObservedAt: observedAt.UTC(),
	}
	return l, nil
}

// ProjectPayout stays unsupported until merchant SFTP data proves a complete
// report, a T0400 bank withdrawal and its bank reference. An RF balance footer
// does not establish a payout.
func ProjectPayout(map[string]string, time.Time) (tender.PayoutLine, error) {
	return tender.PayoutLine{}, tender.ErrUnsupported
}
