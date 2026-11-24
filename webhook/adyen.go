// Package webhook supplies mountable verified provider callback handlers.
package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gravestench/tender"
	"github.com/gravestench/tender/providers/adyen"
)

// AdyenConfig binds a Standard webhook endpoint to one account and environment.
// Use distinct signing keys per environment. OnStored is an optional, prompt
// scheduler wake-up; hosts should also poll their durable pending inbox.
type AdyenConfig struct {
	Store           tender.Store
	Engine          *tender.Engine
	Scope           tender.Scope
	MerchantAccount string
	HMACKeys        []string
	OnStored        func(string)
}
type Adyen struct {
	cfg  AdyenConfig
	keys [][]byte
}

func NewAdyen(c AdyenConfig) (*Adyen, error) {
	if c.Store == nil || c.Engine == nil || c.Scope.Validate() != nil || c.MerchantAccount == "" || len(c.HMACKeys) == 0 {
		return nil, tender.ErrValidation
	}
	h := &Adyen{cfg: c}
	for _, s := range c.HMACKeys {
		key, err := hex.DecodeString(s)
		if err != nil || len(key) != 32 {
			return nil, tender.ErrValidation
		}
		h.keys = append(h.keys, key)
	}
	h.cfg.HMACKeys = nil
	return h, nil
}

// Only these Standard notification fields are signed. The body may also contain
// shopper details, reasons, timestamps and untrusted additionalData.
type evidence struct {
	PSP      string `json:"pspReference"`
	Original string `json:"originalReference"`
	Account  string `json:"merchantAccountCode"`
	Ref      string `json:"merchantReference"`
	Amount   struct {
		Value    *int64 `json:"value"`
		Currency string `json:"currency"`
	} `json:"amount"`
	Code    string `json:"eventCode"`
	Success string `json:"success"`
}

func (e evidence) canonical() (string, bool) {
	if e.PSP == "" || e.Account == "" || e.Code == "" || e.Amount.Value == nil || (e.Success != "true" && e.Success != "false") {
		return "", false
	}
	fields := []string{e.PSP, e.Original, e.Account, e.Ref, strconv.FormatInt(*e.Amount.Value, 10), e.Amount.Currency, e.Code, e.Success}
	// Tender generates delimiter-free merchant references. Restrict the signed
	// tuple's alphabet to keep the mapping to the colon-delimited input unique.
	for _, f := range fields {
		if strings.ContainsAny(f, ":\\") || len(f) > 256 {
			return "", false
		}
	}
	return strings.Join(fields, ":"), true
}
func (e evidence) id() string {
	canonical, _ := e.canonical()
	sum := sha256.Sum256([]byte(canonical))
	// Duplicate PSP/event pairs may contain changed details. Preserve different
	// signed facts for reconciliation, ignoring unsigned eventDate/metadata.
	return e.Code + "." + e.PSP + "." + hex.EncodeToString(sum[:])
}
func (h *Adyen) verified(body []byte) ([]evidence, error) {
	var envelope struct {
		Live  string `json:"live"`
		Items []struct {
			Item struct {
				evidence
				Additional struct {
					Signature string `json:"hmacSignature"`
				} `json:"additionalData"`
			} `json:"NotificationRequestItem"`
		} `json:"notificationItems"`
	}
	if json.Unmarshal(body, &envelope) != nil || len(envelope.Items) == 0 || len(envelope.Items) > 32 {
		return nil, tender.ErrValidation
	}
	if envelope.Live != strconv.FormatBool(h.cfg.Scope.Environment == tender.EnvironmentLive) {
		return nil, tender.ErrAuthentication
	}
	events := make([]evidence, 0, len(envelope.Items))
	for _, n := range envelope.Items {
		e := n.Item.evidence
		canonical, ok := e.canonical()
		if !ok {
			return nil, tender.ErrValidation
		}
		sig, err := base64.StdEncoding.DecodeString(n.Item.Additional.Signature)
		if err != nil {
			return nil, tender.ErrAuthentication
		}
		verified := false
		for _, key := range h.keys {
			mac := hmac.New(sha256.New, key)
			mac.Write([]byte(canonical))
			if hmac.Equal(sig, mac.Sum(nil)) {
				verified = true
			}
		}
		if !verified || e.Account != h.cfg.MerchantAccount {
			return nil, tender.ErrAuthentication
		}
		events = append(events, e)
	}
	return events, nil
}
func (h *Adyen) store(ctx context.Context, e evidence) error {
	key := tender.VerifiedEventKey{Scope: h.cfg.Scope, Provider: "adyen", MerchantAccount: e.Account, ID: e.id()}
	raw, err := json.Marshal(e)
	if err != nil {
		return err
	}
	err = h.cfg.Store.Transact(ctx, func(tx tender.Tx) error {
		inbox, ok := tx.(tender.VerifiedEventTx)
		if !ok {
			return tender.ErrUnsupported
		}
		return inbox.InsertVerifiedEvent(key, raw)
	})
	if !errors.Is(err, tender.ErrConflict) {
		return err
	}
	// SQL serialization conflicts also map to ErrConflict. Acknowledge a replay
	// only after positively observing its durable row in a fresh transaction.
	seen := errors.New("existing inbox event")
	err = h.cfg.Store.Transact(ctx, func(tx tender.Tx) error {
		inbox, ok := tx.(tender.VerifiedEventTx)
		if !ok {
			return tender.ErrUnsupported
		}
		return inbox.ProcessVerifiedEvent(key, time.Time{}, func(json.RawMessage) error { return seen })
	})
	if errors.Is(err, seen) {
		return nil
	}
	return err
}
func (h *Adyen) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 512<<10))
	if err != nil {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		return
	}
	events, err := h.verified(raw)
	if errors.Is(err, tender.ErrAuthentication) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	for _, e := range events {
		if h.store(r.Context(), e) != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if h.cfg.OnStored != nil {
			h.cfg.OnStored(e.id())
		}
	}
	w.WriteHeader(http.StatusAccepted)
}

