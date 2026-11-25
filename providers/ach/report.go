// Package ach maps trusted Adyen US/USD ACH settlement detail rows.
package ach

import (
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strconv"
	"strings"

	"github.com/gravestench/tender"
	"github.com/gravestench/tender/providers/adyen"
)

// SettlementLine projects the financial columns of one domestic ACH report
// row. Gross and Net are signed USD cents. Modification and journal type
// distinguish rows that share the original payment PSP.
type SettlementLine struct {
	Account, PSP, MerchantReference, Modification, Type, Batch string
	Gross, Net                                                 int64
}

var identifier = regexp.MustCompile(`^[A-Za-z0-9_-]{1,80}$`)
var merchantRef = regexp.MustCompile(`^[A-Za-z0-9_-]{1,57}\.[A-Za-z0-9_-]{22}$`)
var decimalUSD = regexp.MustCompile(`^[0-9]+(?:\.[0-9]{1,2})?$`)

func cents(s string) (int64, error) {
	if !decimalUSD.MatchString(s) {
		return 0, tender.ErrValidation
	}
	n, f, _ := strings.Cut(s, ".")
	f = (f + "00")[:2]
	v, err := strconv.ParseInt(n+f, 10, 64)
	if err != nil {
		return 0, tender.ErrValidation
	}
	return v, nil
}
func direction(credit, debit string) (int64, error) {
	if credit != "" && debit != "" || credit == "" && debit == "" {
		return 0, tender.ErrValidation
	}
	if debit != "" {
		v, err := cents(debit)
		return -v, err
	}
	return cents(credit)
}
func (l SettlementLine) valid() bool {
	return identifier.MatchString(l.Account) && identifier.MatchString(l.PSP) && merchantRef.MatchString(l.MerchantReference) && identifier.MatchString(l.Modification) && identifier.MatchString(l.Batch) &&
		((l.Type == "Settled" && l.Gross > 0 && l.Net >= 0 && l.Net <= l.Gross) || (l.Type == "Chargeback" && l.Gross < 0 && l.Net <= 0))
}

// MapSettlement accepts an already parsed record keyed by its CSV headers.
// Additional columns (which can contain shopper data) are never retained.
func MapSettlement(fields map[string]string) (SettlementLine, error) {
	l := SettlementLine{Account: fields["Merchant Account"], PSP: fields["Psp Reference"], MerchantReference: fields["Merchant Reference"], Modification: fields["Modification Reference"], Type: fields["Type"], Batch: fields["Batch Number"]}
	if fields["Payment Method"] != "ach" || fields["Gross Currency"] != "USD" || fields["Net Currency"] != "USD" || l.Type != "Settled" && l.Type != "Chargeback" {
		return SettlementLine{}, tender.ErrUnsupported
	}
	var err error
	l.Gross, err = direction(fields["Gross Credit (GC)"], fields["Gross Debit (GC)"])
	if err != nil {
		return SettlementLine{}, err
	}
	l.Net, err = direction(fields["Net Credit (NC)"], fields["Net Debit (NC)"])
	if err != nil || !l.valid() {
		return SettlementLine{}, tender.ErrValidation
	}
	return l, nil
}

// ReadSettlement reads one projected CSV record and its header, such as a
// row extracted by an authenticated report-ingestion worker.
func ReadSettlement(r io.Reader) (SettlementLine, error) {
	lr := &io.LimitedReader{R: r, N: 65537}
	cr := csv.NewReader(lr)
	header, err := cr.Read()
	if err != nil {
		return SettlementLine{}, tender.ErrValidation
	}
	row, err := cr.Read()
	if err != nil {
		return SettlementLine{}, tender.ErrValidation
	}
	if _, err = cr.Read(); err != io.EOF || lr.N == 0 {
		return SettlementLine{}, tender.ErrValidation
	}
	fields := make(map[string]string, len(row))
	for i, k := range header {
		if _, dup := fields[k]; dup {
			return SettlementLine{}, tender.ErrValidation
		}
		fields[k] = row[i]
	}
	return MapSettlement(fields)
}

