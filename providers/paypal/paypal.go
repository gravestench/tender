// Package paypal implements PayPal wallet Orders v2 with CAPTURE intent.
// The synthetic buyer approval component is available only in MOCK.
package paypal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gravestench/tender"
)

// Config binds one direct merchant REST app, host scope, registered webhook,
// and exact API destination. Secrets and bearer tokens must stay server-side.
type Config struct {
	// Transport optionally supplies a trusted host-controlled HTTP transport.
	Transport                                             http.RoundTripper
	Scope                                                 tender.Scope
	BaseURL, MerchantAccount, ClientID, Secret, WebhookID string
	Now                                                   func() time.Time
	LiveActivated                                         bool
}
type Adapter struct {
	cfg     Config
	client  *http.Client
	mu      sync.Mutex
	token   string
	expires time.Time
}

var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,127}$`)
var mockPath = regexp.MustCompile(`^(/[A-Za-z0-9_-]+)*$`)

func New(c Config) (*Adapter, error) {
	if c.Scope.Validate() != nil || !idPattern.MatchString(c.MerchantAccount) || !idPattern.MatchString(c.WebhookID) || strings.TrimSpace(c.Secret) == "" || strings.TrimSpace(c.ClientID) == "" || strings.Contains(c.ClientID, ":") {
		return nil, tender.ErrValidation
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.User != nil || u.RawQuery != "" || u.ForceQuery || strings.Contains(c.BaseURL, "#") || u.RawPath != "" {
		return nil, tender.ErrValidation
	}
	switch c.Scope.Environment {
	case tender.EnvironmentMock:
		ip, err := netip.ParseAddr(u.Hostname())
		if err != nil || !ip.Unmap().IsLoopback() || ip.Zone() != "" || (u.Scheme != "http" && u.Scheme != "https") || !mockPath.MatchString(u.Path) {
			return nil, tender.ErrValidation
		}
	case tender.EnvironmentSandbox:
		if c.BaseURL != "https://api-m.sandbox.paypal.com" {
			return nil, tender.ErrValidation
		}
	case tender.EnvironmentLive:
		if !c.LiveActivated || c.BaseURL != "https://api-m.paypal.com" {
			return nil, tender.ErrValidation
		}
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	// No proxy or redirects can change the credential destination.
	transport := c.Transport
	if transport == nil {
		transport = &http.Transport{IdleConnTimeout: 60 * time.Second, ForceAttemptHTTP2: true}
	}
	return &Adapter{cfg: c, client: &http.Client{Transport: transport, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

var market = tender.Market{Country: "US", Currency: "USD", Method: tender.MethodPayPal}

func (a *Adapter) Capabilities() tender.Capabilities {
	c := tender.Capabilities{Refund: true, Query: true, QueryAfterDispatch: true, Markets: []tender.MarketCapability{{Market: market, PartialRefund: true}}}
	if a.cfg.Scope.Environment == tender.EnvironmentMock {
		c.Create = true
		c.Confirm = true
		c.ActionModes = []tender.ActionMode{tender.ActionComponent}
	}
	return c
}
func (a *Adapter) tokenFor(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.cfg.Now()
	if a.token != "" && now.Before(a.expires) {
		return a.token, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.BaseURL+"/v1/oauth2/token", strings.NewReader("grant_type=client_credentials"))
	if err != nil {
		return "", tender.ErrValidation
	}
	req.SetBasicAuth(a.cfg.ClientID, a.cfg.Secret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var result struct {
		Access  string `json:"access_token"`
		Type    string `json:"token_type"`
		Seconds int64  `json:"expires_in"`
	}
	if err := a.send(req, &result, false); err != nil {
		return "", err
	}
	if result.Access == "" || strings.ContainsAny(result.Access, " \r\n\t") || !strings.EqualFold(result.Type, "Bearer") || result.Seconds <= 0 || result.Seconds > 86400 {
		return "", tender.ErrAuthentication
	}
	ttl := time.Duration(result.Seconds) * time.Second
	a.token = result.Access
	a.expires = now.Add(ttl - min(30*time.Second, ttl/10))
	return a.token, nil
}

// call accepts only internally constructed paths. PayPal-Request-Id always
// comes from the original durable reservation. A definite 401 can reacquire
// credentials once and use precisely the same method, body and operation key.
func (a *Adapter) call(ctx context.Context, method, path, key string, body []byte, result any) error {
	for attempt := 0; attempt < 2; attempt++ {
		token, err := a.tokenFor(ctx)
		if err != nil {
			if key != "" {
				return beforeDispatch(err)
			}
			return err
		}
		req, err := http.NewRequestWithContext(ctx, method, a.cfg.BaseURL+path, bytes.NewReader(body))
		if err != nil {
			return tender.ErrValidation
		}
		req.GetBody = nil
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/json")
		if method == http.MethodPost {
			req.Header.Set("Content-Type", "application/json")
		}
		if key != "" {
			req.Header.Set("PayPal-Request-Id", key)
			req.Header.Set("Prefer", "return=representation")
		}
		err = a.send(req, result, key != "")
		if err != tender.ErrAuthentication || attempt == 1 {
			return err
		}
		a.mu.Lock()
		if a.token == token {
			a.token = ""
			a.expires = time.Time{}
		}
		a.mu.Unlock()
	}
	return tender.ErrAuthentication
}
func (a *Adapter) send(req *http.Request, result any, mutation bool) error {
	res, err := a.client.Do(req)
	if err != nil {
		if mutation {
			return tender.ErrUnknownOutcome
		}
		return tender.ErrTransport
	}
	defer res.Body.Close()
	switch {
	case res.StatusCode == 401:
		return tender.ErrAuthentication
	case res.StatusCode == 429:
		return tender.ErrRateLimit
	case res.StatusCode < 200 || res.StatusCode > 299:
		if mutation {
			return a.rejection(req.URL.String(), res)
		}
		return tender.ErrTransport
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, (512<<10)+1))
	if err != nil || len(b) > 512<<10 || json.Unmarshal(b, result) != nil {
		if mutation {
			return tender.ErrUnknownOutcome
		}
		return tender.ErrTransport
	}
	return nil
}

// beforeDispatch is used only when a prerequisite failed before the mutation
// was sent, or after a definite 401 refused the original credential.
func beforeDispatch(err error) error {
	if errors.Is(err, tender.ErrTransport) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return tender.ErrNotDispatched
	}
	return err
}

func (a *Adapter) rejection(destination string, res *http.Response) error {
	if res.StatusCode != 400 && res.StatusCode != 422 {
		return tender.ErrUnknownOutcome
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, 16385))
	var body struct {
		Name    string
		Details []struct{ Issue string }
	}
	if err != nil || len(raw) > 16384 || json.Unmarshal(raw, &body) != nil || len(body.Details) == 0 {
		return tender.ErrUnknownOutcome
	}
	path := strings.TrimPrefix(destination, a.cfg.BaseURL)
	order := path == "/v2/checkout/orders"
	capture := strings.HasPrefix(path, "/v2/checkout/orders/") && strings.HasSuffix(path, "/capture")
	refund := strings.HasPrefix(path, "/v2/payments/captures/") && strings.HasSuffix(path, "/refund")
	var kind tender.ErrorKind
	for _, detail := range body.Details {
		var next tender.ErrorKind
		switch {
		case res.StatusCode == 400 && body.Name == "INVALID_REQUEST" && (order || capture):
			switch detail.Issue {
			case "INVALID_PARAMETER_SYNTAX", "INVALID_PARAMETER_VALUE", "INVALID_STRING_LENGTH", "MISSING_REQUIRED_PARAMETER":
				next = tender.ErrValidation
			}
		case res.StatusCode == 422 && body.Name == "UNPROCESSABLE_ENTITY" && capture:
			switch detail.Issue {
			case "INSTRUMENT_DECLINED", "PAYMENT_DENIED":
				next = tender.ErrDeclined
			case "ORDER_NOT_APPROVED":
				next = tender.ErrValidation
			}
		case res.StatusCode == 422 && body.Name == "UNPROCESSABLE_ENTITY" && refund && detail.Issue == "REFUND_AMOUNT_EXCEEDED":
			next = tender.ErrValidation
		}
		if next == "" || kind != "" && kind != next {
			return tender.ErrUnknownOutcome
		}
		kind = next
	}
	return tender.NewProviderError(kind, body.Details[0].Issue)
}
