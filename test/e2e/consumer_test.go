//go:build e2e

package e2e_test

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

type demo struct{ Origin, Guest, Account string }

func launch(t *testing.T, extra ...string) demo {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "consumer")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = "../../examples/consumer"
	build.Env = append(os.Environ(), "GOWORK=off", "GOPROXY=off")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build consumer: %s: %v", out, err)
	}
	cmd := exec.CommandContext(t.Context(), bin, "-listen", "127.0.0.1:0")
	cmd.Env = append([]string{"TENDER_CONSUMER_DATABASE_URL=", "GORACE=halt_on_error=1"}, extra...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	var d demo
	ready := make(chan error, 1)
	go func() { ready <- json.NewDecoder(bufio.NewReader(stdout)).Decode(&d) }()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatalf("consumer startup: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("startup timeout")
	}
	return d
}

func client(t *testing.T, origin string) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	tr := &http.Transport{} // direct connections, including on developer machines with a proxy
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Jar: jar, Transport: localOnly{origin, tr}, Timeout: 5 * time.Second}
}

type localOnly struct {
	origin    string
	transport http.RoundTripper
}

func (l localOnly) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme+"://"+req.URL.Host != l.origin {
		return nil, &url.Error{Op: "egress denied", URL: req.URL.Redacted(), Err: http.ErrNotSupported}
	}
	return l.transport.RoundTrip(req)
}

type page struct {
	status     int
	body, path string
	header     http.Header
}

func request(t *testing.T, c *http.Client, method, target, origin string, fields url.Values) page {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, target, strings.NewReader(fields.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	if fields != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return page{resp.StatusCode, string(b), resp.Request.URL.Path, resp.Header}
}

var csrfField = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

func token(t *testing.T, p page) string {
	t.Helper()
	if h := p.header.Get("X-CSRF-Token"); h != "" {
		return h
	}
	m := csrfField.FindStringSubmatch(p.body)
	if len(m) != 2 {
		t.Fatalf("checkout has no CSRF form token (status %d)", p.status)
	}
	return m[1]
}

func TestConsumerMountsOnOneListener(t *testing.T) {
	d := launch(t)
	g, a := client(t, d.Origin), client(t, d.Origin)
	guest := request(t, g, "GET", d.Guest, "", nil)
	account := request(t, a, "GET", d.Account, "", nil)
	if guest.status != 200 || account.status != 200 || guest.path == account.path {
		t.Fatalf("fixture sessions: guest %d account %d", guest.status, account.status)
	}
	if again := request(t, client(t, d.Origin), "GET", d.Guest, "", nil); again.status != 404 {
		t.Fatalf("used ticket: %d", again.status)
	}
	if sim := request(t, g, "GET", d.Origin+"/simulator/scenarios", "", nil); sim.status != 200 || !strings.Contains(sim.body, "pending") {
		t.Fatalf("simulator: %+v", sim)
	}
	if csp := guest.header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") || strings.Contains(csp, "https:") {
		t.Fatalf("MOCK CSP: %q", csp)
	}
	for _, suffix := range []string{"", "/status"} {
		if got := request(t, g, "GET", d.Origin+account.path+suffix, "", nil); got.status != 404 {
			t.Fatalf("guest accessing account %s: %d", suffix, got.status)
		}
		if got := request(t, a, "GET", d.Origin+guest.path+suffix, "", nil); got.status != 404 {
			t.Fatalf("account accessing guest %s: %d", suffix, got.status)
		}
		if got := request(t, client(t, d.Origin), "GET", d.Origin+guest.path+suffix, "", nil); got.status != 401 {
			t.Fatalf("anonymous %s: %d", suffix, got.status)
		}
	}
	for _, tc := range []struct {
		path, origin, csrf string
		want               int
	}{
		{guest.path, d.Origin, "", 403}, {guest.path, "https://foreign.invalid", token(t, guest), 403},
		{account.path, d.Origin, token(t, guest), 404}, {guest.path, d.Origin, token(t, account), 403},
	} {
		got := request(t, g, "POST", d.Origin+tc.path, tc.origin, url.Values{"scenario": {"collected"}, "csrf": {tc.csrf}})
		if got.status != tc.want {
			t.Errorf("unauthorized checkout got %d want %d", got.status, tc.want)
		}
	}
}