type reportEvidence struct {
	ReportID string
	SettlementLine
}

// ApplySettlement accepts one row only from a host-authenticated Adyen report.
// Never expose this method as a shopper callback. Correlation, receipt and
// funds use the same transactional store as the Standard notification inbox.
func ApplySettlement(ctx context.Context, db tender.Store, engine *tender.Engine, scope tender.Scope, account, reportID string, l SettlementLine) error {
	if db == nil || engine == nil || scope.Validate() != nil || !l.valid() || l.Account != account || len(reportID) > 200 || reportID == "" {
		return tender.ErrValidation
	}
	evidence := reportEvidence{reportID, l}
	raw, _ := json.Marshal(evidence)
	// Include the full projected evidence so inconsistent rows remain distinct
	// and can be reviewed instead of overwriting an earlier receipt.
	hash := sha256.Sum256(raw)
	id := "ach-report." + hex.EncodeToString(hash[:])
	key := tender.VerifiedEventKey{Scope: scope, Provider: "adyen", MerchantAccount: account, ID: id}
	err := db.Transact(ctx, func(tx tender.Tx) error {
		inbox, ok := tx.(tender.VerifiedEventTx)
		if !ok {
			return tender.ErrUnsupported
		}
		return inbox.InsertVerifiedEvent(key, raw)
	})
	if err != nil && !errors.Is(err, tender.ErrConflict) {
		return err
	}
	return engine.ProcessVerifiedEventWithRecords(ctx, id, func(tx tender.EventRecords, raw json.RawMessage) (tender.VerifiedEvent, error) {
		var saved reportEvidence
		if json.Unmarshal(raw, &saved) != nil || saved != evidence {
			return tender.VerifiedEvent{}, tender.ErrConflict
		}
		p, op, err := adyen.ResolveReference(tx, scope, l.MerchantReference)
		if err != nil {
			return tender.VerifiedEvent{}, err
		}
		if p.Provider != "adyen" || p.MerchantAccount != account || p.Market != (tender.Market{Country: "US", Currency: "USD", Method: tender.MethodACH}) || p.Amount.Currency != "USD" || p.ProviderReference != "" && p.ProviderReference != l.PSP || op.Kind != tender.OpCreate || op.Status != tender.OperationSucceeded || op.AttemptID != p.AttemptID || op.Amount != p.Amount || p.Amount.Minor != l.Gross && -p.Amount.Minor != l.Gross {
			return tender.VerifiedEvent{}, tender.ErrConflict
		}
		if p.ProviderReference == "" {
			// A shopper can close the browser and the session can expire before
			// the auth worker runs. Bind a new PSP only with two independent
			// sources agreeing on the payment, not the report by itself.
			found, err := matchingReceipt(tx, p, l, "AUTHORISATION")
			if err != nil {
				return tender.VerifiedEvent{}, err
			}
			if !found {
				return tender.VerifiedEvent{}, tender.ErrConflict
			}
		}
		snap := p.Snapshot
		snap.Action = tender.CustomerAction{}
		switch l.Type {
		case "Settled":
			// A report worker can run before the failure worker. Refuse a
			// contradictory settlement until the host resolves the failed debit.
			failed, err := matchingReceipt(tx, p, l, "CAPTURE_FAILED")
			if err != nil {
				return tender.VerifiedEvent{}, err
			}
			if failed || p.Status == tender.PaymentCaptureFailed {
				return tender.VerifiedEvent{}, tender.ErrConflict
			}
			snap.Funds.Settled = p.Amount.Minor
			if snap.Funds.Returned == 0 {
				held, err := matchingReceipt(tx, p, l, "CHARGEBACK")
				if err != nil {
					return tender.VerifiedEvent{}, err
				}
				if held {
					confirmed, err := processedReturn(tx, p, l, "")
					if err != nil {
						return tender.VerifiedEvent{}, err
					}
					if !confirmed {
						return tender.VerifiedEvent{}, tender.ErrConflict
					}
					snap.Funds.Returned = p.Amount.Minor
				}
			}
		case "Chargeback":
			found, err := matchingReceipt(tx, p, l, "CHARGEBACK")
			if err != nil {
				return tender.VerifiedEvent{}, err
			}
			confirmed, err := processedReturn(tx, p, l, l.Modification)
			if err != nil {
				return tender.VerifiedEvent{}, err
			}
			if !found || !confirmed {
				return tender.VerifiedEvent{}, tender.ErrConflict
			}
			snap.Funds.Returned = p.Amount.Minor
		}
		snap.VerifiedMethod = tender.MethodACH
		if p.Status == tender.PaymentCaptureFailed {
			snap.Status = p.Status
			snap.ProviderStatus = p.ProviderStatus
		} else if snap.Funds.Returned > 0 {
			snap.Status = tender.PaymentReturned
			snap.ProviderStatus = "CHARGEBACK"
		} else {
			snap.Status = tender.PaymentSettled
			snap.ProviderStatus = "Settled"
		}
		return tender.VerifiedEvent{ID: id, Provider: "adyen", Observation: tender.Observation{Scope: scope, PaymentID: p.ID, AttemptID: p.AttemptID, OperationID: op.ID, ProviderReference: l.PSP, MerchantAccount: account, Amount: p.Amount, Status: tender.OperationSucceeded, Snapshot: snap}}, nil
	})
}

