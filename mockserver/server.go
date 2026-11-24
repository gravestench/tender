// Package mockserver provides an embeddable, loopback-only HTTP simulator.
// Fixtures supply the provider wire routes. The host owns the HTTP listener.
package mockserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Config describes one independent simulation. PublicBaseURL includes the host's
// mount prefix. Routes builds the fixture handler in process. SigningKey is a
// synthetic test secret required when any callback target is configured.
type Config struct {
	PublicBaseURL          string
	Seed                   int64
	Clock                  func() time.Time
	Routes                 func(*Server) http.Handler
	AllowedCallbackTargets []string
	SigningKey             []byte
}

// Record is an immutable committed fixture operation. Key must include the
// fixture's provider, merchant and environment when sharing a server.
type Record struct {
	Key, ID string
	At      time.Time
	Data    []byte
}

func (r Record) clone() Record {
	r.Data = bytes.Clone(r.Data)
	return r
}

// Server owns fixture state and a single bounded callback worker.
type Server struct {
	base    string
	seed    int64
	clock   func() time.Time
	handler http.Handler
	targets map[string]bool
	key     []byte
	client  *http.Client
	ctx     context.Context
	cancel  context.CancelFunc
	jobs    chan callbackBatch
	done    chan struct{}

	mu              sync.Mutex
	closed          bool
	active, pending int
	records         map[string]Record
	scenarios       map[string][]Scenario
}

// New validates the local endpoints and constructs a mountable simulator.
func New(cfg Config) (*Server, error) {
	base, err := localURL(cfg.PublicBaseURL)
	if err != nil || base.RawQuery != "" || base.ForceQuery {
		return nil, errors.New("mockserver: public base must be an absolute loopback HTTP(S) URL without a query")
	}
	targets := make(map[string]bool, len(cfg.AllowedCallbackTargets))
	for _, target := range cfg.AllowedCallbackTargets {
		if _, err := localURL(target); err != nil {
			return nil, errors.New("mockserver: callback target must be an absolute loopback HTTP(S) URL")
		}
		targets[target] = true
	}
	if len(targets) > 0 && len(cfg.SigningKey) == 0 {
		return nil, errors.New("mockserver: callbacks require a synthetic signing key")
	}
	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		base:      strings.TrimRight(base.String(), "/"),
		seed:      cfg.Seed,
		clock:     clock,
		handler:   http.NotFoundHandler(),
		targets:   targets,
		key:       bytes.Clone(cfg.SigningKey),
		client:    callbackClient(),
		ctx:       ctx,
		cancel:    cancel,
		jobs:      make(chan callbackBatch, 16),
		done:      make(chan struct{}),
		records:   make(map[string]Record),
		scenarios: make(map[string][]Scenario),
	}
	go s.deliver()
	if cfg.Routes != nil {
		var handler http.Handler
		// An unsuccessful factory (including a panic) can already have
		// enqueued callbacks. Resolve their receipts before leaving New.
		defer func() {
			if handler == nil {
				s.Close()
			}
		}()
		handler = cfg.Routes(s)
		if handler == nil {
			return nil, errors.New("mockserver: nil fixture handler")
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.closed {
			return nil, errors.New("mockserver: fixture closed during setup")
		}
		s.handler = handler
	}
	return s, nil
}

func loopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.Zone() == "" && addr.Unmap().IsLoopback()
}

func localURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || !loopback(u.Hostname()) || u.User != nil || strings.Contains(raw, "#") {
		return nil, errors.New("mockserver: invalid local URL")
	}
	return u, nil
}

// PublicBaseURL returns the configured origin and mount prefix without a trailing slash.
func (s *Server) PublicBaseURL() string { return s.base }

// Handler serves the configured fixture routes for loopback peers and hosts.
// The host can mount it under http.StripPrefix. Bind the host's listener to
// loopback and keep this mount off externally reachable reverse proxies.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer, _, err := net.SplitHostPort(r.RemoteAddr)
		host := (&url.URL{Host: r.Host}).Hostname()
		addr, parseErr := netip.ParseAddr(peer)
		if err != nil || parseErr != nil || !addr.Unmap().IsLoopback() || !loopback(host) {
			http.Error(w, "loopback required", http.StatusForbidden)
			return
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			http.Error(w, "simulator closed", http.StatusServiceUnavailable)
			return
		}
		s.active++
		handler := s.handler
		s.mu.Unlock()
		defer func() {
			s.mu.Lock()
			s.active--
			s.mu.Unlock()
		}()
		handler.ServeHTTP(w, r)
	})
}

// Commit records the first result for key. Replays return the original record.
// IDs depend only on the seed and key, including under concurrent dispatch.
func (s *Server) Commit(key string, data []byte) Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec, ok := s.records[key]; ok {
		return rec.clone()
	}
	hash := sha256.Sum256([]byte(fmt.Sprintf("%d:%s", s.seed, key)))
	rec := Record{Key: key, ID: fmt.Sprintf("mock_%x", hash[:16]), At: s.clock(), Data: bytes.Clone(data)}
	s.records[key] = rec
	return rec.clone()
}

// Lookup returns a copy of the committed fixture operation.
func (s *Server) Lookup(key string) (Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.records[key]
	return rec.clone(), ok
}

// Reset clears records and scripted scenarios. In-flight work must finish first.
func (s *Server) Reset() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("mockserver: closed")
	}
	if s.active != 0 || s.pending != 0 {
		return errors.New("mockserver: simulation in progress")
	}
	clear(s.records)
	clear(s.scenarios)
	return nil
}

// Close rejects new requests, cancels callback delivery and waits for its worker.
// The host remains responsible for stopping its listener and fixture handlers.
func (s *Server) Close() error {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		s.cancel()
		close(s.jobs)
	}
	s.mu.Unlock()
	<-s.done
	return nil
}
