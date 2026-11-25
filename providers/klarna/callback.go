package klarna

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"time"

	"github.com/gravestench/tender"
)

type authorization struct {
	ID         string    `json:"id"`
	Payment    string    `json:"payment"`
	Binding    string    `json:"binding"`
	Session    string    `json:"session"`
	Expires    time.Time `json:"expires"`
	Ciphertext []byte    `json:"sealed"`
}

func (r authorization) aad() []byte {
	b, _ := json.Marshal([]string{r.ID, r.Payment, r.Binding, r.Session, r.Expires.UTC().Format(time.RFC3339Nano)})
	return b
}
func (a *Adapter) mac(parts ...string) string {
	mac := hmac.New(sha256.New, a.cfg.CallbackKey)
	data, _ := json.Marshal(parts)
	mac.Write(data)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
func (a *Adapter) proof(p tender.Payment, op tender.Operation) string {
	return a.mac("callback-v1", binding(p), op.ID, op.ProviderKey)
}
func (a *Adapter) callbackURL(r tender.ProviderRequest) string {
	return a.cfg.CallbackURL + "?" + url.Values{"create": {r.Operation.ID}, "proof": {a.proof(r.Payment, r.Operation)}}.Encode()
}
func (a *Adapter) key(id string) tender.VerifiedEventKey {
	return tender.VerifiedEventKey{Scope: a.cfg.Scope, Provider: "klarna", MerchantAccount: a.cfg.MerchantAccount, ID: id}
}
func (a *Adapter) evidence(tx tender.Tx, fields map[string]string) ([]json.RawMessage, error) {
	reader, ok := tx.(tender.VerifiedEvidenceReader)
	if !ok {
		return nil, tender.ErrUnsupported
	}
	return reader.MatchingVerifiedEvidence(a.cfg.Scope, "klarna", a.cfg.MerchantAccount, fields)
}

// CallbackHandler persists an authenticated authorization without placing an
// order. Mount it at CallbackURL and redact the query and body from access logs.
func (a *Adapter) CallbackHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(405)
			return
		}
		q := r.URL.Query()
		create, proof := q.Get("create"), q.Get("proof")
		if !safeID.MatchString(create) || len(q["create"]) != 1 || len(q["proof"]) != 1 || len(proof) != 43 {
			w.WriteHeader(403)
			return
		}
		var payload struct {
			Session string `json:"session_id"`
			Token   string `json:"authorization_token"`
		}
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8192))
		if err != nil || json.Unmarshal(raw, &payload) != nil || !safeID.MatchString(payload.Session) || !safeID.MatchString(payload.Token) {
			w.WriteHeader(400)
			return
		}
		err = a.cfg.Store.Transact(r.Context(), func(tx tender.Tx) error {
			op, err := tx.Operation(a.cfg.Scope, create)
			if err != nil {
				return err
			}
			p, err := tx.Payment(a.cfg.Scope, op.PaymentID)
			if err != nil {
				return err
			}
			if !a.validRoute(p) || op.Kind != tender.OpCreate || op.AttemptID != p.AttemptID || !hmac.Equal([]byte(a.proof(p, op.Operation)), []byte(proof)) {
				return tender.ErrAuthentication
			}
			if op.ProviderReference == "" {
				return tender.ErrUnknownOutcome
			}
			if op.ProviderReference != payload.Session {
				return tender.ErrAuthentication
			}
			id := a.mac("receipt-v1", binding(p), payload.Session, payload.Token)
			exists, err := a.evidence(tx, map[string]string{"id": id, "payment": p.ID})
			if err != nil || len(exists) > 0 {
				return err
			}
			row := authorization{ID: id, Payment: p.ID, Binding: binding(p), Session: payload.Session, Expires: a.cfg.Now().UTC().Add(time.Hour)}
			row.Ciphertext = a.seal.Seal(nil, nil, []byte(payload.Token), row.aad())
			record, _ := json.Marshal(row)
			writer, ok := tx.(tender.VerifiedEventTx)
			if !ok {
				return tender.ErrUnsupported
			}
			return writer.InsertVerifiedEvent(a.key(id), record)
		})
		switch {
		case err == nil:
			w.WriteHeader(204)
		case errors.Is(err, tender.ErrAuthentication), errors.Is(err, tender.ErrNotFound):
			w.WriteHeader(403)
		default:
			w.WriteHeader(503)
		}
	})
}

