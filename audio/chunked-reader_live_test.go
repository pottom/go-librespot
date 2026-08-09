//go:build test_unit

package audio

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	librespot "github.com/devgianlu/go-librespot"
)

// A stream is read over a real connection, from a real server.
//
// The other tests here hand the reader a body that is already in memory, so
// they never touch the request's context once the response is back. That is the
// one thing that matters about a deadline on a chunk: the body is read *after*
// the request returns, and a deadline cancelled on the way out takes the body
// with it. Ending it there failed every stream at its first chunk with "context
// canceled" — which is to say nothing played at all, and no test said so.
func TestAChunkIsReadOverARealConnection(t *testing.T) {
	const size = 3 * DefaultChunkSize

	body := make([]byte, size)
	for i := range body {
		body[i] = byte(i)
	}

	var served int
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var from, to int64
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &from, &to); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		served++

		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, to, size))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(body[from : to+1])
	}))
	defer origin.Close()

	r, err := NewHttpChunkedReader(&librespot.NullLogger{}, origin.Client(), origin.URL+"/audio")
	if err != nil {
		t.Fatalf("the stream would not open: %v", err)
	}
	defer r.Close() //nolint:errcheck // the test is ending

	if r.len != size {
		t.Errorf("the stream says it is %d bytes, want %d", r.len, size)
	}

	// The first chunk comes back whole, and so does one fetched afterwards.
	first, err := r.fetchChunk(0)
	if err != nil {
		t.Fatalf("the first chunk: %v", err)
	}
	if len(first) != DefaultChunkSize {
		t.Errorf("the first chunk is %d bytes, want %d", len(first), DefaultChunkSize)
	}

	second, err := r.fetchChunk(1)
	if err != nil {
		t.Fatalf("the second chunk: %v", err)
	}
	if len(second) != DefaultChunkSize {
		t.Errorf("the second chunk is %d bytes, want %d", len(second), DefaultChunkSize)
	}
	for i := range second {
		if want := body[DefaultChunkSize+i]; second[i] != want {
			t.Fatalf("the second chunk differs at byte %d", i)
		}
	}
	t.Logf("%d requests served, both chunks read whole", served)
}
