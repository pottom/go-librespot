package daemon

import (
	"context"
	"errors"
	"net"
	"testing"
	"testing/synctest"
	"time"

	librespot "github.com/devgianlu/go-librespot"
	"github.com/devgianlu/go-librespot/ap"
	pb "github.com/devgianlu/go-librespot/proto/spotify"
)

// unreachable is what a dial answers with when there is no network: it is a
// net.Error, which is the whole of what worthAnotherTry looks for.
var unreachable = &net.OpError{Op: "dial", Err: errors.New("connect: network is unreachable")}

// signingApp is an app that can be signed in without a Spotify account, a
// network or a sound card.
func signingApp() *App {
	stub, _ := NewStubApiServer(&librespot.NullLogger{})
	return &App{log: &librespot.NullLogger{}, server: stub, cfg: &Config{}}
}

// A daemon started before the network is up used to take the port, fail at the
// resolver and end. What started another was the interface, if one was running,
// half a minute later — so a device started at login on a machine whose wifi is
// still coming up was simply not there.
func TestKeepSigningInWaitsForTheNetwork(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		app := signingApp()

		tries := 0
		wanted := &AppPlayer{}
		signIn := func(context.Context) (*AppPlayer, error) {
			tries++
			// Turned away far longer than any bounded retry would have lasted.
			if tries <= 90 {
				return nil, unreachable
			}
			return wanted, nil
		}

		got := make(chan *AppPlayer, 1)
		go func() {
			player, err := app.keepSigningIn(context.Background(), signIn)
			if err != nil {
				t.Errorf("keepSigningIn: %v", err)
			}
			got <- player
		}()

		time.Sleep(30 * time.Minute)
		synctest.Wait()

		if since, trying := app.SigningIn(); !trying || since < 29*time.Minute {
			t.Fatalf("says it has been trying %s (trying=%v), want half an hour", since, trying)
		}

		time.Sleep(time.Hour)
		synctest.Wait()

		select {
		case player := <-got:
			if player != wanted {
				t.Fatal("signed in as somebody else")
			}
		default:
			t.Fatalf("never signed in, after %d tries", tries)
		}

		if _, trying := app.SigningIn(); trying {
			t.Fatal("still says it is trying to sign in")
		}
	})
}

// Waiting cannot produce a password.
func TestKeepSigningInDoesNotRetryARefusedLogin(t *testing.T) {
	app := signingApp()

	refused := &ap.AccesspointLoginError{Message: &pb.APLoginFailed{}}
	tries := 0
	_, err := app.keepSigningIn(context.Background(), func(context.Context) (*AppPlayer, error) {
		tries++
		return nil, refused
	})

	if !errors.Is(err, refused) {
		t.Errorf("returned %v, want the refusal itself", err)
	}
	if tries != 1 {
		t.Errorf("tried %d times, want once", tries)
	}
}

// Anything unrecognised behaves exactly as it did before this existed. A loop
// that never ends is only ever right for a reason somebody understood.
func TestKeepSigningInLeavesUnknownFailuresAlone(t *testing.T) {
	app := signingApp()

	tries := 0
	_, err := app.keepSigningIn(context.Background(), func(context.Context) (*AppPlayer, error) {
		tries++
		return nil, errors.New("failed initializing player: no such audio device")
	})

	if err == nil {
		t.Fatal("swallowed a failure that nobody here understands")
	}
	if tries != 1 {
		t.Errorf("tried %d times, want once", tries)
	}
}

// Being asked to stop must end the waiting where it stands.
func TestKeepSigningInStopsWhenAsked(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		app := signingApp()
		ctx, cancel := context.WithCancel(context.Background())

		done := make(chan error, 1)
		go func() {
			_, err := app.keepSigningIn(ctx, func(context.Context) (*AppPlayer, error) {
				return nil, unreachable
			})
			done <- err
		}()

		time.Sleep(5 * time.Minute)
		synctest.Wait()
		cancel()
		synctest.Wait()

		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("ended with %v, want the cancellation", err)
			}
		default:
			t.Fatal("kept trying after it was asked to stop")
		}
	})
}

// A network error is the only thing tried again.
func TestWorthAnotherTry(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		want bool
	}{
		{"a dial that found no network", unreachable, true},
		{"one wrapped on its way up", errors.Join(errors.New("failed getting accesspoint from resolver"), unreachable), true},
		{"a refused login", &ap.AccesspointLoginError{Message: &pb.APLoginFailed{}}, false},
		{"a refusal behind a network error", errors.Join(unreachable, &ap.AccesspointLoginError{Message: &pb.APLoginFailed{}}), false},
		{"something nobody here knows", errors.New("no such audio device"), false},
		{"being asked to stop", context.Canceled, false},
		{"nothing at all", nil, false},
	} {
		if got := worthAnotherTry(c.err); got != c.want {
			t.Errorf("%s: worthAnotherTry = %v, want %v", c.name, got, c.want)
		}
	}
}

// fakeServer is an ApiServer whose channel a test can feed.
type fakeServer struct{ requests chan ApiRequest }

func (f *fakeServer) Emit(*ApiEvent)             {}
func (f *fakeServer) Receive() <-chan ApiRequest { return f.requests }
func (f *fakeServer) Close() error               { return nil }
func (f *fakeServer) SetLive(ApiLive)            {}

// With nobody reading the channel the player reads, every request waits out the
// server's whole patience — ten seconds — before it is told anything at all,
// and the interface asks once a second. Saying so at once is the difference
// between a daemon that is starting and one that looks broken.
func TestNoSessionIsSaidAtOnceWhileSigningIn(t *testing.T) {
	app := signingApp()
	app.server = &fakeServer{requests: make(chan ApiRequest)}

	stop, stopped := make(chan struct{}), make(chan struct{})
	go app.sayNoSession(stop, stopped)
	defer func() { close(stop); <-stopped }()

	req, wait := NewApiRequest(ApiRequestTypeStatus, nil)
	app.server.(*fakeServer).requests <- req

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if _, err := wait(ctx); !errors.Is(err, ErrNoSession) {
		t.Errorf("answered %v, want ErrNoSession", err)
	}
}
