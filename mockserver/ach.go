package mockserver

import (
	"bytes"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// ACHFixture supplies synthetic domestic consumer validation. The consent
// endpoint accepts one fixed decision and an empty body, with no bank form.
type ACHFixture struct {
	MerchantAccount, APIKey, WebhookURL string
	ValidationRefused, PendingResult    bool
}
type achFixtureServer struct {
	*Server
	cfg       ACHFixture
	decisions sync.Mutex
}
type achOrder struct {
	ID, Reference, PSP, Digest string
	Amount                     adyenFixtureAmount
	Expires                    time.Time
}

func ACHRoutes(c ACHFixture) func(*Server) http.Handler {
	return func(s *Server) http.Handler {
		if c.APIKey == "" || c.MerchantAccount == "" || c.WebhookURL != "" && (!s.targets[c.WebhookURL] || len(s.key) != 32) {
			return nil
		}
		f := &achFixtureServer{Server: s, cfg: c}
		mux := http.NewServeMux()
		mux.Handle("POST /v72/sessions", s.Route("ach.sessions", f.create))
		mux.Handle("GET /v72/sessions/{id}", s.Route("ach.result", f.result))
		mux.Handle("POST /mock/consent/{id}/{decision}", s.Route("ach.consent", f.consent))
		mux.Handle("POST /mock/return/{id}", s.Route("ach.return", f.chargeback))
		mux.HandleFunc("GET /mock/report/{id}/{type}", f.report)
		return mux
	}
}
func achHash(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }

func (f *achFixtureServer) create(w http.ResponseWriter, r *http.Request, _ Scenario) {
	if r.Header.Get("X-API-Key") != f.cfg.APIKey {
		w.WriteHeader(401)
		return
	}
	key := r.Header.Get("Idempotency-Key")
	var req struct {
		MerchantAccount, Reference, CountryCode, Channel, ShopperInteraction, ReturnURL, Mode string
		Amount                                                                                adyenFixtureAmount
		AllowedPaymentMethods                                                                 []string
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8192))
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err != nil || key == "" || len(key) > 64 || dec.Decode(&req) != nil || dec.Decode(&struct{}{}) != io.EOF || req.MerchantAccount != f.cfg.MerchantAccount || req.Reference == "" || req.CountryCode != "US" || req.Channel != "Web" || req.ShopperInteraction != "Ecommerce" || req.ReturnURL == "" || req.Mode != "embedded" || req.Amount.Currency != "USD" || req.Amount.Value <= 0 || !slices.Equal(req.AllowedPaymentMethods, []string{"ach"}) {
		fixtureInvalid(w)
		return
	}
	id := "CS" + achHash(fmt.Sprintf("%d/%s/%s", f.seed, req.MerchantAccount, key))[:32]
	p := achOrder{ID: id, PSP: strings.ToUpper(achHash(id + "payment")[:16]), Reference: req.Reference, Digest: achHash(string(raw)), Amount: req.Amount, Expires: f.clock().Add(time.Hour).UTC()}
	data, _ := json.Marshal(p)
	stored := f.Commit("ach/session/"+id, data)
	var original achOrder
	if json.Unmarshal(stored.Data, &original) != nil || original.Digest != p.Digest {
		fixtureInvalid(w)
		return
	}
	w.Header().Set("Idempotency-Key", key)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(201)
	json.NewEncoder(w).Encode(map[string]any{"id": id, "sessionData": "mock_data_" + id, "reference": original.Reference, "amount": original.Amount, "merchantAccount": req.MerchantAccount, "expiresAt": original.Expires, "mode": "embedded"})
}

func (f *achFixtureServer) order(id string) (achOrder, bool) {
	var p achOrder
	rec, ok := f.Lookup("ach/session/" + id)
	ok = ok && json.Unmarshal(rec.Data, &p) == nil
	return p, ok
}

