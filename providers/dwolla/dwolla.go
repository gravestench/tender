// Package dwolla implements Dwolla's US/USD pay-in transfer wire protocol.
// Ordinary SANDBOX and LIVE capabilities remain disabled pending merchant
// qualification, mandate proof, and bank reconciliation evidence.
package dwolla

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/gravestench/tender"
)

const mediaType = "application/vnd.dwolla.v1.hal+json"

var uuid = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
var digest = regexp.MustCompile(`^[0-9a-f]{64}$`)
var requestID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,80}$`)

// Source contains only provider resource identities and a digest of the
// host-held one-time authorization. It never carries bank credentials.
type Source struct {
	CustomerID      string `json:"customerId"`
	FundingSourceID string `json:"fundingSourceId"`
	ProofDigest     string `json:"proofDigest"`
}

// Config binds the adapter to a single Dwolla account and destination bank.
// The host owns OAuth refresh, customer onboarding and approved mandate proof.
type Config struct {
	Environment                tender.Environment
	BaseURL                    string
	MerchantAccount            string
	DestinationFundingSourceID string
	BearerToken                func(context.Context) (string, error)
	AuthorizeSource            func(context.Context, tender.Payment, Source) error
	Transport                  http.RoundTripper
}

type Adapter struct {
	cfg    Config
	client *http.Client
}

func New(c Config) (*Adapter, error) {
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || !uuid.MatchString(c.MerchantAccount) || !uuid.MatchString(c.DestinationFundingSourceID) || c.BearerToken == nil || c.AuthorizeSource == nil {
		return nil, tender.ErrValidation
	}
	switch c.Environment {
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
	if c.Transport == nil {
		c.Transport = &http.Transport{ForceAttemptHTTP2: true}
	}
	return &Adapter{cfg: c, client: &http.Client{Transport: c.Transport, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (a *Adapter) Capabilities() tender.Capabilities {
	if a.cfg.Environment != tender.EnvironmentMock {
		return tender.Capabilities{}
	}
	return tender.Capabilities{Create: true, Markets: []tender.MarketCapability{{Market: tender.Market{Country: "US", Currency: "USD", Method: tender.MethodACH}}}}
}

// CheckStore prevents a bank transfer if the host cannot durably receive and
// distinguish its signed, replayed provider notifications.
func (a *Adapter) CheckStore(kind tender.OperationKind, tx tender.Tx) error {
	if kind != tender.OpCreate {
		return tender.ErrUnsupported
	}
	if _, ok := tx.(interface {
		tender.VerifiedEventTx
		tender.VerifiedEvidenceReader
	}); !ok {
		return tender.ErrUnsupported
	}
	return nil
}

func (a *Adapter) Execute(ctx context.Context, r tender.ProviderRequest) (tender.Observation, error) {
	p, op := r.Payment, r.Operation
	o := tender.Observation{Scope: p.Scope, MerchantAccount: p.MerchantAccount, PaymentID: p.ID, AttemptID: p.AttemptID, OperationID: op.ID, Amount: op.Amount, Status: tender.OperationSucceeded}
	if !a.Capabilities().SupportsIn(op.Kind, p.Market, false) {
		return o, tender.ErrUnsupported
	}
	if p.Scope.Validate() != nil || p.Scope.Environment != a.cfg.Environment || p.Provider != "dwolla" || p.MerchantAccount != a.cfg.MerchantAccount || p.Status != tender.PaymentCreated || p.ID == "" || p.AttemptID == "" || op.Scope != p.Scope || op.PaymentID != p.ID || op.AttemptID != p.AttemptID || op.Status != tender.OperationReserved || op.Amount != p.Amount || p.Amount.ValidatePurchase() != nil || p.Amount.Currency != "USD" || !requestID.MatchString(op.ID) || !requestID.MatchString(op.ProviderKey) {
		return o, tender.ErrValidation
	}
	var source Source
	dec := json.NewDecoder(strings.NewReader(r.Token))
	dec.DisallowUnknownFields()
	if dec.Decode(&source) != nil || dec.Decode(new(any)) != io.EOF || !uuid.MatchString(source.CustomerID) || !uuid.MatchString(source.FundingSourceID) || !digest.MatchString(source.ProofDigest) || source.FundingSourceID == a.cfg.DestinationFundingSourceID {
		return o, tender.ErrValidation
	}
	if err := a.cfg.AuthorizeSource(ctx, p, source); err != nil {
		return o, tender.ErrAuthentication
	}
	token, err := a.cfg.BearerToken(ctx)
	if err != nil || token == "" || len(token) > 2048 || strings.ContainsAny(token, " \t\r\n") {
		return o, tender.ErrNotDispatched
	}
	sourceURL := a.cfg.BaseURL + "/funding-sources/" + source.FundingSourceID
	destURL := a.cfg.BaseURL + "/funding-sources/" + a.cfg.DestinationFundingSourceID
	// A source can only send ACH after Dwolla marks the bank verified. The
	// host callback additionally binds this resource to the customer and the
	// specific signed authorization; this read cannot prove those facts.
	check, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return o, tender.ErrNotDispatched
	}
	check.Header.Set("Accept", mediaType)
	check.Header.Set("Authorization", "Bearer "+token)
	funding, err := a.client.Do(check)
	if err != nil {
		return o, tender.ErrNotDispatched
	}
	preflight, readErr := io.ReadAll(io.LimitReader(funding.Body, (16<<10)+1))
	closeErr := funding.Body.Close()
	if readErr != nil || closeErr != nil || funding.StatusCode != http.StatusOK || len(preflight) > 16<<10 {
		return o, tender.ErrNotDispatched
	}
	var bank struct {
		ID, Status, Type, BankAccountType string
		Removed                           *bool
		Channels                          []string
		Links                             struct{ Self struct{ Href string } } `json:"_links"`
	}
	if json.Unmarshal(preflight, &bank) != nil || bank.ID != source.FundingSourceID || bank.Status != "verified" || bank.Type != "bank" || bank.BankAccountType != "checking" && bank.BankAccountType != "savings" || bank.Removed == nil || *bank.Removed || bank.Links.Self.Href != sourceURL {
		return o, tender.ErrNotDispatched
	}
	achEnabled := false
	for _, channel := range bank.Channels {
		achEnabled = achEnabled || channel == "ach"
	}
	if !achEnabled {
		return o, tender.ErrNotDispatched
	}
	request := struct {
		Links struct {
			Source struct {
				Href string `json:"href"`
			} `json:"source"`
			Destination struct {
				Href string `json:"href"`
			} `json:"destination"`
		} `json:"_links"`
		Amount        struct{ Currency, Value string } `json:"amount"`
		CorrelationID string                           `json:"correlationId"`
	}{CorrelationID: op.ID}
	request.Links.Source.Href = sourceURL
	request.Links.Destination.Href = destURL
	request.Amount.Currency = "USD"
	request.Amount.Value = fmt.Sprintf("%d.%02d", p.Amount.Minor/100, p.Amount.Minor%100)
	body, _ := json.Marshal(request)
	call, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.BaseURL+"/transfers", bytes.NewReader(body))
	if err != nil {
		return o, tender.ErrNotDispatched
	}
	call.GetBody = nil // No transparent replay of a financial POST.
	call.Header.Set("Accept", mediaType)
	call.Header.Set("Content-Type", mediaType)
	call.Header.Set("Authorization", "Bearer "+token)
	call.Header.Set("Idempotency-Key", op.ProviderKey)
	res, err := a.client.Do(call)
	if err != nil {
		return o, tender.ErrUnknownOutcome
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden {
		return o, tender.ErrAuthentication
	}
	if res.StatusCode != http.StatusCreated {
		return o, tender.ErrUnknownOutcome
	}
	location := res.Header.Get("Location")
	prefix := a.cfg.BaseURL + "/transfers/"
	if len(res.Header.Values("Location")) != 1 || !strings.HasPrefix(location, prefix) || !uuid.MatchString(strings.TrimPrefix(location, prefix)) {
		return o, tender.ErrUnknownOutcome
	}
	transferID := strings.TrimPrefix(location, prefix)
	read, err := http.NewRequestWithContext(ctx, http.MethodGet, location, nil)
	if err != nil {
		return o, tender.ErrUnknownOutcome
	}
	read.Header.Set("Accept", mediaType)
	read.Header.Set("Authorization", "Bearer "+token)
	verified, err := a.client.Do(read)
	if err != nil {
		return o, tender.ErrUnknownOutcome
	}
	defer verified.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(verified.Body, (64<<10)+1))
	if err != nil || verified.StatusCode != http.StatusOK || len(raw) > 64<<10 {
		return o, tender.ErrUnknownOutcome
	}
	var detail struct {
		ID, Status, CorrelationID string
		Amount                    struct{ Currency, Value string }
		Links                     struct {
			Source      struct{ Href string }
			Destination struct{ Href string }
		} `json:"_links"`
	}
	if json.Unmarshal(raw, &detail) != nil || detail.ID != transferID || detail.CorrelationID != op.ID || detail.Amount.Currency != "USD" || detail.Amount.Value != request.Amount.Value || detail.Links.Source.Href != sourceURL || detail.Links.Destination.Href != destURL || detail.Status != "pending" && detail.Status != "processed" {
		return o, tender.ErrUnknownOutcome
	}
	o.ProviderReference, o.OperationReference = transferID, transferID
	o.Snapshot = tender.Snapshot{Status: tender.PaymentPending, ProviderStatus: detail.Status}
	return o, nil
}

// Query cannot resolve an abandoned Create without its host-authenticated
// source binding. A future merchant-qualified recovery path must prove that
// binding before it can conclude an unknown transfer.
func (a *Adapter) Query(context.Context, tender.ProviderRequest) (tender.Observation, error) {
	return tender.Observation{}, tender.ErrUnsupported
}

var _ tender.Provider = (*Adapter)(nil)
var _ tender.StoreRequirements = (*Adapter)(nil)
