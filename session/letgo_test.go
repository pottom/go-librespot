package session

import (
	"testing"

	librespot "github.com/devgianlu/go-librespot"
	"github.com/devgianlu/go-librespot/ap"
)

// Everything after the accesspoint can fail, and a failure at any of them used
// to walk away from one that was connected, authenticated and reconnecting on
// its own. One is a leak nobody notices in a process about to exit; a caller
// that tries again is a pile of them, each holding a connection to Spotify in
// this device's name — see keepSigningIn in the daemon.
func TestLetGoClosesWhatWasMade(t *testing.T) {
	point := ap.NewAccesspoint(&librespot.NullLogger{}, nil, "device")
	s := Session{ap: point}

	// The dealer and the event manager were never reached, which is the whole
	// difficulty: Close assumes a session that was finished.
	s.letGo()

	select {
	case <-point.Done():
	default:
		t.Fatal("left the accesspoint connected")
	}
}
