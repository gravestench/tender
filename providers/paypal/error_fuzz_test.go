package paypal

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/gravestench/tender"
)

// Mutated provider failures must never be interpreted as successful financial
// POSTs. Only a narrowly documented response may close an attempt.
func FuzzPayPalMutationRejection(f *testing.F) {
	for _, seed := range []struct {
		status int
		body   []byte
	}{
		{403, []byte(`{"name":"NOT_AUTHORIZED"}`)},
		{422, []byte(`{"name":"UNPROCESSABLE_ENTITY","details":[{"issue":"INSTRUMENT_DECLINED"}]}`)},
		{422, []byte(`{"name":"UNPROCESSABLE_ENTITY","details":[{"issue":"UNKNOWN"}]}`)},
		{500, []byte(`{"name":"NOT_AUTHORIZED"}`)},
		{400, []byte(`not-json`)},
	} {
		f.Add(seed.status, seed.body)
	}
	f.Fuzz(func(t *testing.T, status int, body []byte) {
		if len(body) > 20000 {
			return
		}
		a := &Adapter{cfg: Config{BaseURL: "https://api-m.sandbox.paypal.com"}}
		res := &http.Response{StatusCode: status, Body: io.NopCloser(bytes.NewReader(body))}
		err := a.rejection("https://api-m.sandbox.paypal.com/v2/checkout/orders/ORDER-1/capture", res)
		if err == nil {
			t.Fatal("provider failure became success")
		}
		if status == 403 && bytes.Equal(body, []byte(`{"name":"NOT_AUTHORIZED"}`)) && !errors.Is(err, tender.ErrAuthentication) {
			t.Fatalf("documented permission denial was not classified: %v", err)
		}
		if status == 422 && bytes.Equal(body, []byte(`{"name":"UNPROCESSABLE_ENTITY","details":[{"issue":"INSTRUMENT_DECLINED"}]}`)) && !errors.Is(err, tender.ErrDeclined) {
			t.Fatalf("documented funding decline was not classified: %v", err)
		}
		if status != 400 && status != 403 && status != 422 && !errors.Is(err, tender.ErrUnknownOutcome) {
			t.Fatalf("unlisted HTTP %d closed an uncertain attempt: %v", status, err)
		}
	})
}
