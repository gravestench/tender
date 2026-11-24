package mockserver

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"testing/synctest"
)

// stalledDelivery holds the callback inside RoundTrip while cancellation and
// constructor return proceed independently.
type stalledDelivery struct {
	started chan struct{}
	release chan struct{}
}

func (d stalledDelivery) RoundTrip(r *http.Request) (*http.Response, error) {
	close(d.started)
	<-d.release
	return nil, r.Context().Err()
}

func TestConcurrentFactoryCloseJoinsCallbacks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const target = "http://127.0.0.1/events"
		delivery := stalledDelivery{started: make(chan struct{}), release: make(chan struct{})}
		returned := make(chan error, 1)
		var receipt <-chan error

		go func() {
			_, err := New(Config{
				PublicBaseURL: target, AllowedCallbackTargets: []string{target}, SigningKey: []byte("fixture"),
				Routes: func(s *Server) http.Handler {
					s.client.Transport = delivery
					var err error
					receipt, err = s.Enqueue([]Callback{{Target: target}}, Scenario{})
					if err != nil {
						t.Error(err)
						return nil
					}
					<-delivery.started
					go s.Close()
					<-s.ctx.Done()
					return http.NotFoundHandler()
				},
			})
			returned <- err
			close(returned)
		}()

		// At quiescence the factory has returned a valid handler, Close has
		// canceled the request, and the transport has yet to release the worker.
		synctest.Wait()
		select {
		case <-returned:
			t.Error("New returned while the accepted callback was still pending")
		default:
		}

		close(delivery.release)
		if err, ok := <-returned; ok && err == nil {
			t.Error("accepted a closed simulator")
		}
		select {
		case err := <-receipt:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("callback result: %v", err)
			}
		default:
			t.Error("callback receipt unresolved after New returned")
		}
	})
}
