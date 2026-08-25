package ap

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	librespot "github.com/devgianlu/go-librespot"
	pb "github.com/devgianlu/go-librespot/proto/spotify"
)

// unreachable is a port nothing answers on, so that every attempt at the
// accesspoint fails at once and the waiting between them is all there is to
// watch.
func unreachable(context.Context) string { return "127.0.0.1:1" }

// A laptop shut for the night comes back to a working network, and the device
// has to be there when it does. The retry this replaced was allowed fifteen
// minutes, and after that the receive channels were closed: the device played
// on, answered its own API, and was out of everybody's reach until the process
// was started again.
func TestKeepReconnectingNeverGivesUp(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ap := NewAccesspoint(&librespot.NullLogger{}, unreachable, "device")
		ap.welcome = &pb.APWelcome{}

		back := make(chan bool, 1)
		go func() { back <- ap.keepReconnecting() }()

		// Four times as long as the old retry was ever allowed.
		time.Sleep(time.Hour)
		synctest.Wait()

		select {
		case got := <-back:
			t.Fatalf("stopped trying after an hour and returned %v", got)
		default:
		}

		if since, lost := ap.OutOfTouch(); !lost || since < time.Hour {
			t.Fatalf("out of touch for %s (lost=%v), want at least an hour", since, lost)
		}

		ap.Close()
		synctest.Wait()

		select {
		case got := <-back:
			if got {
				t.Fatal("claimed to have reconnected a closed accesspoint")
			}
		default:
			t.Fatal("kept trying after the accesspoint was closed")
		}
	})
}

// The one failure that must end the trying: without the credentials the
// accesspoint handed back at login there is nothing to log in with, and no
// amount of waiting will produce them.
func TestKeepReconnectingStopsWithNothingToLogInWith(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ap := NewAccesspoint(&librespot.NullLogger{}, unreachable, "device")
		ap.welcome = nil

		back := make(chan bool, 1)
		go func() { back <- ap.keepReconnecting() }()

		time.Sleep(2 * reconnectCeiling)
		synctest.Wait()

		select {
		case got := <-back:
			if got {
				t.Fatal("claimed to have reconnected without credentials")
			}
		default:
			t.Fatal("kept trying with nothing to try with")
		}

		if _, lost := ap.OutOfTouch(); lost {
			t.Fatal("still says it is out of touch after it stopped trying")
		}

		ap.Close()
		synctest.Wait()
	})
}
