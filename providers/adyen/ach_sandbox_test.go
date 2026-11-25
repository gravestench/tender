//go:build sandbox

package adyen_test

import (
	"errors"
	"net/http"
	"testing"

	"github.com/gravestench/tender"
	"github.com/gravestench/tender/providers/adyen"
)

func TestACHSandboxQualificationIsolation(t *testing.T) {
	cfg := adyen.Config{Environment: tender.EnvironmentSandbox, BaseURL: "https://checkout-test.adyen.com/v72", APIKey: "test-key", MerchantAccount: "merchant", Origin: "https://shop.example.test", ReturnURL: "https://shop.example.test/return"}
	ordinary, err := adyen.NewACH(cfg)
	if err != nil || ordinary.Capabilities().Create {
		t.Fatal("ordinary sandbox ACH became available", err)
	}
	qualified, err := adyen.NewACHSandboxQualification(cfg)
	if err != nil || !qualified.Capabilities().Create || !qualified.Capabilities().Confirm {
		t.Fatal("protected qualification cannot exercise ACH adapter", err)
	}
	for _, mutate := range []func(*adyen.Config){
		func(c *adyen.Config) { c.Environment = tender.EnvironmentLive },
		func(c *adyen.Config) { c.Environment = tender.EnvironmentMock },
		func(c *adyen.Config) { c.BaseURL = "http://127.0.0.1:8080/v72" },
		func(c *adyen.Config) { c.Transport = http.DefaultTransport },
		func(c *adyen.Config) {
			c.Origin = "http://127.0.0.1:8080"
			c.ReturnURL = "http://127.0.0.1:8080/return"
		},
	} {
		bad := cfg
		mutate(&bad)
		if _, err := adyen.NewACHSandboxQualification(bad); !errors.Is(err, tender.ErrValidation) {
			t.Fatalf("unsafe qualification config accepted: %v", err)
		}
	}
}
