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
	if strings.HasPrefix(id, "REFUND_FAILED.") {
		return h.processAdverse(ctx, id)
	}
	err := h.processObservation(ctx, id)
	// ACH CHARGEBACK changes ACH funds through its own signed-event path.
	// A card CHARGEBACK is unsupported there and instead queues a review-only
	// notice. The first transaction rolls back before the adverse path runs.
	if strings.HasPrefix(id, "CHARGEBACK.") && errors.Is(err, tender.ErrUnsupported) {
		return h.processAdverse(ctx, id)
	}
	return err
}

func (h *Adyen) processAdverse(ctx context.Context, id string) error {
	return h.cfg.Engine.ProcessVerifiedAdverseEvent(ctx, id, func(tx tender.EventRecords, raw json.RawMessage) (tender.AdverseNotice, error) {
		var e evidence
		if json.Unmarshal(raw, &e) != nil {
			return tender.AdverseNotice{}, tender.ErrValidation
		}
		return h.adverse(tx, id, e)
	})
}

func (h *Adyen) processObservation(ctx context.Context, id string) error {
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

// adverse projects only independently HMAC-signed financial exceptions. The
// notice records host review work; neither a dispute nor refund failure erases
// previously collected, refunded or fulfilled history.
func (h *Adyen) adverse(tx tender.EventRecords, id string, e evidence) (tender.AdverseNotice, error) {
	var n tender.AdverseNotice
	if _, ok := e.canonical(); !ok || e.id() != id || e.Account != h.cfg.MerchantAccount || e.Success != "true" || e.Code != "CHARGEBACK" && e.Code != "REFUND_FAILED" {
		return n, tender.ErrConflict
	}
	p, refOp, err := adyen.ResolveReference(tx, h.cfg.Scope, e.Ref)
	if err != nil {
		return n, err
	}
	if p.Provider != "adyen" || p.MerchantAccount != e.Account || refOp.PaymentID != p.ID || refOp.AttemptID != p.AttemptID ||
		(p.Market.Method != tender.MethodVisa && p.Market.Method != tender.MethodMastercard) ||
		p.ProviderReference == "" || e.Original != p.ProviderReference || e.PSP == e.Original ||
		e.Amount.Value == nil || *e.Amount.Value <= 0 || e.Amount.Currency != p.Amount.Currency ||
		*e.Amount.Value > p.Amount.Minor || *e.Amount.Value > p.Funds.Captured {
		return n, tender.ErrConflict
	}
	n = tender.AdverseNotice{
		Scope: h.cfg.Scope, Provider: "adyen", MerchantAccount: e.Account,
		PaymentID: p.ID, ProviderEventID: id, ProviderReference: e.PSP,
		OriginalReference: e.Original, ModificationReference: e.PSP,
		Amount:     tender.Money{Minor: *e.Amount.Value, Currency: e.Amount.Currency},
		ObservedAt: time.Now().UTC(),
	}
	if e.Code == "CHARGEBACK" {
		n.Kind = "chargeback"
		// A modification PSP belonging to an operation is not an independent
		// dispute identity. Do not let such a collision authorize a return.
		if _, err := tx.OperationByReference(h.cfg.Scope, p.ID, e.PSP); err == nil {
			return tender.AdverseNotice{}, tender.ErrConflict
		} else if !errors.Is(err, tender.ErrNotFound) {
			return tender.AdverseNotice{}, err
		}
		return n, nil
	}
	n.Kind = "refund_failed"
	op := refOp
	if op.Kind != tender.OpRefund {
		op, err = tx.OperationByReference(h.cfg.Scope, p.ID, e.PSP)
		if err != nil {
			return tender.AdverseNotice{}, err
		}
	}
	if op.Kind != tender.OpRefund || op.PaymentID != p.ID || op.AttemptID != p.AttemptID || op.ProviderReference != e.PSP || op.Amount.Minor != *e.Amount.Value || op.Amount.Currency != e.Amount.Currency || op.Status == tender.OperationFailed {
		return tender.AdverseNotice{}, tender.ErrConflict
	}
	n.OperationID = op.ID
	return n, nil
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
	if kind == tender.OpRefund && e.Success == "true" {
		failed, err := h.refundFailure(tx, p, op, e)
		if err != nil {
			return o, err
		}
		if failed {
			return o, tender.ErrConflict
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

// A received, independently signed scheme rejection fences a reordered
// positive REFUND event until an operator resolves the contradictory facts.
// A failure arriving after a completed refund remains review-only history.
func (h *Adyen) refundFailure(tx tender.EventRecords, p tender.Payment, op tender.OperationRecord, event evidence) (bool, error) {
	reader, ok := tx.(tender.VerifiedEvidenceReader)
	if !ok {
		return false, tender.ErrUnsupported
	}
	rows, err := reader.MatchingVerifiedEvidence(p.Scope, "adyen", p.MerchantAccount, map[string]string{"eventCode": "REFUND_FAILED", "originalReference": p.ProviderReference, "pspReference": event.PSP})
	if err != nil {
		return false, err
	}
	for _, raw := range rows {
		var failure evidence
		if json.Unmarshal(raw, &failure) != nil {
			return false, tender.ErrValidation
		}
		if _, valid := failure.canonical(); !valid || failure.Account != p.MerchantAccount || failure.Code != "REFUND_FAILED" || failure.Success != "true" || failure.Original != p.ProviderReference || failure.PSP != event.PSP || failure.Amount.Value == nil || *failure.Amount.Value != op.Amount.Minor || failure.Amount.Currency != op.Amount.Currency {
			continue
		}
		boundPayment, bound, err := adyen.ResolveReference(tx, p.Scope, failure.Ref)
		if errors.Is(err, tender.ErrNotFound) || errors.Is(err, tender.ErrConflict) {
			continue
		}
		if err != nil {
			return false, err
		}
		if boundPayment.ID == p.ID && boundPayment.AttemptID == p.AttemptID && (bound.ID == op.ID || bound.Kind == tender.OpConfirm) {
			return true, nil
		}
	}
	return false, nil
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
