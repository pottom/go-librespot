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

// A read that has been answered once keeps answering while the loop is stuck.
//
// The 503 above is the floor, not the aim. The interface asks for the status
// several times a second, and ten seconds of nothing per ask is a frozen screen
// however correct the status code is — the daemon has to go on saying what is
// playing while the stream that is playing it has stopped moving.
func TestAReadKeepsAnsweringWhileTheLoopIsStuck(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close() //nolint:errcheck // the test is ending

	s := &ConcreteApiServer{log: &librespot.NullLogger{}, listener: listener}
	s.requests = make(chan ApiRequest)

	was, wasHeld := apiPatience, apiHeldPatience
	apiPatience, apiHeldPatience = 5*time.Second, 50*time.Millisecond
	defer func() { apiPatience, apiHeldPatience = was, wasHeld }()

	// One healthy answer, which is what there is to fall back to.
	stuck := make(chan struct{})
	go func() {
		req := <-s.requests
		req.Reply(&ApiResponseStatus{Username: "someone", Volume: 42}, nil)
		<-s.requests // and then the loop stops moving
		<-stuck
	}()

	first := httptest.NewRecorder()
	s.handleRequest(ApiRequest{Type: ApiRequestTypeStatus}, first)
	if first.Code != http.StatusOK {
		t.Fatalf("the first status was answered %d", first.Code)
	}
	if first.Header().Get("Warning") != "" {
		t.Error("a fresh answer was marked stale")
	}

	// And now with the loop stuck.
	at := time.Now()
	second := httptest.NewRecorder()
	s.handleRequest(ApiRequest{Type: ApiRequestTypeStatus}, second)
	took := time.Since(at)
	close(stuck)

	t.Logf("the second status came back after %s with %d", took, second.Code)
	if second.Code != http.StatusOK {
		t.Errorf("the status was answered %d while the loop was stuck, want %d",
			second.Code, http.StatusOK)
	}
	if second.Body.String() != first.Body.String() {
		t.Errorf("the held answer is %q, not the one that was given: %q",
			second.Body.String(), first.Body.String())
	}
	if second.Header().Get("Warning") == "" {
		t.Error("an answer out of the cupboard did not say it was old")
	}
	if took > time.Second {
		t.Errorf("the held answer took %s, which is long enough to be a freeze", took)
	}
}

// A command never comes from the cupboard.
//
// "pause" answered out of a cupboard would report that the music had stopped
// while it played on, which is worse than saying nothing at all.
func TestACommandIsNeverAnsweredFromWhatItLastSaid(t *testing.T) {
	for _, req := range []ApiRequestType{
		ApiRequestTypePause, ApiRequestTypeResume, ApiRequestTypeNext, ApiRequestTypePrev,
		ApiRequestTypeSeek, ApiRequestTypePlay, ApiRequestTypeSetVolume,
		ApiRequestTypeSetQueue, ApiRequestTypeDrop, ApiRequestTypeReorder,
	} {
		if apiHeldReads[req] {
			t.Errorf("%s may be answered from what it last said", req)
		}
	}
	// And the two that take an argument are out for a second reason: the last
	// answer was to a different question.
	for _, req := range []ApiRequestType{ApiRequestTypeContext, ApiRequestTypeLyrics} {
		if apiHeldReads[req] {
			t.Errorf("%s takes an argument and may be answered from a held one", req)
		}
	}
}
