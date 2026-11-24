// Package adyen implements the card Advanced flow for Adyen Checkout v72.
package adyen

import (
	"bytes"
	"context"
	"encoding/json"
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

// Config binds an adapter to one explicitly selected merchant and environment.
// BaseURL includes the v72 path. Supply only trusted transport implementations.
// MultiplePartialCaptures requires merchant-specific provider enablement.
type Config struct {
	// LiveActivated records the host's explicit approval for this production merchant.
	LiveActivated                                       bool
	Environment                                         tender.Environment
	BaseURL, APIKey, MerchantAccount, Origin, ReturnURL string
	MultiplePartialCaptures                             bool
	Transport                                           http.RoundTripper
	Now                                                 func() time.Time
}
type Adapter struct {
	cfg    Config
	client *http.Client
}

var liveHost = regexp.MustCompile(`(?i)^[a-z0-9][a-z0-9-]*-checkout-live\.adyenpayments\.com$`)
var reference = regexp.MustCompile(`^[A-Za-z0-9_-]{1,80}$`)

func endpoint(raw string) (*url.URL, bool) {
	u, err := url.Parse(raw)
	return u, err == nil && u.Hostname() != "" && u.User == nil && !u.ForceQuery && u.RawQuery == "" && !strings.ContainsAny(raw, "#%\\") && (u.Scheme == "https" || u.Scheme == "http")
}
func loopback(u *url.URL) bool {
	ip, err := netip.ParseAddr(u.Hostname())
	return err == nil && ip.Zone() == "" && ip.Unmap().IsLoopback()
}

// New validates the environment against its allowed API endpoint and checkout origin.
func New(c Config) (*Adapter, error) {
	u, ok := endpoint(c.BaseURL)
	if !ok || strings.TrimSpace(c.APIKey) == "" || !reference.MatchString(c.MerchantAccount) {
		return nil, tender.ErrValidation
	}
	origin, ok := endpoint(c.Origin)
	if !ok || origin.Path != "" || len(c.Origin) > 80 {
		return nil, tender.ErrValidation
	}
	ret, ok := endpoint(c.ReturnURL)
	if !ok || len(c.ReturnURL) > 1024 || ret.Host != origin.Host || ret.Scheme != origin.Scheme || strings.Contains(ret.Path, "//") {
		return nil, tender.ErrValidation
	}
	switch c.Environment {
	case tender.EnvironmentMock:
		if !loopback(u) || !loopback(origin) || !strings.HasSuffix(u.Path, "/v72") {
			return nil, tender.ErrValidation
		}
	case tender.EnvironmentSandbox:
		if c.BaseURL != "https://checkout-test.adyen.com/v72" || origin.Scheme != "https" {
			return nil, tender.ErrValidation
		}
	case tender.EnvironmentLive:
		if !c.LiveActivated || u.Scheme != "https" || u.Host != u.Hostname() || !liveHost.MatchString(u.Host) || u.Path != "/checkout/v72" || origin.Scheme != "https" {
			return nil, tender.ErrValidation
		}
	default:
		return nil, tender.ErrValidation
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Transport == nil {
		c.Transport = &http.Transport{ForceAttemptHTTP2: true}
	}
	client := &http.Client{
		Transport: c.Transport, Timeout: 15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return &Adapter{cfg: c, client: client}, nil
}
func (a *Adapter) Capabilities() tender.Capabilities {
	caps := tender.Capabilities{Create: true, Confirm: true, Capture: true, Void: true, Refund: true, ActionModes: []tender.ActionMode{tender.ActionComponent}}
	for _, method := range []tender.PaymentMethod{tender.MethodVisa, tender.MethodMastercard} {
		caps.Markets = append(caps.Markets, tender.MarketCapability{
			Market:         tender.Market{Country: "US", Currency: "USD", Method: method},
			PartialCapture: a.cfg.MultiplePartialCaptures, PartialRefund: true,
		})
	}
	return caps
}

type amount struct {
	Value    int64  `json:"value"`
	Currency string `json:"currency"`
}

func money(m tender.Money) amount { return amount{m.Minor, m.Currency} }
func (a *Adapter) post(ctx context.Context, path, key string, data, out any) error {
	body, err := json.Marshal(data)
	if err != nil {
		return tender.ErrValidation
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return tender.ErrValidation
	}
	// Leave mutation recovery with the engine. Go may otherwise resend a body
	// on a reused connection simply because Idempotency-Key is present.
	req.GetBody = nil
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", a.cfg.APIKey)
	req.Header.Set("Idempotency-Key", key)
	resp, err := a.client.Do(req)
	if err != nil {
		return tender.ErrUnknownOutcome
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return tender.ErrAuthentication
	case http.StatusTooManyRequests:
		e := tender.NewProviderError(tender.ErrRateLimit, "RateLimited")
		if n, err := strconv.ParseUint(resp.Header.Get("Retry-After"), 10, 31); err == nil {
			e.RetryAfter = time.Duration(n) * time.Second
		}
		return e
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		// Adyen code 704 (a concurrent idempotent request) also uses 422.
		var failure struct{ ErrorCode string }
		_ = json.NewDecoder(io.LimitReader(resp.Body, 8192)).Decode(&failure)
		if failure.ErrorCode == "" || failure.ErrorCode == "704" || resp.Header.Get("transient-error") == "true" {
			return tender.ErrUnknownOutcome
		}
		return tender.ErrValidation
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return tender.ErrUnknownOutcome
	}
	const limit = 512 << 10
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil || len(raw) > limit || json.Unmarshal(raw, out) != nil {
		return tender.ErrUnknownOutcome
	}
	return nil
}
func (a *Adapter) Execute(ctx context.Context, r tender.ProviderRequest) (tender.Observation, error) {
	p, op := r.Payment, r.Operation
	o := tender.Observation{
		Scope: p.Scope, MerchantAccount: p.MerchantAccount, PaymentID: p.ID, AttemptID: p.AttemptID, OperationID: op.ID,
		Amount: op.Amount, ProviderReference: p.ProviderReference, Snapshot: p.Snapshot, Status: tender.OperationSucceeded,
	}
	routeOK := p.Scope.Validate() == nil && p.Scope.Environment == a.cfg.Environment && p.Provider == "adyen" && p.MerchantAccount == a.cfg.MerchantAccount
	bound := p.ID != "" && p.AttemptID != "" && op.Scope == p.Scope && op.PaymentID == p.ID && op.AttemptID == p.AttemptID
	idsOK := reference.MatchString(op.ID) && len(op.ID) <= 57 && reference.MatchString(op.ProviderKey) && len(op.ProviderKey) <= 64
	if !routeOK || !bound || !idsOK || op.Amount.ValidatePurchase() != nil || op.Amount.Currency != p.Market.Currency {
		return o, tender.ErrValidation
	}
	if !a.Capabilities().SupportsIn(op.Kind, p.Market, false) {
		return o, tender.ErrUnsupported
	}
	switch op.Kind {
	case tender.OpCreate:
		return a.discover(ctx, r, o)
	case tender.OpConfirm:
		return a.confirm(ctx, r, o)
	default:
		return a.modify(ctx, r, o)
	}
}

// Query returns ErrUnsupported. Advanced flow has no general read-only payment query.
func (a *Adapter) Query(context.Context, tender.ProviderRequest) (tender.Observation, error) {
	return tender.Observation{}, tender.ErrUnsupported
}
func (a *Adapter) modify(ctx context.Context, r tender.ProviderRequest, o tender.Observation) (tender.Observation, error) {
	if !reference.MatchString(r.Payment.ProviderReference) || r.Payment.Funds.Authorized == 0 {
		return o, tender.ErrUnsupported
	}
	paths := map[tender.OperationKind]string{tender.OpVoid: "cancels", tender.OpCapture: "captures", tender.OpRefund: "refunds"}
	ref := merchantReference(r.Payment, r.Operation)
	data := map[string]any{"merchantAccount": a.cfg.MerchantAccount, "reference": ref}
	if r.Operation.Kind != tender.OpVoid {
		data["amount"] = money(r.Operation.Amount)
	}
	var reply struct {
		MerchantAccount, Reference, PaymentPspReference, PspReference, Status string
		Amount                                                                *amount
	}
	path := "/payments/" + r.Payment.ProviderReference + "/" + paths[r.Operation.Kind]
	if err := a.post(ctx, path, r.Operation.ProviderKey, data, &reply); err != nil {
		return o, err
	}
	if !reference.MatchString(reply.PspReference) || reply.Status != "received" || reply.PaymentPspReference != r.Payment.ProviderReference ||
		reply.MerchantAccount != a.cfg.MerchantAccount || reply.Reference != ref || reply.Amount != nil && *reply.Amount != money(o.Amount) {
		return o, tender.ErrUnknownOutcome
	}
	o.OperationReference = reply.PspReference
	o.Status = tender.OperationPending
	return o, nil
}
