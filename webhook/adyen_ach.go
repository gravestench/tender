package webhook

import (
	"encoding/json"
	"errors"

	"github.com/gravestench/tender"
)

// The signed reference identifies the ACH-only session creation. The session
// result exchange binds its own operation to that same merchant reference.
func achObservation(tx tender.EventRecords, p tender.Payment, create tender.OperationRecord, e evidence) (tender.Observation, error) {
	var o tender.Observation
	if e.Code != "AUTHORISATION" && e.Code != "CHARGEBACK" && e.Code != "CAPTURE_FAILED" {
		return o, tender.ErrUnsupported
	}
	m := tender.Money{Minor: *e.Amount.Value, Currency: e.Amount.Currency}
	if p.Market.Country != "US" || p.Market.Currency != "USD" || create.Kind != tender.OpCreate || p.Amount != m || create.Amount != m {
		return o, tender.ErrConflict
	}
	op, err := tx.OperationByReference(p.Scope, p.ID, e.Ref)
	if errors.Is(err, tender.ErrNotFound) {
		op = create
		// The read-only receipt exchange may be in flight or have lost its
		// HTTP reply. Only one confirmation can own this exact ACH session.
		active, lookupErr := tx.ActiveOperation(p.Scope, p.ID)
		if lookupErr == nil && active.ID != create.ID {
			if active.Kind != tender.OpConfirm || active.AttemptID != p.AttemptID || active.Amount != m || p.Status != tender.PaymentRequiresAction {
				return o, tender.ErrConflict
			}
			op = active
		} else if lookupErr != nil && !errors.Is(lookupErr, tender.ErrNotFound) {
			return o, lookupErr
		}
	} else if err != nil {
		return o, err
	} else if op.Kind != tender.OpConfirm || op.AttemptID != p.AttemptID || op.Amount != m {
		return o, tender.ErrConflict
	}
	o = tender.Observation{Scope: p.Scope, PaymentID: p.ID, AttemptID: p.AttemptID, OperationID: op.ID, MerchantAccount: p.MerchantAccount, Amount: m, Snapshot: p.Snapshot, Status: op.Status, ProviderReference: p.ProviderReference}
	switch e.Code {
	case "AUTHORISATION":
		if e.Original != "" || p.ProviderReference != "" && p.ProviderReference != e.PSP {
			return o, tender.ErrConflict
		}
		if e.Success == "false" {
			// A failed attempt can share a session with a later successful
			// payment. Acknowledge only a session already verified as refused.
			if p.Status != tender.PaymentDeclined || op.Kind != tender.OpConfirm {
				return o, tender.ErrConflict
			}
		} else if p.Status == tender.PaymentRequiresAction {
			// Bind the signed provisional PSP even if the browser disappeared.
			// Keep the action evidence so an in-flight receipt can be checked.
			if op.Kind == tender.OpConfirm {
				o.Status = tender.OperationSucceeded
				o.OperationReference = e.Ref
			}
		} else if p.Status != tender.PaymentPending && p.Status != tender.PaymentSettled && p.Status != tender.PaymentReturned && p.Status != tender.PaymentCaptureFailed {
			return o, tender.ErrConflict
		}
		o.ProviderReference = e.PSP
	case "CAPTURE_FAILED":
		// The signed capture PSP is distinct from both the original payment
		// and the session/receipt operation reference. Its explanatory reason
		// and bank-specific decline code are outside the Standard item HMAC.
		if e.Original == "" || e.Original == e.PSP || p.ProviderReference != "" && e.Original != p.ProviderReference {
			return o, tender.ErrConflict
		}
		if p.ProviderReference == "" {
			if err := checkACHFailureOriginal(tx, p, e); err != nil {
				return o, err
			}
		}
		o.ProviderReference = e.Original
		o.CaptureFailure = true
		o.AutomaticCaptureReference = e.PSP
		o.Snapshot.Status = tender.PaymentCaptureFailed
		o.Snapshot.ProviderStatus = "CAPTURE_FAILED"
		o.Snapshot.Action = tender.CustomerAction{}
		if op.Status != tender.OperationSucceeded {
			o.Status = tender.OperationFailed
		}
	case "CHARGEBACK":
		if e.Success != "true" || e.Original == "" || e.Original != p.ProviderReference || e.PSP == e.Original || op.Status != tender.OperationSucceeded {
			return o, tender.ErrConflict
		}
		o.Snapshot.Action = tender.CustomerAction{}
		o.Snapshot.Status = tender.PaymentReturned
		o.Snapshot.Funds.Returned = m.Minor
		o.Snapshot.ProviderStatus = "CHARGEBACK"
		if p.Status == tender.PaymentCaptureFailed {
			o.Snapshot.Status = p.Status
			o.Snapshot.ProviderStatus = p.ProviderStatus
		}
	}
	return o, nil
}

// A session may include multiple attempted debits. Before the host binds its
// PSP, a stored positive authorization takes precedence over a failed sibling.
func checkACHFailureOriginal(tx tender.EventRecords, p tender.Payment, failure evidence) error {
	reader, ok := tx.(tender.VerifiedEvidenceReader)
	if !ok {
		return tender.ErrUnsupported
	}
	rows, err := reader.MatchingVerifiedEvidence(p.Scope, p.Provider, p.MerchantAccount, map[string]string{"eventCode": "AUTHORISATION", "success": "true", "merchantReference": failure.Ref, "originalReference": ""})
	if err != nil {
		return err
	}
	for _, row := range rows {
		var auth evidence
		if json.Unmarshal(row, &auth) != nil || auth.Amount.Value == nil {
			return tender.ErrConflict
		}
		if auth.PSP == failure.Original && auth.Account == failure.Account && auth.Amount.Currency == failure.Amount.Currency && *auth.Amount.Value == *failure.Amount.Value {
			return nil
		}
	}
	if len(rows) > 0 {
		return tender.ErrConflict
	}
	// Even without an authorization, an authenticated failure naming this
	// session and its original PSP is sufficient to stop collection for review.
	return nil
}
