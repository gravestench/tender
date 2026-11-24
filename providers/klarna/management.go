package klarna

import (
	"context"
	"errors"
	"net/http"

	"github.com/gravestench/tender"
)

func modificationReference(r tender.ProviderRequest) string {
	return digest("klarna-modification-v1", binding(r.Payment), r.Operation.ID, r.Operation.ProviderKey, string(r.Operation.Kind))
}
func (a *Adapter) modify(ctx context.Context, r tender.ProviderRequest, o tender.Observation) (tender.Observation, error) {
	p, op := r.Payment, r.Operation
	if !safeID.MatchString(p.ProviderReference) {
		return o, tender.ErrUnsupported
	}
	if r.Token != "" || p.VerifiedMethod != tender.MethodKlarna {
		return o, tender.ErrValidation
	}
	var route, header string
	var body any
	expected := http.StatusCreated
	switch op.Kind {
	case tender.OpCapture:
		if (p.Status != tender.PaymentAuthorized && p.Status != tender.PaymentPartiallyCaptured) || op.Amount.Minor > p.Funds.Authorized-p.Funds.Captured {
			return o, tender.ErrValidation
		}
		route, header = "captures", "Capture-Id"
		body = map[string]any{"captured_amount": op.Amount.Minor, "reference": modificationReference(r)}
	case tender.OpRefund:
		if op.Amount.Minor > p.Funds.Captured-p.Funds.Refunded {
			return o, tender.ErrValidation
		}
		route, header = "refunds", "Refund-Id"
		body = map[string]any{"refunded_amount": op.Amount.Minor, "reference": modificationReference(r)}
	case tender.OpVoid:
		if (p.Status != tender.PaymentAuthorized && p.Status != tender.PaymentPartiallyCaptured) || op.Amount.Minor != p.Funds.Authorized-p.Funds.Captured {
			return o, tender.ErrValidation
		}
		route = "cancel"
		if p.Funds.Captured > 0 {
			route = "release-remaining-authorization"
		}
		expected = http.StatusNoContent
	}
	path := "/ordermanagement/v1/orders/" + p.ProviderReference + "/" + route
	h, err := a.call(ctx, "POST", path, op.ProviderKey, body, expected, nil)
	if err != nil {
		return o, err
	}
	if header != "" {
		if len(h.Values(header)) != 1 || !safeID.MatchString(h.Get(header)) {
			return o, tender.ErrUnknownOutcome
		}
		id := h.Get(header)
		// Location is evidence only. Never follow it or forward credentials.
		if locations := h.Values("Location"); len(locations) > 0 && (len(locations) != 1 || locations[0] != a.cfg.BaseURL+path+"/"+id && locations[0] != path+"/"+id) {
			return o, tender.ErrUnknownOutcome
		}
		o.OperationReference = id
	}
	return modificationOutcome(r, o), nil
}
func modificationOutcome(r tender.ProviderRequest, o tender.Observation) tender.Observation {
	switch r.Operation.Kind {
	case tender.OpCapture:
		o.Snapshot.Funds.Captured += r.Operation.Amount.Minor
		o.Snapshot.Status = tender.PaymentPartiallyCaptured
		if o.Snapshot.Funds.Captured == o.Snapshot.Funds.Authorized {
			o.Snapshot.Status = tender.PaymentCaptured
		}
		o.Snapshot.ProviderStatus = "Captured"
	case tender.OpRefund:
		o.Snapshot.Funds.Refunded += r.Operation.Amount.Minor
		o.Snapshot.ProviderStatus = "Refunded"
	case tender.OpVoid:
		o.Snapshot.Status = tender.PaymentVoided
		o.Snapshot.ProviderStatus = "AuthorizationReleased"
	}
	return o
}

// IsUSFinancing checks the authenticated Order Management market and initial
// payment method against this adapter's pinned pay-over-time product.
func IsUSFinancing(country, currency, method string) bool {
	return country == "US" && currency == "USD" && financingMethod(method)
}

type modificationResource struct {
	CaptureID string `json:"capture_id"`
	RefundID  string `json:"refund_id"`
	Reference string `json:"reference"`
	Captured  int64  `json:"captured_amount"`
	Refunded  int64  `json:"refunded_amount"`
}

func (m modificationResource) identity(kind tender.OperationKind) (string, int64) {
	if kind == tender.OpRefund {
		return m.RefundID, m.Refunded
	}
	return m.CaptureID, m.Captured
}

