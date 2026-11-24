package main

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"time"

	"github.com/gravestench/tender"
	"github.com/gravestench/tender/mockserver"
)

var scenarios = []string{"pending", "action", "collected"}

func knownScenario(s string) bool { return slices.Contains(scenarios, s) }

func fixtureRoutes(sim *mockserver.Server) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /scenarios", sim.Route("scenarios", func(w http.ResponseWriter, _ *http.Request, _ mockserver.Scenario) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(scenarios)
	}))
	return mux
}

// fixture is an in-process simulation for exercising the consumer seam. It
// does not implement any processor's protocol or perform network requests.
type fixture struct{ sim *mockserver.Server }

func (fixture) Capabilities() tender.Capabilities {
	return tender.Capabilities{Create: true, Confirm: true, Markets: []tender.MarketCapability{{Market: market}}, ActionModes: []tender.ActionMode{tender.ActionComponent}}
}

func (f fixture) Execute(_ context.Context, r tender.ProviderRequest) (tender.Observation, error) {
	var snapshot tender.Snapshot
	switch {
	case r.Operation.Kind == tender.OpCreate && r.Token == "pending":
		snapshot.Status = tender.PaymentPending
	case r.Operation.Kind == tender.OpCreate && r.Token == "action":
		snapshot.Status = tender.PaymentRequiresAction
		snapshot.Action = tender.CustomerAction{Mode: tender.ActionComponent, Data: "fixture-step", ExpiresAt: time.Now().Add(5 * time.Minute)}
	case r.Operation.Kind == tender.OpCreate && r.Token == "collected", r.Operation.Kind == tender.OpConfirm && r.Token == "fixture-approved":
		snapshot.Status = tender.PaymentCaptured
		snapshot.VerifiedMethod = tender.MethodVisa
		snapshot.Funds = tender.Funds{Authorized: r.Payment.Amount.Minor, Captured: r.Payment.Amount.Minor}
	default:
		return tender.Observation{}, tender.ErrUnsupported
	}
	body, err := json.Marshal(snapshot)
	if err != nil {
		return tender.Observation{}, err
	}
	record := f.sim.Commit("synthetic/consumer-demo/MOCK/"+r.Operation.ProviderKey, body)
	if err := json.Unmarshal(record.Data, &snapshot); err != nil {
		return tender.Observation{}, err
	}
	return tender.Observation{Scope: r.Payment.Scope, MerchantAccount: r.Payment.MerchantAccount, PaymentID: r.Payment.ID, AttemptID: r.Payment.AttemptID, OperationID: r.Operation.ID,
		Amount: r.Operation.Amount, Status: tender.OperationSucceeded, ProviderReference: "fixture-" + r.Payment.ID, OperationReference: record.ID, Snapshot: snapshot}, nil
}

func (fixture) Query(context.Context, tender.ProviderRequest) (tender.Observation, error) {
	return tender.Observation{}, tender.ErrUnsupported
}
