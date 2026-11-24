package paypal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/gravestench/tender"
	"net/http"
	"net/url"
	"time"
)

type amount struct {
	Currency string `json:"currency_code"`
	Value    string `json:"value"`
}

func wireMoney(m tender.Money) amount {
	return amount{m.Currency, fmt.Sprintf("%d.%02d", m.Minor/100, m.Minor%100)}
}

type payee struct {
	Merchant string `json:"merchant_id"`
}
type capture struct {
	ID, Status string
	Amount     amount
	Final      bool `json:"final_capture"`
}
type unit struct {
	Reference string `json:"reference_id"`
	Custom    string `json:"custom_id"`
	Amount    amount `json:"amount"`
	Payee     payee  `json:"payee"`
	Payments  *struct {
		Captures []capture `json:"captures"`
	} `json:"payments,omitempty"`
}
type order struct {
	ID, Intent, Status string
	Units              []unit                     `json:"purchase_units"`
	Sources            map[string]json.RawMessage `json:"payment_source"`
}
type refund struct {
	ID, Status string
	Amount     amount
	Custom     string `json:"custom_id"`
	Links      []struct{ Rel, Href, Method string }
}

func binding(p tender.Payment) string {
	raw, _ := json.Marshal([]any{p.Scope, p.Provider, p.MerchantAccount, p.ID, p.AttemptID, p.OrderID, p.Market, p.Amount})
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:])
}
func purchase(p tender.Payment) unit {
	return unit{Reference: binding(p), Custom: p.ID, Amount: wireMoney(p.Amount), Payee: payee{p.MerchantAccount}}
}
func (o order) matches(p tender.Payment) bool {
	if !idPattern.MatchString(o.ID) || o.ID != p.ProviderReference || o.Intent != "CAPTURE" || len(o.Units) != 1 {
		return false
	}
	u, want := o.Units[0], purchase(p)
	return u.Reference == want.Reference && u.Custom == want.Custom && u.Amount == want.Amount && u.Payee == want.Payee
}
func (o order) buyerID() string {
	var buyer struct {
		ID string `json:"account_id"`
	}
	if len(o.Sources) != 1 || json.Unmarshal(o.Sources["paypal"], &buyer) != nil || !idPattern.MatchString(buyer.ID) {
		return ""
	}
	return buyer.ID
}
func (o order) singleCapture(p tender.Payment) (capture, bool) {
	if !o.matches(p) || o.buyerID() == "" || o.Status != "COMPLETED" || o.Units[0].Payments == nil || len(o.Units[0].Payments.Captures) != 1 {
		return capture{}, false
	}
	c := o.Units[0].Payments.Captures[0]
	return c, idPattern.MatchString(c.ID) && c.Amount == wireMoney(p.Amount) && c.Final
}
func observation(r tender.ProviderRequest) tender.Observation {
	p := r.Payment
	return tender.Observation{Scope: p.Scope, PaymentID: p.ID, AttemptID: p.AttemptID, MerchantAccount: p.MerchantAccount, OperationID: r.Operation.ID, ProviderReference: p.ProviderReference, Amount: r.Operation.Amount, Status: tender.OperationSucceeded, Snapshot: p.Snapshot}
}
func (a *Adapter) validate(r tender.ProviderRequest) error {
	p, op := r.Payment, r.Operation
	if p.Scope != a.cfg.Scope || p.Provider != "paypal" || p.MerchantAccount != a.cfg.MerchantAccount || p.Market != market {
		return tender.ErrUnsupported
	}
	if !idPattern.MatchString(p.ID) || p.Amount.ValidatePurchase() != nil || p.Amount.Currency != "USD" || op.Scope != p.Scope || op.PaymentID != p.ID || op.AttemptID != p.AttemptID || p.AttemptID == "" || !idPattern.MatchString(op.ID) || !idPattern.MatchString(op.ProviderKey) || len(op.ProviderKey) > 38 || op.Amount.ValidatePurchase() != nil || op.Amount.Currency != "USD" || op.Amount.Minor > p.Amount.Minor || op.Kind != tender.OpRefund && op.Amount != p.Amount || r.Token != "" {
		return tender.ErrValidation
	}
	if op.Kind != tender.OpCreate && !idPattern.MatchString(p.ProviderReference) {
		return tender.ErrValidation
	}
	return nil
}
func (a *Adapter) getOrder(ctx context.Context, id string) (order, error) {
	var o order
	if !idPattern.MatchString(id) {
		return o, tender.ErrValidation
	}
	err := a.call(ctx, "GET", "/v2/checkout/orders/"+id, "", nil, &o)
	return o, err
}
func (a *Adapter) Execute(ctx context.Context, r tender.ProviderRequest) (tender.Observation, error) {
	var empty tender.Observation
	if !a.Capabilities().Supports(r.Operation.Kind) {
		return empty, tender.ErrUnsupported
	}
	if err := a.validate(r); err != nil {
		return empty, err
	}
	p := r.Payment
	switch r.Operation.Kind {
	case tender.OpCreate:
		if p.ProviderReference != "" {
			return empty, tender.ErrConflict
		}
		body, _ := json.Marshal(struct {
			Intent  string         `json:"intent"`
			Sources map[string]any `json:"payment_source"`
			Units   []unit         `json:"purchase_units"`
		}{"CAPTURE", map[string]any{"paypal": struct{}{}}, []unit{purchase(p)}})
		var receipt order
		if err := a.call(ctx, "POST", "/v2/checkout/orders", r.Operation.ProviderKey, body, &receipt); err != nil {
			return empty, err
		}
		if !idPattern.MatchString(receipt.ID) {
			return empty, tender.ErrUnknownOutcome
		}
		p.ProviderReference = receipt.ID
		o, err := a.getOrder(ctx, receipt.ID)
		if err != nil || !o.matches(p) || (o.Status != "CREATED" && o.Status != "PAYER_ACTION_REQUIRED") {
			return empty, tender.ErrUnknownOutcome
		}
		obs := observation(r)
		obs.ProviderReference = o.ID
		obs.Snapshot = tender.Snapshot{Status: tender.PaymentRequiresAction, ProviderStatus: o.Status, Action: tender.CustomerAction{Mode: tender.ActionComponent, Data: o.ID, ExpiresAt: a.cfg.Now().Add(10 * time.Minute)}}
		return obs, nil
	case tender.OpConfirm:
		o, err := a.getOrder(ctx, p.ProviderReference)
		if err != nil {
			return empty, beforeDispatch(err)
		}
		if !o.matches(p) {
			return empty, tender.ErrConflict
		}
		if o.Status != "APPROVED" || o.buyerID() == "" {
			return empty, tender.ErrValidation
		}
		if err := r.BindEvidence(buyerDigest(p, o.buyerID())); err != nil {
			return empty, err
		}
		var receipt order
		if err := a.call(ctx, "POST", "/v2/checkout/orders/"+o.ID+"/capture", r.Operation.ProviderKey, []byte("{}"), &receipt); err != nil {
			return empty, err
		}
		current, err := a.getOrder(ctx, p.ProviderReference)
		// PayPal's capture representation may omit the purchase-unit amount/payee;
		// the authenticated order GET supplies those fields. Both resources must
		// nevertheless identify the approved buyer, order, unit and capture.
		if err != nil || !receipt.correlates(o, current, p) {
			pending := observation(r)
			pending.Status = tender.OperationPending
			pending.Snapshot = tender.Snapshot{Status: tender.PaymentPending, ProviderStatus: evidenceConflict, ProviderEvidenceDigest: buyerDigest(p, o.buyerID())}
			return pending, nil
		}
		return captured(r, current)
	case tender.OpRefund:
		if p.VerifiedMethod != tender.MethodPayPal || r.Operation.Amount.Minor > p.Funds.Captured-p.Funds.Refunded {
			return empty, tender.ErrValidation
		}
		o, err := a.getOrder(ctx, p.ProviderReference)
		if err != nil {
			return empty, beforeDispatch(err)
		}
		c, ok := o.singleCapture(p)
		if !ok || (c.Status != "COMPLETED" && c.Status != "PARTIALLY_REFUNDED") {
			return empty, tender.ErrConflict
		}
		body, _ := json.Marshal(struct {
			Amount amount `json:"amount"`
			Custom string `json:"custom_id"`
		}{wireMoney(r.Operation.Amount), r.Operation.ID})
		var reply refund
		if err := a.call(ctx, "POST", "/v2/payments/captures/"+c.ID+"/refund", r.Operation.ProviderKey, body, &reply); err != nil {
			return empty, err
		}
		return a.refundResult(r, c.ID, reply)
	default:
		return empty, tender.ErrUnsupported
	}
}

