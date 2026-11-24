package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gravestench/tender"
	"github.com/gravestench/tender/mockserver"
	"github.com/gravestench/tender/providers/adyen"
	"github.com/gravestench/tender/webhook"
)

//go:embed adyen.html
var cardHTML string

//go:embed component.html
var componentHTML string

//go:embed component.js
var componentJS string

//go:embed order.js
var orderJS string

var cardPage = template.Must(template.New("card").Parse(cardHTML))

const shopperOrigin = "https://checkoutshopper-test.adyen.com"
const cdnOrigin = "https://checkoutshopper-test.cdn.adyen.com"

type cardCheckout struct {
	store                                           tender.Store
	clientKey, shopperEmail, operatorKey, acsOrigin string
}

var exactDNSName = regexp.MustCompile(`^(?:[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?\.)*[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)

func sandboxOrigin(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && exactDNSName.MatchString(u.Hostname()) && raw == "https://"+u.Host && u.User == nil && !strings.ContainsAny(raw, "#%\\")
}

func (a *app) installCardCheckout(mux *http.ServeMux, storage tender.Store) (func(), error) {
	if a.ledger == nil {
		return nil, errors.New("consumer: Adyen requires a migrated SQL store")
	}
	// The two-order example serializes SQL transactions on one pooled connection.
	a.ledger.SetMaxOpenConns(1)
	a.card = &cardCheckout{store: storage, operatorKey: os.Getenv("TENDER_CONSUMER_OPERATOR_KEY")}
	if a.card.operatorKey != "" && len(a.card.operatorKey) < 32 {
		return nil, errors.New("consumer: operator key must have at least 32 characters")
	}
	a.provider = "adyen"
	method := os.Getenv("TENDER_ADYEN_BRAND")
	switch method {
	case "", "visa":
	case "mc":
		a.market.Method = tender.MethodMastercard
	default:
		return nil, errors.New("consumer: unsupported Adyen card brand")
	}
	cfg := adyen.Config{Environment: a.scope.Environment, Origin: a.origin, ReturnURL: a.origin + "/checkout-return"}
	var sim *mockserver.Server
	webhookKey := os.Getenv("TENDER_ADYEN_HMAC_KEY")
	if a.scope.Environment == tender.EnvironmentMock {
		cfg.MerchantAccount, cfg.APIKey, cfg.BaseURL = "TenderFixture", "synthetic-only", a.origin+"/simulator/v72"
		secret := sha256.Sum256([]byte("Tender offline Adyen fixture only"))
		webhookKey = hex.EncodeToString(secret[:])
		target := a.origin + "/webhooks/adyen"
		result := os.Getenv("TENDER_ADYEN_MOCK_RESULT")
		if !slices.Contains([]string{"", "Authorised", "Refused", "Pending", "ChallengeShopper", "IdentifyShopper", "RedirectShopper", "UnknownAction"}, result) {
			return nil, errors.New("consumer: invalid mock result")
		}
		var err error
		sim, err = mockserver.New(mockserver.Config{PublicBaseURL: a.origin + "/simulator", AllowedCallbackTargets: []string{target}, SigningKey: secret[:], Routes: mockserver.AdyenRoutes(mockserver.AdyenFixture{MerchantAccount: cfg.MerchantAccount, APIKey: cfg.APIKey, Brands: []string{"visa", "mc"}, PaymentResult: result, WebhookURL: target})})
		if err != nil {
			return nil, err
		}
		lost := os.Getenv("TENDER_ADYEN_MOCK_LOST_RESPONSE")
		if !slices.Contains([]string{"", "payments", "captures"}, lost) {
			sim.Close()
			return nil, errors.New("consumer: invalid lost response route")
		}
		for _, route := range []string{"payments", "details", "captures", "refunds", "cancels"} {
			step := mockserver.Scenario{DuplicateCallbacks: true}
			if route == lost {
				step.Fault = mockserver.LoseResponse
			}
			if err := sim.SetScenarios("adyen."+route, step); err != nil {
				sim.Close()
				return nil, err
			}
		}
		mux.Handle("/simulator/", http.StripPrefix("/simulator", sim.Handler()))
	} else {
		cfg.BaseURL = "https://checkout-test.adyen.com/v72"
		a.card.acsOrigin = os.Getenv("TENDER_ADYEN_TEST_ACS_ORIGIN")
		if a.card.acsOrigin == "" {
			a.card.acsOrigin = shopperOrigin
		}
		if !sandboxOrigin(a.card.acsOrigin) {
			return nil, errors.New("consumer: invalid sandbox issuer origin")
		}
		cfg.APIKey, cfg.MerchantAccount = os.Getenv("TENDER_ADYEN_API_KEY"), os.Getenv("TENDER_ADYEN_MERCHANT_ACCOUNT")
		a.card.clientKey, a.card.shopperEmail = os.Getenv("TENDER_ADYEN_CLIENT_KEY"), os.Getenv("TENDER_ADYEN_TEST_SHOPPER_EMAIL")
		if !strings.HasPrefix(a.card.clientKey, "test_") || len(a.card.clientKey) <= 5 || a.card.shopperEmail == "" {
			return nil, errors.New("consumer: sandbox requires a test client key and trusted test shopper email")
		}
	}
	success := false
	defer func() {
		if sim != nil && !success {
			sim.Close()
		}
	}()
	adapter, err := adyen.New(cfg)
	if err != nil {
		return nil, err
	}
	a.account = cfg.MerchantAccount
	a.engine, err = tender.NewEngine(tender.EngineConfig{Store: storage, Provider: adapter, Scope: a.scope, Market: a.market, ProviderName: a.provider, MerchantAccount: a.account})
	if err != nil {
		return nil, err
	}
	wake := make(chan struct{}, 1)
	wh, err := webhook.NewAdyen(webhook.AdyenConfig{Store: storage, Engine: a.engine, Scope: a.scope, MerchantAccount: a.account, HMACKeys: []string{webhookKey}, OnStored: func(string) {
		select {
		case wake <- struct{}{}:
		default:
		}
	}})
	if err != nil {
		return nil, err
	}
	mux.Handle("/webhooks/adyen", wh)
	mux.HandleFunc("GET /orders/{order}", a.owned(a.cardView))
	mux.HandleFunc("POST /orders/{order}", a.owned(a.prepareCard))
	if a.scope.Environment == tender.EnvironmentMock {
		mux.HandleFunc("POST /orders/{order}/confirm/{version}", a.owned(a.confirmCard))
	} else {
		mux.HandleFunc("POST /orders/{order}/confirm", a.owned(a.confirmCard))
		mux.HandleFunc("GET /orders/{order}/component", a.owned(a.component))
		mux.HandleFunc("GET /orders/{order}/card-state", a.owned(a.cardState))
		mux.HandleFunc("GET /order.js", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/javascript")
			io.WriteString(w, orderJS)
		})
		mux.HandleFunc("GET /component.js", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/javascript")
			io.WriteString(w, componentJS)
		})
	}
	mux.HandleFunc("POST /operator/orders/{order}/{operation}", a.operator)
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			if ctx.Err() != nil {
				return
			}
			a.processCardEvents(ctx, wh)
			select {
			case <-ctx.Done():
				return
			case <-wake:
			case <-ticker.C:
			}
		}
	}()
	success = true
	return func() {
		stop()
		<-done
		if sim != nil {
			sim.Close()
		}
	}, nil
}

// The inbox survives dropped wakes and restarts. Close the reader before using
// more connections. Unmatched or temporarily conflicting receipts remain pending.
func (a *app) processCardEvents(ctx context.Context, h *webhook.Adyen) {
	rows, err := a.ledger.QueryContext(ctx, `SELECT event_id FROM tender_provider_events WHERE merchant_id=$1 AND environment=$2 AND provider='adyen' AND merchant_account=$3 AND processed_at IS NULL ORDER BY received_at,event_id`, a.scope.MerchantID, a.scope.Environment, a.account)
	if err != nil {
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) != nil {
			rows.Close()
			return
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return
	}
	for _, id := range ids {
		_ = h.Process(ctx, id)
	}
	for _, o := range orders {
		_ = a.project(ctx, o)
	}
}

func decodeStrict(r io.Reader, out any) bool {
	d := json.NewDecoder(r)
	d.DisallowUnknownFields()
	return d.Decode(out) == nil && d.Decode(new(any)) == io.EOF
}

// Only these representations reach the SDK. In particular the SDK fingerprint
// flow can produce its own redirect without a second host policy decision.
func browserAction(p tender.Payment, now time.Time) adyen.CardAction {
	var action adyen.CardAction
	if p.Status != tender.PaymentRequiresAction || p.Action.Mode != tender.ActionComponent || p.Action.Validate(now) != nil || !p.Action.ExpiresAt.After(now) || !decodeStrict(strings.NewReader(p.Action.Data), &action) {
		return adyen.CardAction{}
	}
	want := "visa"
	if p.Market.Method == tender.MethodMastercard {
		want = "mc"
	}
	switch action.Type {
	case "card":
		if slices.Equal(action.Brands, []string{want}) && action.Action == nil && action.Reference == "" {
			return action
		}
	case "threeDS2":
		n := action.Action
		if n != nil && len(action.Brands) == 0 && action.Reference != "" && n.Allowed() {
			action.Reference = "" // internal correlation stays server-side
			n.PaymentData = ""    // only the adapter sends this binding back to Checkout
			return action
		}
	}
	return adyen.CardAction{}
}

// Verify the destinations embedded in the native SDK token before exposing it.
func (a *app) cardAction(p tender.Payment) adyen.CardAction {
	action := browserAction(p, time.Now())
	if a.scope.Environment != tender.EnvironmentSandbox || action.Type != "threeDS2" {
		return action
	}
	var data []byte
	var err error
	for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		data, err = encoding.DecodeString(action.Action.Token)
		if err == nil {
			break
		}
	}
	var token struct{ ACSURL, ThreeDSNotificationURL, ACSTransID, ThreeDSServerTransID, MessageVersion string }
	if err != nil || json.Unmarshal(data, &token) != nil || token.ACSTransID == "" || token.ThreeDSServerTransID == "" || token.MessageVersion == "" {
		return adyen.CardAction{}
	}
	for _, endpoint := range [][2]string{{token.ACSURL, a.card.acsOrigin}, {token.ThreeDSNotificationURL, shopperOrigin}} {
		u, err := url.Parse(endpoint[0])
		if err != nil || u.User != nil || u.Scheme+"://"+u.Host != endpoint[1] || strings.ContainsAny(endpoint[0], "\\#") {
			return adyen.CardAction{}
		}
	}
	return action
}

func (a *app) cardPayment(ctx context.Context, o order) (tender.Payment, error) {
	p, err := a.engine.Get(ctx, o.PaymentID)
	if err == nil && (p.OrderID != o.ID || p.Amount != o.Total) {
		err = tender.ErrConflict
	}
	if err == nil && p.Status == tender.PaymentRequiresAction {
		// A lost authorization reply can leave the previously prepared action
		// in the payment snapshot. Only the existing reservation may resolve.
		err = a.card.store.Transact(ctx, func(tx tender.Tx) error {
			_, activeErr := tx.ActiveOperation(a.scope, p.ID)
			if errors.Is(activeErr, tender.ErrNotFound) {
				return nil
			}
			if activeErr == nil {
				p.Action = tender.CustomerAction{}
			}
			return activeErr
		})
	}
	return p, err
}
func (a *app) cardView(w http.ResponseWriter, r *http.Request, o order, s session) {
	p, err := a.cardPayment(r.Context(), o)
	st := string(p.Status)
	if errors.Is(err, tender.ErrNotFound) {
		st = "unpaid"
	} else if err != nil {
		http.Error(w, "checkout unavailable", 503)
		return
	}
	data := struct {
		Order, Price, Status, CSRF, Action string
		Sandbox, Review                    bool
		Version                            uint64
	}{Order: o.ID, Price: fmt.Sprintf("$%d.%02d %s", o.Total.Minor/100, o.Total.Minor%100, o.Total.Currency), Status: st, CSRF: s.csrf, Action: a.cardAction(p).Type, Sandbox: a.scope.Environment == tender.EnvironmentSandbox, Version: p.Version}
	data.Review = p.Status == tender.PaymentCaptureFailed || p.ProviderStatus == "ActionUnavailable" || (p.Status == tender.PaymentRequiresAction && data.Action == "")
	w.Header().Set("X-CSRF-Token", s.csrf)
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if data.Sandbox {
		w.Header().Set("Content-Security-Policy", "default-src 'none'; form-action 'self'; frame-src 'self'; script-src 'self'; base-uri 'none'; frame-ancestors 'none'")
	}
	_ = cardPage.Execute(w, data)
}
func (a *app) prepareCard(w http.ResponseWriter, r *http.Request, o order, s session) {
	if _, ok := a.form(w, r, s, false); !ok {
		return
	}
	_, err := a.engine.Create(r.Context(), tender.Request{PaymentID: o.PaymentID, OrderID: o.ID, Key: "prepare/" + o.ID, Amount: o.Total})
	a.finish(w, r, o, err)
}

func (a *app) component(w http.ResponseWriter, r *http.Request, o order, s session) {
	if r.Header.Get("Sec-Fetch-Dest") != "iframe" {
		http.Error(w, "open the order checkout", 403)
		return
	}
	// The response sandbox also applies when this document is opened directly.
	// No top-navigation, external handoff, download or popup permission.
	csp := "sandbox allow-forms allow-scripts allow-same-origin; default-src 'none'; frame-ancestors 'self'; base-uri 'none'; script-src 'self' " + cdnOrigin + "; style-src 'unsafe-inline' " + cdnOrigin + "; img-src data: " + shopperOrigin + " " + cdnOrigin + "; connect-src 'self' " + shopperOrigin + "; frame-src " + shopperOrigin + " " + a.card.acsOrigin + "; form-action " + shopperOrigin + " " + a.card.acsOrigin
	w.Header().Set("Content-Security-Policy", csp)
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, componentHTML)
}
func (a *app) cardState(w http.ResponseWriter, r *http.Request, o order, s session) {
	p, err := a.cardPayment(r.Context(), o)
	action := a.cardAction(p)
	if err != nil || action.Type == "" {
		http.Error(w, "card action unavailable", 409)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(struct {
		ClientKey, CSRF string
		Version         uint64
		ExpiresAt       time.Time
		Card            adyen.CardAction
		Amount          tender.Money
	}{a.card.clientKey, s.csrf, p.Version, p.Action.ExpiresAt, action, o.Total})
}

// Keep the submitted method transient and validate the nested wire contract
// before reserving an engine operation.
type browserCard struct {
	PaymentMethod struct {
		Type                  string `json:"type"`
		Brand                 string `json:"brand"`
		HolderName            string `json:"holderName,omitempty"`
		EncryptedCardNumber   string `json:"encryptedCardNumber"`
		EncryptedExpiryMonth  string `json:"encryptedExpiryMonth"`
		EncryptedExpiryYear   string `json:"encryptedExpiryYear"`
		EncryptedSecurityCode string `json:"encryptedSecurityCode"`
		CheckoutAttemptID     string `json:"checkoutAttemptId,omitempty"`
	} `json:"paymentMethod"`
	BrowserInfo struct {
		AcceptHeader      string `json:"acceptHeader"`
		Language          string `json:"language"`
		UserAgent         string `json:"userAgent"`
		ColorDepth        int    `json:"colorDepth"`
		ScreenHeight      int    `json:"screenHeight"`
		ScreenWidth       int    `json:"screenWidth"`
		JavaEnabled       *bool  `json:"javaEnabled"`
		JavaScriptEnabled *bool  `json:"javaScriptEnabled,omitempty"`
		TimeZoneOffset    *int   `json:"timeZoneOffset"`
	} `json:"browserInfo"`
}

var cardJWE = regexp.MustCompile(`^eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]*\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+$`)

func (a *app) confirmCard(w http.ResponseWriter, r *http.Request, o order, s session) {
	p, err := a.cardPayment(r.Context(), o)
	action := a.cardAction(p)
	if err != nil || action.Type == "" {
		http.Error(w, "card action unavailable", 409)
		return
	}
	var token string
	if a.scope.Environment == tender.EnvironmentMock {
		if r.PathValue("version") != strconv.FormatUint(p.Version, 10) {
			http.Error(w, "stale card step", 409)
			return
		}
		if _, ok := a.form(w, r, s, false); !ok {
			return
		}
		if action.Type == "card" {
			token = strings.Replace(syntheticCard, "BRAND", action.Brands[0], 1)
		} else {
			raw, _ := json.Marshal(map[string]any{"details": map[string]string{"threeDSResult": action.Action.Token}})
			token = string(raw)
		}
	} else {
		if r.URL.RawQuery != "" || r.Header.Get("Origin") != a.origin || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(s.csrf)) != 1 {
			http.Error(w, "request rejected", 403)
			return
		}
		if r.Header.Get("Content-Type") != "application/json" || r.Header.Get("X-Action-Version") != strconv.FormatUint(p.Version, 10) {
			http.Error(w, "stale card step", 409)
			return
		}
		token, err = a.browserToken(w, r, action.Type)
		if err != nil {
			http.Error(w, "invalid card step", 400)
			return
		}
	}
	digest := sha256.Sum256([]byte(token))
	result, err := a.engine.Confirm(r.Context(), tender.Request{PaymentID: o.PaymentID, Amount: o.Total, Key: fmt.Sprintf("card/%s/%d/%x", o.ID, p.Version, digest[:12]), Token: token})
	if errors.Is(err, tender.ErrDeclined) && result.Payment.Status == tender.PaymentDeclined {
		err = nil
	}
	if a.scope.Environment == tender.EnvironmentMock {
		a.finish(w, r, o, err)
		return
	}
	if err != nil {
		http.Error(w, "refresh the order for its current status", 409)
		return
	}
	code := result.Payment.ProviderStatus
	if a.cardAction(result.Payment).Type == "threeDS2" {
		code = "ChallengeShopper"
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"resultCode": code})
}

// Decode only the browser fields that may be forwarded. The edge proxy must
// overwrite X-Tender-Client-IP with the observed client IP on every request.
func (a *app) browserToken(w http.ResponseWriter, r *http.Request, kind string) (string, error) {
	body := http.MaxBytesReader(w, r.Body, 120<<10)
	var data any
	if kind == "threeDS2" {
		var v struct {
			Details struct {
				Result string `json:"threeDSResult"`
			} `json:"details"`
		}
		if !decodeStrict(body, &v) || v.Details.Result == "" || len(v.Details.Result) > 50000 {
			return "", tender.ErrValidation
		}
		data = v
	} else {
		var v browserCard
		if !decodeStrict(body, &v) {
			return "", tender.ErrValidation
		}
		method := v.PaymentMethod
		for _, field := range []string{method.EncryptedCardNumber, method.EncryptedExpiryMonth, method.EncryptedExpiryYear, method.EncryptedSecurityCode} {
			if len(field) > 15000 || !cardJWE.MatchString(field) {
				return "", tender.ErrValidation
			}
		}
		peer, _, err := net.SplitHostPort(r.RemoteAddr)
		peerIP, e := netip.ParseAddr(peer)
		ip, ipErr := netip.ParseAddr(r.Header.Get("X-Tender-Client-IP"))
		if err != nil || e != nil || !peerIP.Unmap().IsLoopback() || ipErr != nil {
			return "", tender.ErrValidation
		}
		data = map[string]any{"paymentMethod": v.PaymentMethod, "browserInfo": v.BrowserInfo, "shopperEmail": a.card.shopperEmail, "shopperIP": ip.String()}
	}
	raw, err := json.Marshal(data)
	return string(raw), err
}

const syntheticCard = `{"paymentMethod":{"type":"scheme","brand":"BRAND","holderName":"Fixture","encryptedCardNumber":"eyJfixture..a.b.c","encryptedExpiryMonth":"eyJfixture..a.b.c","encryptedExpiryYear":"eyJfixture..a.b.c","encryptedSecurityCode":"eyJfixture..a.b.c"},"browserInfo":{"acceptHeader":"*/*","colorDepth":24,"javaEnabled":false,"language":"en-US","screenHeight":900,"screenWidth":1200,"timeZoneOffset":0,"userAgent":"Loopback fixture"},"shopperEmail":"fixture@example.invalid","shopperIP":"127.0.0.1"}`

// The operator token is never given to the shopper. Each sample operation is
// full-amount and has a stable host key for safe retries.
func (a *app) operator(w http.ResponseWriter, r *http.Request) {
	if a.card.operatorKey == "" || r.Header.Get("Origin") != "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+a.card.operatorKey)) != 1 {
		http.NotFound(w, r)
		return
	}
	for _, o := range orders {
		if r.PathValue("order") != o.ID {
			continue
		}
		op := r.PathValue("operation")
		req := tender.Request{PaymentID: o.PaymentID, Amount: o.Total, Key: op + "/" + o.ID}
		var err error
		switch op {
		case "capture":
			_, err = a.engine.Capture(r.Context(), req)
		case "void":
			_, err = a.engine.Void(r.Context(), req)
		case "refund":
			_, err = a.engine.Refund(r.Context(), req)
		default:
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, "inspect the stored operation before retrying", 409)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		return
	}
	http.NotFound(w, r)
}
