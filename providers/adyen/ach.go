package adyen

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"time"

	"github.com/gravestench/tender"
)

// ACH implements the US consumer debit candidate using Checkout v72 Sessions.
// Strict first-use collection is qualified only against our loopback fixture.
type ACH struct{ api *Adapter }

func NewACH(c Config) (*ACH, error) {
	api, err := New(c)
	if err != nil {
		return nil, err
	}
	return &ACH{api: api}, nil
}

var usDebit = tender.Market{Country: "US", Currency: "USD", Method: tender.MethodACH}
var fixtureResult = regexp.MustCompile(`^mr_[a-f0-9]{64}$`)

func (a *ACH) Capabilities() tender.Capabilities {
	if a.api.cfg.Environment != tender.EnvironmentMock {
		return tender.Capabilities{}
	}
	return tender.Capabilities{Create: true, Confirm: true, Markets: []tender.MarketCapability{{Market: usDebit}}, ActionModes: []tender.ActionMode{tender.ActionComponent}}
}

func (a *ACH) CheckStore(_ tender.OperationKind, tx tender.Tx) error {
	if _, ok := tx.(interface {
		tender.VerifiedEventTx
		tender.VerifiedEvidenceReader
	}); !ok {
		return tender.ErrUnsupported
	}
	return nil
}

// ACHSession carries synthetic component data and the echoed host binding.
// The merchant page uses only ID and SessionData for browser handoff.
type ACHSession struct {
	ID          string `json:"id"`
	SessionData string `json:"sessionData"`
	Reference   string `json:"reference"`
}

func (a *ACH) CheckoutSession(p tender.Payment) (ACHSession, error) {
	if p.ProviderReference != "" {
		// A signed AUTHORISATION can arrive after the browser closed. The
		// outstanding browser receipt can still be verified, but never remount consent.
		return ACHSession{}, tender.ErrUnsupported
	}
	return a.session(p)
}

func (a *ACH) session(p tender.Payment) (ACHSession, error) {
	var s ACHSession
	c := a.api.cfg
	if c.Environment != tender.EnvironmentMock || p.Scope.Environment != c.Environment || p.Market != usDebit || p.Provider != "adyen" || p.MerchantAccount != c.MerchantAccount || p.Status != tender.PaymentRequiresAction || p.Action.Mode != tender.ActionComponent || p.Action.Validate(c.Now()) != nil {
		return s, tender.ErrUnsupported
	}
	if json.Unmarshal([]byte(p.Action.Data), &s) != nil || !reference.MatchString(s.ID) || s.SessionData == "" || s.Reference == "" {
		return ACHSession{}, tender.ErrValidation
	}
	return s, nil
}
func (a *ACH) Execute(ctx context.Context, r tender.ProviderRequest) (tender.Observation, error) {
	p, op := r.Payment, r.Operation
	o := tender.Observation{Scope: p.Scope, MerchantAccount: p.MerchantAccount, PaymentID: p.ID, AttemptID: p.AttemptID, OperationID: op.ID, Amount: op.Amount, Status: tender.OperationSucceeded}
	if !a.Capabilities().SupportsIn(op.Kind, p.Market, false) {
		return o, tender.ErrUnsupported
	}
	if p.Scope.Validate() != nil || p.Scope.Environment != a.api.cfg.Environment || p.MerchantAccount != a.api.cfg.MerchantAccount || p.Provider != "adyen" || p.ID == "" || p.AttemptID == "" || op.Scope != p.Scope || op.AttemptID != p.AttemptID || op.PaymentID != p.ID || op.Amount != p.Amount || p.Amount.ValidatePurchase() != nil || p.Amount.Currency != "USD" || !reference.MatchString(op.ID) || len(op.ID) > 57 || !reference.MatchString(op.ProviderKey) || len(op.ProviderKey) > 64 {
		return o, tender.ErrValidation
	}
	if op.Kind == tender.OpConfirm {
		return a.result(ctx, r, o)
	}
	if r.Token != "" || p.Status != tender.PaymentCreated {
		return o, tender.ErrValidation
	}
	ref := merchantReference(p, op)
	req := map[string]any{"merchantAccount": p.MerchantAccount, "reference": ref, "amount": money(p.Amount), "countryCode": "US", "channel": "Web", "shopperInteraction": "Ecommerce", "returnUrl": a.api.cfg.ReturnURL, "allowedPaymentMethods": []string{"ach"}, "mode": "embedded"}
	var reply struct {
		ACHSession
		MerchantAccount, Mode, URL string
		Amount                     amount
		ExpiresAt                  time.Time
	}
	if err := a.api.post(ctx, "/sessions", op.ProviderKey, req, &reply); err != nil {
		return o, err
	}
	if reply.Reference != ref || reply.MerchantAccount != p.MerchantAccount || reply.Amount != money(p.Amount) || !reference.MatchString(reply.ID) || reply.SessionData == "" || len(reply.SessionData) > 65536 || reply.URL != "" || reply.Mode != "embedded" || !reply.ExpiresAt.After(a.api.cfg.Now()) {
		return o, tender.ErrUnknownOutcome
	}
	raw, _ := json.Marshal(reply.ACHSession)
	o.OperationReference = reply.ID
	o.Snapshot = tender.Snapshot{Status: tender.PaymentRequiresAction, ProviderStatus: "SessionCreated", Action: tender.CustomerAction{Mode: tender.ActionComponent, Data: string(raw), ExpiresAt: reply.ExpiresAt}}
	return o, nil
}