// Process applies one already authenticated inbox row atomically. Unresolved
// rows remain pending for host retry or investigation.
func (h *Adyen) Process(ctx context.Context, id string) error {
	return h.cfg.Engine.ProcessVerifiedEventWithRecords(ctx, id, func(tx tender.EventRecords, raw json.RawMessage) (tender.VerifiedEvent, error) {
		var e evidence
		if json.Unmarshal(raw, &e) != nil {
			return tender.VerifiedEvent{}, tender.ErrValidation
		}
		if _, ok := e.canonical(); !ok || e.id() != id || e.Account != h.cfg.MerchantAccount {
			return tender.VerifiedEvent{}, tender.ErrConflict
		}
		o, err := h.observation(tx, e)
		return tender.VerifiedEvent{ID: e.id(), Provider: "adyen", Observation: o}, err
	})
}
func (h *Adyen) observation(tx tender.EventRecords, e evidence) (tender.Observation, error) {
	var o tender.Observation
	kind := map[string]tender.OperationKind{"AUTHORISATION": tender.OpConfirm, "CAPTURE": tender.OpCapture, "CAPTURE_FAILED": tender.OpCapture, "CANCELLATION": tender.OpVoid, "REFUND": tender.OpRefund}[e.Code]
	if kind == "" && e.Code != "CHARGEBACK" {
		return o, tender.ErrUnsupported
	}
	scope := h.cfg.Scope
	p, original, err := adyen.ResolveReference(tx, scope, e.Ref)
	if err != nil {
		return o, err
	}
	if p.Provider != "adyen" || p.MerchantAccount != e.Account || original.AttemptID != p.AttemptID {
		return o, tender.ErrConflict
	}
	if p.Market.Method == tender.MethodACH {
		return achObservation(tx, p, original, e)
	}
	if kind == "" {
		return o, tender.ErrUnsupported
	}
	op := original
	if kind != tender.OpConfirm && original.Kind == tender.OpConfirm {
		// Some deliveries use the original merchant payment reference. Only an
		// already bound modification PSP can disambiguate multiple partial captures.
		op, err = tx.OperationByReference(scope, p.ID, e.PSP)
		if err != nil {
			return o, err
		}
	}
	if op.Kind != kind || op.AttemptID != p.AttemptID || op.PaymentID != p.ID {
		return o, tender.ErrConflict
	}
	m := tender.Money{Minor: *e.Amount.Value, Currency: e.Amount.Currency}
	if m != op.Amount {
		return o, tender.ErrConflict
	}
	if kind == tender.OpConfirm {
		if e.Original != "" || p.ProviderReference != "" && p.ProviderReference != e.PSP {
			return o, tender.ErrConflict
		}
		// /details has its own host reservation, but Adyen echoes the original
		// /payments merchant reference. Match the exact action being completed.
		if p.Status == tender.PaymentRequiresAction && original.Status == tender.OperationSucceeded {
			var a adyen.CardAction
			if json.Unmarshal([]byte(p.Action.Data), &a) != nil || a.Type != "threeDS2" || a.Reference != e.Ref {
				return o, tender.ErrConflict
			}
			active, err := tx.ActiveOperation(scope, p.ID)
			if err == nil {
				if active.Kind != kind || active.AttemptID != p.AttemptID || active.Amount != m {
					return o, tender.ErrConflict
				}
				op = active
			} else if !errors.Is(err, tender.ErrNotFound) {
				return o, err
			}
		}
		if p.Status == tender.PaymentPending && original.Status == tender.OperationSucceeded {
			active, err := tx.ActiveOperation(scope, p.ID)
			if err != nil {
				return o, err
			}
			if active.Kind != kind || active.Amount != m || active.AttemptID != p.AttemptID || active.ProviderReference != e.Ref {
				return o, tender.ErrConflict
			}
			op = active
		}
	} else if e.Original == "" || e.Original != p.ProviderReference || e.PSP == e.Original {
		return o, tender.ErrConflict
	}
	o = tender.Observation{
		Scope: scope, MerchantAccount: e.Account, PaymentID: p.ID, AttemptID: op.AttemptID, OperationID: op.ID,
		ProviderReference: e.Original, OperationReference: e.PSP, Amount: m, Status: tender.OperationSucceeded, Snapshot: p.Snapshot,
	}
	if kind == tender.OpConfirm {
		o.ProviderReference = e.PSP
		o.OperationReference = ""
		if e.Success == "true" {
			if p.Funds.Authorized == m.Minor && p.VerifiedMethod == p.Market.Method {
				return o, nil
			}
			// The Standard HMAC omits paymentMethod. Only an authenticated
			// Checkout response establishes the actual card network.
			return o, tender.ErrConflict
		} else {
			if p.Status == tender.PaymentDeclined && op.Status == tender.OperationFailed {
				o.Status = tender.OperationFailed
				return o, nil
			}
			if op.Status != tender.OperationSucceeded {
				o.Status = tender.OperationFailed
			}
			o.Snapshot = tender.Snapshot{Status: tender.PaymentDeclined, ProviderStatus: "Refused"}
		}
		return o, nil
	}
	if kind == tender.OpCapture {
		failed := e.Code == "CAPTURE_FAILED"
		if !failed && e.Success == "true" {
			failed, err = h.captureFailure(tx, p, op, e)
			if err != nil {
				return o, err
			}
		}
		if !failed && e.Success == "true" && p.VerifiedMethod != p.Market.Method {
			return o, tender.ErrConflict
		}
		if failed {
			o.CaptureFailure = true
			o.Snapshot.Status = tender.PaymentCaptureFailed
			o.Snapshot.ProviderStatus = "CAPTURE_FAILED"
			if op.Status != tender.OperationSucceeded {
				o.Status = tender.OperationFailed
			}
			return o, nil
		}
	}
	if e.Success == "false" {
		o.Status = tender.OperationFailed
		return o, nil
	}
	// Exact duplicate delivery is handled by the inbox. Never count a completed
	// modification twice, even when later evidence is being investigated.
	if op.Status == tender.OperationSucceeded {
		return o, nil
	}
	switch kind {
	case tender.OpCapture:
		o.Snapshot.Funds.Captured += m.Minor
		o.Snapshot.Status = tender.PaymentPartiallyCaptured
		if o.Snapshot.Funds.Captured == p.Funds.Authorized {
			o.Snapshot.Status = tender.PaymentCaptured
		}
	case tender.OpRefund:
		o.Snapshot.Funds.Refunded += m.Minor
	case tender.OpVoid:
		o.Snapshot.Status = tender.PaymentVoided
	}
	if p.Status == tender.PaymentCaptureFailed {
		o.Snapshot.Status = p.Status
		o.Snapshot.ProviderStatus = "CAPTURE_FAILED"
	} else {
		o.Snapshot.ProviderStatus = e.Code
	}
	return o, nil
}

