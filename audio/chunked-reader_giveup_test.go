//go:build test_unit

package audio

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A stream whose url has stopped answering is given up on, and everybody
// waiting on it is told.
//
// This is what a network that went away and came back somewhere else looks
// like from in here: the url belongs to a session that has expired, so every
// request against it runs out of time. Each failure used to put the next
// waiter in the fetcher's chair to fail the same way, forever — and the
// decoder, the audio callback and the daemon's whole api queued up behind it.
func TestAStreamThatCannotBeFedIsGivenUpOn(t *testing.T) {
	was := ChunkDeadline
	ChunkDeadline = 200 * time.Millisecond
	defer func() { ChunkDeadline = was }()

	transport, started := newBlockingRoundTripper()
	r := newFetchTestReader(t, transport)

	// Somebody else waiting on the same chunk, as a prefetch would be.
	waiting := make(chan error, 1)
	go func() {
		<-started
		waiting <- func() error {
			_, err := r.fetchChunk(0)
			return err
		}()
	}()

	at := time.Now()
	_, err := r.fetchChunk(0)
	took := time.Since(at)
	t.Logf("the fetcher gave up after %s with %v", took, err)

	if err == nil {
		t.Fatal("a chunk that never arrived came back without an error")
	}
	if took > 5*time.Second {
		t.Errorf("the fetcher took %s to give up on a %s deadline", took, ChunkDeadline)
	}
	if !r.isClosed() {
		t.Error("the stream was not given up on, so the next read would wait all over again")
	}

	select {
	case err := <-waiting:
		t.Logf("the one waiting behind was told: %v", err)
		if err == nil {
			t.Error("the waiter was handed a chunk that does not exist")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the one waiting behind was never woken, which is the whole bug")
	}

	if !errors.Is(r.ctx.Err(), context.Canceled) {
		t.Errorf("the reader's own context says %v, want it cancelled", r.ctx.Err())
	}
}

// And a chunk already on its way does not collect a goroutine per read.
func TestPrefetchDoesNotPileUpOnOneChunk(t *testing.T) {
	transport, started := newBlockingRoundTripper()
	r := newFetchTestReader(t, transport)
	defer r.cancel()

	go func() { _, _ = r.fetchChunk(0) }()
	<-started

	for range 20 {
		r.startPrefetch(0)
	}

	done := make(chan struct{})
	go func() {
		r.prefetchWg.Wait()
		close(done)
	}()

	select {
	case <-done:
		t.Log("no prefetcher was started for a chunk already on its way")
	case <-time.After(time.Second):
		t.Error("prefetchers piled up on a chunk that was already being fetched")
	}
}
