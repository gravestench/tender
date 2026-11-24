package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gravestench/tender"
	"github.com/gravestench/tender/providers/adyen"
)

func TestAdyenBrowserActions(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name   string
		mode   tender.ActionMode
		data   string
		expiry time.Time
		want   string
	}{
		{"prepare", tender.ActionComponent, `{"type":"card","brands":["visa"]}`, now.Add(time.Minute), "card"},
		{"native challenge", tender.ActionComponent, `{"type":"threeDS2","reference":"ref","action":{"type":"threeDS2","subtype":"challenge","token":"opaque","authorisationToken":"auth","paymentMethodType":"scheme"}}`, now.Add(time.Minute), "threeDS2"},
		{"fingerprint SDK fallback", tender.ActionComponent, `{"type":"threeDS2","reference":"ref","action":{"type":"threeDS2","subtype":"fingerprint","token":"opaque","authorisationToken":"auth","paymentMethodType":"scheme"}}`, now.Add(time.Minute), ""},
		{"unapproved popup", tender.ActionPopup, `{"type":"card","brands":["visa"]}`, now.Add(time.Minute), ""},
		{"unexpected URL", tender.ActionComponent, `{"type":"threeDS2","reference":"ref","action":{"type":"threeDS2","subtype":"challenge","url":"https://issuer.invalid","token":"opaque","authorisationToken":"auth","paymentMethodType":"scheme"}}`, now.Add(time.Minute), ""},
		{"expired", tender.ActionComponent, `{"type":"card","brands":["visa"]}`, now, ""},
		{"wrong brand", tender.ActionComponent, `{"type":"card","brands":["mc"]}`, now.Add(time.Minute), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := tender.Payment{Market: market, Snapshot: tender.Snapshot{Status: tender.PaymentRequiresAction, Action: tender.CustomerAction{Mode: tc.mode, Data: tc.data, ExpiresAt: tc.expiry}}}
			if got := browserAction(p, now).Type; got != tc.want {
				t.Fatalf("action type %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSandboxCheckoutOriginsAreExact(t *testing.T) {
	for _, origin := range []string{"https://*.example.test", "https://issuer.example.test;", "https://user@issuer.example.test", "https://issuer.example.test/path", "https://issuer.example.test?", "https://issuer.example.test#", "http://issuer.example.test"} {
		if sandboxOrigin(origin) {
			t.Errorf("accepted non-origin %q", origin)
		}
	}
	if !sandboxOrigin("https://checkout.example.test:9443") {
		t.Fatal("registered sandbox origin was rejected")
	}
}

func TestSandboxChallengeChecksItsDecodedDestinations(t *testing.T) {
	a := &app{scope: tender.Scope{Environment: tender.EnvironmentSandbox}, card: &cardCheckout{acsOrigin: "https://issuer.example.test"}}
	for _, tc := range []struct {
		name, acs, notification string
		want                    bool
	}{
		{"approved", "https://issuer.example.test/challenge", shopperOrigin + "/notify", true},
		{"unapproved issuer", "https://unapproved.example.test/challenge", shopperOrigin + "/notify", false},
		{"foreign notification", "https://issuer.example.test/challenge", "https://unapproved.example.test/notify", false},
		{"missing ACS", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(map[string]string{"acsURL": tc.acs, "threeDSNotificationURL": tc.notification, "acsTransID": "acs", "threeDSServerTransID": "server", "messageVersion": "2.2.0"})
			action, _ := json.Marshal(adyen.CardAction{Type: "threeDS2", Reference: "internal", Action: &adyen.NativeAction{Type: "threeDS2", Subtype: "challenge", Token: base64.StdEncoding.EncodeToString(raw), PaymentMethodType: "scheme", PaymentData: "internal-binding", AuthorisationToken: "opaque"}})
			p := tender.Payment{Snapshot: tender.Snapshot{Status: tender.PaymentRequiresAction, Action: tender.CustomerAction{Mode: tender.ActionComponent, Data: string(action), ExpiresAt: time.Now().Add(time.Minute)}}}
			got := a.cardAction(p)
			if (got.Type == "threeDS2") != tc.want || got.Reference != "" || (got.Action != nil && got.Action.PaymentData != "") {
				t.Fatal("incorrect browser action or private reference exposed")
			}
		})
	}
}

func TestHostRejectsUnencryptedCardFields(t *testing.T) {
	a := &app{card: &cardCheckout{shopperEmail: "test@example.invalid"}}
	method := map[string]any{"type": "scheme", "brand": "visa", "holderName": "Fixture", "encryptedCardNumber": "eyJfixture..a.b.c", "encryptedExpiryMonth": "eyJfixture..a.b.c", "encryptedExpiryYear": "eyJfixture..a.b.c", "encryptedSecurityCode": "unwrapped"}
	raw, _ := json.Marshal(map[string]any{"paymentMethod": method, "browserInfo": map[string]any{"userAgent": "test"}})
	r := httptest.NewRequest("POST", "https://checkout.example.test/orders/order/confirm", bytes.NewReader(raw))
	r.RemoteAddr = "127.0.0.1:12345"
	r.Header.Set("X-Tender-Client-IP", "192.0.2.7")
	if _, err := a.browserToken(httptest.NewRecorder(), r, "card"); err == nil {
		t.Fatal("host accepted non-JWE credential field")
	}
}
