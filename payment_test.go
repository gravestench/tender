package tender

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestPaymentTransitions(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	newPayment := func(t *testing.T) Payment {
		t.Helper()
		p, err := NewPayment(Scope{"merchant", EnvironmentMock}, "payment", "order", Money{100, "USD"})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	apply := func(t *testing.T, p *Payment, s Snapshot) Change {
		t.Helper()
		c, err := p.Apply(s, now)
		if err != nil {
			t.Fatalf("apply %+v: %v", s, err)
		}
		return c
	}
	reject := func(t *testing.T, p *Payment, s Snapshot) {
		t.Helper()
		before := *p
		if _, err := p.Apply(s, now); !errors.Is(err, ErrValidation) || *p != before {
			t.Fatalf("invalid transition %+v changed payment or returned %v", s, err)
		}
	}
	t.Run("immediate capture and independent facts", func(t *testing.T) {
		p := newPayment(t)
		s := Snapshot{Status: PaymentCaptured, Funds: Funds{Authorized: 100, Captured: 100}, ProviderStatus: "Authorised"}
		c := apply(t, &p, s)
		if p.Snapshot != s || c.Before.Status != PaymentCreated || c.After != s || c.Version != p.Version {
			t.Fatalf("invalid applied change: %+v, %+v", p, c)
		}
		s.Funds.Refunded = 25
		s.Funds.Settled = 90
		s.Funds.Disputed = 15
		s.Funds.Returned = 10
		apply(t, &p, s)
		if p.Funds != (Funds{Authorized: 100, Captured: 100, Refunded: 25, Settled: 90, Disputed: 15, Returned: 10}) {
			t.Fatal(p.Funds)
		}
		s.Funds.Refunded = 24
		reject(t, &p, s)
		s.Funds.Refunded = 101
		reject(t, &p, s)
	})
	t.Run("expire pending and prohibit later collection", func(t *testing.T) {
		p := newPayment(t)
		apply(t, &p, Snapshot{Status: PaymentPending})
		apply(t, &p, Snapshot{Status: PaymentExpired})
		reject(t, &p, Snapshot{Status: PaymentCaptured, Funds: Funds{Authorized: 100, Captured: 100}})
	})
	t.Run("partial and complete captures", func(t *testing.T) {
		p := newPayment(t)
		apply(t, &p, Snapshot{Status: PaymentAuthorized, Funds: Funds{Authorized: 100}})
		reject(t, &p, Snapshot{Status: PaymentCaptured, Funds: Funds{Authorized: 100, Captured: 40}})
		apply(t, &p, Snapshot{Status: PaymentPartiallyCaptured, Funds: Funds{Authorized: 100, Captured: 40}})
		reject(t, &p, Snapshot{Status: PaymentPartiallyCaptured, Funds: Funds{Authorized: 100, Captured: 100}})
		apply(t, &p, Snapshot{Status: PaymentCaptured, Funds: Funds{Authorized: 100, Captured: 100}})
	})
	t.Run("terminal return freezes principal", func(t *testing.T) {
		p := newPayment(t)
		s := Snapshot{Status: PaymentReturned, Funds: Funds{Returned: 100}}
		apply(t, &p, s)
		s.Funds.Authorized = 100
		reject(t, &p, s)
		s.Funds.Captured = 100
		reject(t, &p, s)
		basis := Correction{Actor: "ops", Reason: "Matched capture in provider ledger"}
		c, err := p.Correct(s, now, basis)
		if err != nil || c.Correction != basis || c.Before.Funds.Captured != 0 || c.After.Funds.Captured != 100 {
			t.Fatalf("correction %+v, %v", c, err)
		}
	})
	t.Run("audited settlement supersedes erroneous void", func(t *testing.T) {
		p := newPayment(t)
		voided := Snapshot{Status: PaymentVoided, Funds: Funds{Authorized: 100}}
		apply(t, &p, voided)
		observed := Snapshot{Status: PaymentSettled, Funds: Funds{Authorized: 100, Captured: 100, Settled: 100}}
		reject(t, &p, observed)
		basis := Correction{Actor: "ops", Reason: "Authoritative settlement report confirms full capture"}
		change, err := p.Correct(observed, now, basis)
		want := Snapshot{Status: PaymentCaptured, Funds: Funds{Authorized: 100, Captured: 100, Settled: 100}}
		if err != nil || p.Snapshot != want || change.After != want || change.Before != voided || change.Correction != basis {
			t.Fatalf("audited settlement: %+v, %v", change, err)
		}
	})
	t.Run("settled partial capture can void remainder", func(t *testing.T) {
		p := newPayment(t)
		apply(t, &p, Snapshot{Status: PaymentAuthorized, Funds: Funds{Authorized: 100}})
		f := Funds{Authorized: 100, Captured: 40, Settled: 40}
		c := apply(t, &p, Snapshot{Status: PaymentSettled, Funds: f})
		if c.After.Status != PaymentPartiallyCaptured || p.Status != PaymentPartiallyCaptured {
			t.Errorf("settlement masked partial capture: %+v", p.Snapshot)
		}
		apply(t, &p, Snapshot{Status: PaymentVoided, Funds: f})
		if p.Funds.Captured != 40 || p.Funds.Settled != 40 {
			t.Fatal(p.Funds)
		}
	})
	t.Run("persisted settlement retains open authorization", func(t *testing.T) {
		for _, existingStatus := range []PaymentStatus{PaymentSettled, PaymentCaptured} {
			p := newPayment(t)
			p.Snapshot = Snapshot{Status: existingStatus, Funds: Funds{Authorized: 100, Captured: 40, Settled: 40}}
			before := p.Snapshot
			change := apply(t, &p, Snapshot{Status: PaymentVoided, Funds: before.Funds})
			if change.Before != before || p.Funds.Settled != 40 || p.Funds.Captured != 40 {
				t.Fatal(change)
			}
		}
	})
	t.Run("pending and expiring action", func(t *testing.T) {
		p := newPayment(t)
		apply(t, &p, Snapshot{Status: PaymentPending})
		action := CustomerAction{Mode: ActionFrame, Data: "opaque", ExpiresAt: now}
		reject(t, &p, Snapshot{Status: PaymentRequiresAction, Action: action})
		action.ExpiresAt = now.Add(time.Minute)
		apply(t, &p, Snapshot{Status: PaymentRequiresAction, Action: action})
		apply(t, &p, Snapshot{Status: PaymentAuthorized, Funds: Funds{Authorized: 100}})
		reject(t, &p, Snapshot{Status: PaymentCaptured, Funds: Funds{Authorized: 100, Captured: 101}})
		apply(t, &p, Snapshot{Status: PaymentPartiallyCaptured, Funds: Funds{Authorized: 100, Captured: 40}})
		apply(t, &p, Snapshot{Status: PaymentVoided, Funds: Funds{Authorized: 100, Captured: 40}})
		reject(t, &p, Snapshot{Status: PaymentCaptured, Funds: Funds{Authorized: 100, Captured: 100}})
	})
	t.Run("bank return before settlement", func(t *testing.T) {
		p := newPayment(t)
		apply(t, &p, Snapshot{Status: PaymentPending})
		apply(t, &p, Snapshot{Status: PaymentReturned, Funds: Funds{Returned: 100}})
		reject(t, &p, Snapshot{Status: PaymentCaptured, Funds: Funds{Authorized: 100, Captured: 100, Returned: 100}})
		if p.Funds.Settled != 0 || p.Funds.Captured != 0 {
			t.Fatal(p.Funds)
		}
	})
	t.Run("return after bank settlement", func(t *testing.T) {
		p := newPayment(t)
		apply(t, &p, Snapshot{Status: PaymentPending})
		apply(t, &p, Snapshot{Status: PaymentSettled, Funds: Funds{Settled: 100}})
		apply(t, &p, Snapshot{Status: PaymentSettled, Funds: Funds{Settled: 100, Returned: 100}})
		if p.Funds.Captured != 0 || p.Funds.Settled != 100 || p.Funds.Returned != 100 {
			t.Fatal(p.Funds)
		}
	})
	t.Run("terminal outcomes require audited correction", func(t *testing.T) {
		for _, terminal := range []PaymentStatus{PaymentDeclined, PaymentFailed, PaymentVoided} {
			p := newPayment(t)
			apply(t, &p, Snapshot{Status: terminal})
			next := Snapshot{Status: PaymentCaptured, Funds: Funds{Authorized: 100, Captured: 100}}
			reject(t, &p, next)
			if _, err := p.Correct(next, now, Correction{Actor: "ops"}); !errors.Is(err, ErrValidation) {
				t.Fatal("missing correction reason", err)
			}
			basis := Correction{Actor: "ops", Reason: "Matched acquiring report line 41"}
			c, err := p.Correct(next, now, basis)
			if err != nil || c.Correction != basis || c.At != now || c.Before.Status != terminal || c.After != next || p.Snapshot != next {
				t.Fatalf("correction: %+v, %v", c, err)
			}
		}
	})
	t.Run("unknown operation can be authoritatively resolved", func(t *testing.T) {
		op := Operation{Status: OperationReserved}
		for _, status := range []OperationStatus{OperationUnknown, OperationSucceeded} {
			if err := op.SetStatus(status); err != nil {
				t.Fatal(err)
			}
		}
		if err := op.SetStatus(OperationUnknown); !errors.Is(err, ErrConflict) || op.Status != OperationSucceeded {
			t.Fatalf("late timeout: %+v, %v", op, err)
		}
		if err := op.SetStatus("unexpected"); !errors.Is(err, ErrValidation) {
			t.Fatalf("unknown status accepted: %v", err)
		}
	})
	t.Run("invalid snapshots and terminal financial changes", func(t *testing.T) {
		p := newPayment(t)
		for _, s := range []Snapshot{
			{Status: "unmapped"}, {Status: PaymentAuthorized}, {Status: PaymentCaptured}, {Status: PaymentSettled},
			{Status: PaymentPending, Funds: Funds{Captured: 10}}, {Status: PaymentDeclined, Funds: Funds{Authorized: 10}},
			{Status: PaymentRequiresAction}, {Status: PaymentCreated, Action: CustomerAction{Mode: ActionFrame}},
			{Status: PaymentAuthorized, Funds: Funds{Authorized: -1}}, {Status: PaymentAuthorized, Funds: Funds{Authorized: 101}},
			{Status: PaymentPending, Funds: Funds{Disputed: 101}}, {Status: PaymentPending, Funds: Funds{Returned: 101}},
		} {
			reject(t, &p, s)
		}
		apply(t, &p, Snapshot{Status: PaymentAuthorized, Funds: Funds{Authorized: 100}})
		apply(t, &p, Snapshot{Status: PaymentVoided, Funds: Funds{Authorized: 100}})
		reject(t, &p, Snapshot{Status: PaymentVoided, Funds: Funds{Authorized: 100, Captured: 100}})
	})
	t.Run("scope and capabilities", func(t *testing.T) {
		for _, s := range []Scope{{"merchant", "TEST"}, {"", EnvironmentLive}, {"merchant", ""}} {
			if _, err := NewPayment(s, "id", "order", Money{1, "USD"}); !errors.Is(err, ErrValidation) {
				t.Fatalf("invalid scope: %v", err)
			}
		}
		caps := Capabilities{Create: true, Query: true}
		if !caps.Supports(OpCreate) || caps.Supports(OpRefund) || caps.Supports("invented") {
			t.Fatal("incorrect capabilities")
		}
	})
	t.Run("provider errors and safe status", func(t *testing.T) {
		for _, kind := range []ErrorKind{ErrUnsupported, ErrDeclined, ErrValidation, ErrAuthentication, ErrRateLimit, ErrTransport, ErrUnknownOutcome} {
			e := NewProviderError(kind, "Pending")
			if !errors.Is(e, kind) || e.ProviderStatus != "Pending" {
				t.Fatalf("lost category or status: %v", e)
			}
		}
		for _, raw := range []string{"Error\nsecret=123", strings.Repeat("A", 65)} {
			e := NewProviderError(ErrTransport, raw)
			if e.ProviderStatus != "" || strings.Contains(e.Error(), raw) {
				t.Fatal("unsafe provider status retained")
			}
			p := newPayment(t)
			apply(t, &p, Snapshot{Status: PaymentPending, ProviderStatus: raw})
			if p.ProviderStatus != "" {
				t.Fatal("unsafe status persisted")
			}
		}
	})
}

func TestMarketCapabilities(t *testing.T) {
	us := Market{Country: "US", Currency: "USD", Method: MethodVisa}
	ca := Market{Country: "CA", Currency: "CAD", Method: MethodVisa}
	caps := Capabilities{Create: true, Capture: true, Refund: true, Markets: []MarketCapability{
		{Market: us, PartialCapture: true},
		{Market: ca, PartialRefund: true},
	}}
	for _, c := range []struct {
		kind          OperationKind
		market        Market
		partial, want bool
	}{
		{OpCreate, us, false, true}, {OpRefund, ca, true, true}, {OpRefund, us, true, false},
		{OpCapture, ca, true, false}, {OpCapture, us, true, true}, {OpRefund, us, false, true},
		{OpCreate, us, true, false}, {OpVoid, us, false, false},
		{OpCreate, Market{"US", "USD", MethodMastercard}, false, false},
		{OpCreate, Market{"CA", "USD", MethodVisa}, false, false},
		{OpCreate, Market{"US", "CAD", MethodVisa}, false, false},
		{OpCreate, Market{}, false, false},
	} {
		if got := caps.SupportsIn(c.kind, c.market, c.partial); got != c.want {
			t.Errorf("SupportsIn(%s,%+v,partial=%v) = %v, want %v", c.kind, c.market, c.partial, got, c.want)
		}
	}
	if (Capabilities{Create: true}).SupportsIn(OpCreate, us, false) {
		t.Error("missing market eligibility must fail closed")
	}
}
