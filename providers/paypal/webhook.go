package paypal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/gravestench/tender"
	"io"
	"net/http"
	"strings"
	"time"
)

// Webhook is the receiver and asynchronous processor for an app's registered
// PAYMENT.CAPTURE.COMPLETED events. OnStored may wake a host worker.
type Webhook struct {
	api      *Adapter
	store    tender.Store
	engine   *tender.Engine
	onStored func(string)
}

func (a *Adapter) Webhook(store tender.Store, engine *tender.Engine, onStored func(string)) (*Webhook, error) {
	if store == nil || engine == nil {
		return nil, tender.ErrValidation
	}
	return &Webhook{a, store, engine, onStored}, nil
}

type receipt struct {
	ID, Kind, OrderID, CaptureID, Merchant string
	Amount                                 amount
}

func (h *Webhook) key(id string) tender.VerifiedEventKey {
	return tender.VerifiedEventKey{Scope: h.api.cfg.Scope, Provider: "paypal", MerchantAccount: h.api.cfg.MerchantAccount, ID: id}
}
func (h *Webhook) verify(ctx context.Context, header http.Header, raw []byte) error {
	meta := map[string]string{"webhook_id": h.api.cfg.WebhookID}
	for key, field := range map[string]string{"PayPal-Auth-Algo": "auth_algo", "PayPal-Cert-Url": "cert_url", "PayPal-Transmission-Id": "transmission_id", "PayPal-Transmission-Sig": "transmission_sig", "PayPal-Transmission-Time": "transmission_time"} {
		values := header.Values(key)
		if len(values) != 1 || values[0] == "" || len(values[0]) > 2048 {
			return tender.ErrAuthentication
		}
		meta[field] = values[0]
	}
	cert := meta["cert_url"]
	_, certID, ok := strings.Cut(cert, "/v1/notifications/certs/")
	if !ok || !strings.HasPrefix(certID, "CERT-") || !idPattern.MatchString(certID) || !h.api.resourceURL(cert, "/v1/notifications/certs/"+certID) || meta["auth_algo"] != "SHA256withRSA" {
		return tender.ErrAuthentication
	}
	if _, err := time.Parse(time.RFC3339Nano, meta["transmission_time"]); err != nil {
		return tender.ErrAuthentication
	}
	if !json.Valid(raw) || !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) {
		return tender.ErrValidation
	}
	// Preserve the event's original JSON inside the postback, including internal
	// whitespace and escaping. The cert URL is data passed to PayPal, never fetched.
	b, _ := json.Marshal(meta)
	b = append(b[:len(b)-1], `,"webhook_event":`...)
	b = append(b, raw...)
	b = append(b, '}')
	var reply struct {
		Status string `json:"verification_status"`
	}
	if err := h.api.call(ctx, "POST", "/v1/notifications/verify-webhook-signature", "", b, &reply); err != nil {
		return err
	}
	if reply.Status != "SUCCESS" {
		return tender.ErrAuthentication
	}
	return nil
}
func (h *Webhook) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		w.Header().Set("Allow", "POST")
		w.WriteHeader(405)
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 256<<10))
	if err != nil {
		w.WriteHeader(413)
		return
	}
	if err = h.verify(r.Context(), r.Header, raw); err != nil {
		switch {
		case errors.Is(err, tender.ErrAuthentication):
			w.WriteHeader(401)
		case errors.Is(err, tender.ErrValidation):
			w.WriteHeader(400)
		default:
			w.WriteHeader(503)
		}
		return
	}
	var event struct {
		ID           string
		Type         string `json:"event_type"`
		ResourceType string `json:"resource_type"`
		Resource     struct {
			ID, Status string
			Amount     amount
			Payee      payee
			Related    struct {
				IDs struct {
					Order string `json:"order_id"`
				} `json:"related_ids"`
			} `json:"supplementary_data"`
		}
	}
	if json.Unmarshal(raw, &event) != nil || !idPattern.MatchString(event.ID) || event.Type != "PAYMENT.CAPTURE.COMPLETED" || event.ResourceType != "capture" || event.Resource.Status != "COMPLETED" || event.Resource.Payee.Merchant != h.api.cfg.MerchantAccount || !idPattern.MatchString(event.Resource.ID) || !idPattern.MatchString(event.Resource.Related.IDs.Order) || event.Resource.Amount.Currency != "USD" {
		w.WriteHeader(400)
		return
	}
	e := receipt{event.ID, event.Type, event.Resource.Related.IDs.Order, event.Resource.ID, event.Resource.Payee.Merchant, event.Resource.Amount}
	body, _ := json.Marshal(e)
	err = h.store.Transact(r.Context(), func(tx tender.Tx) error {
		inbox, ok := tx.(tender.VerifiedEventTx)
		if !ok {
			return tender.ErrUnsupported
		}
		return inbox.InsertVerifiedEvent(h.key(e.ID), body)
	})
	if errors.Is(err, tender.ErrConflict) {
		saved, _, readErr := h.load(r.Context(), e.ID)
		err = readErr
		if err == nil && saved.ID != "" && saved != e {
			err = tender.ErrConflict
		}
	}
	if err != nil {
		w.WriteHeader(503)
		return
	}
	if h.onStored != nil {
		h.onStored(e.ID)
	}
	w.WriteHeader(202)
}

