package daemon

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	librespot "github.com/devgianlu/go-librespot"
)

// A request the player never takes is answered anyway.
//
// Every request goes through the one loop that also runs the session, so a
// session that has got itself stuck used to take every request with it: the
// port stayed open, the connection was accepted, and nothing ever came back.
// From outside that is indistinguishable from a healthy daemon.
func TestARequestIsAnsweredEvenWithNobodyListening(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close() //nolint:errcheck // the test is ending

	s := &ConcreteApiServer{log: &librespot.NullLogger{}, listener: listener}
	s.requests = make(chan ApiRequest) // and nobody receives from it

	was := apiPatience
	apiPatience = 50 * time.Millisecond
	defer func() { apiPatience = was }()

	rec := httptest.NewRecorder()
	done := make(chan time.Duration, 1)
	go func() {
		at := time.Now()
		s.handleRequest(ApiRequest{Type: ApiRequestTypeStatus}, rec)
		done <- time.Since(at)
	}()

	select {
	case took := <-done:
		t.Logf("the request came back after %s with %d", took, rec.Code)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("the request was answered %d, want %d", rec.Code, http.StatusServiceUnavailable)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the request never came back, which is the whole bug")
	}
}

// And one the player takes and then sits on is answered too.
func TestARequestIsAnsweredWhenThePlayerNeverReplies(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close() //nolint:errcheck // the test is ending

	s := &ConcreteApiServer{log: &librespot.NullLogger{}, listener: listener}
	s.requests = make(chan ApiRequest)

	was := apiPatience
	apiPatience = 50 * time.Millisecond
	defer func() { apiPatience = was }()

	// A player that takes the request and forgets about it.
	go func() {
		<-s.requests
		select {}
	}()

	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		s.handleRequest(ApiRequest{Type: ApiRequestTypeStatus}, rec)
		close(done)
	}()

	select {
	case <-done:
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("the request was answered %d, want %d", rec.Code, http.StatusServiceUnavailable)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a request the player swallowed never came back")
	}
}