// A received HMAC-signed return fences fulfillment immediately, but its report
// cannot project returned funds until the same receipt has committed a durable
// review notice. The evidence reader omits unprocessed or forged notices.
func processedReturn(tx tender.EventRecords, p tender.Payment, l SettlementLine, modification string) (bool, error) {
	reader, ok := tx.(tender.AdverseEvidenceReader)
	if !ok {
		return false, tender.ErrUnsupported
	}
	notices, err := reader.AdverseEvidence(p.Scope, p.Provider, p.MerchantAccount, p.ID)
	if err != nil {
		return false, err
	}
	for _, n := range notices {
		if n.Scope == p.Scope && n.Provider == p.Provider && n.MerchantAccount == p.MerchantAccount && n.PaymentID == p.ID &&
			n.Kind == "returned" && n.OriginalReference == l.PSP && n.Amount == p.Amount &&
			n.ModificationReference != "" && n.ProviderReference == n.ModificationReference &&
			(modification == "" || n.ModificationReference == modification) {
			return true, nil
		}
	}
	return false, nil
}

// Signed receipts and report amounts must agree before applying funds. In
// particular, a received return must fence fulfillment while its worker waits.
func matchingReceipt(tx tender.EventRecords, p tender.Payment, l SettlementLine, code string) (bool, error) {
	reader, ok := tx.(tender.VerifiedEvidenceReader)
	if !ok {
		return false, tender.ErrUnsupported
	}
	fields := map[string]string{"eventCode": code, "merchantReference": l.MerchantReference, "success": "true"}
	if code == "CAPTURE_FAILED" {
		// CAPTURE_FAILED identifies a rejected modification. The signed
		// event code is sufficient to withhold collection pending review.
		delete(fields, "success")
	}
	if code == "CHARGEBACK" && l.Type == "Chargeback" {
		fields["pspReference"] = l.Modification
	}
	if code == "AUTHORISATION" {
		fields["pspReference"] = l.PSP
		fields["originalReference"] = ""
	} else {
		fields["originalReference"] = l.PSP
	}
	rows, err := reader.MatchingVerifiedEvidence(p.Scope, p.Provider, p.MerchantAccount, fields)
	if err != nil {
		return false, err
	}
	for _, row := range rows {
		var e struct {
			PspReference, MerchantAccountCode string
			Amount                            struct {
				Value    int64
				Currency string
			}
		}
		if json.Unmarshal(row, &e) != nil || e.MerchantAccountCode != p.MerchantAccount || e.PspReference == "" || code != "AUTHORISATION" && e.PspReference == l.PSP || e.Amount.Value != p.Amount.Minor || e.Amount.Currency != p.Amount.Currency {
			return false, tender.ErrConflict
		}
	}
	return len(rows) > 0, nil
}