// load reads unprocessed evidence before external I/O. Returning the sentinel
// rolls the read back and leaves the receipt pending until the atomic apply.
func (h *Webhook) load(ctx context.Context, id string) (e receipt, processed bool, err error) {
	found := errors.New("read pending receipt")
	err = h.store.Transact(ctx, func(tx tender.Tx) error {
		inbox, ok := tx.(tender.VerifiedEventTx)
		if !ok {
			return tender.ErrUnsupported
		}
		return inbox.ProcessVerifiedEvent(h.key(id), time.Time{}, func(raw json.RawMessage) error {
			if json.Unmarshal(raw, &e) != nil {
				return tender.ErrValidation
			}
			return found
		})
	})
	if errors.Is(err, found) {
		return e, false, nil
	}
	return e, err == nil, err
}

// Process joins the signed event to a read-only, authenticated order and the
// locally reserved confirmation. It performs no network I/O in a transaction.
func (h *Webhook) Process(ctx context.Context, id string) error {
	event, done, err := h.load(ctx, id)
	if err != nil || done {
		return err
	}
	if event.ID != id || event.Kind != "PAYMENT.CAPTURE.COMPLETED" || event.Merchant != h.api.cfg.MerchantAccount {
		return tender.ErrConflict
	}
	o, err := h.api.getOrder(ctx, event.OrderID)
	if err != nil {
		return err
	}
	if len(o.Units) != 1 || !idPattern.MatchString(o.Units[0].Custom) {
		return tender.ErrConflict
	}
	return h.engine.ProcessVerifiedEventWithRecords(ctx, id, func(tx tender.EventRecords, raw json.RawMessage) (tender.VerifiedEvent, error) {
		var stored receipt
		var empty tender.VerifiedEvent
		if json.Unmarshal(raw, &stored) != nil || stored != event {
			return empty, tender.ErrConflict
		}
		p, err := tx.Payment(h.api.cfg.Scope, o.Units[0].Custom)
		if err != nil {
			return empty, err
		}
		c, ok := o.singleCapture(p)
		if !ok || p.Provider != "paypal" || p.MerchantAccount != event.Merchant || p.Market != market || p.ProviderReference != event.OrderID || c.ID != event.CaptureID || c.Amount != event.Amount {
			return empty, tender.ErrConflict
		}
		op, err := tx.OperationByReference(p.Scope, p.ID, c.ID)
		if errors.Is(err, tender.ErrNotFound) {
			op, err = tx.ActiveOperation(p.Scope, p.ID)
		}
		if err != nil {
			return empty, err
		}
		if op.Kind != tender.OpConfirm || op.AttemptID != p.AttemptID || op.PaymentID != p.ID || op.Amount != p.Amount {
			return empty, tender.ErrConflict
		}
		r := tender.ProviderRequest{Payment: p, Operation: op.Operation}
		var obs tender.Observation
		if op.Status == tender.OperationSucceeded && p.Funds.Captured == p.Amount.Minor && p.VerifiedMethod == tender.MethodPayPal && op.ProviderReference == c.ID {
			obs = observation(r)
			obs.OperationReference = c.ID
		} else {
			if c.Status != "COMPLETED" {
				return empty, tender.ErrConflict
			}
			obs, err = captured(r, o)
			if err != nil {
				return empty, err
			}
		}
		return tender.VerifiedEvent{ID: id, Provider: "paypal", Observation: obs}, nil
	})
}