type orderDetails struct {
	OrderID     string `json:"order_id"`
	MerchantRef string `json:"merchant_reference1"`
	Binding     string `json:"merchant_reference2"`
	Country     string `json:"purchase_country"`
	Initial     struct {
		Type string `json:"type"`
	} `json:"initial_payment_method"`
	Currency  string                 `json:"purchase_currency"`
	Original  *int64                 `json:"original_order_amount"`
	Amount    *int64                 `json:"order_amount"`
	Captured  *int64                 `json:"captured_amount"`
	Refunded  *int64                 `json:"refunded_amount"`
	Remaining *int64                 `json:"remaining_authorized_amount"`
	Fraud     string                 `json:"fraud_status"`
	Status    string                 `json:"status"`
	Captures  []modificationResource `json:"captures"`
	Refunds   []modificationResource `json:"refunds"`
}

// Query reads a bound order. A capture or refund requires its exact merchant
// operation reference as well as the expected cumulative totals. A missing
// acknowledgment for a release cannot be attributed to this operation by GET.
func (a *Adapter) Query(ctx context.Context, r tender.ProviderRequest) (tender.Observation, error) {
	o, err := a.observation(r)
	if err != nil {
		return o, err
	}
	p, op := r.Payment, r.Operation
	if !safeID.MatchString(p.ProviderReference) || op.Kind == tender.OpVoid || op.Kind == tender.OpCreate {
		return o, tender.ErrUnknownOutcome
	}
	if op.Status != tender.OperationUnknown && op.Status != tender.OperationReserved && op.Status != tender.OperationPending {
		return o, tender.ErrUnsupported
	}
	var data orderDetails
	if _, err := a.call(ctx, "GET", "/ordermanagement/v1/orders/"+p.ProviderReference, "", nil, 200, &data); err != nil {
		return o, err
	}
	if data.OrderID != p.ProviderReference || data.MerchantRef != p.OrderID || data.Binding != binding(p) || !IsUSFinancing(data.Country, data.Currency, data.Initial.Type) || data.Original == nil || *data.Original != p.Amount.Minor || data.Amount == nil || data.Captured == nil || data.Refunded == nil || data.Remaining == nil {
		return o, tender.ErrUnknownOutcome
	}
	if op.Kind == tender.OpConfirm {
		if op.ProviderReference != p.ProviderReference || p.VerifiedMethod != tender.MethodKlarna || *data.Captured != 0 || *data.Refunded != 0 || *data.Amount != p.Amount.Minor {
			return o, tender.ErrUnknownOutcome
		}
		switch data.Fraud {
		case "PENDING", "ACCEPTED":
			if data.Status != "AUTHORIZED" || *data.Remaining != p.Amount.Minor {
				return o, tender.ErrUnknownOutcome
			}
		case "REJECTED":
			if data.Status != "CANCELLED" || *data.Remaining != 0 {
				return o, tender.ErrUnknownOutcome
			}
		default:
			return o, tender.ErrUnknownOutcome
		}
		o.OperationReference = p.ProviderReference
		return orderDecision(o, data.Fraud, p.Amount.Minor)
	}
	if data.Fraud != "ACCEPTED" {
		return o, tender.ErrUnknownOutcome
	}
	expect := modificationOutcome(r, o)
	if *data.Captured != expect.Snapshot.Funds.Captured || *data.Refunded != expect.Snapshot.Funds.Refunded {
		return o, tender.ErrUnknownOutcome
	}
	remaining := p.Amount.Minor - *data.Captured
	if p.Status == tender.PaymentVoided {
		remaining = 0
	}
	expectedStatus := "PART_CAPTURED"
	if remaining == 0 {
		expectedStatus = "CAPTURED"
	}
	amountMatches := *data.Amount == p.Amount.Minor || p.Status == tender.PaymentVoided && *data.Amount == p.Funds.Captured
	if !amountMatches || data.Status != expectedStatus || *data.Remaining != remaining {
		return o, tender.ErrUnknownOutcome
	}
	items, route := data.Captures, "captures"
	switch op.Kind {
	case tender.OpRefund:
		items, route = data.Refunds, "refunds"
	case tender.OpCapture:
	default:
		return o, tender.ErrUnsupported
	}
	ref := modificationReference(r)
	var id string
	for _, item := range items {
		if item.Reference != ref {
			continue
		}
		candidate, amount := item.identity(op.Kind)
		if id != "" || !safeID.MatchString(candidate) || amount != op.Amount.Minor {
			return o, tender.ErrUnknownOutcome
		}
		id = candidate
	}
	if id == "" {
		return o, tender.ErrUnknownOutcome
	}
	// The order's embedded list locates the resource. The resource's own
	// authenticated detail must independently agree with this reservation.
	var detail modificationResource
	if _, err := a.call(ctx, "GET", "/ordermanagement/v1/orders/"+p.ProviderReference+"/"+route+"/"+id, "", nil, 200, &detail); err != nil {
		return o, errors.Join(tender.ErrUnknownOutcome, err)
	}
	actual, amount := detail.identity(op.Kind)
	if actual != id || detail.Reference != ref || amount != op.Amount.Minor {
		return o, tender.ErrUnknownOutcome
	}
	expect.OperationReference = id
	return expect, nil
}