func (a *ACH) result(ctx context.Context, r tender.ProviderRequest, o tender.Observation) (tender.Observation, error) {
	session, err := a.session(r.Payment)
	if err != nil {
		return o, err
	}
	// The mock receipt grammar admits only tokens issued by the fixture. A live
	// receipt format and destination need verification before the gate can open.
	if !fixtureResult.MatchString(r.Token) {
		return o, tender.ErrValidation
	}
	u := a.api.cfg.BaseURL + "/sessions/" + session.ID + "?" + url.Values{"sessionResult": {r.Token}}.Encode()
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return o, tender.ErrValidation
	}
	req.Header.Set("X-API-Key", a.api.cfg.APIKey)
	res, err := a.api.client.Do(req)
	if err != nil {
		return o, tender.ErrUnknownOutcome
	}
	defer res.Body.Close()
	var reply struct {
		ID, Reference, Status string
		Payments              []struct {
			PspReference, ResultCode string
			Amount                   amount
			PaymentMethod            struct{ Type string }
		}
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, 65537))
	if err != nil || res.StatusCode != http.StatusOK || len(raw) > 65536 || json.Unmarshal(raw, &reply) != nil || reply.ID != session.ID || reply.Reference != session.Reference {
		return o, tender.ErrUnknownOutcome
	}
	o.OperationReference = session.Reference
	switch reply.Status {
	case "completed", "paymentPending":
		if len(reply.Payments) > 1 || reply.Status == "completed" && len(reply.Payments) != 1 {
			return o, tender.ErrUnknownOutcome
		}
		if len(reply.Payments) == 1 {
			pay := reply.Payments[0]
			if !reference.MatchString(pay.PspReference) || pay.Amount != money(r.Payment.Amount) || pay.PaymentMethod.Type != "" && pay.PaymentMethod.Type != "ach" {
				return o, tender.ErrUnknownOutcome
			}
			switch pay.ResultCode {
			case "Authorised":
				if pay.PaymentMethod.Type != "ach" {
					return o, tender.ErrUnknownOutcome
				}
				o.Snapshot.VerifiedMethod = tender.MethodACH
			case "Received", "Pending":
				if reply.Status != "paymentPending" {
					return o, tender.ErrUnknownOutcome
				}
			default:
				return o, tender.ErrUnknownOutcome
			}
			o.ProviderReference = pay.PspReference
		}
		o.Snapshot.Status = tender.PaymentPending
	case "refused", "canceled", "expired":
		if len(reply.Payments) != 0 || r.Payment.ProviderReference != "" {
			return o, tender.ErrUnknownOutcome
		}
		o.Status = tender.OperationFailed
		o.Snapshot.Status = tender.PaymentDeclined
	default:
		return o, tender.ErrUnknownOutcome
	}
	o.Snapshot.ProviderStatus = reply.Status
	return o, nil
}

// Query never polls a static session result. Confirm exchanges the browser
// receipt once. Later authoritative updates come from webhooks and reports.
func (a *ACH) Query(context.Context, tender.ProviderRequest) (tender.Observation, error) {
	return tender.Observation{}, tender.ErrUnsupported
}
