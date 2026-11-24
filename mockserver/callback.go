package mockserver

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"
)

// Callback is a synthetic POST to an exact, preconfigured URL. Header and
// Body may carry provider-specific test signatures. The worker additionally
// signs the raw body with HMAC-SHA256 in X-Tender-Mock-Signature (lowercase hex).
type Callback struct {
	Target string
	Header http.Header
	Body   []byte
}

type callbackBatch struct {
	events    []Callback
	duplicate bool
	result    chan error
}

func callbackClient() *http.Client {
	// No proxy, DNS lookup or redirect may change the configured destination.
	dialer := new(net.Dialer)
	transport := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil || !loopback(host) {
			return nil, errors.New("mockserver: callback dial denied")
		}
		if strings.EqualFold(host, "localhost") {
			host = "127.0.0.1"
		}
		return dialer.DialContext(ctx, network, net.JoinHostPort(host, port))
	}}
	return &http.Client{Transport: transport, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// Enqueue submits up to 32 callbacks to a single FIFO worker. At most 16
// batches can wait. A full queue returns an error immediately. A buffered
// receipt returns one error (nil on success) and closes, including on shutdown.
// Delivery stops at the first failed POST in the batch.
func (s *Server) Enqueue(events []Callback, step Scenario) (<-chan error, error) {
	if len(events) == 0 || len(events) > 32 {
		return nil, errors.New("mockserver: expected 1 to 32 callbacks")
	}
	for _, event := range events {
		if !s.targets[event.Target] {
			return nil, errors.New("mockserver: callback endpoint denied")
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errors.New("mockserver: closed")
	}
	if len(s.jobs) == cap(s.jobs) {
		return nil, errors.New("mockserver: callback queue full")
	}
	batch := callbackBatch{events: make([]Callback, len(events)), duplicate: step.DuplicateCallbacks, result: make(chan error, 1)}
	for i, event := range events {
		batch.events[i] = Callback{Target: event.Target, Body: bytes.Clone(event.Body), Header: event.Header.Clone()}
	}
	if step.ReverseCallbacks {
		slices.Reverse(batch.events)
	}
	s.pending++
	s.jobs <- batch
	return batch.result, nil
}

func (s *Server) deliver() {
	defer close(s.done)
	defer s.client.CloseIdleConnections()
	for batch := range s.jobs {
		err := s.deliverBatch(batch)
		s.mu.Lock()
		s.pending--
		s.mu.Unlock()
		batch.result <- err
		close(batch.result)
	}
}

func (s *Server) deliverBatch(batch callbackBatch) error {
	copies := 1
	if batch.duplicate {
		copies = 2
	}
	for _, event := range batch.events {
		for range copies {
			if err := s.ctx.Err(); err != nil {
				return err
			}
			if err := s.post(event); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Server) post(event Callback) error {
	req, err := http.NewRequestWithContext(s.ctx, http.MethodPost, event.Target, bytes.NewReader(event.Body))
	if err != nil {
		return err
	}
	for key, values := range event.Header {
		for _, v := range values {
			req.Header.Add(key, v)
		}
	}
	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	mac := hmac.New(sha256.New, s.key)
	mac.Write(event.Body)
	req.Header.Set("X-Tender-Mock-Signature", hex.EncodeToString(mac.Sum(nil)))
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("mockserver: callback returned HTTP %d", resp.StatusCode)
	}
	_, err = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return err
}