const evidenceConflict = "CaptureEvidenceConflict"

func buyerDigest(p tender.Payment, buyer string) [32]byte {
	raw, _ := json.Marshal([]string{binding(p), buyer})
	return sha256.Sum256(raw)
}
func (receipt order) correlates(approved, current order, p tender.Payment) bool {
	cap, ok := current.singleCapture(p)
	if !ok || receipt.ID != approved.ID || current.ID != approved.ID || current.buyerID() != approved.buyerID() || receipt.buyerID() != approved.buyerID() || len(receipt.Units) != 1 {
		return false
	}
	u := receipt.Units[0]
	if u.Reference != approved.Units[0].Reference || u.Custom != "" && u.Custom != p.ID || u.Payee.Merchant != "" && u.Payee.Merchant != p.MerchantAccount || u.Amount != (amount{}) && u.Amount != wireMoney(p.Amount) || u.Payments == nil || len(u.Payments.Captures) != 1 {
		return false
	}
	c := u.Payments.Captures[0]
	return c.ID == cap.ID && c.Amount == cap.Amount && c.Final && receipt.Status == "COMPLETED" && (c.Status == cap.Status || c.Status == "PENDING")
}
func captured(r tender.ProviderRequest, o order) (tender.Observation, error) {
	obs := observation(r)
	c, ok := o.singleCapture(r.Payment)
	digest := buyerDigest(r.Payment, o.buyerID())
	if !ok || r.Operation.ProviderReference != "" && r.Operation.ProviderReference != c.ID || r.Operation.ProviderEvidenceDigest != ([32]byte{}) && r.Operation.ProviderEvidenceDigest != digest || r.Payment.ProviderEvidenceDigest != ([32]byte{}) && r.Payment.ProviderEvidenceDigest != digest {
		return tender.Observation{}, tender.ErrUnknownOutcome
	}
	obs.OperationReference = c.ID
	switch c.Status {
	case "PENDING":
		obs.Status = tender.OperationPending
		obs.Snapshot = tender.Snapshot{Status: tender.PaymentPending, ProviderStatus: "PENDING", VerifiedMethod: tender.MethodPayPal, ProviderEvidenceDigest: buyerDigest(r.Payment, o.buyerID())}
	case "COMPLETED":
		obs.Snapshot.Action = tender.CustomerAction{}
		obs.Snapshot.ProviderEvidenceDigest = [32]byte{}
		obs.Snapshot.Status = tender.PaymentCaptured
		obs.Snapshot.ProviderStatus = "COMPLETED"
		obs.Snapshot.VerifiedMethod = tender.MethodPayPal
		obs.Snapshot.Funds.Authorized = r.Payment.Amount.Minor
		obs.Snapshot.Funds.Captured = r.Payment.Amount.Minor
	case "DECLINED", "FAILED":
		obs.Status = tender.OperationFailed
		obs.Snapshot = tender.Snapshot{Status: tender.PaymentDeclined, ProviderStatus: c.Status}
	default:
		return tender.Observation{}, tender.ErrUnknownOutcome
	}
	return obs, nil
}
func (a *Adapter) Query(ctx context.Context, r tender.ProviderRequest) (tender.Observation, error) {
	var empty tender.Observation
	if err := a.validate(r); err != nil {
		return empty, err
	}
	// A reserved operation can still have a live finisher comparing the POST
	// with the GET, even after its deadline. Only a signed event corroborates
	// an abandoned reservation. A completed finisher records unknown/pending.
	if r.Operation.Kind == tender.OpConfirm && (r.Operation.Status == tender.OperationReserved || r.Payment.ProviderStatus == evidenceConflict || r.Operation.ProviderEvidenceDigest == ([32]byte{}) && r.Payment.ProviderEvidenceDigest == ([32]byte{}) && r.Operation.ProviderReference == "") {
		return empty, tender.ErrUnknownOutcome
	}
	if r.Operation.Kind != tender.OpConfirm && (r.Operation.Kind != tender.OpRefund || !idPattern.MatchString(r.Operation.ProviderReference)) {
		return empty, tender.ErrUnsupported
	}
	o, err := a.getOrder(ctx, r.Payment.ProviderReference)
	if err != nil {
		return empty, err
	}
	if r.Operation.Kind == tender.OpConfirm {
		return captured(r, o)
	}
	c, ok := o.singleCapture(r.Payment)
	if !ok {
		return empty, tender.ErrUnknownOutcome
	}
	var reply refund
	if err := a.call(ctx, http.MethodGet, "/v2/payments/refunds/"+r.Operation.ProviderReference, "", nil, &reply); err != nil {
		return empty, err
	}
	if reply.ID != r.Operation.ProviderReference {
		return empty, tender.ErrUnknownOutcome
	}
	return a.refundResult(r, c.ID, reply)
}
func (a *Adapter) refundResult(r tender.ProviderRequest, capID string, f refund) (tender.Observation, error) {
	uri := "/v2/payments/captures/" + capID
	up := 0
	for _, l := range f.Links {
		if l.Rel == "up" {
			if l.Method != "GET" || !a.resourceURL(l.Href, uri) {
				return tender.Observation{}, tender.ErrUnknownOutcome
			}
			up++
		}
	}
	if up != 1 || !idPattern.MatchString(f.ID) || f.Amount != wireMoney(r.Operation.Amount) || f.Custom != r.Operation.ID {
		return tender.Observation{}, tender.ErrUnknownOutcome
	}
	obs := observation(r)
	obs.OperationReference = f.ID
	switch f.Status {
	case "COMPLETED":
		if r.Operation.Status != tender.OperationSucceeded {
			obs.Snapshot.Funds.Refunded += r.Operation.Amount.Minor
		}
	case "PENDING":
		obs.Status = tender.OperationPending
	case "FAILED", "CANCELLED":
		obs.Status = tender.OperationFailed
	default:
		return tender.Observation{}, tender.ErrUnknownOutcome
	}
	return obs, nil
}
func (a *Adapter) resourceURL(raw, path string) bool {
	if raw == a.cfg.BaseURL+path {
		return true
	}
	if a.cfg.Scope.Environment == tender.EnvironmentMock {
		return false
	}
	u, err := url.Parse(a.cfg.BaseURL)
	return err == nil && raw == "https://api."+u.Host[len("api-m."):]+path
}
