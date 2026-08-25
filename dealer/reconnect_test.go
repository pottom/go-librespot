package dealer

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/coder/websocket"
	librespot "github.com/devgianlu/go-librespot"
)

// countingLogger is a logger that remembers whether anything was said at error
// level, which is how a test watches for the ping ticker acting on a deadline
// it should have left alone.
type countingLogger struct {
	librespot.NullLogger
	errors atomic.Int64
}

func (l *countingLogger) Errorf(string, ...interface{}) { l.errors.Add(1) }
func (l *countingLogger) WithError(error) librespot.Logger {
	return l
}
func (l *countingLogger) WithField(string, interface{}) librespot.Logger { return l }

// fakeDealer is a websocket server that can be told to refuse the handshake, so
// that a test can watch what the dealer does about it.
type fakeDealer struct {
	srv *httptest.Server

	// down is the status every handshake is answered with while it is set, as
	// an unreachable Spotify would.
	down atomic.Int64

	// refused is a token this server will not accept, and the status it says so
	// with. Naming the token rather than counting attempts is what makes the
	// test about the token rather than about timing.
	refused       atomic.Value
	refusedStatus atomic.Int64

	// tokens is every access token the handshake was offered, in order.
	tokens atomic.Value
}

func newFakeDealer(t *testing.T) *fakeDealer {
	t.Helper()

	f := &fakeDealer{}
	f.tokens.Store([]string{})
	f.refused.Store("")
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.URL.Query().Get("access_token")
		f.tokens.Store(append(f.offered(), token))

		if status := f.down.Load(); status != 0 {
			w.WriteHeader(int(status))
			return
		}

		if token == f.refused.Load().(string) {
			w.WriteHeader(int(f.refusedStatus.Load()))
			return
		}

		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}

		// Held open by reading until the other end goes. Waiting on the
		// request's context instead leaves the handler running after the dealer
		// has hung up — an upgraded connection is no longer the server's to
		// cancel — and every test then pays five seconds to shut the server.
		for {
			if _, _, err := conn.Read(context.Background()); err != nil {
				_ = conn.CloseNow()
				return
			}
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeDealer) offered() []string { return f.tokens.Load().([]string) }

// dealerTo builds a dealer pointed at the fake, handing out numbered tokens so
// that a test can tell a renewed one from the one already in hand.
func dealerTo(t *testing.T, f *fakeDealer) (*Dealer, *atomic.Int64) {
	t.Helper()

	var renewals atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// As Login5 does it: renewing replaces the token in hand, so what is asked
	// for next without forcing is the new one.
	var current atomic.Value
	current.Store("the-one-in-hand")

	d := &Dealer{
		log:    &librespot.NullLogger{},
		client: f.srv.Client(),
		addr: func(context.Context) string {
			return strings.TrimPrefix(f.srv.URL, "https://")
		},
		accessToken: func(_ context.Context, force bool) (string, error) {
			if force {
				current.Store(fmt.Sprintf("renewed-%d", renewals.Add(1)))
			}
			return current.Load().(string), nil
		},
		ctx:              ctx,
		cancel:           cancel,
		done:             make(chan struct{}),
		requestReceivers: map[string]requestReceiver{},
	}
	return d, &renewals
}

// unreachableDealer is a dealer that cannot get as far as a socket, so that the
// waiting between attempts can be watched on a clock that a test controls.
func unreachableDealer(log librespot.Logger) (*Dealer, *atomic.Int64) {
	var tries atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())

	d := &Dealer{
		log: log,
		accessToken: func(context.Context, bool) (string, error) {
			tries.Add(1)
			return "", errors.New("the network is not there")
		},
		ctx:              ctx,
		cancel:           cancel,
		done:             make(chan struct{}),
		requestReceivers: map[string]requestReceiver{},
	}
	return d, &tries
}

// The whole reason Connect was lost for good: the token in hand is renewed only
// when our own clock says it has expired, so one refused at the other end is
// offered again and again until the retrying gives up.
func TestConnectRenewsATokenThatWasRefused(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			f := newFakeDealer(t)
			d, renewals := dealerTo(t, f)
			defer d.Close()

			// The token in hand is the one this server will not have. Any
			// other is welcome.
			f.refused.Store("the-one-in-hand")
			f.refusedStatus.Store(int64(status))

			if err := d.connect(context.Background()); err != nil {
				t.Fatalf("connect: %v", err)
			}

			if got := renewals.Load(); got != 1 {
				t.Fatalf("asked for %d fresh tokens, want 1", got)
			}
			offered := f.offered()
			if len(offered) != 2 {
				t.Fatalf("offered %d tokens, want 2: %q", len(offered), offered)
			}
			if offered[0] != "the-one-in-hand" || offered[1] != "renewed-1" {
				t.Fatalf("offered %q, want the one in hand and then a renewed one", offered)
			}
		})
	}
}

