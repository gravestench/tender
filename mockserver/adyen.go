package mockserver

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"time"
)

// AdyenFixture describes synthetic Checkout v72 card responses. Set PaymentResult
// or DetailsResult to Authorised (default), Refused, Pending, IdentifyShopper,
// ChallengeShopper, RedirectShopper or UnknownAction. WebhookURL must be an
// allowed callback target; SigningKey is the decoded test HMAC key.
type AdyenFixture struct {
	CompanyAccount                                                    string
	MerchantAccount, APIKey, PaymentResult, DetailsResult, WebhookURL string
	Brands                                                            []string
}

// AdyenRecord holds the committed wire references and amount, without card
// data or credentials. Digest detects changed requests under the same key.
type AdyenRecord struct {
	Route, Reference, PaymentPSP, PSP, Currency, Result, Digest, Brand, MerchantAccount string
	Brands                                                                              []string
	Value                                                                               int64
}

func AdyenKey(account, key string) string {
	return fmt.Sprintf("adyen/MOCK/%d/%s/%s", len(account), account, key)
}

type adyenFixtureAmount struct {
	Currency string `json:"currency"`
	Value    int64  `json:"value"`
}
type adyenFixtureRequest struct {
	MerchantAccount, Reference, Channel, CountryCode, Origin, ReturnURL, PaymentData string
	Amount                                                                           adyenFixtureAmount
	AllowedPaymentMethods                                                            []string
	PaymentMethod                                                                    struct{ Type, Brand, EncryptedCardNumber, EncryptedExpiryMonth, EncryptedExpiryYear, EncryptedSecurityCode string }
	BrowserInfo                                                                      struct{ UserAgent string }
	AdditionalData                                                                   struct{ ManualCapture string }
	AuthenticationData                                                               struct {
		ThreeDSRequestData struct{ NativeThreeDS string }
	}
	Details struct{ ThreeDSResult string }
}
type adyenFixtureServer struct {
	*Server
	config AdyenFixture
}

func AdyenRoutes(c AdyenFixture) func(*Server) http.Handler {
	if c.CompanyAccount == "" {
		c.CompanyAccount = c.MerchantAccount
	}
	c.Brands = slices.Clone(c.Brands)
	return func(s *Server) http.Handler {
		if c.MerchantAccount == "" || c.APIKey == "" || c.WebhookURL != "" && (!s.targets[c.WebhookURL] || len(s.key) != 32) {
			return nil
		}
		f := &adyenFixtureServer{Server: s, config: c}
		mux := http.NewServeMux()
		routes := map[string]string{
			"/v72/paymentMethods": "paymentMethods", "/v72/payments": "payments", "/v72/payments/details": "details",
			"/v72/payments/{psp}/captures": "captures", "/v72/payments/{psp}/cancels": "cancels", "/v72/payments/{psp}/refunds": "refunds",
		}
		for path, route := range routes {
			mux.Handle("POST "+path, s.Route("adyen."+route, func(w http.ResponseWriter, r *http.Request, step Scenario) { f.serve(w, r, route, step) }))
		}
		return mux
	}
}
func (f *adyenFixtureServer) serve(w http.ResponseWriter, r *http.Request, route string, step Scenario) {
	w.Header().Set("Content-Type", "application/json")
	if r.Header.Get("X-API-Key") != f.config.APIKey {
		w.WriteHeader(401)
		return
	}
	key := r.Header.Get("Idempotency-Key")
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 256<<10))
	var req adyenFixtureRequest
	if err != nil || key == "" || len(key) > 64 || json.Unmarshal(raw, &req) != nil || route != "details" && req.MerchantAccount != f.config.MerchantAccount {
		fixtureInvalid(w)
		return
	}
	fx, first, ok := f.commit(route, r.PathValue("psp"), key, req, raw, r.URL.Path)
	if !ok {
		fixtureInvalid(w)
		return
	}
	w.Header().Set("Idempotency-Key", key)
	if first && f.config.WebhookURL != "" {
		if notification := f.notification(fx); notification != nil {
			_, err = f.Enqueue([]Callback{{Target: f.config.WebhookURL, Body: notification}}, step)
			if err != nil {
				w.WriteHeader(503)
				return
			}
		}
	}
	if fx.Route == "captures" || fx.Route == "refunds" || fx.Route == "cancels" {
		w.WriteHeader(201)
	}
	_ = json.NewEncoder(w).Encode(f.reply(fx))
}

