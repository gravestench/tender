package main

import (
	"testing"
	"time"

	"github.com/gravestench/tender"
)

func TestMockDatabaseRejectsExternalHosts(t *testing.T) {
	for _, dsn := range []string{"host=database.example.invalid dbname=demo", "host=127.0.0.1,203.0.113.80 dbname=demo"} {
		db, err := openDB(dsn, "demo")
		if err == nil {
			db.Close()
			t.Errorf("accepted an external database host in %q", dsn)
		}
	}
}
func TestSyntheticCheckoutRequiresMock(t *testing.T) {
	t.Setenv("TENDER_CONSUMER_PROVIDER", "")
	for _, env := range []string{"SANDBOX", "LIVE", ""} {
		if err := run("127.0.0.1:invalid", env); err == nil || err.Error() != "consumer: checkout is available only for the MOCK fixture" {
			t.Errorf("environment %q: %v", env, err)
		}
	}
}
func TestFixtureActionMustBeCurrentInPageComponent(t *testing.T) {
	p := tender.Payment{Snapshot: tender.Snapshot{Status: tender.PaymentRequiresAction, Action: tender.CustomerAction{Mode: tender.ActionComponent, Data: "fixture-step", ExpiresAt: time.Now().Add(time.Minute)}}}
	if !allowAction(p) {
		t.Fatal("current component rejected")
	}
	for _, mode := range []tender.ActionMode{tender.ActionPopup, tender.ActionRedirect, tender.ActionFrame, tender.ActionModal} {
		other := p
		other.Action.Mode = mode
		if allowAction(other) {
			t.Errorf("allowed unconfigured %s", mode)
		}
	}
	p.Action.ExpiresAt = time.Now().Add(-time.Second)
	if allowAction(p) {
		t.Fatal("allowed expired component")
	}
}
