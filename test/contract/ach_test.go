//go:build integration

package contract_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gravestench/tender"
	"github.com/gravestench/tender/mockserver"
	"github.com/gravestench/tender/providers/ach"
	"github.com/gravestench/tender/providers/adyen"
	"github.com/gravestench/tender/store"
	"github.com/gravestench/tender/store/postgres"
	"github.com/gravestench/tender/webhook"
)

func TestACHContract(t *testing.T) {
	run := func(t *testing.T, db adyenStore, returnFirst bool, loss string) {
		ctx := t.Context()
		mux := http.NewServeMux()
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)
		inbox := make(chan string, 8)
		key, _ := hex.DecodeString(hmacHex)
		callback := srv.URL + "/notifications"
		sim, err := mockserver.New(mockserver.Config{PublicBaseURL: srv.URL + "/sim", SigningKey: key, AllowedCallbackTargets: []string{callback}, Routes: mockserver.ACHRoutes(mockserver.ACHFixture{MerchantAccount: testAccount, APIKey: "mock-key", WebhookURL: callback, PendingResult: returnFirst})})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { sim.Close() })
		mux.Handle("/sim/", http.StripPrefix("/sim", sim.Handler()))
		adapter, err := adyen.NewACH(adyen.Config{Environment: tender.EnvironmentMock, MerchantAccount: testAccount, APIKey: "mock-key", BaseURL: srv.URL + "/sim/v72", Origin: srv.URL, ReturnURL: srv.URL + "/checkout"})
		if err != nil {
			t.Fatal(err)
		}
		if c := adapter.Capabilities(); c.Query || c.Void || c.Refund || c.Capture {
			t.Fatal("unimplemented operations advertised", c)
		}
		engine, err := tender.NewEngine(tender.EngineConfig{Store: db, Provider: adapter, Scope: dbScope, Market: tender.Market{Country: "US", Currency: "USD", Method: tender.MethodACH}, ProviderName: "adyen", MerchantAccount: testAccount})
		if err != nil {
			t.Fatal(err)
		}
		handler, err := webhook.NewAdyen(webhook.AdyenConfig{Store: db, Engine: engine, Scope: dbScope, MerchantAccount: testAccount, HMACKeys: []string{hmacHex}, OnStored: func(id string) { inbox <- id }})
		if err != nil {
			t.Fatal(err)
		}
		mux.Handle("/notifications", handler)
		req := tender.Request{PaymentID: "debit", OrderID: "order", Amount: tender.Money{Minor: 1234, Currency: "USD"}, Key: "create"}
		created, err := engine.Create(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		var action adyen.ACHSession
		if json.Unmarshal([]byte(created.Payment.Action.Data), &action) != nil || action.ID == "" || action.SessionData == "" || action.Reference == "" || created.Payment.Funds != (tender.Funds{}) {
			t.Fatal("missing or unsafe session", created)
		}
		replay, err := engine.Create(ctx, req)
		if err != nil || created.Operation.ID != replay.Operation.ID || replay.Payment.Action != created.Payment.Action {
			t.Fatal("session replay", err)
		}
		client := srv.Client()
		post := func(path, body string, want int) []byte {
			t.Helper()
			r, err := client.Post(srv.URL+path, "application/json", strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			defer r.Body.Close()
			v, _ := io.ReadAll(r.Body)
			if r.StatusCode != want {
				t.Fatalf("%s returned %d (want %d)", path, r.StatusCode, want)
			}
			return v
		}
		if loss == "immediate-bank-rejection" {
			failure := eventFields{psp: "CAPTUREFAIL00001", original: "AUTOBANK00000001", account: testAccount, ref: action.Reference, value: "1234", currency: "USD", code: "CAPTURE_FAILED", success: "false"}
			post("/notifications", string(notification(failure, true)), 202)
			select {
			case id := <-inbox:
				if err := handler.Process(ctx, id); err != nil {
					t.Fatal("initial signed bank failure", err)
				}
			case <-time.After(4 * time.Second):
				t.Fatal("failure missing")
			}
			p, err := engine.Get(ctx, "debit")
			if err != nil || p.Status != tender.PaymentCaptureFailed || p.Funds != (tender.Funds{}) || p.ProviderReference != failure.original {
				t.Fatal("immediate bank rejection", p, err)
			}
			if _, err := adapter.CheckoutSession(p); !errors.Is(err, tender.ErrUnsupported) {
				t.Fatal("reopened a failed debit", err)
			}
			return
		}
		post("/sim/mock/consent/"+action.ID+"/approve", `{"accountNumber":"synthetic-sentinel"}`, 400)
		raw := post("/sim/mock/consent/"+action.ID+"/approve", "", 200)
		var receipt struct{ SessionResult string }
		if json.Unmarshal(raw, &receipt) != nil || receipt.SessionResult == "" {
			t.Fatal("missing synthetic result")
		}
		if !bytes.Equal(raw, post("/sim/mock/consent/"+action.ID+"/approve", "", 200)) {
			t.Fatal("consent replay changed")
		}
		post("/sim/mock/consent/"+action.ID+"/deny", "", 409)
		next := func() string {
			t.Helper()
			select {
			case id := <-inbox:
				return id
			case <-time.After(4 * time.Second):
				t.Fatal("missing signed callback")
			}
			return ""
		}
		authID := next()
		req.OrderID = ""
		req.Key = "result"
		req.Token = receipt.SessionResult
		pending := created
		if loss == "result" {
			if err := sim.SetScenarios("ach.result", mockserver.Scenario{Fault: mockserver.MalformedResponse}); err != nil {
				t.Fatal(err)
			}
			pending, err = engine.Confirm(ctx, req)
			if !errors.Is(err, tender.ErrUnknownOutcome) {
				t.Fatal("lost session result", err)
			}
		} else if loss == "" || loss == "capture-failed" || loss == "capture-failed-late" {
			pending, err = engine.Confirm(ctx, req)
			if err != nil || pending.Payment.Status != tender.PaymentPending || pending.Payment.Funds != (tender.Funds{}) || returnFirst != (pending.Payment.ProviderReference == "") {
				t.Fatal("session result", pending, err)
			}
		}
		if loss != "report-first" && loss != "capture-failed-unbound" {
			if err = handler.Process(ctx, authID); err != nil {
				t.Fatal("authorization", err)
			}
			pending.Payment, err = engine.Get(ctx, "debit")
			if err != nil || pending.Payment.ProviderReference == "" {
				t.Fatal("signed authorization did not bind PSP", err)
			}
			if loss == "browser" || loss == "result" {
				if _, err := adapter.CheckoutSession(pending.Payment); !errors.Is(err, tender.ErrUnsupported) {
					t.Fatal("reopened already submitted bank consent", err)
				}
			}
		}
		if _, err = engine.Recover(ctx, pending.Operation.ID); !errors.Is(err, tender.ErrUnsupported) {
			t.Fatal("polled a static session result", err)
		}
		if o, err := db.Outbox(ctx, dbScope); err != nil || len(o) != 0 {
			t.Fatal("pending debit became collectible", o, err)
		}
		r, err := client.Get(srv.URL + "/sim/mock/report/" + action.ID + "/Settled")
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		line, err := ach.ReadSettlement(r.Body)
		if err != nil || line.Gross != 1234 || line.Net != 1209 || loss != "report-first" && loss != "capture-failed-unbound" && line.PSP != pending.Payment.ProviderReference || line.Modification == "" || line.MerchantReference != action.Reference {
			t.Fatal("settlement mapping", line, err)
		}
		apply := func(l ach.SettlementLine) error {
			return ach.ApplySettlement(ctx, db, engine, dbScope, testAccount, "settlement_detail_report_batch_7.csv", l)
		}
		wrong := line
		wrong.Gross++
		if apply(wrong) == nil {
			t.Fatal("wrong amount advanced settlement")
		}
		wrong = line
		wrong.PSP = "DIFFERENT1234567"
		if apply(wrong) == nil {
			t.Fatal("unrelated PSP advanced settlement")
		}
		wrong = line
		wrong.Account = "another"
		if apply(wrong) == nil {
			t.Fatal("cross-account report accepted")
		}
		if strings.HasPrefix(loss, "capture-failed") {
			late := loss == "capture-failed-late"
			if late {
				if err := apply(line); err != nil {
					t.Fatal(err)
				}
			}
			failure := eventFields{psp: "CAPTUREFAIL00001", original: line.PSP, account: testAccount, ref: action.Reference, value: "1234", currency: "USD", code: "CAPTURE_FAILED", success: "true"}
			cross := failure
			cross.original = "OTHERPAYMENT0001"

			post("/notifications", string(notification(cross, true)), 202)
			if err := handler.Process(ctx, next()); !errors.Is(err, tender.ErrConflict) && !errors.Is(err, tender.ErrNotFound) {
				t.Errorf("wrong payment failure %v", err)
			}
			// Reason and error code are outside the Standard item HMAC. They
			// must not become a permanent account classification in the ledger.
			body := string(bytes.Replace(notification(failure, true), []byte(`"additionalData":{`), []byte(`"reason":"1063_101","additionalData":{`), 1))
			post("/notifications", body, 202)
			id := next()
			if !late {
				if err := apply(line); !errors.Is(err, tender.ErrConflict) {
					t.Errorf("settlement raced a received capture failure: %v", err)
				}
			}
			for range 2 {
				if err := handler.Process(ctx, id); err != nil {
					t.Fatalf("processing correlated automatic capture failure: %v", err)
				}
			}
			p, err := engine.Get(ctx, "debit")
			want := tender.Funds{}
			wantCount := 0
			if late {
				want.Settled = 1234
				wantCount = 1
			}
			if err != nil || p.Status != tender.PaymentCaptureFailed || p.ProviderStatus != "CAPTURE_FAILED" || p.Funds != want || p.Action != (tender.CustomerAction{}) {
				t.Fatal("capture failure lost history or left consent open", p, err)
			}
			req.Key = "retry"
			if _, err := engine.Confirm(ctx, req); !errors.Is(err, tender.ErrConflict) {
				t.Fatal("held debit allowed a retry", err)
			}
			out, err := db.Outbox(ctx, dbScope)
			if err != nil || len(out) != wantCount {
				t.Fatal("held debit delivered", out, err)
			}
			queue := db.(interface {
				CaptureFailures(context.Context, tender.Scope) ([]tender.CaptureFailureNotice, error)
			})
			review, err := queue.CaptureFailures(ctx, dbScope)
			if err != nil || len(review) != 1 || review[0].OperationReference != failure.psp || review[0].ProviderReference != line.PSP || review[0].PaymentID != "debit" {
				t.Fatal("missing reviewable capture failure", review, err)
			}
			return
		}
		unverified := line
		unverified.Type = "Chargeback"
		unverified.Modification = "NORETURN00000001"
		unverified.Gross = -1234
		unverified.Net = -1234
		if err := apply(unverified); !errors.Is(err, tender.ErrConflict) {
			t.Error("return with no matching signed receipt applied", err)
		}
		var returnID string
		if returnFirst {
			post("/sim/mock/return/"+action.ID, "", 202)
			returnID = next()
		}
		if err = apply(line); err != nil {
			t.Fatal("apply settlement", err)
		}
		if err = apply(line); err != nil {
			t.Fatal("replay settlement", err)
		}
		if err := handler.Process(ctx, authID); err != nil {
			t.Fatal("deferred signed receipt", err)
		}
		wantOut := 1
		if returnFirst {
			wantOut = 0
		}
		if o, err := db.Outbox(ctx, dbScope); err != nil || len(o) != wantOut {
			t.Fatal("collection before/after known chargeback", o, err)
		}
		if !returnFirst {
			p, err := engine.Get(ctx, "debit")
			if err != nil || p.Status != tender.PaymentSettled || p.Funds != (tender.Funds{Settled: 1234}) {
				t.Fatal("settlement", p, err)
			}
			post("/sim/mock/return/"+action.ID, "", 202)
			returnID = next()
		}
		for range 2 {
			if err = handler.Process(ctx, returnID); err != nil {
				t.Fatal("chargeback", err)
			}
		}
		r2, err := client.Get(srv.URL + "/sim/mock/report/" + action.ID + "/Chargeback")
		if err != nil {
			t.Fatal(err)
		}
		defer r2.Body.Close()
		returned, err := ach.ReadSettlement(r2.Body)
		if err != nil || returned.Net != -1234 || returned.Gross != -1234 || returned.PSP != line.PSP {
			t.Fatal("chargeback report", returned, err)
		}
		mismatch := returned
		mismatch.Modification = "WRONGRETURN00001"
		if err := apply(mismatch); !errors.Is(err, tender.ErrConflict) {
			t.Error("unrelated chargeback modification accepted", err)
		}
		if err = apply(returned); err != nil {
			t.Fatal(err)
		}
		p, err := engine.Get(ctx, "debit")
		if err != nil || p.Status != tender.PaymentReturned || p.Funds != (tender.Funds{Settled: 1234, Returned: 1234}) {
			t.Fatal("return erased gross or fabricated card funds", p, err)
		}
		if o, err := db.Outbox(ctx, dbScope); err != nil || len(o) != wantOut {
			t.Fatal("duplicate collection", o, err)
		}
		post("/notifications", string(notification(eventFields{psp: "forged", ref: action.Reference, code: "CHARGEBACK", success: "true", account: testAccount, value: "1234", currency: "USD"}, false)), 401)
	}
	t.Run("memory", func(t *testing.T) { run(t, store.NewMemory(), false, "") })
	t.Run("received-return-before-settlement", func(t *testing.T) { run(t, store.NewMemory(), true, "") })
	t.Run("lost-browser-result", func(t *testing.T) { run(t, store.NewMemory(), false, "browser") })
	t.Run("lost-host-result", func(t *testing.T) { run(t, store.NewMemory(), false, "result") })
	t.Run("settlement-before-auth-worker", func(t *testing.T) { run(t, store.NewMemory(), false, "report-first") })
	for _, scenario := range []string{"immediate-bank-rejection", "capture-failed", "capture-failed-late", "capture-failed-unbound"} {
		t.Run(scenario, func(t *testing.T) { run(t, store.NewMemory(), false, scenario) })
	}
	t.Run("postgres", func(t *testing.T) {
		dsn := os.Getenv("TENDER_TEST_DATABASE_URL")
		if dsn == "" {
			t.Skip("set a disposable local TENDER_TEST_DATABASE_URL")
		}
		if !adyenLocalDatabase(dsn) {
			t.Fatal("nonlocal SQL")
		}
		for _, scenario := range []string{"", "capture-failed", "capture-failed-late", "capture-failed-unbound"} {
			name := scenario
			if name == "" {
				name = "settled-and-returned"
			}
			t.Run(name, func(t *testing.T) { run(t, postgres.New(postgresFixture(t)()), false, scenario) })
		}
	})
}
