package mockserver

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// PayPalFixture emulates direct merchant Orders v2, Payments v2, OAuth and
// signature-verification postbacks. Buyer decisions exist only on this fixture.
type PayPalFixture struct{ MerchantAccount, ClientID, Secret, WebhookID, WebhookURL string }
type PayPalRecord struct {
	Route, OrderID, ResourceID, MerchantAccount, Value, Currency string
	Reply                                                        json.RawMessage
}

func PayPalKey(account, key string) string {
	return fmt.Sprintf("paypal/MOCK/%d/%s/%s", len(account), account, key)
}

type ppFixture struct {
	*Server
	cfg PayPalFixture
}
type ppMoney struct {
	Value    string `json:"value"`
	Currency string `json:"currency_code"`
}
type ppPayee struct {
	Merchant string `json:"merchant_id"`
}
type ppCapture struct {
	ID     string  `json:"id"`
	Status string  `json:"status"`
	Amount ppMoney `json:"amount"`
	Final  bool    `json:"final_capture"`
}
type ppUnit struct {
	Ref      string  `json:"reference_id"`
	Custom   string  `json:"custom_id"`
	Money    ppMoney `json:"amount"`
	Payee    ppPayee `json:"payee"`
	Payments *struct {
		Captures []ppCapture `json:"captures"`
	} `json:"payments,omitempty"`
}
type ppOrder struct {
	ID     string         `json:"id"`
	Intent string         `json:"intent"`
	Status string         `json:"status"`
	Source map[string]any `json:"payment_source"`
	Units  []ppUnit       `json:"purchase_units"`
}