func (f *achFixtureServer) consent(w http.ResponseWriter, r *http.Request, sc Scenario) {
	if r.ContentLength != 0 {
		w.WriteHeader(400)
		return
	}
	p, ok := f.order(r.PathValue("id"))
	decision := r.PathValue("decision")
	if !ok || decision != "approve" && decision != "deny" {
		http.NotFound(w, r)
		return
	}
	f.decisions.Lock()
	defer f.decisions.Unlock()
	key := "ach/consent/" + p.ID
	prev, exists := f.Lookup(key)
	if exists && string(prev.Data) != decision {
		w.WriteHeader(409)
		return
	}
	if !exists && !p.Expires.After(f.clock()) {
		w.WriteHeader(410)
		return
	}
	f.Commit(key, []byte(decision))
	if decision == "approve" && f.cfg.WebhookURL != "" {
		if _, queued := f.Lookup(key + "/queued"); !queued {
			success := "true"
			if f.cfg.ValidationRefused {
				success = "false"
			}
			if err := f.send(p, p.PSP, "", "AUTHORISATION", success, sc); err != nil {
				w.WriteHeader(503)
				return
			}
			f.Commit(key+"/queued", nil)
		}
	}
	json.NewEncoder(w).Encode(map[string]string{"sessionResult": "mr_" + achHash(p.ID+decision)})
}
func (f *achFixtureServer) send(p achOrder, psp, original, code, success string, sc Scenario) error {
	body := f.standardNotification(f.cfg.MerchantAccount, p.Reference, p.Amount.Value, p.Amount.Currency, psp, original, code, success)
	_, err := f.Enqueue([]Callback{{Target: f.cfg.WebhookURL, Body: body}}, sc)
	return err
}
func (f *achFixtureServer) result(w http.ResponseWriter, r *http.Request, _ Scenario) {
	if r.Header.Get("X-API-Key") != f.cfg.APIKey {
		w.WriteHeader(401)
		return
	}
	p, ok := f.order(r.PathValue("id"))
	decision, exists := f.Lookup("ach/consent/" + p.ID)
	if !ok || !exists || r.URL.Query().Get("sessionResult") != "mr_"+achHash(p.ID+string(decision.Data)) {
		w.WriteHeader(403)
		return
	}
	reply := map[string]any{"id": p.ID, "reference": p.Reference}
	switch {
	case string(decision.Data) == "deny":
		reply["status"] = "canceled"
	case f.cfg.ValidationRefused:
		reply["status"] = "refused"
	case f.cfg.PendingResult:
		reply["status"] = "paymentPending"
	default:
		reply["status"] = "completed"
		reply["payments"] = []any{map[string]any{"pspReference": p.PSP, "amount": p.Amount, "resultCode": "Authorised", "paymentMethod": map[string]string{"type": "ach"}}}
	}
	json.NewEncoder(w).Encode(reply)
}
func (f *achFixtureServer) approved(id string) (achOrder, bool) {
	p, ok := f.order(id)
	decision, exists := f.Lookup("ach/consent/" + id)
	return p, ok && exists && string(decision.Data) == "approve" && !f.cfg.ValidationRefused
}
func (f *achFixtureServer) chargeback(w http.ResponseWriter, r *http.Request, sc Scenario) {
	p, ok := f.approved(r.PathValue("id"))
	if !ok || f.cfg.WebhookURL == "" {
		w.WriteHeader(409)
		return
	}
	psp := strings.ToUpper(achHash(p.ID + "chargeback")[:16])
	if err := f.send(p, psp, p.PSP, "CHARGEBACK", "true", sc); err != nil {
		w.WriteHeader(503)
		return
	}
	f.Commit("ach/return/"+p.ID, []byte(psp))
	w.WriteHeader(202)
}
func (f *achFixtureServer) report(w http.ResponseWriter, r *http.Request) {
	p, ok := f.approved(r.PathValue("id"))
	kind := r.PathValue("type")
	if !ok || kind != "Settled" && kind != "Chargeback" {
		w.WriteHeader(404)
		return
	}
	modification := strings.ToUpper(achHash(p.ID + "settlement")[:16])
	decimal := func(v int64) string { return fmt.Sprintf("%d.%02d", v/100, v%100) }
	var credit, debit, netCredit, netDebit string
	if kind == "Settled" {
		credit = decimal(p.Amount.Value)
		netCredit = decimal(p.Amount.Value - min(p.Amount.Value, 25))
	} else {
		rec, ok := f.Lookup("ach/return/" + p.ID)
		if !ok {
			w.WriteHeader(409)
			return
		}
		modification = string(rec.Data)
		debit = decimal(p.Amount.Value)
		netDebit = debit
	}
	w.Header().Set("Content-Type", "text/csv")
	csvw := csv.NewWriter(w)
	csvw.Write([]string{"Merchant Account", "Psp Reference", "Merchant Reference", "Payment Method", "Type", "Modification Reference", "Gross Currency", "Gross Debit (GC)", "Gross Credit (GC)", "Net Currency", "Net Debit (NC)", "Net Credit (NC)", "Batch Number"})
	csvw.Write([]string{f.cfg.MerchantAccount, p.PSP, p.Reference, "ach", kind, modification, "USD", debit, credit, "USD", netDebit, netCredit, "7"})
	csvw.Flush()
}
