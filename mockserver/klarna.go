package mockserver

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
)

// KlarnaFixture supplies a single synthetic US/USD Klarna Payments merchant.
// CallbackURL pins the allowed loopback recipient; only its per-session proof
// and operation ID may vary. The mock approval route is never a real SDK.
type KlarnaFixture struct {
	MerchantAccount, APIKey, CallbackURL string
	Category, FraudStatus, FraudOnRead   string
	RejectOrder                          bool
}
type KlarnaRecord struct {
	Route, OrderID, Reference, Currency string
	Amount                              int64
}

func KlarnaKey(account, key string) string {
	return fmt.Sprintf("klarna:MOCK:%d:%s:%s", len(account), account, key)
}

type klarnaCart struct {
	Country  string            `json:"purchase_country"`
	Currency string            `json:"purchase_currency"`
	Locale   string            `json:"locale"`
	Amount   int64             `json:"order_amount"`
	Tax      int64             `json:"order_tax_amount"`
	Lines    []json.RawMessage `json:"order_lines"`
	Merchant string            `json:"merchant_reference1"`
	Binding  string            `json:"merchant_reference2"`
}
type klarnaSession struct {
	cart                   klarnaCart
	callback, token, order string
	approved               bool
}
type klarnaChange struct {
	CaptureID string `json:"capture_id,omitempty"`
	RefundID  string `json:"refund_id,omitempty"`
	Ref       string `json:"reference"`
	Captured  int64  `json:"captured_amount,omitempty"`
	Refunded  int64  `json:"refunded_amount,omitempty"`
}
type klarnaOrder struct {
	cart                          klarnaCart
	fraud, status                 string
	captured, refunded, remaining int64
	captures, refunds             []klarnaChange
}
type klarnaSimulator struct {
	s        *Server
	cfg      KlarnaFixture
	mu       sync.Mutex
	sessions map[string]*klarnaSession
	tokens   map[string]*klarnaSession
	orders   map[string]*klarnaOrder
}

func klarnaJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func klarnaError(w http.ResponseWriter, status int, code string) {
	klarnaJSON(w, status, map[string]string{"error_code": code})
}
func klarnaRead(w http.ResponseWriter, r *http.Request, v any) bool {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 512<<10))
	return err == nil && json.Unmarshal(b, v) == nil
}
func mockKlarnaID(key string) string {
	sum := sha256.Sum256([]byte(key))
	return fmt.Sprintf("%x-%x-%x-%x-%x", sum[:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
}

// KlarnaRoutes serves only the documented subset and an explicit synthetic
// buyer approval route. Mutations use independent wire parsing and state.
func KlarnaRoutes(cfg KlarnaFixture) func(*Server) http.Handler {
	return func(s *Server) http.Handler {
		cb, err := localURL(cfg.CallbackURL)
		if err != nil || cb.RawQuery != "" || cb.ForceQuery || cfg.APIKey == "" || cfg.MerchantAccount == "" {
			return nil
		}
		if cfg.Category == "" {
			cfg.Category = "pay_over_time"
		}
		if cfg.FraudStatus == "" {
			cfg.FraudStatus = "ACCEPTED"
		}
		k := &klarnaSimulator{s: s, cfg: cfg}
		mux := http.NewServeMux()
		route := func(pattern, name string, handler http.HandlerFunc) {
			mux.Handle(pattern, s.Route("klarna."+name, func(w http.ResponseWriter, r *http.Request, _ Scenario) {
				if name != "authorize" && r.Header.Get("Authorization") != "Basic "+cfg.APIKey {
					klarnaError(w, 401, "UNAUTHORIZED")
					return
				}
				k.mu.Lock()
				defer k.mu.Unlock()
				marker := KlarnaKey(cfg.MerchantAccount, "state")
				if _, ok := s.Lookup(marker); !ok {
					k.sessions = make(map[string]*klarnaSession)
					k.tokens = make(map[string]*klarnaSession)
					k.orders = make(map[string]*klarnaOrder)
					s.Commit(marker, nil)
				}
				handler(w, r)
			}))
		}
		route("POST /payments/v1/sessions", "sessions", k.session)
		route("POST /mock/authorize/{session}", "authorize", k.approve)
		route("POST /payments/v1/authorizations/{token}/order", "order", k.order)
		for _, name := range []string{"captures", "refunds", "release-remaining-authorization", "cancel"} {
			route("POST /ordermanagement/v1/orders/{order}/"+name, name, func(w http.ResponseWriter, r *http.Request) { k.modify(w, r, name) })
		}
		route("GET /ordermanagement/v1/orders/{order}", "read", k.read)
		return mux
	}
}
func (k *klarnaSimulator) session(w http.ResponseWriter, r *http.Request) {
	var req struct {
		klarnaCart
		Intent string            `json:"intent"`
		URLs   map[string]string `json:"merchant_urls"`
	}
	if !klarnaRead(w, r, &req) || req.Intent != "buy" || req.Country != "US" || req.Currency != "USD" || req.Locale != "en-US" || req.Amount <= 0 || len(req.Lines) == 0 || req.Merchant == "" || req.Binding == "" {
		klarnaError(w, 400, "BAD_VALUE")
		return
	}
	var total, tax int64
	for _, raw := range req.Lines {
		var line struct {
			Type     string `json:"type"`
			Quantity int64  `json:"quantity"`
			Price    int64  `json:"unit_price"`
			Amount   int64  `json:"total_amount"`
			Discount int64  `json:"total_discount_amount"`
			Name     string `json:"name"`
		}
		if json.Unmarshal(raw, &line) != nil || line.Name == "" || line.Quantity < 1 || line.Amount != line.Quantity*line.Price-line.Discount {
			klarnaError(w, 400, "BAD_VALUE")
			return
		}
		total += line.Amount
		if line.Type == "sales_tax" {
			tax += line.Amount
		}
	}
	if total != req.Amount || tax != req.Tax {
		klarnaError(w, 400, "BAD_VALUE")
		return
	}
	addr, err := url.Parse(req.URLs["authorization"])
	if err != nil {
		klarnaError(w, 400, "BAD_VALUE")
		return
	}
	query := addr.Query()
	addr.RawQuery = ""
	if addr.String() != k.cfg.CallbackURL || len(req.URLs) != 1 || len(query) != 2 || len(query["create"]) != 1 || len(query["proof"]) != 1 {
		klarnaError(w, 400, "BAD_VALUE")
		return
	}
	id := mockKlarnaID(fmt.Sprintf("%s/%s/%d", req.Binding, query.Get("create"), len(k.sessions)))
	token := mockKlarnaID("auth/" + id)
	s := &klarnaSession{cart: req.klarnaCart, callback: req.URLs["authorization"], token: token}
	k.sessions[id] = s
	k.tokens[token] = s
	klarnaJSON(w, 200, map[string]any{"session_id": id, "client_token": "mock-client-" + id, "payment_method_categories": []map[string]string{{"identifier": k.cfg.Category, "name": "Synthetic Klarna"}}})
}
func (k *klarnaSimulator) approve(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("session")
	session := k.sessions[id]
	if session == nil {
		w.WriteHeader(404)
		return
	}
	if k.cfg.Category != "pay_over_time" {
		w.WriteHeader(403)
		return
	}
	session.approved = true
	body, _ := json.Marshal(map[string]string{"session_id": id, "authorization_token": session.token})
	req, err := http.NewRequestWithContext(r.Context(), "POST", session.callback, bytes.NewReader(body))
	if err != nil {
		w.WriteHeader(500)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	// The exact configured callback endpoint was checked on session creation.
	// The simulator client independently restricts the socket to loopback.
	res, err := k.s.client.Do(req)
	if err != nil {
		w.WriteHeader(502)
		return
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		w.WriteHeader(502)
		return
	}
	w.WriteHeader(204)
}
func (k *klarnaSimulator) order(w http.ResponseWriter, r *http.Request) {
	s := k.tokens[r.PathValue("token")]
	if s == nil || !s.approved {
		klarnaError(w, 404, "NOT_FOUND")
		return
	}
	var req struct {
		klarnaCart
		Auto *bool `json:"auto_capture"`
	}
	if !klarnaRead(w, r, &req) || req.Auto == nil || *req.Auto || !reflect.DeepEqual(req.klarnaCart, s.cart) {
		klarnaError(w, 409, "CONFLICT")
		return
	}
	if k.cfg.RejectOrder {
		klarnaError(w, 403, "REJECTED")
		return
	}
	if s.order == "" {
		s.order = mockKlarnaID("order/" + s.token)
		k.orders[s.order] = &klarnaOrder{cart: s.cart, fraud: k.cfg.FraudStatus, status: "AUTHORIZED", remaining: s.cart.Amount}
	}
	klarnaJSON(w, 200, map[string]string{"order_id": s.order, "fraud_status": k.cfg.FraudStatus, "authorized_payment_method": "fixed_amount"})
}
func (k *klarnaSimulator) modify(w http.ResponseWriter, r *http.Request, route string) {
	id, key := r.PathValue("order"), r.Header.Get("Klarna-Idempotency-Key")
	ord := k.orders[id]
	if ord == nil {
		klarnaError(w, 404, "NO_SUCH_ORDER")
		return
	}
	if key == "" {
		klarnaError(w, 400, "BAD_REQUEST")
		return
	}
	var item struct {
		Captured int64  `json:"captured_amount"`
		Refunded int64  `json:"refunded_amount"`
		Ref      string `json:"reference"`
	}
	if route == "captures" || route == "refunds" {
		if !klarnaRead(w, r, &item) || item.Ref == "" {
			klarnaError(w, 400, "BAD_REQUEST")
			return
		}
	}
	recKey := KlarnaKey(k.cfg.MerchantAccount, key)
	if rec, ok := k.s.Lookup(recKey); ok {
		var prev KlarnaRecord
		_ = json.Unmarshal(rec.Data, &prev)
		k.modificationResponse(w, prev)
		return
	}
	rec := KlarnaRecord{OrderID: id, Route: route, Currency: ord.cart.Currency}
	changeID := mockKlarnaID(recKey)
	if ord.fraud != "ACCEPTED" {
		klarnaError(w, 403, "PERMISSION_DENIED")
		return
	}
	switch route {
	case "captures":
		if item.Captured <= 0 || item.Captured > ord.remaining {
			klarnaError(w, 403, "CAPTURE_NOT_ALLOWED")
			return
		}
		rec.Amount = item.Captured
		rec.Reference = changeID
		ord.captured += item.Captured
		ord.remaining -= item.Captured
		ord.status = "PART_CAPTURED"
		if ord.remaining == 0 {
			ord.status = "CAPTURED"
		}
		ord.captures = append(ord.captures, klarnaChange{CaptureID: changeID, Ref: item.Ref, Captured: item.Captured})
	case "refunds":
		if item.Refunded <= 0 || item.Refunded > ord.captured-ord.refunded {
			klarnaError(w, 403, "REFUND_NOT_ALLOWED")
			return
		}
		rec.Amount = item.Refunded
		rec.Reference = changeID
		ord.refunded += item.Refunded
		ord.refunds = append(ord.refunds, klarnaChange{RefundID: changeID, Ref: item.Ref, Refunded: item.Refunded})
	case "cancel", "release-remaining-authorization":
		if ord.remaining == 0 || (route == "cancel") != (ord.captured == 0) {
			klarnaError(w, 403, "CANCEL_NOT_ALLOWED")
			return
		}
		rec.Amount = ord.remaining
		ord.remaining = 0
		ord.status = "CANCELLED"
		if ord.captured > 0 {
			ord.status = "CAPTURED"
		}
	}
	raw, _ := json.Marshal(rec)
	k.s.Commit(recKey, raw)
	k.modificationResponse(w, rec)
}
func (k *klarnaSimulator) modificationResponse(w http.ResponseWriter, rec KlarnaRecord) {
	switch rec.Route {
	case "captures", "refunds":
		name := strings.TrimSuffix(rec.Route, "s")
		w.Header().Set(name+"-id", rec.Reference)
		w.Header().Set("Location", k.s.PublicBaseURL()+"/ordermanagement/v1/orders/"+rec.OrderID+"/"+rec.Route+"/"+rec.Reference)
		w.WriteHeader(201)
	default:
		w.WriteHeader(204)
	}
}
func (k *klarnaSimulator) read(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("order")
	o := k.orders[id]
	if o == nil {
		klarnaError(w, 404, "NO_SUCH_ORDER")
		return
	}
	if k.cfg.FraudOnRead != "" {
		o.fraud = k.cfg.FraudOnRead
	}
	klarnaJSON(w, 200, map[string]any{"order_id": id, "merchant_reference1": o.cart.Merchant, "merchant_reference2": o.cart.Binding, "purchase_currency": o.cart.Currency, "order_amount": o.cart.Amount, "original_order_amount": o.cart.Amount, "captured_amount": o.captured, "refunded_amount": o.refunded, "remaining_authorized_amount": o.remaining, "fraud_status": o.fraud, "status": o.status, "captures": o.captures, "refunds": o.refunds})
}