// A handshake that fails for any other reason is not the token's fault, and
// renewing it would spend a Login5 call on nothing.
func TestConnectDoesNotRenewOnOtherFailures(t *testing.T) {
	f := newFakeDealer(t)
	d, renewals := dealerTo(t, f)
	defer d.Close()

	f.down.Store(http.StatusInternalServerError)

	err := d.connect(context.Background())
	if err == nil {
		t.Fatal("expected the dial to fail")
	}
	if errors.Is(err, ErrTokenRefused) {
		t.Fatalf("a 500 was read as a refused token: %v", err)
	}
	if got := renewals.Load(); got != 0 {
		t.Fatalf("asked for %d fresh tokens, want none", got)
	}
}

// Both halves of the mend at once: a dealer that has been refusing for longer
// than the old retry was allowed still comes back, and comes back on a token it
// renewed rather than the one that was refused.
func TestKeepReconnectingComesBackOnARenewedToken(t *testing.T) {
	was := reconnectCeiling
	reconnectCeiling = 10 * time.Millisecond
	defer func() { reconnectCeiling = was }()

	f := newFakeDealer(t)
	d, renewals := dealerTo(t, f)
	defer d.Close()

	// Unreachable at first, and then reachable but holding the token in hand
	// against us — the state this device was actually found in.
	f.down.Store(http.StatusServiceUnavailable)
	f.refused.Store("the-one-in-hand")
	f.refusedStatus.Store(http.StatusUnauthorized)

	back := make(chan bool, 1)
	go func() { back <- d.keepReconnecting() }()

	// Turned away for a while first, so that this is a reconnection and not a
	// first attempt that happened to work.
	for len(f.offered()) < 6 {
		time.Sleep(time.Millisecond)
	}
	if _, lost := d.OutOfTouch(); !lost {
		t.Fatal("did not report itself out of touch while trying")
	}
	f.down.Store(0)

	select {
	case got := <-back:
		if !got {
			t.Fatal("gave up rather than reconnecting")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("never reconnected")
	}

	if _, lost := d.OutOfTouch(); lost {
		t.Fatal("still out of touch after reconnecting")
	}
	if renewals.Load() == 0 {
		t.Fatal("never renewed the refused token")
	}
	if last := f.offered()[len(f.offered())-1]; !strings.HasPrefix(last, "renewed-") {
		t.Fatalf("reconnected on %q, want a renewed token", last)
	}
}

// Getting the connection back is worth trying for as long as the dealer is
// open. The bounded retry this replaced stopped after fifteen minutes and took
// Spotify Connect with it until the process was started again.
func TestKeepReconnectingNeverGivesUp(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d, tries := unreachableDealer(&librespot.NullLogger{})

		back := make(chan bool, 1)
		go func() { back <- d.keepReconnecting() }()

		// Four times as long as the old retry was ever allowed.
		time.Sleep(time.Hour)
		synctest.Wait()

		select {
		case got := <-back:
			t.Fatalf("stopped trying after an hour and returned %v", got)
		default:
		}

		if since, lost := d.OutOfTouch(); !lost || since < time.Hour {
			t.Fatalf("out of touch for %s (lost=%v), want at least an hour", since, lost)
		}

		// About one attempt a minute rather than thousands: the waiting is
		// capped, but it is capped at something.
		if got := tries.Load(); got < 55 || got > 80 {
			t.Fatalf("tried %d times in an hour, want about one a minute", got)
		}

		d.Close()
		synctest.Wait()
	})
}

// Closing must end the waiting at once, not when the next attempt was due.
func TestKeepReconnectingStopsWhenTheDealerCloses(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d, _ := unreachableDealer(&librespot.NullLogger{})

		back := make(chan bool, 1)
		go func() { back <- d.keepReconnecting() }()

		time.Sleep(5 * time.Minute)
		synctest.Wait()

		d.Close()
		synctest.Wait()

		select {
		case got := <-back:
			if got {
				t.Fatal("claimed to have reconnected a closed dealer")
			}
		default:
			t.Fatal("kept trying after the dealer was closed")
		}
	})
}

// The ping ticker reads a deadline belonging to the socket that has gone.
// Acting on it while there is no connection closes the next one the moment it
// is made, and the reconnecting starts over for nothing.
func TestPingTickerStandsAsideWhileReconnecting(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		log := &countingLogger{}
		d, _ := unreachableDealer(log)

		// Out of touch, with the last pong long enough ago that the ticker
		// would otherwise declare the connection dead.
		d.lostAt.Store(time.Now().Add(-time.Hour).UnixNano())
		d.lastPongLock.Lock()
		d.lastPong = time.Now().Add(-time.Hour)
		d.lastPongLock.Unlock()

		go d.pingTicker()

		time.Sleep(3 * pingInterval)
		synctest.Wait()

		if got := log.errors.Load(); got != 0 {
			t.Fatalf("the ticker complained %d times about a connection that was already gone", got)
		}

		d.Close()
		synctest.Wait()
	})
}
