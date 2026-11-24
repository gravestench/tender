// Package klarna implements North American Klarna Payments v1 and Order
// Management v1. The customer interaction is available only in local simulation.
package klarna

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gravestench/tender"
)

const (
	PlaygroundURL = "https://api-na.playground.klarna.com"
	LiveURL       = "https://api-na.klarna.com"
	Category      = "pay_over_time"
)

var market = tender.Market{Country: "US", Currency: "USD", Method: tender.MethodKlarna}
var safeID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,255}$`)

// Line describes a trusted host order line. US sales tax uses a separate line.
type Line struct {
	Type                string `json:"type"`
	Name                string `json:"name"`
	Reference           string `json:"reference,omitempty"`
	Quantity            int64  `json:"quantity"`
	UnitPrice           int64  `json:"unit_price"`
	TotalAmount         int64  `json:"total_amount"`
	TotalDiscountAmount int64  `json:"total_discount_amount,omitempty"`
}

type Config struct {
	Scope                                         tender.Scope
	Store                                         tender.Store
	BaseURL, APIKey, MerchantAccount, CallbackURL string
	// CallbackKey is a persistent, uniformly random 32-byte secret for this route.
	CallbackKey []byte
	// OrderLines retrieves the immutable server-priced cart for the claimed order.
	OrderLines    func(context.Context, tender.Payment) ([]Line, error)
	LiveActivated bool
	Transport     http.RoundTripper
	Now           func() time.Time
}

type Adapter struct {
	cfg    Config
	client *http.Client
	seal   cipher.AEAD
}

func endpoint(raw string) (*url.URL, bool) {
	u, err := url.Parse(raw)
	return u, err == nil && u.Hostname() != "" && (u.Scheme == "https" || u.Scheme == "http") && u.User == nil && u.RawQuery == "" && !u.ForceQuery && !strings.ContainsAny(raw, "#%\\")
}
func loopback(u *url.URL) bool {
	ip, err := netip.ParseAddr(u.Hostname())
	return err == nil && ip.Zone() == "" && ip.Unmap().IsLoopback()
}

func New(c Config) (*Adapter, error) {
	api, ok := endpoint(c.BaseURL)
	cb, valid := endpoint(c.CallbackURL)
	if !ok || !valid || c.Scope.Validate() != nil || c.Store == nil || c.OrderLines == nil || len(c.CallbackKey) != 32 || !safeID.MatchString(c.MerchantAccount) || strings.TrimSpace(c.APIKey) == "" || strings.ContainsAny(c.APIKey, " \t\r\n") {
		return nil, tender.ErrValidation
	}
	switch c.Scope.Environment {
	case tender.EnvironmentMock:
		if !loopback(api) || !loopback(cb) || strings.HasPrefix(c.APIKey, "klarna_live_") || strings.HasPrefix(c.APIKey, "klarna_test_") {
			return nil, tender.ErrValidation
		}
	case tender.EnvironmentSandbox:
		if c.BaseURL != PlaygroundURL || cb.Scheme != "https" || !strings.HasPrefix(c.APIKey, "klarna_test_api_") {
			return nil, tender.ErrValidation
		}
	case tender.EnvironmentLive:
		if !c.LiveActivated || c.BaseURL != LiveURL || cb.Scheme != "https" || !strings.HasPrefix(c.APIKey, "klarna_live_api_") {
			return nil, tender.ErrValidation
		}
	}
	c.BaseURL = strings.TrimRight(c.BaseURL, "/")
	c.CallbackKey = bytes.Clone(c.CallbackKey)
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Transport == nil {
		c.Transport = &http.Transport{ForceAttemptHTTP2: true}
	}
	key := hmac.New(sha256.New, c.CallbackKey)
	key.Write([]byte("tender/klarna/callback-encryption/v1"))
	block, err := aes.NewCipher(key.Sum(nil))
	if err != nil {
		return nil, err
	}
	seal, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return nil, err
	}
	return &Adapter{cfg: c, seal: seal, client: &http.Client{Transport: c.Transport, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (a *Adapter) Capabilities() tender.Capabilities {
	if a.cfg.Scope.Environment != tender.EnvironmentMock {
		return tender.Capabilities{}
	}
	return tender.Capabilities{Create: true, Confirm: true, Capture: true, Refund: true, Void: true, Query: true, ActionModes: []tender.ActionMode{tender.ActionComponent}, Markets: []tender.MarketCapability{{Market: market, PartialCapture: true, PartialRefund: true}}}
}
func (a *Adapter) CheckStore(_ tender.OperationKind, tx tender.Tx) error {
	_, write := tx.(tender.VerifiedEventTx)
	_, read := tx.(tender.VerifiedEvidenceReader)
	if !write || !read {
		return tender.ErrUnsupported
	}
	return nil
}

func digest(parts ...string) string {
	raw, _ := json.Marshal(parts)
	sum := sha256.Sum256(raw)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
func binding(p tender.Payment) string {
	return digest("klarna-order-v1", p.Scope.MerchantID, string(p.Scope.Environment), p.MerchantAccount, p.AttemptID, p.ID, p.OrderID, p.Amount.Currency, strconv.FormatInt(p.Amount.Minor, 10))
}
func (a *Adapter) validRoute(p tender.Payment) bool {
	return p.Scope == a.cfg.Scope && p.MerchantAccount == a.cfg.MerchantAccount && p.Provider == "klarna" && p.Market == market && p.Amount.Currency == market.Currency
}
func (a *Adapter) observation(r tender.ProviderRequest) (tender.Observation, error) {
	p, op := r.Payment, r.Operation
	if a.cfg.Scope.Environment != tender.EnvironmentMock {
		return tender.Observation{}, tender.ErrUnsupported
	}
	if !a.validRoute(p) || op.Scope != p.Scope || op.AttemptID != p.AttemptID || op.PaymentID != p.ID || !safeID.MatchString(op.ProviderKey) || !safeID.MatchString(op.ID) || p.Amount.ValidatePurchase() != nil || op.Amount.ValidatePurchase() != nil || op.Amount.Currency != p.Amount.Currency || op.Amount.Minor > p.Amount.Minor {
		return tender.Observation{}, tender.ErrValidation
	}
	return tender.Observation{Scope: p.Scope, MerchantAccount: p.MerchantAccount, PaymentID: p.ID, AttemptID: p.AttemptID, OperationID: op.ID, Amount: op.Amount, ProviderReference: p.ProviderReference, Status: tender.OperationSucceeded, Snapshot: p.Snapshot}, nil
}

type cart struct {
	Country     string `json:"purchase_country"`
	Currency    string `json:"purchase_currency"`
	Locale      string `json:"locale"`
	Amount      int64  `json:"order_amount"`
	Tax         int64  `json:"order_tax_amount"`
	Lines       []Line `json:"order_lines"`
	MerchantRef string `json:"merchant_reference1"`
	Binding     string `json:"merchant_reference2"`
}

const maxAmount int64 = 200000000

func (a *Adapter) cart(ctx context.Context, p tender.Payment) (cart, error) {
	lines, err := a.cfg.OrderLines(ctx, p)
	if err != nil {
		return cart{}, tender.ErrValidation
	}
	if len(lines) == 0 || len(lines) > 1000 || len(p.OrderID) > 255 || p.Amount.Minor > maxAmount {
		return cart{}, tender.ErrValidation
	}
	var sum, tax int64
	for _, line := range lines {
		if line.Quantity <= 0 || line.Quantity > 100000 || line.UnitPrice < -maxAmount || line.UnitPrice > maxAmount || line.TotalAmount < -maxAmount || line.TotalAmount > maxAmount || line.TotalDiscountAmount < 0 || line.TotalDiscountAmount > maxAmount || line.TotalAmount != line.Quantity*line.UnitPrice-line.TotalDiscountAmount || strings.TrimSpace(line.Name) == "" || len(line.Name) > 255 || len(line.Reference) > 255 {
			return cart{}, tender.ErrValidation
		}
		switch line.Type {
		case "sales_tax":
			if line.TotalAmount < 0 {
				return cart{}, tender.ErrValidation
			}
			tax += line.TotalAmount
		case "physical", "digital", "shipping_fee", "discount", "surcharge", "gift_card", "store_credit":
		default:
			return cart{}, tender.ErrValidation
		}
		sum += line.TotalAmount
	}
	if sum != p.Amount.Minor {
		return cart{}, tender.ErrValidation
	}
	return cart{"US", "USD", "en-US", p.Amount.Minor, tax, lines, p.OrderID, binding(p)}, nil
}

type sessionAction struct {
	Session    string `json:"session_id"`
	Client     string `json:"client_token"`
	Category   string `json:"payment_method_category"`
	CartDigest string `json:"cart_digest"`
}

func cartDigest(c cart) string { b, _ := json.Marshal(c); return digest(string(b)) }

func (a *Adapter) Execute(ctx context.Context, r tender.ProviderRequest) (tender.Observation, error) {
	o, err := a.observation(r)
	if err != nil {
		return o, err
	}
	switch r.Operation.Kind {
	case tender.OpCreate:
		return a.session(ctx, r, o)
	case tender.OpConfirm:
		return a.order(ctx, r, o)
	case tender.OpCapture, tender.OpVoid, tender.OpRefund:
		return a.modify(ctx, r, o)
	default:
		return o, tender.ErrUnsupported
	}
}

func (a *Adapter) session(ctx context.Context, r tender.ProviderRequest, o tender.Observation) (tender.Observation, error) {
	if r.Token != "" || r.Operation.Amount != r.Payment.Amount {
		return o, tender.ErrValidation
	}
	c, err := a.cart(ctx, r.Payment)
	if err != nil {
		return o, err
	}
	body := struct {
		cart
		Intent string            `json:"intent"`
		URLs   map[string]string `json:"merchant_urls"`
	}{c, "buy", map[string]string{"authorization": a.callbackURL(r)}}
	var res struct {
		Session    string `json:"session_id"`
		Client     string `json:"client_token"`
		Categories []struct {
			ID string `json:"identifier"`
		} `json:"payment_method_categories"`
	}
	created := a.cfg.Now()
	if _, err := a.call(ctx, "POST", "/payments/v1/sessions", "", body, 200, &res); err != nil {
		return o, err
	}
	if !safeID.MatchString(res.Session) || res.Client == "" || len(res.Client) > 16384 {
		return o, tender.ErrUnknownOutcome
	}
	for _, cat := range res.Categories {
		if cat.ID == Category {
			data, _ := json.Marshal(sessionAction{res.Session, res.Client, Category, cartDigest(c)})
			o.OperationReference = res.Session
			o.Snapshot = tender.Snapshot{Status: tender.PaymentRequiresAction, ProviderStatus: "SessionCreated", Action: tender.CustomerAction{Mode: tender.ActionComponent, Data: string(data), ExpiresAt: created.Add(48 * time.Hour)}}
			return o, nil
		}
	}
	return o, tender.NewProviderError(tender.ErrUnsupported, "CategoryUnavailable")
}

type orderResponse struct {
	OrderID  string `json:"order_id"`
	Fraud    string `json:"fraud_status"`
	Method   string `json:"authorized_payment_method"`
	Redirect string `json:"redirect_url"`
}

func (a *Adapter) order(ctx context.Context, r tender.ProviderRequest, o tender.Observation) (tender.Observation, error) {
	p := r.Payment
	if p.Action.Mode != tender.ActionComponent || r.Operation.Amount != p.Amount {
		return o, tender.ErrValidation
	}
	if !p.Action.ExpiresAt.After(a.cfg.Now()) {
		o.Snapshot = tender.Snapshot{Status: tender.PaymentExpired, ProviderStatus: "SessionExpired"}
		o.Status = tender.OperationFailed
		return o, nil
	}
	var action sessionAction
	if json.Unmarshal([]byte(p.Action.Data), &action) != nil || action.Category != Category {
		return o, tender.ErrValidation
	}
	receipt, err := a.receipt(ctx, p.ID, r.Token)
	if err != nil {
		return o, err
	}
	if receipt.Session != action.Session || receipt.Binding != binding(p) || !receipt.Expires.After(a.cfg.Now()) {
		return o, tender.NewProviderError(tender.ErrValidation, "InvalidAuthorization")
	}
	token, err := a.seal.Open(nil, nil, receipt.Ciphertext, receipt.aad())
	if err != nil || !safeID.Match(token) {
		return o, tender.ErrValidation
	}
	c, err := a.cart(ctx, p)
	if err != nil || cartDigest(c) != action.CartDigest {
		return o, tender.NewProviderError(tender.ErrValidation, "CartChanged")
	}
	var reply orderResponse
	body := struct {
		cart
		AutoCapture bool `json:"auto_capture"`
	}{c, false}
	if _, err = a.call(ctx, "POST", "/payments/v1/authorizations/"+string(token)+"/order", "", body, 200, &reply); err != nil {
		if errors.Is(err, tender.ErrDeclined) {
			o.Snapshot = tender.Snapshot{Status: tender.PaymentDeclined, ProviderStatus: "REJECTED"}
			o.Status = tender.OperationFailed
			return o, nil
		}
		return o, err
	}
	if !safeID.MatchString(reply.OrderID) || reply.Redirect != "" {
		return o, tender.ErrUnknownOutcome
	}
	if !financingMethod(reply.Method) {
		return o, tender.ErrUnknownOutcome
	}
	o.ProviderReference = reply.OrderID
	o.OperationReference = reply.OrderID
	return orderDecision(o, reply.Fraud, p.Amount.Minor)
}
func financingMethod(method string) bool {
	return strings.EqualFold(method, "FIXED_AMOUNT")
}

func orderDecision(o tender.Observation, fraud string, amount int64) (tender.Observation, error) {
	o.Snapshot = tender.Snapshot{ProviderStatus: fraud, VerifiedMethod: tender.MethodKlarna}
	switch fraud {
	case "ACCEPTED":
		o.Snapshot.Status = tender.PaymentAuthorized
		o.Snapshot.Funds.Authorized = amount
	case "PENDING":
		o.Snapshot.Status = tender.PaymentPending
		o.Status = tender.OperationPending
	case "REJECTED":
		o.Snapshot.Status = tender.PaymentDeclined
		o.Status = tender.OperationFailed
	default:
		return o, tender.ErrUnknownOutcome
	}
	return o, nil
}

func (a *Adapter) call(ctx context.Context, method, path, key string, body any, status int, out any) (http.Header, error) {
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return nil, tender.ErrValidation
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, a.cfg.BaseURL+path, bytes.NewReader(data))
	if err != nil {
		return nil, tender.ErrValidation
	}
	req.GetBody = nil
	req.Header.Set("Authorization", "Basic "+a.cfg.APIKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Klarna-Idempotency-Key", key)
	}
	res, err := a.client.Do(req)
	if err != nil {
		if method == "GET" {
			return nil, tender.ErrTransport
		}
		return nil, tender.ErrUnknownOutcome
	}
	defer res.Body.Close()
	if res.StatusCode != status {
		var failure struct {
			Code string `json:"error_code"`
		}
		_ = json.NewDecoder(io.LimitReader(res.Body, 8192)).Decode(&failure)
		switch res.StatusCode {
		case 401:
			return nil, tender.ErrAuthentication
		case 429:
			e := tender.NewProviderError(tender.ErrRateLimit, "RateLimited")
			n, _ := strconv.ParseUint(res.Header.Get("Retry-After"), 10, 31)
			e.RetryAfter = time.Duration(n) * time.Second
			return nil, e
		case 400, 403, 404, 422:
			if res.StatusCode == 403 && failure.Code == "NOT_ALLOWED" && method == "POST" && strings.HasSuffix(path, "/release-remaining-authorization") {
				return nil, tender.NewProviderError(tender.ErrValidation, "NOT_ALLOWED")
			}
			if res.StatusCode == 403 && failure.Code == "MERCHANT_INACTIVE" {
				return nil, tender.NewProviderError(tender.ErrUnsupported, "MERCHANT_INACTIVE")
			}
			if res.StatusCode == 403 && failure.Code == "REJECTED" && strings.HasPrefix(path, "/payments/v1/authorizations/") {
				return nil, tender.ErrDeclined
			}
			switch failure.Code {
			case "BAD_VALUE", "BAD_REQUEST", "NOT_FOUND", "INVALID_ORDER_ID", "NO_SUCH_ORDER", "CAPTURE_NOT_ALLOWED", "REFUND_NOT_ALLOWED", "CANCEL_NOT_ALLOWED", "PERMISSION_DENIED":
				return nil, tender.NewProviderError(tender.ErrValidation, failure.Code)
			}
		}
		return nil, tender.ErrUnknownOutcome
	}
	if out != nil {
		const limit = 512 << 10
		raw, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
		if err != nil || len(raw) > limit || json.Unmarshal(raw, out) != nil {
			return nil, tender.ErrUnknownOutcome
		}
	}
	return res.Header, nil
}

// BrowserSession is the public projection of a simulated Klarna component.
// The checkout worker retains callback proofs and authorization tokens.
type BrowserSession struct {
	SessionID   string `json:"session_id"`
	ClientToken string `json:"client_token"`
	Category    string `json:"payment_method_category"`
}

// CheckoutAction rechecks the policy and expiry on each checkout refresh.
func (a *Adapter) CheckoutAction(p tender.Payment) (BrowserSession, error) {
	if a.cfg.Scope.Environment != tender.EnvironmentMock || !a.validRoute(p) || p.Status != tender.PaymentRequiresAction || p.Action.Mode != tender.ActionComponent || p.Action.Validate(a.cfg.Now()) != nil {
		return BrowserSession{}, tender.ErrUnsupported
	}
	var s sessionAction
	if json.Unmarshal([]byte(p.Action.Data), &s) != nil || s.Category != Category || !safeID.MatchString(s.Session) || s.Client == "" {
		return BrowserSession{}, tender.ErrUnsupported
	}
	return BrowserSession{SessionID: s.Session, ClientToken: s.Client, Category: s.Category}, nil
}