func PayPalRoutes(c PayPalFixture) func(*Server) http.Handler {
	return func(s *Server) http.Handler {
		if c.ClientID == "" || c.Secret == "" || c.MerchantAccount == "" || c.WebhookID == "" || c.WebhookURL != "" && !s.targets[c.WebhookURL] {
			return nil
		}
		f := &ppFixture{s, c}
		mux := http.NewServeMux()
		mux.HandleFunc("POST /v1/oauth2/token", f.oauth)
		mux.HandleFunc("POST /fixture/orders/{id}/{decision}", f.buyer)
		mux.HandleFunc("POST /fixture/capture-next/{status}", f.captureNext)
		mux.HandleFunc("POST /fixture/orders/{id}/capture-status/{status}", f.captureStatus)
		for route, path := range map[string]string{"create": "POST /v2/checkout/orders", "order": "GET /v2/checkout/orders/{id}", "capture": "POST /v2/checkout/orders/{id}/capture", "refund": "POST /v2/payments/captures/{id}/refund", "showRefund": "GET /v2/payments/refunds/{id}", "verify": "POST /v1/notifications/verify-webhook-signature"} {
			mux.Handle(path, s.Route("paypal."+route, func(w http.ResponseWriter, r *http.Request, step Scenario) { f.serve(w, r, route, step) }))
		}
		return mux
	}
}
func ppID(prefix, key string) string {
	sum := sha256.Sum256([]byte(prefix + key))
	return prefix + strings.ToUpper(hex.EncodeToString(sum[:8]))
}
func ppJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
func (f *ppFixture) oauth(w http.ResponseWriter, r *http.Request) {
	id, secret, ok := r.BasicAuth()
	r.Body = http.MaxBytesReader(w, r.Body, 2048)
	if !ok || id != f.cfg.ClientID || secret != f.cfg.Secret || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" || r.ParseForm() != nil || r.PostForm.Get("grant_type") != "client_credentials" {
		w.WriteHeader(401)
		return
	}
	now := f.clock()
	token := ppID("TOKEN", f.cfg.ClientID+now.UTC().Format(time.RFC3339Nano))
	f.Commit("paypal/bearer/"+token, nil)
	ppJSON(w, map[string]any{"access_token": token, "token_type": "Bearer", "expires_in": 60})
}
func (f *ppFixture) readOrder(id string) (ppOrder, bool) {
	rec, ok := f.records["paypal/order/"+id]
	var o ppOrder
	if !ok || json.Unmarshal(rec.Data, &o) != nil {
		return ppOrder{}, false
	}
	return o, true
}
func (f *ppFixture) saveOrder(o ppOrder) {
	raw, _ := json.Marshal(o)
	key := "paypal/order/" + o.ID
	f.records[key] = Record{Key: key, ID: o.ID, Data: raw, At: f.clock()}
}
func (f *ppFixture) buyer(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Content-Type") != "application/json" {
		w.WriteHeader(415)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.readOrder(r.PathValue("id"))
	if ok && o.Status == "APPROVED" && r.PathValue("decision") == "approve" {
		w.WriteHeader(204)
		return
	}
	if !ok || o.Status != "PAYER_ACTION_REQUIRED" {
		w.WriteHeader(409)
		return
	}
	switch r.PathValue("decision") {
	case "cancel": // Closing the buyer UI makes no change at PayPal.
	case "approve":
		o.Status = "APPROVED"
		o.Source = map[string]any{"paypal": map[string]string{"account_id": "MOCKBUYER"}}
		f.saveOrder(o)
	default:
		w.WriteHeader(400)
		return
	}
	w.WriteHeader(204)
}
func (f *ppFixture) captureNext(w http.ResponseWriter, r *http.Request) {
	status := r.PathValue("status")
	if status != "PENDING" && status != "DENIED" {
		w.WriteHeader(400)
		return
	}
	f.mu.Lock()
	f.records["paypal/capture-next"] = Record{Key: "paypal/capture-next", Data: []byte(status)}
	f.mu.Unlock()
	w.WriteHeader(204)
}
func (f *ppFixture) captureStatus(w http.ResponseWriter, r *http.Request) {
	status := r.PathValue("status")
	if status != "COMPLETED" && status != "PENDING" && status != "DENIED" {
		w.WriteHeader(400)
		return
	}
	f.mu.Lock()
	o, ok := f.readOrder(r.PathValue("id"))
	if !ok || len(o.Units) != 1 || o.Units[0].Payments == nil || len(o.Units[0].Payments.Captures) != 1 {
		f.mu.Unlock()
		w.WriteHeader(409)
		return
	}
	c := &o.Units[0].Payments.Captures[0]
	if status == "DENIED" {
		c.Status = "DECLINED"
	} else {
		c.Status = status
	}
	fx := PayPalRecord{OrderID: o.ID, ResourceID: c.ID, MerchantAccount: f.cfg.MerchantAccount, Value: c.Amount.Value, Currency: c.Amount.Currency}
	f.saveOrder(o)
	f.mu.Unlock()
	f.notifyStatus(fx, c.Status, Scenario{})
	w.WriteHeader(204)
}
func (f *ppFixture) serve(w http.ResponseWriter, r *http.Request, route string, step Scenario) {
	auth := r.Header.Get("Authorization")
	rec, ok := f.Lookup("paypal/bearer/" + strings.TrimPrefix(auth, "Bearer "))
	if !strings.HasPrefix(auth, "Bearer ") || !ok || !f.clock().Before(rec.At.Add(time.Minute)) {
		w.WriteHeader(401)
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 512<<10))
	if err != nil {
		w.WriteHeader(400)
		return
	}
	switch route {
	case "verify":
		f.verify(w, raw)
		return
	case "order":
		f.mu.Lock()
		o, ok := f.readOrder(r.PathValue("id"))
		f.mu.Unlock()
		if !ok {
			w.WriteHeader(404)
			return
		}
		ppJSON(w, o)
		return
	case "showRefund":
		rec, ok := f.Lookup("paypal/refund/" + r.PathValue("id"))
		if !ok {
			w.WriteHeader(404)
			return
		}
		ppJSON(w, json.RawMessage(rec.Data))
		return
	}
	key := r.Header.Get("PayPal-Request-Id")
	if key == "" || len(key) > 38 || r.Header.Get("Prefer") != "return=representation" {
		w.WriteHeader(400)
		return
	}
	fx, first, ok := f.mutate(route, r.PathValue("id"), key, raw)
	if !ok {
		w.WriteHeader(422)
		return
	}
	if first && route == "capture" && f.cfg.WebhookURL != "" {
		status := "COMPLETED"
		var o ppOrder
		if json.Unmarshal(fx.Reply, &o) == nil && len(o.Units) == 1 && o.Units[0].Payments != nil && len(o.Units[0].Payments.Captures) == 1 {
			if o.Units[0].Payments.Captures[0].Status == "PENDING" {
				status = "PENDING"
			}
			if o.Units[0].Payments.Captures[0].Status == "DECLINED" {
				status = "DECLINED"
			}
		}
		f.notifyStatus(fx, status, step)
	}
	ppJSON(w, fx.Reply)
}
func ppCents(m ppMoney) (int64, bool) {
	whole, part, ok := strings.Cut(m.Value, ".")
	if !ok || m.Currency != "USD" || len(part) != 2 || len(whole) > 16 {
		return 0, false
	}
	for _, c := range whole + part {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(whole+part, 10, 64)
	return n, err == nil && n > 0
}
func (f *ppFixture) mutate(route, id, key string, raw []byte) (PayPalRecord, bool, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := PayPalKey(f.cfg.MerchantAccount, key)
	var fx PayPalRecord
	if rec, ok := f.records[k]; ok {
		if json.Unmarshal(rec.Data, &fx) != nil || fx.Route != route {
			return fx, false, false
		}
		// A duplicate returns the current state of the original resource and
		// never reapplies the operation or sends another notification.
		if route == "refund" {
			ref, found := f.records["paypal/refund/"+fx.ResourceID]
			fx.Reply = ref.Data
			return fx, false, found
		}
		o, found := f.readOrder(fx.OrderID)
		if !found {
			return fx, false, false
		}
		fx.Reply, _ = json.Marshal(o)
		return fx, false, true
	}
	fx.Route = route
	fx.MerchantAccount = f.cfg.MerchantAccount
	var o ppOrder
	switch route {
	case "create":
		if json.Unmarshal(raw, &o) != nil || o.Intent != "CAPTURE" || len(o.Source) != 1 || o.Source["paypal"] == nil || len(o.Units) != 1 {
			return fx, false, false
		}
		u := o.Units[0]
		if _, ok := ppCents(u.Money); !ok || u.Payee.Merchant != f.cfg.MerchantAccount || u.Ref == "" || u.Custom == "" {
			return fx, false, false
		}
		o.ID = ppID("O", k)
		o.Status = "PAYER_ACTION_REQUIRED"
		fx.Value, fx.Currency = u.Money.Value, u.Money.Currency
		fx.Reply, _ = json.Marshal(o)
	case "capture":
		var ok bool
		o, ok = f.readOrder(id)
		if !ok || o.Status != "APPROVED" || !bytes.Equal(raw, []byte("{}")) {
			return fx, false, false
		}
		u := &o.Units[0]
		fx.ResourceID = ppID("C", k)
		fx.Value, fx.Currency = u.Money.Value, u.Money.Currency
		o.Status = "COMPLETED"
		captureStatus := "COMPLETED"
		if next, found := f.records["paypal/capture-next"]; found {
			captureStatus = string(next.Data)
			delete(f.records, "paypal/capture-next")
		}
		if captureStatus == "DENIED" {
			captureStatus = "DECLINED"
		}
		u.Payments = &struct {
			Captures []ppCapture `json:"captures"`
		}{[]ppCapture{{fx.ResourceID, captureStatus, u.Money, true}}}
		fx.Reply, _ = json.Marshal(o)
		f.commitLocked("paypal/capture/"+fx.ResourceID, []byte(o.ID))
	case "refund":
		rec, ok := f.records["paypal/capture/"+id]
		if !ok {
			return fx, false, false
		}
		o, ok = f.readOrder(string(rec.Data))
		if !ok {
			return fx, false, false
		}
		var refund struct {
			Amount ppMoney
			Custom string `json:"custom_id"`
		}
		if json.Unmarshal(raw, &refund) != nil || refund.Custom == "" {
			return fx, false, false
		}
		n, valid := ppCents(refund.Amount)
		total, _ := ppCents(o.Units[0].Money)
		balanceKey := "paypal/refunded/" + id
		past := int64(0)
		if b, ok := f.records[balanceKey]; ok {
			if json.Unmarshal(b.Data, &past) != nil {
				return fx, false, false
			}
		}
		if !valid || n > total-past {
			return fx, false, false
		}
		b, _ := json.Marshal(past + n)
		f.records[balanceKey] = Record{Key: balanceKey, Data: b}
		c := &o.Units[0].Payments.Captures[0]
		c.Status = "PARTIALLY_REFUNDED"
		if n == total-past {
			c.Status = "REFUNDED"
		}
		fx.ResourceID = ppID("R", k)
		fx.Value, fx.Currency = refund.Amount.Value, refund.Amount.Currency
		fx.Reply, _ = json.Marshal(map[string]any{"id": fx.ResourceID, "amount": refund.Amount, "status": "COMPLETED", "custom_id": refund.Custom, "links": []map[string]string{{"rel": "up", "method": "GET", "href": f.base + "/v2/payments/captures/" + id}}})
		f.commitLocked("paypal/refund/"+fx.ResourceID, fx.Reply)
	default:
		return fx, false, false
	}
	fx.OrderID = o.ID
	f.saveOrder(o)
	encoded, _ := json.Marshal(fx)
	f.commitLocked(k, encoded)
	return fx, true, true
}

// The local verifier emulates PayPal's remote postback decision with a secret
// known only to the simulator. It does not emit real PayPal RSA signatures.
func (f *ppFixture) signature(raw []byte, transmission, at string) string {
	m := hmac.New(sha256.New, f.key)
	fmt.Fprintf(m, "%s|%s|%s|", transmission, at, f.cfg.WebhookID)
	m.Write(raw)
	return base64.StdEncoding.EncodeToString(m.Sum(nil))
}
func (f *ppFixture) notifyStatus(fx PayPalRecord, status string, step Scenario) {
	eid := "WH-" + fx.ResourceID
	if status != "COMPLETED" {
		label := status
		if status == "DECLINED" {
			label = "DENIED"
		}
		eid = "WH-" + label + "-" + fx.ResourceID
	}
	at := f.clock().UTC().Format(time.RFC3339Nano)
	tid := "TX-" + eid
	eventStatus := status
	if status == "DECLINED" {
		eventStatus = "DENIED"
	}
	raw, _ := json.Marshal(map[string]any{"id": eid, "resource_type": "capture", "event_type": "PAYMENT.CAPTURE." + eventStatus, "resource": map[string]any{"id": fx.ResourceID, "status": status, "payee": ppPayee{fx.MerchantAccount}, "amount": ppMoney{fx.Value, fx.Currency}, "supplementary_data": map[string]any{"related_ids": map[string]string{"order_id": fx.OrderID}}}})
	h := make(http.Header)
	h.Set("PayPal-Auth-Algo", "SHA256withRSA")
	h.Set("PayPal-Cert-Url", f.base+"/v1/notifications/certs/CERT-MOCK")
	h.Set("PayPal-Transmission-Id", tid)
	h.Set("PayPal-Transmission-Time", at)
	h.Set("PayPal-Transmission-Sig", f.signature(raw, tid, at))
	_, _ = f.Enqueue([]Callback{{Target: f.cfg.WebhookURL, Header: h, Body: raw}}, step)
}
func (f *ppFixture) verify(w http.ResponseWriter, raw []byte) {
	var in struct {
		Algo    string          `json:"auth_algo"`
		Cert    string          `json:"cert_url"`
		ID      string          `json:"transmission_id"`
		Sig     string          `json:"transmission_sig"`
		Time    string          `json:"transmission_time"`
		Webhook string          `json:"webhook_id"`
		Event   json.RawMessage `json:"webhook_event"`
	}
	result := "FAILURE"
	if len(f.key) > 0 && json.Unmarshal(raw, &in) == nil && in.Algo == "SHA256withRSA" && in.Cert == f.base+"/v1/notifications/certs/CERT-MOCK" && in.Webhook == f.cfg.WebhookID && hmac.Equal([]byte(in.Sig), []byte(f.signature(in.Event, in.ID, in.Time))) {
		result = "SUCCESS"
	}
	ppJSON(w, map[string]string{"verification_status": result})
}
