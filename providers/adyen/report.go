package adyen

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gravestench/tender"
)

// The Tender Source keys are authenticated report-ingestion metadata, not
// columns emitted by Adyen. A host must attach them after authenticating the
// complete report artifact and assigning each row to its part and section.
const (
	sourcePartKey    = "Tender Source Part ID"
	sourceSectionKey = "Tender Source Section ID"
	expectedAcctKey  = "Tender Merchant Account"
)

var reportOpaque = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,200}$`)
var reportPSP = regexp.MustCompile(`^[A-Za-z0-9]{1,16}$`)
var payoutDescription = regexp.MustCompile(`^(TX[A-Za-z0-9]+XT) batch ([0-9]+)(, | ).+$`)

func cardReportIdentity(f map[string]string, observedAt time.Time) (account, batch, part, section string, err error) {
	account, batch, part, section = f["Merchant Account"], f["Batch Number"], f[sourcePartKey], f[sourceSectionKey]
	if observedAt.IsZero() || account == "" || account != f[expectedAcctKey] || !reportOpaque.MatchString(account) || !reportOpaque.MatchString(batch) || !reportOpaque.MatchString(part) || !reportOpaque.MatchString(section) {
		return "", "", "", "", tender.ErrValidation
	}
	if _, e := strconv.ParseUint(batch, 10, 64); e != nil {
		return "", "", "", "", tender.ErrValidation
	}
	return account, batch, part, section, nil
}

func cardMinor(s string, signed bool) (int64, error) {
	if s == "" {
		return 0, tender.ErrValidation
	}
	neg := false
	if signed && strings.HasPrefix(s, "-") {
		neg, s = true, s[1:]
	}
	whole, frac, dot := strings.Cut(s, ".")
	if whole == "" || len(frac) > 2 || dot && len(frac) == 0 || !digits(whole) || !digits(frac) {
		return 0, tender.ErrValidation
	}
	if dot {
		frac += strings.Repeat("0", 2-len(frac))
	} else {
		frac = "00"
	}
	v, err := strconv.ParseInt(whole+frac, 10, 64)
	if err != nil || v == 0 {
		return 0, tender.ErrValidation
	}
	if neg {
		return -v, nil
	}
	return v, nil
}

func digits(s string) bool {
	for _, b := range []byte(s) {
		if b < '0' || b > '9' {
			return false
		}
	}
	return true
}

func cardDirection(f map[string]string, credit, debit string) (int64, error) {
	c, d := f[credit], f[debit]
	if c == "" == (d == "") {
		return 0, tender.ErrValidation
	}
	if c != "" {
		if c == "0" || c == "0.0" || c == "0.00" {
			return 0, nil
		}
		return cardMinor(c, false)
	}
	if d == "0" || d == "0.0" || d == "0.00" {
		return 0, nil
	}
	v, err := cardMinor(d, false)
	return -v, err
}

func optionalFee(f map[string]string, key string) (int64, bool, error) {
	s := f[key]
	if s == "" {
		return 0, false, nil
	}
	if s == "0" || s == "0.0" || s == "0.00" {
		return 0, true, nil
	}
	v, err := cardMinor(s, true)
	return v, true, err
}

func cardFee(f map[string]string, gross, net int64) (int64, error) {
	if gross > 0 && net < 0 || gross < 0 && net > 0 || net == math.MinInt64 {
		return 0, tender.ErrValidation
	}
	fee := gross - net
	commission, hasCommission, err := optionalFee(f, "Commission (NC)")
	if err != nil {
		return 0, err
	}
	var split int64
	hasSplit := false
	for _, key := range []string{"Markup (NC)", "Scheme Fees (NC)", "Interchange (NC)"} {
		v, present, e := optionalFee(f, key)
		if e != nil {
			return 0, e
		}
		if present {
			hasSplit = true
			if v > 0 && split > math.MaxInt64-v || v < 0 && split < math.MinInt64-v {
				return 0, tender.ErrValidation
			}
			split += v
		}
	}
	if hasCommission && hasSplit || hasCommission && commission != fee || hasSplit && split != fee || !hasCommission && !hasSplit && fee != 0 {
		return 0, tender.ErrValidation
	}
	if paymentFee, present, e := optionalFee(f, "Payment Fees (NC)"); e != nil || present && paymentFee != fee {
		return 0, tender.ErrValidation
	}
	return fee, nil
}

func cardRowID(prefix string, fields ...string) string {
	h := sha256.Sum256([]byte(strings.Join(fields, "\x00")))
	return prefix + hex.EncodeToString(h[:])
}

// ProjectCardSettlement maps one host-authenticated Adyen Settlement details
// row for a US/USD Visa or Mastercard route. A Settled journal row is compared
// with Tender's succeeded capture operation; the report confirms settlement
// but this projection alone never advances payment funds. A Chargeback row is
// compared only after a correlated signed adverse receipt is processed.
func ProjectCardSettlement(f map[string]string, observedAt time.Time) (tender.SettlementLine, error) {
	account, batch, part, section, err := cardReportIdentity(f, observedAt)
	if err != nil {
		return tender.SettlementLine{}, err
	}
	var kind tender.SettlementEntryKind
	switch f["Type"] {
	case "Settled":
		kind = tender.SettlementCaptured
	case "Refunded":
		kind = tender.SettlementRefunded
	case "Chargeback":
		kind = tender.SettlementReturned
	default:
		return tender.SettlementLine{}, tender.ErrUnsupported
	}
	if f["Payment Method"] != "visa" && f["Payment Method"] != "mc" {
		return tender.SettlementLine{}, tender.ErrUnsupported
	}
	if f["Gross Currency"] != "USD" || f["Net Currency"] != "USD" {
		return tender.SettlementLine{}, tender.ErrUnsupported
	}
	original, modification := f["Psp Reference"], f["Modification Reference"]
	if !reportPSP.MatchString(original) || !reportPSP.MatchString(modification) || original == modification || len(f["Merchant Reference"]) > 80 || !reportOpaque.MatchString(f["Merchant Reference"]) {
		return tender.SettlementLine{}, tender.ErrValidation
	}
	gross, err := cardDirection(f, "Gross Credit (GC)", "Gross Debit (GC)")
	if err != nil {
		return tender.SettlementLine{}, err
	}
	net, err := cardDirection(f, "Net Credit (NC)", "Net Debit (NC)")
	if err != nil {
		return tender.SettlementLine{}, err
	}
	if kind == tender.SettlementCaptured && gross <= 0 || kind != tender.SettlementCaptured && gross >= 0 {
		return tender.SettlementLine{}, tender.ErrValidation
	}
	fee, err := cardFee(f, gross, net)
	if err != nil {
		return tender.SettlementLine{}, err
	}
	return tender.SettlementLine{
		ID:                cardRowID("adyen-card-", account, batch, part, section, f["Type"], f["Payment Method"], original, modification, f["Merchant Reference"], fmt.Sprint(gross), fmt.Sprint(net)),
		ProviderReference: original, OriginalReference: original, ModificationReference: modification,
		BatchID: batch, SourcePartID: part, SourceSectionID: section, Kind: kind,
		Currency: "USD", GrossMinor: gross, FeeMinor: fee, NetMinor: net, ObservedAt: observedAt.UTC(),
	}, nil
}

// ProjectPayout converts a MerchantPayout debit from Adyen's payable batch
// into Tender's merchant-positive bank payout convention. It does not by
// itself prove that the bank received the transfer.
func ProjectPayout(f map[string]string, observedAt time.Time) (tender.PayoutLine, error) {
	account, batch, part, section, err := cardReportIdentity(f, observedAt)
	if err != nil {
		return tender.PayoutLine{}, err
	}
	if f["Type"] != "MerchantPayout" {
		return tender.PayoutLine{}, tender.ErrUnsupported
	}
	if f["Net Currency"] != "USD" || f["Net Credit (NC)"] != "" || f["Gross Credit (GC)"] != "" || f["Gross Debit (GC)"] != "" {
		return tender.PayoutLine{}, tender.ErrValidation
	}
	description := f["Modification Reference"]
	matches := payoutDescription.FindStringSubmatch(description)
	if len(description) > 256 || len(matches) != 4 || matches[2] != batch {
		return tender.PayoutLine{}, tender.ErrValidation
	}
	base := matches[1] + " batch " + batch
	accountDescription := base + ", " + account
	if description != accountDescription && description != base+" "+account && description != accountDescription+" batch "+batch+", "+account {
		return tender.PayoutLine{}, tender.ErrValidation
	}
	amount, err := cardMinor(f["Net Debit (NC)"], false)
	if err != nil {
		return tender.PayoutLine{}, err
	}
	return tender.PayoutLine{ID: cardRowID("adyen-payout-", account, batch, part, section, matches[1], fmt.Sprint(amount)), BatchID: batch, SourcePartID: part, SourceSectionID: section, Currency: "USD", PayoutReference: matches[1], AmountMinor: amount, ObservedAt: observedAt.UTC()}, nil
}

// ProjectAdjustment maps Adyen's separately booked monthly transaction Fee
// journal as a negative payout-batch adjustment. Reserve and deposit journals
// can point in either direction and remain unsupported until qualified against
// an authenticated merchant report.
func ProjectAdjustment(f map[string]string, observedAt time.Time) (tender.PayoutAdjustment, error) {
	account, batch, part, section, err := cardReportIdentity(f, observedAt)
	if err != nil {
		return tender.PayoutAdjustment{}, err
	}
	if f["Type"] != "Fee" {
		return tender.PayoutAdjustment{}, tender.ErrUnsupported
	}
	if f["Net Currency"] != "USD" || f["Net Credit (NC)"] != "" || f["Gross Credit (GC)"] != "" || f["Gross Debit (GC)"] != "" ||
		f["Psp Reference"] != "" || f["Merchant Reference"] != "" || f["Payment Method"] != "" || len(f["Modification Reference"]) > 256 || !strings.HasPrefix(f["Modification Reference"], "Transaction Fees ") {
		return tender.PayoutAdjustment{}, tender.ErrValidation
	}
	amount, err := cardMinor(f["Net Debit (NC)"], false)
	if err != nil {
		return tender.PayoutAdjustment{}, err
	}
	return tender.PayoutAdjustment{
		ID:      cardRowID("adyen-fee-", account, batch, part, section, f["Modification Reference"], fmt.Sprint(amount)),
		BatchID: batch, SourcePartID: part, SourceSectionID: section, Currency: "USD", Type: "Fee", SignedMinor: -amount,
	}, nil
}
