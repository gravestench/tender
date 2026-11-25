package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/gravestench/tender"
)

var dwollaUUID = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
var dwollaTopic = regexp.MustCompile(`^[a-z][a-z0-9_]{0,79}$`)

// DwollaConfig binds one subscription secret and account to one Tender route.
// Separate keys are required for MOCK, SANDBOX and LIVE.
type DwollaConfig struct {
	Store           tender.Store
	Engine          *tender.Engine
	Scope           tender.Scope
	BaseURL         string
	MerchantAccount string
	HMACSecrets     []string
	OnStored        func(string)
}

// Dwolla receives signed events into the durable inbox. Matched failed
// transfer events create review notices but never change funds.
type Dwolla struct {
	cfg  DwollaConfig
	keys [][]byte
}

func NewDwolla(c DwollaConfig) (*Dwolla, error) {
	u, err := url.Parse(c.BaseURL)
	if c.Store == nil || c.Scope.Validate() != nil || err != nil || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || !dwollaUUID.MatchString(c.MerchantAccount) || len(c.HMACSecrets) == 0 {
		return nil, tender.ErrValidation
	}
	switch c.Scope.Environment {
	case tender.EnvironmentMock:
		ip, err := netip.ParseAddr(u.Hostname())
		if err != nil || !ip.IsLoopback() || u.Scheme != "http" {
			return nil, tender.ErrValidation
		}
	case tender.EnvironmentSandbox:
		if c.BaseURL != "https://api-sandbox.dwolla.com" {
			return nil, tender.ErrValidation
		}
	case tender.EnvironmentLive:
		if c.BaseURL != "https://api.dwolla.com" {
			return nil, tender.ErrValidation
		}
	default:
		return nil, tender.ErrValidation
	}
	h := &Dwolla{cfg: c}
	for _, key := range c.HMACSecrets {
		if len(key) < 32 || len(key) > 256 || strings.TrimSpace(key) != key {
			return nil, tender.ErrValidation
		}
		h.keys = append(h.keys, []byte(key))
	}
	h.cfg.HMACSecrets = nil
	return h, nil
}

type dwollaReceipt struct {
	ID            string `json:"id"`
	ResourceID    string `json:"resourceId"`
	CorrelationID string `json:"correlationId"`
	Topic         string `json:"topic"`
	Timestamp     string `json:"timestamp"`
	Created       string `json:"created"`
	Links         struct {
		Account struct {
			Href string `json:"href"`
		} `json:"account"`
		Resource struct {
			Href string `json:"href"`
		} `json:"resource"`
	} `json:"_links"`
}

func (h *Dwolla) verified(raw []byte, header http.Header) (dwollaReceipt, bool, error) {
	var event dwollaReceipt
	values := header.Values("X-Request-Signature-SHA-256")
	if len(values) != 1 || len(values[0]) != 64 {
		return event, false, tender.ErrAuthentication
	}
	signature, err := hex.DecodeString(values[0])
	if err != nil || len(signature) != sha256.Size {
		return event, false, tender.ErrAuthentication
	}
	valid := false
	for _, key := range h.keys {
		mac := hmac.New(sha256.New, key)
		mac.Write(raw)
		if hmac.Equal(mac.Sum(nil), signature) {
			valid = true
		}
	}
	if !valid {
		return event, false, tender.ErrAuthentication
	}
	if json.Unmarshal(raw, &event) != nil || !dwollaUUID.MatchString(event.ID) || !dwollaUUID.MatchString(event.ResourceID) || !dwollaTopic.MatchString(event.Topic) || event.Timestamp == "" && event.Created == "" || event.Timestamp != "" && event.Created != "" && event.Timestamp != event.Created {
		return event, false, tender.ErrValidation
	}
	stamp := event.Created
	if stamp == "" {
		stamp = event.Timestamp
	}
	if _, err := time.Parse(time.RFC3339Nano, stamp); err != nil {
		return event, false, tender.ErrValidation
	}
	if event.Links.Account.Href != h.cfg.BaseURL+"/accounts/"+h.cfg.MerchantAccount {
		return event, false, tender.ErrAuthentication
	}
	resource := event.Links.Resource.Href
	if resource == h.cfg.BaseURL+"/transfers/"+event.ResourceID {
		// Resource identity, not a known topic-name list, decides whether to
		// retain a transfer event. Unknown return/adverse topics remain pending.
		return event, true, nil
	}
	if strings.Contains(event.Topic, "transfer") {
		return event, false, tender.ErrAuthentication
	}
	// Dwolla subscriptions send all event topics. Only explicitly unrelated
	// customer/funding/account resources may be acknowledged without storage.
	for _, pair := range [][2]string{{"customer_funding_source_", "/funding-sources/"}, {"customer_", "/customers/"}, {"funding_source_", "/funding-sources/"}, {"account_", "/accounts/"}} {
		if strings.HasPrefix(event.Topic, pair[0]) && resource == h.cfg.BaseURL+pair[1]+event.ResourceID {
			return event, false, nil
		}
	}
	// An unrecognized signed resource cannot be classified safely. Keep it
	// pending for operator review, provided it stays on the configured API.
	parsed, err := url.Parse(resource)
	base, _ := url.Parse(h.cfg.BaseURL)
	if err != nil || parsed.Scheme != base.Scheme || parsed.Host != base.Host || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || !strings.HasSuffix(parsed.Path, "/"+event.ResourceID) {
		return event, false, tender.ErrAuthentication
	}
	return event, true, nil
}

