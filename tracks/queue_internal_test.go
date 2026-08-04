//go:build test_unit

package tracks

import (
	"testing"

	connectpb "github.com/devgianlu/go-librespot/proto/spotify/connectstate"
)

// The callers build their queues from three places — the queue, the tracks
// taken from the context and the ones passed on the way — and the same song can
// be in more than one of them. Left alone it is heard twice without anyone
// having asked for it.
func TestQueueTracksKeepsOneCopyOfATrack(t *testing.T) {
	tl := &List{}
	tl.queueTracks([]*connectpb.ContextTrack{
		{Uri: "spotify:track:a"},
		{Uri: "spotify:track:b"},
		{Uri: "spotify:track:a"},
		{Uri: "spotify:track:c"},
	})

	got := make([]string, 0, len(tl.queue))
	for _, track := range tl.queue {
		got = append(got, track.Uri)
	}
	want := []string{"spotify:track:a", "spotify:track:b", "spotify:track:c"}
	if len(got) != len(want) {
		t.Fatalf("queue = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("queue = %v, want %v — the first copy is the one the caller meant", got, want)
		}
	}
}

// The track sounding now is the head of the queue while it plays. Queueing it
// again would play it a second time the moment it ended.
func TestQueueTracksWillNotQueueWhatIsPlaying(t *testing.T) {
	tl := &List{
		playingQueue: true,
		queue:        []*connectpb.ContextTrack{{Uri: "spotify:track:now"}},
	}
	tl.queueTracks([]*connectpb.ContextTrack{
		{Uri: "spotify:track:next"},
		{Uri: "spotify:track:now"},
	})

	if len(tl.queue) != 2 {
		t.Fatalf("queue has %d tracks, want the one playing and the one after it", len(tl.queue))
	}
	if tl.queue[1].Uri != "spotify:track:next" {
		t.Errorf("queue = %v, want the playing track not to be waiting as well", tl.queue)
	}
}
