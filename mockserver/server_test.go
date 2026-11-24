package mockserver_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gravestench/tender"
	"github.com/gravestench/tender/mockserver"
	"github.com/gravestench/tender/store"
	"github.com/gravestench/tender/test/contract"
)

func fixture(t *testing.T, cfg mockserver.Config) *mockserver.Server {
	t.Helper()
	if cfg.PublicBaseURL == "" {
		cfg.PublicBaseURL = "http://127.0.0.1:8080/payments/mock"
	}
	s, err := mockserver.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

func routes(s *mockserver.Server) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST /charges/{key}", s.Route("charge", func(w http.ResponseWriter, r *http.Request, _ mockserver.Scenario) {
		key := r.PathValue("key")
		rec := s.Commit(key, []byte("captured"))
		w.Header().Set("Location", s.PublicBaseURL()+"/charges/"+key)
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, rec.ID)
	}))
	return mux
}

func request(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "http://localhost"+path, nil)
	req.RemoteAddr = "127.0.0.1:5000"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestMountAndClose(t *testing.T) {
	cfg := mockserver.Config{Seed: 19, Routes: routes, Clock: func() time.Time { return time.Unix(100, 0) }}
	left, right := fixture(t, cfg), fixture(t, cfg)
	host := http.NewServeMux()
	host.Handle("/mock/", http.StripPrefix("/mock", left.Handler()))
	got := request(t, host, "/mock/charges/one")
	if got.Code != 201 || got.Header().Get("Location") != "http://127.0.0.1:8080/payments/mock/charges/one" {
		t.Fatalf("mounted response: %v", got.Result())
	}
	if _, ok := right.Lookup("one"); ok {
		t.Fatal("state shared across mounts")
	}
	rec, ok := left.Lookup("one")
	if !ok || !rec.At.Equal(time.Unix(100, 0)) || string(rec.Data) != "captured" {
		t.Fatalf("committed record: %+v", rec)
	}
	rec.Data[0] = 'x'
	if got := request(t, right.Handler(), "/charges/one"); got.Body.String() != rec.ID {
		t.Fatal("same seed produced different IDs")
	}
	if replay := left.Commit("one", []byte("refunded")); string(replay.Data) != "captured" || replay.ID != rec.ID {
		t.Fatal("replay replaced committed state")
	}
	if got := request(t, host, "/mock/scenarios"); got.Code != 404 {
		t.Fatalf("unexpected control route: %d", got.Code)
	}
	for _, addr := range []string{"203.0.113.5:1234", ""} {
		req := httptest.NewRequest("POST", "http://localhost/charges/outside", nil)
		req.RemoteAddr = addr
		w := httptest.NewRecorder()
		left.Handler().ServeHTTP(w, req)
		if w.Code != 403 {
			t.Fatalf("remote %q returned %d", addr, w.Code)
		}
	}
	if err := left.Reset(); err != nil {
		t.Fatal(err)
	}
	if _, ok := left.Lookup("one"); ok {
		t.Fatal("reset retained record")
	}
	if got := request(t, left.Handler(), "/charges/one"); got.Body.String() != rec.ID {
		t.Fatal("reset failed to rewind IDs")
	}
	if err := left.Close(); err != nil {
		t.Fatal(err)
	}
	if got := request(t, left.Handler(), "/charges/closed"); got.Code != 503 {
		t.Fatalf("after close: %d", got.Code)
	}
}