// Process consumes a signed failure for an exact, previously bound transfer.
// The caller supplies the payment ID after resolving the transfer reference in
// its own index. Failed legs without that exact binding remain pending review.
func (h *Dwolla) Process(ctx context.Context, paymentID, eventID string) error {
	if h.cfg.Engine == nil || paymentID == "" || !dwollaUUID.MatchString(eventID) {
		return tender.ErrValidation
	}
	return h.cfg.Engine.ProcessVerifiedAdverseEvent(ctx, eventID, func(tx tender.EventRecords, raw json.RawMessage) (tender.AdverseNotice, error) {
		var e dwollaReceipt
		if json.Unmarshal(raw, &e) != nil || e.ID != eventID || e.Links.Account.Href != h.cfg.BaseURL+"/accounts/"+h.cfg.MerchantAccount || e.Links.Resource.Href != h.cfg.BaseURL+"/transfers/"+e.ResourceID || !dwollaUUID.MatchString(e.ResourceID) || e.CorrelationID == "" {
			return tender.AdverseNotice{}, tender.ErrConflict
		}
		if e.Topic != "transfer_failed" && e.Topic != "customer_transfer_failed" && e.Topic != "customer_bank_transfer_failed" {
			return tender.AdverseNotice{}, tender.ErrUnsupported
		}
		stamp := e.Created
		if stamp == "" {
			stamp = e.Timestamp
		}
		observed, err := time.Parse(time.RFC3339Nano, stamp)
		if err != nil || e.Created != "" && e.Timestamp != "" && e.Created != e.Timestamp {
			return tender.AdverseNotice{}, tender.ErrConflict
		}
		p, err := tx.Payment(h.cfg.Scope, paymentID)
		if err != nil {
			return tender.AdverseNotice{}, err
		}
		if p.Provider != "dwolla" || p.MerchantAccount != h.cfg.MerchantAccount || p.Market != (tender.Market{Country: "US", Currency: "USD", Method: tender.MethodACH}) || p.ProviderReference != e.ResourceID || p.Status != tender.PaymentPending && p.Status != tender.PaymentFailed || p.Funds != (tender.Funds{}) {
			return tender.AdverseNotice{}, tender.ErrConflict
		}
		op, err := tx.OperationByReference(p.Scope, p.ID, e.ResourceID)
		if err != nil {
			return tender.AdverseNotice{}, err
		}
		if op.ID != e.CorrelationID || op.Kind != tender.OpCreate || op.Status != tender.OperationSucceeded || op.AttemptID != p.AttemptID || op.Amount != p.Amount {
			return tender.AdverseNotice{}, tender.ErrConflict
		}
		return tender.AdverseNotice{Scope: p.Scope, Provider: "dwolla", MerchantAccount: p.MerchantAccount,
			PaymentID: p.ID, OperationID: op.ID, ProviderEventID: e.ID, Kind: "ach_return",
			ProviderReference: e.ResourceID, OriginalReference: p.ProviderReference, ModificationReference: op.ProviderReference,
			Amount: p.Amount, ObservedAt: observed.UTC()}, nil
	})
}

func (h *Dwolla) store(ctx context.Context, event dwollaReceipt) (bool, error) {
	key := tender.VerifiedEventKey{Scope: h.cfg.Scope, Provider: "dwolla", MerchantAccount: h.cfg.MerchantAccount, ID: event.ID}
	allowed, _ := json.Marshal(event)
	err := h.cfg.Store.Transact(ctx, func(tx tender.Tx) error {
		inbox, ok := tx.(tender.VerifiedEventTx)
		if !ok {
			return tender.ErrUnsupported
		}
		return inbox.InsertVerifiedEvent(key, allowed)
	})
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, tender.ErrConflict) {
		return false, err
	}
	// A serialization conflict also maps to ErrConflict. Confirm the exact
	// existing, allowlisted row before acknowledging a duplicate delivery.
	err = h.cfg.Store.Transact(ctx, func(tx tender.Tx) error {
		reader, ok := tx.(tender.VerifiedEvidenceReader)
		if !ok {
			return tender.ErrUnsupported
		}
		rows, err := reader.MatchingVerifiedEvidence(h.cfg.Scope, "dwolla", h.cfg.MerchantAccount, map[string]string{"id": event.ID})
		if err != nil {
			return err
		}
		if len(rows) != 1 || !bytes.Equal(rows[0], allowed) {
			return tender.ErrConflict
		}
		return nil
	})
	return false, err
}

func (h *Dwolla) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 256<<10))
	if err != nil {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		return
	}
	event, transfer, err := h.verified(raw, r.Header)
	if errors.Is(err, tender.ErrAuthentication) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if !transfer {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	created, err := h.store(r.Context(), event)
	if errors.Is(err, tender.ErrConflict) {
		w.WriteHeader(http.StatusConflict)
		return
	}
	if err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if created && h.cfg.OnStored != nil {
		h.cfg.OnStored(event.ID)
	}
	w.WriteHeader(http.StatusAccepted)
}
