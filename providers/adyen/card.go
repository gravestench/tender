package adyen

import (
	"context"
	"encoding/json"
	"io"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/gravestench/tender"
)

// CardAction is the JSON carried in CustomerAction.Data. "card" initializes
// the card component. "threeDS2" carries an allowlisted native action.
// Reference is server-generated correlation data, never trusted from a browser.
type CardAction struct {
	Type      string        `json:"type"`
	Brands    []string      `json:"brands,omitempty"`
	Action    *NativeAction `json:"action,omitempty"`
	Reference string        `json:"reference,omitempty"`
}
type NativeAction struct {
	Type               string `json:"type"`
	Subtype            string `json:"subtype"`
	Token              string `json:"token"`
	PaymentMethodType  string `json:"paymentMethodType"`
	PaymentData        string `json:"paymentData,omitempty"`
	AuthorisationToken string `json:"authorisationToken,omitempty"`
}

func (a *Adapter) action(o tender.Observation, data CardAction) (tender.Observation, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return o, tender.ErrUnknownOutcome
	}
	o.Snapshot = tender.Snapshot{Status: tender.PaymentRequiresAction, Action: tender.CustomerAction{Mode: tender.ActionComponent, Data: string(raw), ExpiresAt: a.cfg.Now().UTC().Add(10 * time.Minute)}}
	return o, nil
}
func brand(method tender.PaymentMethod) string {
	if method == tender.MethodMastercard {
		return "mc"
	}
	return "visa"
}
func (a *Adapter) discover(ctx context.Context, r tender.ProviderRequest, o tender.Observation) (tender.Observation, error) {
	if r.Token != "" {
		return o, tender.ErrValidation
	}
	var reply struct {
		PaymentMethods []struct {
			Type   string
			Brands []string
		}
	}
	data := map[string]any{
		"merchantAccount": a.cfg.MerchantAccount, "amount": money(o.Amount), "countryCode": r.Payment.Market.Country,
		"channel": "Web", "allowedPaymentMethods": []string{"scheme"},
	}
	if err := a.post(ctx, "/paymentMethods", r.Operation.ProviderKey, data, &reply); err != nil {
		return o, err
	}
	for _, m := range reply.PaymentMethods {
		if b := brand(r.Payment.Market.Method); m.Type == "scheme" && slices.Contains(m.Brands, b) {
			return a.action(o, CardAction{Type: "card", Brands: []string{b}})
		}
	}
	return o, tender.ErrUnsupported
}

type cardMethod struct {
	Type                  string `json:"type"`
	Brand                 string `json:"brand,omitempty"`
	HolderName            string `json:"holderName,omitempty"`
	EncryptedCardNumber   string `json:"encryptedCardNumber"`
	EncryptedExpiryMonth  string `json:"encryptedExpiryMonth"`
	EncryptedExpiryYear   string `json:"encryptedExpiryYear"`
	EncryptedSecurityCode string `json:"encryptedSecurityCode"`
	CheckoutAttemptID     string `json:"checkoutAttemptId,omitempty"`
}
type browserInfo struct {
	AcceptHeader      string `json:"acceptHeader"`
	ColorDepth        int    `json:"colorDepth"`
	JavaEnabled       *bool  `json:"javaEnabled"`
	JavaScriptEnabled *bool  `json:"javaScriptEnabled,omitempty"`
	Language          string `json:"language"`
	ScreenHeight      int    `json:"screenHeight"`
	ScreenWidth       int    `json:"screenWidth"`
	TimeZoneOffset    *int   `json:"timeZoneOffset"`
	UserAgent         string `json:"userAgent"`
}
type cardInput struct {
	PaymentMethod cardMethod  `json:"paymentMethod"`
	BrowserInfo   browserInfo `json:"browserInfo"`
	ShopperEmail  string      `json:"shopperEmail"`
	ShopperIP     string      `json:"shopperIP"`
}

var encrypted = regexp.MustCompile(`^eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]*\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+$`)

func strict(raw string, out any) bool {
	if len(raw) > 128<<10 {
		return false
	}
	d := json.NewDecoder(strings.NewReader(raw))
	d.DisallowUnknownFields()
	return d.Decode(out) == nil && d.Decode(new(any)) == io.EOF
}
func (i cardInput) valid(b string) bool {
	p, bi := i.PaymentMethod, i.BrowserInfo
	if p.Type != "scheme" || p.Brand != "" && p.Brand != b || b == "visa" && (p.HolderName == "" || i.ShopperEmail == "" || i.ShopperIP == "") {
		return false
	}
	if i.ShopperIP != "" {
		if _, err := netip.ParseAddr(i.ShopperIP); err != nil {
			return false
		}
	}
	for _, field := range []string{p.EncryptedCardNumber, p.EncryptedExpiryMonth, p.EncryptedExpiryYear, p.EncryptedSecurityCode} {
		if len(field) > 15000 || !encrypted.MatchString(field) {
			return false
		}
	}
	return bi.AcceptHeader != "" && bi.JavaEnabled != nil && bi.Language != "" && bi.TimeZoneOffset != nil && bi.ScreenHeight > 0 && bi.ScreenWidth > 0 && bi.ColorDepth > 0 && bi.UserAgent != ""
}