// Commit the company-scoped idempotent response and the merchant ledger under
// one lock, including when multiple fixtures share this simulator.
func (f *adyenFixtureServer) commit(route, original, key string, req adyenFixtureRequest, raw []byte, path string) (AdyenRecord, bool, bool) {
	f.Server.mu.Lock()
	defer f.Server.mu.Unlock()
	companyKey := fmt.Sprintf("adyen/company/%d/%s/%s", len(f.config.CompanyAccount), f.config.CompanyAccount, key)
	if rec, found := f.Server.records[companyKey]; found {
		var fx AdyenRecord
		err := json.Unmarshal(rec.Data, &fx)
		return fx, false, err == nil
	}
	merchantKey := AdyenKey(f.config.MerchantAccount, key)
	digest := sha256.Sum256(append([]byte(path+"\x00"), raw...))
	psp := sha256.Sum256([]byte(fmt.Sprintf("%d/%s", f.seed, merchantKey)))
	fx := AdyenRecord{Route: route, Reference: req.Reference, PSP: strings.ToUpper(hex.EncodeToString(psp[:8])),
		Currency: req.Amount.Currency, Value: req.Amount.Value, Digest: hex.EncodeToString(digest[:]),
		MerchantAccount: f.config.MerchantAccount, Brands: f.config.Brands,
	}
	if !f.prepare(&fx, req, original) {
		return fx, false, false
	}
	data, _ := json.Marshal(fx)
	f.commitLocked(companyKey, data)
	f.commitLocked(merchantKey, data)
	return fx, true, true
}
func fixtureInvalid(w http.ResponseWriter) {
	w.WriteHeader(422)
	_, _ = io.WriteString(w, `{"status":422,"errorCode":"101","errorType":"validation"}`)
}
func (f *adyenFixtureServer) records() []AdyenRecord {
	var list []AdyenRecord
	for k, r := range f.Server.records {
		if strings.HasPrefix(k, AdyenKey(f.config.MerchantAccount, "")) {
			var s AdyenRecord
			if json.Unmarshal(r.Data, &s) == nil {
				list = append(list, s)
			}
		}
	}
	return list
}
func (f *adyenFixtureServer) prepare(fx *AdyenRecord, req adyenFixtureRequest, original string) bool {
	switch fx.Route {
	case "paymentMethods":
		return req.Amount.Currency == "USD" && req.Amount.Value > 0 && req.Channel == "Web" && req.CountryCode == "US" && slices.Equal(req.AllowedPaymentMethods, []string{"scheme"})
	case "payments":
		pm := req.PaymentMethod
		web := req.Origin != "" && req.ReturnURL != "" && req.Channel == "Web" && req.BrowserInfo.UserAgent != ""
		card := pm.Type == "scheme" && (pm.Brand == "" || slices.Contains(f.config.Brands, pm.Brand)) &&
			pm.EncryptedCardNumber != "" && pm.EncryptedExpiryMonth != "" && pm.EncryptedExpiryYear != "" && pm.EncryptedSecurityCode != ""
		if !web || !card || req.Reference == "" || req.Amount.Currency != "USD" || req.Amount.Value <= 0 ||
			req.AdditionalData.ManualCapture != "true" || req.AuthenticationData.ThreeDSRequestData.NativeThreeDS != "preferred" {
			return false
		}
		fx.Brand = pm.Brand
		fx.PaymentPSP = fx.PSP
		fx.Result = f.config.PaymentResult
	case "details":
		if req.Details.ThreeDSResult == "" {
			return false
		}
		for _, p := range f.records() {
			if p.Route == "payments" && (p.Result == "IdentifyShopper" || p.Result == "ChallengeShopper") && req.Details.ThreeDSResult == "mock-native-"+p.PSP && (req.PaymentData == "" || req.PaymentData == "mock-data-"+p.PSP) {
				fx.Brand = p.Brand
				fx.Reference = p.Reference
				fx.PaymentPSP = p.PSP
				fx.Currency = p.Currency
				fx.Value = p.Value
				break
			}
		}
		if fx.PaymentPSP == "" {
			return false
		}
		fx.Result = f.config.DetailsResult
	default:
		var auth, captured, refunded int64
		var voided bool
		var currency string
		for _, p := range f.records() {
			if p.PaymentPSP != original {
				continue
			}
			switch p.Route {
			case "payments", "details":
				if p.Result == "Authorised" {
					auth = p.Value
					currency = p.Currency
				}
			case "captures":
				captured += p.Value
			case "refunds":
				refunded += p.Value
			case "cancels":
				voided = true
			}
		}
		if auth == 0 || req.Reference == "" {
			return false
		}
		switch fx.Route {
		case "captures":
			if voided || fx.Value <= 0 || fx.Currency != currency || fx.Value > auth-captured {
				return false
			}
		case "refunds":
			if fx.Value <= 0 || fx.Currency != currency || fx.Value > captured-refunded {
				return false
			}
		case "cancels":
			if voided || captured == auth {
				return false
			}
			fx.Currency = currency
			fx.Value = auth - captured
		}
		fx.PaymentPSP = original
		fx.Result = "received"
		return true
	}
	if fx.Result == "" {
		fx.Result = "Authorised"
	}
	return true
}
func (f *adyenFixtureServer) reply(p AdyenRecord) any {
	amt := adyenFixtureAmount{p.Currency, p.Value}
	switch p.Route {
	case "paymentMethods":
		return map[string]any{"paymentMethods": []any{map[string]any{"name": "Cards", "type": "scheme", "brands": p.Brands}}}
	case "payments", "details":
		v := map[string]any{"resultCode": p.Result, "merchantReference": p.Reference}
		switch p.Result {
		case "IdentifyShopper", "ChallengeShopper":
			sub := "fingerprint"
			if p.Result == "ChallengeShopper" {
				sub = "challenge"
			}
			v["action"] = map[string]any{"type": "threeDS2", "subtype": sub, "paymentMethodType": "scheme", "token": "mock-native-" + p.PaymentPSP, "paymentData": "mock-data-" + p.PaymentPSP, "authorisationToken": "mock-auth-" + p.PaymentPSP}
		case "RedirectShopper":
			v["action"] = map[string]any{"type": "redirect", "url": "https://issuer.invalid/never-navigate", "method": "GET"}
		case "UnknownAction":
			v["action"] = map[string]any{"type": "fixture-unknown"}
		default:
			if p.Result == "Authorised" {
				v["paymentMethod"] = map[string]string{"type": "scheme", "brand": p.Brand}
			}
			v["pspReference"] = p.PaymentPSP
			v["amount"] = amt
		}
		return v
	default:
		v := map[string]any{"status": "received", "reference": p.Reference, "merchantAccount": p.MerchantAccount, "pspReference": p.PSP, "paymentPspReference": p.PaymentPSP}
		if p.Route != "cancels" {
			v["amount"] = amt
		}
		return v
	}
}
func (f *adyenFixtureServer) notification(p AdyenRecord) []byte {
	code := map[string]string{"captures": "CAPTURE", "refunds": "REFUND", "cancels": "CANCELLATION"}[p.Route]
	original, psp, success := p.PaymentPSP, p.PSP, "true"
	if p.Route == "payments" || p.Route == "details" {
		if p.Result != "Authorised" && p.Result != "Refused" {
			return nil
		}
		code = "AUTHORISATION"
		original = ""
		psp = p.PaymentPSP
		if p.Result == "Refused" {
			success = "false"
		}
	}
	if code == "" {
		return nil
	}
	return f.Server.standardNotification(f.config.MerchantAccount, p.Reference, p.Value, p.Currency, psp, original, code, success)
}

func (f *Server) standardNotification(account, reference string, value int64, currency, psp, original, code, success string) []byte {
	material := strings.Join([]string{psp, original, account, reference, strconv.FormatInt(value, 10), currency, code, success}, ":")
	mac := hmac.New(sha256.New, f.key)
	mac.Write([]byte(material))
	item := map[string]any{
		"pspReference": psp, "merchantAccountCode": account, "merchantReference": reference,
		"amount": adyenFixtureAmount{currency, value}, "eventCode": code, "success": success,
		"eventDate":      f.clock().UTC().Format(time.RFC3339Nano),
		"additionalData": map[string]string{"hmacSignature": base64.StdEncoding.EncodeToString(mac.Sum(nil))},
	}
	if original != "" {
		item["originalReference"] = original
	}
	body, _ := json.Marshal(map[string]any{"live": "false", "notificationItems": []any{map[string]any{"NotificationRequestItem": item}}})
	return body
}
