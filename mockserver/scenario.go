package mockserver

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"time"
)

// Fault alters a fixture response. A zero Fault leaves the response intact.
type Fault string

const (
	Throttle          Fault = "throttle"
	Unavailable       Fault = "unavailable"
	MalformedResponse Fault = "malformed"
	LoseResponse      Fault = "lost"
)

// Scenario scripts one invocation. Delay holds the response after the fixture
// runs. Callback controls apply to a batch enqueued by that fixture.
type Scenario struct {
	Fault                                Fault
	Delay                                time.Duration
	ReverseCallbacks, DuplicateCallbacks bool
}

// SetScenarios replaces the FIFO script for a named route. Requests resume
// normal behavior after the last step. This operation is in-process only.
func (s *Server) SetScenarios(name string, steps ...Scenario) error {
	for _, step := range steps {
		switch step.Fault {
		case "", Throttle, Unavailable, MalformedResponse, LoseResponse:
		default:
			return errors.New("mockserver: unknown fault")
		}
		if step.Delay < 0 {
			return errors.New("mockserver: negative delay")
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("mockserver: closed")
	}
	s.scenarios[name] = slices.Clone(steps)
	return nil
}

// Route wraps a provider fixture with its scenario script. Throttling and
// unavailability skip the fixture. Corrupt and lost replies preserve its
// committed state. A lost response aborts the HTTP stream via http.ErrAbortHandler.
func (s *Server) Route(name string, fixture func(http.ResponseWriter, *http.Request, Scenario)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		var step Scenario
		if steps := s.scenarios[name]; len(steps) > 0 {
			step = steps[0]
			s.scenarios[name] = steps[1:]
		}
		s.mu.Unlock()
		switch step.Fault {
		case Throttle:
			w.Header().Set("Retry-After", "1")
			http.Error(w, "throttled", http.StatusTooManyRequests)
			return
		case Unavailable:
			http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
		if step.Fault == "" && step.Delay == 0 {
			fixture(w, r, step)
			return
		}
		reply := httptest.NewRecorder()
		fixture(reply, r, step)
		if step.Delay > 0 {
			timer := time.NewTimer(step.Delay)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-r.Context().Done():
				return
			case <-s.ctx.Done():
				return
			}
		}
		switch step.Fault {
		case LoseResponse:
			panic(http.ErrAbortHandler)
		case MalformedResponse:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("{"))
		default:
			res := reply.Result()
			defer res.Body.Close()
			for key, values := range res.Header {
				for _, v := range values {
					w.Header().Add(key, v)
				}
			}
			w.WriteHeader(res.StatusCode)
			_, _ = w.Write(reply.Body.Bytes())
		}
	})
}