// A success worker can run before the worker for a failure that is already
// durably received. Inspect the same transaction's verified inbox before a
// capture can enqueue collection. Other payments' or unbound evidence cannot
// put this payment on hold.
func (h *Adyen) captureFailure(tx tender.EventRecords, p tender.Payment, op tender.OperationRecord, event evidence) (bool, error) {
	inbox, ok := tx.(interface {
		tender.VerifiedEvidenceReader
		tender.CaptureFailureTx
	})
	if !ok {
		return false, tender.ErrUnsupported
	}
	rows, err := inbox.MatchingVerifiedEvidence(p.Scope, "adyen", p.MerchantAccount, map[string]string{"eventCode": "CAPTURE_FAILED", "originalReference": event.Original})
	if err != nil {
		return false, err
	}
	for _, raw := range rows {
		var failure evidence
		if json.Unmarshal(raw, &failure) != nil {
			return false, tender.ErrValidation
		}
		if _, ok := failure.canonical(); !ok || failure.Account != p.MerchantAccount || failure.Original != p.ProviderReference || failure.Amount.Currency != p.Amount.Currency {
			continue
		}
		boundPayment, bound, err := adyen.ResolveReference(tx, p.Scope, failure.Ref)
		if errors.Is(err, tender.ErrNotFound) || errors.Is(err, tender.ErrConflict) {
			continue
		}
		if err != nil {
			return false, err
		}
		if boundPayment.ID != p.ID || boundPayment.AttemptID != p.AttemptID {
			continue
		}
		if bound.Kind == tender.OpConfirm {
			// The positive delivery itself can bind an otherwise-unresolved
			// modification PSP when the failure named the original payment.
			if failure.PSP == event.PSP {
				bound = op
			} else {
				bound, err = tx.OperationByReference(p.Scope, p.ID, failure.PSP)
				if errors.Is(err, tender.ErrNotFound) {
					continue
				}
				if err != nil {
					return false, err
				}
			}
		}
		if bound.Kind != tender.OpCapture || bound.Amount.Minor != *failure.Amount.Value || bound.ProviderReference != "" && bound.ProviderReference != failure.PSP {
			continue
		}
		if bound.ID == op.ID && failure.PSP == event.PSP {
			return true, nil
		}
		if p.Status != tender.PaymentCaptureFailed && bound.ID != op.ID {
			return false, tender.ErrConflict
		}
	}
	return false, nil
}