type orderStatus struct {
	OrderID, PaymentID, Currency, Status string
	Minor                                int64
	Fulfilled                            bool
}

func status(t *testing.T, c *http.Client, target string) orderStatus {
	t.Helper()
	p := request(t, c, "GET", target+"/status", "", nil)
	var s orderStatus
	if p.status != 200 || json.Unmarshal([]byte(p.body), &s) != nil {
		t.Fatalf("status: %d %s", p.status, p.body)
	}
	return s
}

func TestTrustedOrderAmount(t *testing.T) {
	d := launch(t)
	c := client(t, d.Origin)
	p := request(t, c, "GET", d.Guest, "", nil)
	csrf := token(t, p)
	target := d.Origin + p.path
	for _, fields := range []url.Values{
		{"scenario": {"collected"}, "csrf": {csrf}, "amount": {"1"}, "currency": {"JPY"}},
		{"scenario": {"collected", "pending"}, "csrf": {csrf}},
		{"scenario": {"unknown"}, "csrf": {csrf}},
	} {
		if got := request(t, c, "POST", target, d.Origin, fields); got.status != 400 {
			t.Fatalf("unexpected input accepted: %d", got.status)
		}
	}
	if pre := status(t, c, target); pre.PaymentID != "" || pre.Fulfilled {
		t.Fatalf("unexpected payment after invalid submissions: %+v", pre)
	}
	fields := url.Values{"scenario": {"pending"}, "csrf": {csrf}}
	for range 2 {
		if got := request(t, c, "POST", target, d.Origin, fields); got.status != 200 {
			t.Fatalf("checkout: %d %s", got.status, got.body)
		}
		s := status(t, c, target)
		if s.Minor != 2599 || s.Currency != "USD" || s.Status != "pending" || s.Fulfilled || s.PaymentID == "" || s.OrderID == s.PaymentID {
			t.Fatalf("payment: %+v", s)
		}
	}
	if got := request(t, c, "POST", target, d.Origin, url.Values{"csrf": {csrf}, "scenario": {"collected"}}); got.status != 409 {
		t.Fatalf("changed replay: %d", got.status)
	}
}

func TestOfflineCheckoutBrowser(t *testing.T) {
	d := launch(t)
	cmd := exec.CommandContext(t.Context(), "node", "offline-browser.cjs")
	cmd.Env = append(os.Environ(), "TENDER_ORIGIN="+d.Origin, "TENDER_GUEST="+d.Guest)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("browser: %s: %v", output, err)
	}
}

func TestConsumerSyntheticAction(t *testing.T) {
	d := launch(t)
	c := client(t, d.Origin)
	p := request(t, c, "GET", d.Account, "", nil)
	target := d.Origin + p.path
	action := request(t, c, "POST", target, d.Origin, url.Values{"csrf": {token(t, p)}, "scenario": {"action"}})
	if action.status != 200 || !strings.Contains(action.body, "Complete synthetic action") {
		t.Fatalf("in-page action missing: %d", action.status)
	}
	if s := status(t, c, target); s.Status != "requires_action" || s.Fulfilled {
		t.Fatalf("before confirmation: %+v", s)
	}
	for range 2 {
		confirmed := request(t, c, "POST", target+"/confirm", d.Origin, url.Values{"csrf": {token(t, action)}})
		if confirmed.status != 200 {
			t.Fatalf("confirmation: %d %s", confirmed.status, confirmed.body)
		}
		if s := status(t, c, target); s.Status != "captured" || s.Minor != 4895 {
			t.Fatalf("after confirmation: %+v", s)
		}
	}
}