type paymentReply struct {
	ResultCode, MerchantReference, PspReference string
	Amount                                      *amount
	Action                                      json.RawMessage
}

func (a *Adapter) confirm(ctx context.Context, r tender.ProviderRequest, o tender.Observation) (tender.Observation, error) {
	var saved CardAction
	if r.Payment.Status != tender.PaymentRequiresAction || r.Payment.Action.Mode != tender.ActionComponent || !r.Payment.Action.ExpiresAt.After(a.cfg.Now()) || json.Unmarshal([]byte(r.Payment.Action.Data), &saved) != nil {
		return o, tender.ErrValidation
	}
	var data map[string]any
	path, ref := "/payments", merchantReference(r.Payment, r.Operation)
	switch saved.Type {
	case "card":
		var input cardInput
		if !strict(r.Token, &input) || !input.valid(brand(r.Payment.Market.Method)) {
			return o, tender.ErrValidation
		}
		data = map[string]any{
			"paymentMethod": input.PaymentMethod, "browserInfo": input.BrowserInfo,
			"countryCode": r.Payment.Market.Country, "amount": money(o.Amount), "merchantAccount": a.cfg.MerchantAccount,
			"returnUrl": a.cfg.ReturnURL, "origin": a.cfg.Origin, "channel": "Web", "reference": ref,
			"additionalData":     map[string]string{"manualCapture": "true"},
			"authenticationData": map[string]any{"threeDSRequestData": map[string]string{"nativeThreeDS": "preferred"}},
		}
		if input.ShopperEmail != "" {
			data["shopperEmail"] = input.ShopperEmail
		}
		if input.ShopperIP != "" {
			data["shopperIP"] = input.ShopperIP
		}
	case "threeDS2":
		var input struct {
			Details struct {
				ThreeDSResult string `json:"threeDSResult"`
			} `json:"details"`
		}
		if saved.Action == nil || saved.Reference == "" || !strict(r.Token, &input) || input.Details.ThreeDSResult == "" || len(input.Details.ThreeDSResult) > 50000 {
			return o, tender.ErrValidation
		}
		path, ref = "/payments/details", saved.Reference
		data = map[string]any{"details": input.Details}
		if saved.Action.PaymentData != "" {
			data["paymentData"] = saved.Action.PaymentData
		}
	default:
		return o, tender.ErrValidation
	}
	// The signed AUTHORISATION for a /details request still names the earlier
	// /payments reservation. Retain that provider-echoed correlation on a pending
	// details operation after its browser action is cleared.
	if path == "/payments/details" {
		o.OperationReference = ref
	}
	var reply paymentReply
	if err := a.post(ctx, path, r.Operation.ProviderKey, data, &reply); err != nil {
		return o, err
	}
	// A details result originates in the browser. A positive correlation must come
	// back from Adyen before attaching its PSP reference or outcome to this payment.
	wrongReference := reply.MerchantReference != "" && reply.MerchantReference != ref
	matchedPSP := r.Payment.ProviderReference != "" && r.Payment.ProviderReference == reply.PspReference
	unboundDetails := path == "/payments/details" && reply.MerchantReference != ref && !matchedPSP
	if wrongReference || unboundDetails || reply.Amount != nil && *reply.Amount != money(o.Amount) {
		return o, tender.ErrUnknownOutcome
	}
	if reply.PspReference != "" {
		if !reference.MatchString(reply.PspReference) {
			return o, tender.ErrUnknownOutcome
		}
		o.ProviderReference = reply.PspReference
	}
	if len(reply.Action) > 0 && string(reply.Action) != "null" {
		var n NativeAction
		native := json.Unmarshal(reply.Action, &n) == nil && n.Type == "threeDS2" && n.PaymentMethodType == "scheme" && n.Token != ""
		fingerprint := n.Subtype == "fingerprint" && reply.ResultCode == "IdentifyShopper"
		challenge := n.Subtype == "challenge" && reply.ResultCode == "ChallengeShopper"
		if native && (fingerprint || challenge) {
			o.OperationReference = ""
			return a.action(o, CardAction{Type: "threeDS2", Reference: ref, Action: &n})
		}
		o.Status = tender.OperationPending
		o.Snapshot = tender.Snapshot{Status: tender.PaymentPending, ProviderStatus: "ActionUnavailable"}
		return o, nil
	}
	switch reply.ResultCode {
	case "Authorised":
		o.OperationReference = ""
		if o.ProviderReference == "" {
			return o, tender.ErrUnknownOutcome
		}
		o.Snapshot = tender.Snapshot{Status: tender.PaymentAuthorized, Funds: tender.Funds{Authorized: o.Amount.Minor}, ProviderStatus: "Authorised"}
	case "Refused", "Cancelled":
		o.OperationReference = ""
		o.Status = tender.OperationFailed
		o.Snapshot = tender.Snapshot{Status: tender.PaymentDeclined, ProviderStatus: reply.ResultCode}
	case "Pending", "Received":
		o.Status = tender.OperationPending
		o.Snapshot = tender.Snapshot{Status: tender.PaymentPending, ProviderStatus: reply.ResultCode}
	default:
		return o, tender.ErrUnknownOutcome
	}
	return o, nil
}
