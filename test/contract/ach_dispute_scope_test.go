//go:build integration

package contract_test

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/gravestench/tender"
	"github.com/gravestench/tender/mockserver"
	"github.com/gravestench/tender/providers/adyen"
	"github.com/gravestench/tender/store"
	"github.com/gravestench/tender/store/postgres"
	"github.com/gravestench/tender/webhook"
)

func TestACHReturnPSPCannotBeAnotherPaymentsOperation(t *testing.T) {
	run := func(t *testing.T, db adyenAdverseStore) {
		mux := http.NewServeMux()
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)
		key, _ := hex.DecodeString(hmacHex)
		callback := srv.URL + "/notifications"
		sim, err := mockserver.New(mockserver.Config{PublicBaseURL: srv.URL + "/sim", SigningKey: key, AllowedCallbackTargets: []string{callback}, Routes: mockserver.ACHRoutes(mockserver.ACHFixture{MerchantAccount: testAccount, APIKey: "mock-key", WebhookURL: callback})})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { sim.Close() })
		mux.Handle("/sim/", http.StripPrefix("/sim", sim.Handler()))
		adapter, err := adyen.NewACH(adyen.Config{Environment: tender.EnvironmentMock, MerchantAccount: testAccount, APIKey: "mock-key", BaseURL: srv.URL + "/sim/v72", Origin: srv.URL, ReturnURL: srv.URL + "/checkout"})
		if err != nil {
			t.Fatal(err)
		}
		engine, err := tender.NewEngine(tender.EngineConfig{Store: db, Provider: adapter, Scope: dbScope, Market: tender.Market{Country: "US", Currency: "USD", Method: tender.MethodACH}, ProviderName: "adyen", MerchantAccount: testAccount})
		if err != nil {
			t.Fatal(err)
		}
		inbox := make(chan string, 4)
		hook, err := webhook.NewAdyen(webhook.AdyenConfig{Store: db, Engine: engine, Scope: dbScope, MerchantAccount: testAccount, HMACKeys: []string{hmacHex}, OnStored: func(id string) { inbox <- id }})
		if err != nil {
			t.Fatal(err)
		}
		mux.Handle("/notifications", hook)
		next := func() string {
			t.Helper()
			select {
			case id := <-inbox:
				return id
			case <-time.After(4 * time.Second):
				t.Fatal("signed callback missing")
				return ""
			}
		}
		money := tender.Money{Minor: 1234, Currency: "USD"}
		createdA, err := engine.Create(t.Context(), tender.Request{PaymentID: "debit-a", OrderID: "order-a", Key: "create-a", Amount: money})
		if err != nil {
			t.Fatal(err)
		}
		createdB, err := engine.Create(t.Context(), tender.Request{PaymentID: "debit-b", OrderID: "order-b", Key: "create-b", Amount: money})
		if err != nil || createdB.Operation.ProviderReference == "" {
			t.Fatalf("second ACH session operation = %+v %v", createdB, err)
		}
		var session adyen.ACHSession
		if json.Unmarshal([]byte(createdA.Payment.Action.Data), &session) != nil {
			t.Fatal("missing ACH session")
		}
		resp, err := srv.Client().Post(srv.URL+"/sim/mock/consent/"+session.ID+"/approve", "application/json", http.NoBody)
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("consent = %v %v", resp, err)
		}
		var receipt struct{ SessionResult string }
		if json.NewDecoder(resp.Body).Decode(&receipt) != nil || receipt.SessionResult == "" {
			t.Fatal("missing synthetic consent result")
		}
		resp.Body.Close()
		authID := next()
		if _, err := engine.Confirm(t.Context(), tender.Request{PaymentID: "debit-a", Key: "confirm-a", Amount: money, Token: receipt.SessionResult}); err != nil {
			t.Fatal(err)
		}
		if err := hook.Process(t.Context(), authID); err != nil {
			t.Fatal(err)
		}
		before, err := engine.Get(t.Context(), "debit-a")
		if err != nil || before.Status != tender.PaymentPending || before.ProviderReference == "" || before.Funds != (tender.Funds{}) {
			t.Fatalf("pending debit = %+v %v", before, err)
		}
		body := notification(eventFields{psp: createdB.Operation.ProviderReference, original: before.ProviderReference, account: testAccount, ref: session.Reference, value: "1234", currency: "USD", code: "CHARGEBACK", success: "true"}, true)
		resp, err = srv.Client().Post(callback, "application/json", bytes.NewReader(body))
		if err != nil || resp.StatusCode != http.StatusAccepted {
			t.Fatalf("signed return callback = %v %v", resp, err)
		}
		resp.Body.Close()
		id := next()
		for range 2 {
			if err := hook.Process(t.Context(), id); !errors.Is(err, tender.ErrConflict) {
				t.Fatalf("ACH return reused another payment's operation PSP: %v", err)
			}
		}
		after, err := engine.Get(t.Context(), "debit-a")
		if err != nil || after.Status != before.Status || after.Funds != before.Funds {
			t.Fatalf("collision changed ACH funds: %+v %v", after, err)
		}
		reviews, err := db.AdverseReviews(t.Context(), dbScope)
		if err != nil || len(reviews) != 0 {
			t.Fatalf("collision committed ACH review: %+v %v", reviews, err)
		}
	}
	t.Run("memory", func(t *testing.T) { run(t, store.NewMemory()) })
	t.Run("postgres", func(t *testing.T) {
		if os.Getenv("TENDER_TEST_DATABASE_URL") == "" {
			t.Skip("set TENDER_TEST_DATABASE_URL")
		}
		run(t, postgres.New(postgresFixture(t)()))
	})
}
