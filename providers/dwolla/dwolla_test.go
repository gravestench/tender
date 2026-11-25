package dwolla_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gravestench/tender"
	"github.com/gravestench/tender/providers/dwolla"
	"github.com/gravestench/tender/store"
)

type noInboxTx struct{ tender.Tx }

func TestCreateRequiresDurableVerifiedInboxBeforeDispatch(t *testing.T) {
	a, err := dwolla.New(dwolla.Config{Environment: tender.EnvironmentMock, BaseURL: "http://127.0.0.1:8080", MerchantAccount: accountID, DestinationFundingSourceID: merchantBankID,
		BearerToken: func(context.Context) (string, error) { return "token", nil }, AuthorizeSource: func(context.Context, tender.Payment, dwolla.Source) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	requirements, ok := any(a).(tender.StoreRequirements)
	if !ok {
		t.Fatal("Dwolla transfer did not require a durable verified inbox")
	}
	storage := store.NewMemory()
	err = storage.Transact(t.Context(), func(tx tender.Tx) error {
		if err := requirements.CheckStore(tender.OpCreate, noInboxTx{tx}); !errors.Is(err, tender.ErrUnsupported) {
			t.Fatalf("accepted store without inbox: %v", err)
		}
		return requirements.CheckStore(tender.OpCreate, tx)
	})
	if err != nil {
		t.Fatal(err)
	}
}

const (
	accountID      = "11111111-1111-4111-8111-111111111111"
	merchantBankID = "22222222-2222-4222-8222-222222222222"
	customerID     = "33333333-3333-4333-8333-333333333333"
	bankID         = "44444444-4444-4444-8444-444444444444"
	transferID     = "55555555-5555-4555-8555-555555555555"
	opID           = "safe_operation_01"
)

func request(source dwolla.Source) tender.ProviderRequest {
	token, _ := json.Marshal(source)
	scope := tender.Scope{MerchantID: "shop", Environment: tender.EnvironmentMock}
	amount := tender.Money{Currency: "USD", Minor: 1234}
	return tender.ProviderRequest{
		Payment:   tender.Payment{Scope: scope, ID: "payment", OrderID: "order", Provider: "dwolla", MerchantAccount: accountID, AttemptID: "attempt", Market: tender.Market{Country: "US", Currency: "USD", Method: tender.MethodACH}, Amount: amount, Snapshot: tender.Snapshot{Status: tender.PaymentCreated}},
		Operation: tender.Operation{Scope: scope, ID: opID, PaymentID: "payment", AttemptID: "attempt", Kind: tender.OpCreate, Status: tender.OperationReserved, ProviderKey: "safe_key_01", Amount: amount},
		Token:     string(token),
	}
}

func TestConfigFailsClosed(t *testing.T) {
	base := dwolla.Config{Environment: tender.EnvironmentMock, BaseURL: "https://api-sandbox.dwolla.com", MerchantAccount: accountID, DestinationFundingSourceID: merchantBankID,
		BearerToken: func(context.Context) (string, error) { return "token", nil }, AuthorizeSource: func(context.Context, tender.Payment, dwolla.Source) error { return nil }}
	if _, err := dwolla.New(base); !errors.Is(err, tender.ErrValidation) {
		t.Fatalf("MOCK accepted official sandbox host: %v", err)
	}
	base.Environment = tender.EnvironmentSandbox
	if a, err := dwolla.New(base); err != nil || a.Capabilities().Create {
		t.Fatalf("ordinary sandbox must stay disabled: %v %+v", err, a)
	}
	base.Environment = tender.EnvironmentLive
	base.BaseURL = "https://api.dwolla.com"
	if a, err := dwolla.New(base); err != nil || a.Capabilities().Create {
		t.Fatalf("ordinary live must stay disabled: %v %+v", err, a)
	}
	base.AuthorizeSource = nil
	if _, err := dwolla.New(base); !errors.Is(err, tender.ErrValidation) {
		t.Fatalf("accepted missing host authorization guard: %v", err)
	}
}

func TestCreateUsesAuthenticatedDwollaTransferAndKeepsFundsPending(t *testing.T) {
	const proof = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	source := dwolla.Source{CustomerID: customerID, FundingSourceID: bankID, ProofDigest: proof}
	var posts, gets, fundingReads, guard int
	var baseURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer local-token" || r.Header.Get("Accept") != "application/vnd.dwolla.v1.hal+json" {
			t.Errorf("missing Dwolla authentication/media header")
			w.WriteHeader(401)
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /funding-sources/" + bankID:
			fundingReads++
			_, _ = w.Write([]byte(`{"id":"` + bankID + `","status":"verified","type":"bank","bankAccountType":"checking","removed":false,"channels":["ach"],"_links":{"self":{"href":"` + baseURL + `/funding-sources/` + bankID + `"}}}`))
		case "POST /transfers":
			posts++
			if r.Header.Get("Idempotency-Key") != "safe_key_01" {
				t.Errorf("missing stable idempotency key")
			}
			var body struct {
				Links         struct{ Source, Destination struct{ Href string } } `json:"_links"`
				Amount        struct{ Currency, Value string }
				CorrelationID string `json:"correlationId"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil || body.Links.Source.Href != baseURL+"/funding-sources/"+bankID || body.Links.Destination.Href != baseURL+"/funding-sources/"+merchantBankID || body.Amount.Currency != "USD" || body.Amount.Value != "12.34" || body.CorrelationID != opID {
				t.Errorf("incorrect or sensitive transfer body: %+v", body)
			}
			w.Header().Set("Location", baseURL+"/transfers/"+transferID)
			w.WriteHeader(201)
		case "GET /transfers/" + transferID:
			gets++
			w.Header().Set("Content-Type", "application/vnd.dwolla.v1.hal+json")
			_, _ = w.Write([]byte(`{"id":"` + transferID + `","status":"pending","correlationId":"` + opID + `","amount":{"currency":"USD","value":"12.34"},"_links":{"source":{"href":"` + baseURL + `/funding-sources/` + bankID + `"},"destination":{"href":"` + baseURL + `/funding-sources/` + merchantBankID + `"}}}`))
		default:
			t.Errorf("unexpected provider request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	baseURL = srv.URL
	a, err := dwolla.New(dwolla.Config{Environment: tender.EnvironmentMock, BaseURL: baseURL, MerchantAccount: accountID, DestinationFundingSourceID: merchantBankID,
		BearerToken: func(context.Context) (string, error) { return "local-token", nil },
		AuthorizeSource: func(_ context.Context, p tender.Payment, got dwolla.Source) error {
			guard++
			if p.ID != "payment" || got != source {
				return tender.ErrAuthentication
			}
			return nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	obs, err := a.Execute(t.Context(), request(source))
	if err != nil {
		t.Fatal(err)
	}
	if guard != 1 || fundingReads != 1 || posts != 1 || gets != 1 || obs.ProviderReference != transferID || obs.OperationReference != transferID || obs.Status != tender.OperationSucceeded || obs.Snapshot.Status != tender.PaymentPending || obs.Snapshot.Funds != (tender.Funds{}) || obs.Snapshot.VerifiedMethod != "" {
		t.Fatalf("transfer did not stay pending: guard=%d funding=%d posts=%d gets=%d obs=%+v", guard, fundingReads, posts, gets, obs)
	}
	if strings.Contains(obs.Snapshot.ProviderStatus, proof) {
		t.Fatal("consent proof leaked into status")
	}
}

func TestCreateRefusesUnverifiedSourceBeforeTransfer(t *testing.T) {
	const proof = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	var posts int
	var baseURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/funding-sources/"+bankID {
			_, _ = w.Write([]byte(`{"id":"` + bankID + `","status":"unverified","type":"bank","bankAccountType":"checking","removed":false,"channels":["ach"],"_links":{"self":{"href":"` + baseURL + `/funding-sources/` + bankID + `"}}}`))
			return
		}
		posts++
		w.WriteHeader(201)
	}))
	defer srv.Close()
	baseURL = srv.URL
	a, err := dwolla.New(dwolla.Config{Environment: tender.EnvironmentMock, BaseURL: srv.URL, MerchantAccount: accountID, DestinationFundingSourceID: merchantBankID,
		BearerToken: func(context.Context) (string, error) { return "local-token", nil }, AuthorizeSource: func(context.Context, tender.Payment, dwolla.Source) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.Execute(t.Context(), request(dwolla.Source{CustomerID: customerID, FundingSourceID: bankID, ProofDigest: proof}))
	if !errors.Is(err, tender.ErrNotDispatched) || posts != 0 {
		t.Fatalf("unverified bank dispatched transfer: posts=%d err=%v", posts, err)
	}
}