func TestDeterministicScenarios(t *testing.T) {
	s := fixture(t, mockserver.Config{Routes: routes})
	s.SetScenarios("charge", mockserver.Scenario{Fault: mockserver.Throttle}, mockserver.Scenario{Fault: mockserver.Unavailable}, mockserver.Scenario{Fault: mockserver.MalformedResponse})
	for _, code := range []int{429, 503} {
		got := request(t, s.Handler(), "/charges/a")
		if got.Code != code {
			t.Fatalf("want %d, got %d", code, got.Code)
		}
		if _, ok := s.Lookup("a"); ok {
			t.Fatal("failed request committed")
		}
	}
	if got := request(t, s.Handler(), "/charges/a"); got.Code != 200 || got.Body.Len() == 0 || json.Valid(got.Body.Bytes()) {
		t.Fatalf("malformed response: %v %q", got.Code, got.Body.String())
	}
	if _, ok := s.Lookup("a"); !ok {
		t.Fatal("malformed response erased commit")
	}
	if got := request(t, s.Handler(), "/charges/a"); got.Code != 201 {
		t.Fatalf("scenario did not recover: %d", got.Code)
	}
	s.SetScenarios("charge", mockserver.Scenario{Fault: mockserver.LoseResponse})
	live := httptest.NewServer(s.Handler())
	defer live.Close()
	resp, err := live.Client().Post(live.URL+"/charges/lost", "application/json", nil)
	if err == nil {
		resp.Body.Close()
		t.Fatal("lost response returned HTTP reply")
	}
	if _, ok := s.Lookup("lost"); !ok {
		t.Fatal("lost response erased commit")
	}
	s.SetScenarios("charge", mockserver.Scenario{Delay: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequestWithContext(ctx, "POST", "http://localhost/charges/delay", nil)
	req.RemoteAddr = "127.0.0.1:3333"
	done := make(chan struct{})
	go func() { s.Handler().ServeHTTP(httptest.NewRecorder(), req); close(done) }()
	deadline := time.After(2 * time.Second)
	for {
		if _, ok := s.Lookup("delay"); ok {
			break
		}
		select {
		case <-done:
			t.Fatal("response was not delayed")
		case <-deadline:
			t.Fatal("delay preceded commit")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if err := s.Reset(); err == nil {
		t.Error("reset active route")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("delay ignored cancellation")
	}
}

func TestCallbackEndpointPolicy(t *testing.T) {
	key := []byte("synthetic-test-key")
	got := make(chan string, 8)
	release := make(chan struct{})
	var followed atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		h := hmac.New(sha256.New, key)
		h.Write(b)
		if r.Method != "POST" || r.Header.Get("X-Tender-Mock-Signature") != hex.EncodeToString(h.Sum(nil)) || r.Header.Get("X-Fixture") != "test" {
			t.Error("invalid callback method, headers or signature")
		}
		got <- string(b)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	mux.HandleFunc("/redirect", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/other", 307) })
	mux.HandleFunc("/other", func(w http.ResponseWriter, r *http.Request) { followed.Store(true) })
	h := httptest.NewServer(mux)
	defer h.Close()
	defer close(release)
	target := h.URL + "/events"
	cfg := mockserver.Config{AllowedCallbackTargets: []string{target, h.URL + "/redirect"}, SigningKey: append([]byte(nil), key...)}
	s := fixture(t, cfg)
	cfg.AllowedCallbackTargets[0] = h.URL + "/other"
	cfg.SigningKey[0] = '!'
	send := func(url string) (<-chan error, error) {
		return s.Enqueue([]mockserver.Callback{{Target: url}}, mockserver.Scenario{})
	}
	for _, url := range []string{h.URL + "/other", target + "?else=1", "http://example.test/events"} {
		if _, err := send(url); err == nil {
			t.Fatalf("accepted %q", url)
		}
	}
	redirect, err := send(h.URL + "/redirect")
	if err != nil {
		t.Fatal(err)
	}
	if err = <-redirect; err == nil || followed.Load() {
		t.Fatal("allowed redirect")
	}
	for _, url := range []string{"https://example.test", "http://user@127.0.0.1:80", "file:///tmp/test"} {
		for _, cfg := range []mockserver.Config{{PublicBaseURL: url}, {PublicBaseURL: h.URL, AllowedCallbackTargets: []string{url}, SigningKey: key}} {
			if bad, err := mockserver.New(cfg); err == nil {
				bad.Close()
				t.Fatalf("accepted %q", url)
			}
		}
	}
	batch := []mockserver.Callback{{Target: target, Body: []byte("A"), Header: http.Header{"X-Fixture": {"test"}}}, {Target: target, Body: []byte("B"), Header: http.Header{"X-Fixture": {"test"}}}}
	done, err := s.Enqueue(batch, mockserver.Scenario{ReverseCallbacks: true, DuplicateCallbacks: true})
	if err != nil {
		t.Fatal(err)
	}
	batch[0].Body[0] = 'x'
	select {
	case first := <-got:
		if first != "B" {
			t.Fatalf("order: %s", first)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("callback missing")
	}
	if err := s.Reset(); err == nil {
		t.Fatal("reset with pending callbacks")
	}
	// Hold the worker at the host and fill the queue without blocking dispatch.
	var queued []<-chan error
	for range 16 {
		result, err := send(target)
		if err != nil {
			t.Fatal(err)
		}
		queued = append(queued, result)
	}
	if _, err := send(target); err == nil {
		t.Fatal("accepted more than 16 waiting batches")
	}
	// Cancel an HTTP callback blocked on the host and wait for its worker.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for _, result := range queued {
		if err := <-result; !errors.Is(err, context.Canceled) {
			t.Fatalf("queued batch after close: %v", err)
		}
	}
	if err := <-done; err == nil {
		t.Fatal("cancelled batch succeeded")
	}
	if _, err := send(target); err == nil {
		t.Fatal("accepted work after close")
	}
}

func TestDuplicateAndReverseCallbacks(t *testing.T) {
	var sequence []string
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		sequence = append(sequence, string(b))
	}))
	defer h.Close()
	s := fixture(t, mockserver.Config{AllowedCallbackTargets: []string{h.URL}, SigningKey: []byte("fixture")})
	done, err := s.Enqueue([]mockserver.Callback{{Target: h.URL, Body: []byte("opened")}, {Target: h.URL, Body: []byte("settled")}}, mockserver.Scenario{ReverseCallbacks: true, DuplicateCallbacks: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	h.Close()
	if want := "settled settled opened opened"; strings.Join(sequence, " ") != want {
		t.Fatalf("callbacks: %v", sequence)
	}
}

func TestContractReadsBothSides(t *testing.T) {
	s := fixture(t, mockserver.Config{})
	memory := store.NewMemory()
	scope := tender.Scope{MerchantID: "merchant", Environment: tender.EnvironmentMock}
	if err := contract.Verify(t.Context(), s, memory, scope, "op", "unknown", func(contract.Records) error { return nil }); err == nil || errors.Is(err, tender.ErrNotFound) {
		t.Fatal("missing simulator state accepted")
	}
	s.Commit("known", nil)
	if err := contract.Verify(context.Background(), s, memory, scope, "op", "known", func(contract.Records) error { return nil }); !errors.Is(err, tender.ErrNotFound) {
		t.Fatalf("missing host state: %v", err)
	}
}

func TestContractComparesPaymentAndOperation(t *testing.T) {
	s := fixture(t, mockserver.Config{})
	memory := store.NewMemory()
	scope := tender.Scope{MerchantID: "merchant", Environment: tender.EnvironmentMock}
	p, err := tender.NewPayment(scope, "payment", "order", tender.Money{Minor: 100, Currency: "USD"})
	if err != nil {
		t.Fatal(err)
	}
	p.AttemptID, p.Provider, p.MerchantAccount = "attempt", "fixture", "account"
	rec := s.Commit("known", []byte("100 USD"))
	op := tender.OperationRecord{Operation: tender.Operation{Scope: scope, ID: "op", PaymentID: p.ID, Key: "host-key", ProviderKey: "known", ProviderReference: rec.ID, Amount: p.Amount, AttemptID: p.AttemptID, Kind: tender.OpCreate, Status: tender.OperationSucceeded}}
	if err := memory.Transact(t.Context(), func(tx tender.Tx) error {
		if err := tx.InsertPayment(p); err != nil {
			return err
		}
		return tx.InsertOperation(op)
	}); err != nil {
		t.Fatal(err)
	}
	mismatch := errors.New("comparison mismatch")
	err = contract.Verify(t.Context(), s, memory, scope, "op", "known", func(records contract.Records) error {
		if records.Operation.ProviderReference != records.Fixture.ID || records.Payment.OrderID != "order" || string(records.Fixture.Data) != "100 USD" {
			t.Fatalf("inconsistent records: %+v", records)
		}
		return mismatch
	})
	if !errors.Is(err, mismatch) {
		t.Fatalf("comparison did not run or its result was lost: %v", err)
	}
}

func TestCloseAbortsDelayedCommittedResponse(t *testing.T) {
	committed := make(chan struct{})
	s := fixture(t, mockserver.Config{Routes: func(s *mockserver.Server) http.Handler {
		return s.Route("charge", func(w http.ResponseWriter, r *http.Request, _ mockserver.Scenario) {
			s.Commit("payment", []byte("captured"))
			close(committed)
			w.WriteHeader(http.StatusCreated)
		})
	}})
	if err := s.SetScenarios("charge", mockserver.Scenario{Delay: time.Hour, Fault: mockserver.LoseResponse}); err != nil {
		t.Fatal(err)
	}
	host := httptest.NewServer(s.Handler())
	defer host.Close()
	client := host.Client()
	client.Timeout = 5 * time.Second
	result := make(chan error, 1)
	go func() {
		resp, err := client.Post(host.URL, "application/json", nil)
		if err == nil {
			resp.Body.Close()
			result <- fmt.Errorf("received HTTP %d after close", resp.StatusCode)
			return
		}
		if !errors.Is(err, io.EOF) {
			result <- fmt.Errorf("expected an aborted HTTP stream, got %w", err)
			return
		}
		result <- nil
	}()
	select {
	case <-committed:
	case <-time.After(2 * time.Second):
		t.Fatal("fixture never committed")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if rec, ok := s.Lookup("payment"); !ok || string(rec.Data) != "captured" {
		t.Fatalf("lost committed state: %+v", rec)
	}
}

func TestRouteFactoryCanClose(t *testing.T) {
	result := make(chan error, 1)
	go func() {
		s, err := mockserver.New(mockserver.Config{PublicBaseURL: "http://localhost", Routes: func(s *mockserver.Server) http.Handler {
			_ = s.Close()
			return http.NotFoundHandler()
		}})
		if s != nil {
			_ = s.Close()
		}
		result <- err
	}()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("constructed a closed simulator")
		}
	case <-time.After(time.Second):
		t.Fatal("factory deadlocked in Close")
	}
}

func TestFailedRouteFactorySettlesCallbacks(t *testing.T) {
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer host.Close()
	var receipt <-chan error
	_, err := mockserver.New(mockserver.Config{PublicBaseURL: host.URL, AllowedCallbackTargets: []string{host.URL}, SigningKey: []byte("fixture"), Routes: func(s *mockserver.Server) http.Handler {
		var err error
		receipt, err = s.Enqueue([]mockserver.Callback{{Target: host.URL}}, mockserver.Scenario{})
		if err != nil {
			t.Fatal(err)
		}
		return nil
	}})
	if err == nil {
		t.Fatal("accepted nil handler")
	}
	select {
	case err := <-receipt:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("setup cleanup returned %v", err)
		}
	default:
		t.Fatal("callback still pending after failed setup")
	}
}