// Authorizations lists durable, opaque receipt identifiers for a host payment.
// An identical callback has one ID. Reauthorization can produce another ID.
func (a *Adapter) Authorizations(ctx context.Context, payment string) ([]string, error) {
	var ids []string
	err := a.cfg.Store.Transact(ctx, func(tx tender.Tx) error {
		rows, err := a.evidence(tx, map[string]string{"payment": payment})
		if err != nil {
			return err
		}
		for _, raw := range rows {
			var item authorization
			if json.Unmarshal(raw, &item) != nil || item.ID == "" {
				return tender.ErrConflict
			}
			ids = append(ids, item.ID)
		}
		return nil
	})
	sort.Strings(ids)
	return ids, err
}
func (a *Adapter) receipt(ctx context.Context, payment, id string) (authorization, error) {
	var item authorization
	err := a.cfg.Store.Transact(ctx, func(tx tender.Tx) error {
		rows, err := a.evidence(tx, map[string]string{"payment": payment, "id": id})
		if err != nil {
			return err
		}
		if len(rows) != 1 {
			return tender.ErrValidation
		}
		return json.Unmarshal(rows[0], &item)
	})
	return item, err
}

// ProcessAuthorization is a worker operation, invoked after callback
// acknowledgment. Engine.Confirm and the inbox marker can each be retried.
func (a *Adapter) ProcessAuthorization(ctx context.Context, e *tender.Engine, payment, id string) (tender.Result, error) {
	started := time.Now()
	p, err := e.Get(ctx, payment)
	if err != nil {
		return tender.Result{}, err
	}
	item, err := a.receipt(ctx, payment, id)
	if err != nil {
		return tender.Result{}, err
	}
	if !a.validRoute(p) || item.Binding != binding(p) {
		return tender.Result{}, tender.ErrConflict
	}
	res, err := e.Confirm(ctx, tender.Request{Key: "klarna-order-" + id, PaymentID: payment, Amount: p.Amount, Token: id})
	// A different receipt can already have completed this same attempt.
	// Uncertain or pending authority leaves the competing receipt available.
	superseded := res.Operation.ID == "" && errors.Is(err, tender.ErrConflict)
	if !superseded && res.Operation.Status != tender.OperationFailed && res.Operation.Status != tender.OperationPending && res.Operation.Status != tender.OperationSucceeded {
		return res, err
	}
	var receivedAt time.Time
	var processed bool
	mark := a.cfg.Store.Transact(ctx, func(tx tender.Tx) error {
		if superseded {
			current, readErr := tx.Payment(a.cfg.Scope, p.ID)
			if readErr != nil {
				return readErr
			}
			if !a.validRoute(current) || binding(current) != item.Binding {
				return tender.ErrConflict
			}
			switch current.Status {
			case tender.PaymentAuthorized, tender.PaymentPartiallyCaptured, tender.PaymentCaptured, tender.PaymentSettled, tender.PaymentVoided, tender.PaymentDeclined, tender.PaymentExpired, tender.PaymentReturned:
			default:
				return tender.ErrConflict
			}
			res = tender.Result{Payment: current}
		}
		w, ok := tx.(tender.VerifiedEventTx)
		if !ok {
			return tender.ErrUnsupported
		}
		if reader, ok := tx.(tender.VerifiedReceiptTimeReader); ok {
			if at, readErr := reader.VerifiedEventReceivedAt(a.key(id)); readErr == nil {
				receivedAt = at
			}
		}
		return w.ProcessVerifiedEvent(a.key(id), a.cfg.Now(), func(json.RawMessage) error { processed = true; return nil })
	})
	if mark == nil && processed {
		e.ObserveVerifiedReceipt(ctx, started, receivedAt, p.ID, res.Operation.ID)
	}
	if superseded {
		return res, mark
	}
	return res, errors.Join(err, mark)
}
