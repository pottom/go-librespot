package daemon

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cenkalti/backoff/v4"
	librespot "github.com/devgianlu/go-librespot"
	"github.com/devgianlu/go-librespot/ap"
	"github.com/devgianlu/go-librespot/apresolve"
	"github.com/devgianlu/go-librespot/cache"
	"github.com/devgianlu/go-librespot/mpris"
	"github.com/devgianlu/go-librespot/player"
	"github.com/devgianlu/go-librespot/playplay"
	devicespb "github.com/devgianlu/go-librespot/proto/spotify/connectstate/devices"
	"github.com/devgianlu/go-librespot/session"
	"github.com/devgianlu/go-librespot/zeroconf"
	"golang.org/x/exp/rand"
)

type App struct {
	log librespot.Logger
	cfg *Config

	stateStore librespot.StateStore

	client *http.Client

	resolver *apresolve.ApResolver
	zeroconf *zeroconf.Zeroconf

	deviceId    string
	deviceType  devicespb.DeviceType
	clientToken string
	state       *librespot.AppState

	server   ApiServer
	mpris    mpris.Server
	logoutCh chan *AppPlayer

	audioCache *cache.Cache

	// tempos remembers the beat rate of tracks already played, so a controller
	// can show one for a track further down the queue.
	tempos *tempoStore

	// signingInSince is when the device started trying to sign in, in unix
	// nanoseconds, or nought when it is not waiting on that. Read from outside
	// the loop — a watchdog wants to say why a device has not come up — so it
	// is atomic rather than guarded.
	signingInSince atomic.Int64

	closed bool
}

func parseDeviceType(val string) (devicespb.DeviceType, error) {
	valEnum, ok := devicespb.DeviceType_value[strings.ToUpper(val)]
	if !ok {
		return 0, fmt.Errorf("invalid device type: %s", val)
	}

	return devicespb.DeviceType(valEnum), nil
}

func New(opts *Options) (*App, error) {
	if opts == nil {
		return nil, errors.New("daemon: Options is required")
	}
	if opts.Logger == nil {
		return nil, errors.New("daemon: Options.Logger is required")
	}
	if opts.Config == nil {
		return nil, errors.New("daemon: Options.Config is required")
	}
	if opts.StateStore == nil {
		return nil, errors.New("daemon: Options.StateStore is required")
	}

	app := &App{
		log:        opts.Logger,
		cfg:        opts.Config,
		stateStore: opts.StateStore,
		logoutCh:   make(chan *AppPlayer),
		client:     &http.Client{Timeout: 30 * time.Second},
	}

	var err error
	app.deviceType, err = parseDeviceType(app.cfg.DeviceType)
	if err != nil {
		return nil, err
	}

	app.state, err = opts.StateStore.Load()
	if err != nil {
		return nil, fmt.Errorf("loading state: %w", err)
	}
	if app.state == nil {
		app.state = &librespot.AppState{}
	}

	app.resolver = apresolve.NewApResolver(app.log, app.client)

	if app.cfg.DeviceId != "" {
		app.deviceId = app.cfg.DeviceId
	} else if app.state.DeviceId != "" {
		app.deviceId = app.state.DeviceId
	} else {
		deviceIdBytes := make([]byte, 20)
		_, _ = rand.Read(deviceIdBytes)
		app.deviceId = hex.EncodeToString(deviceIdBytes)
		app.log.Infof("generated new device id: %s", app.deviceId)

		app.state.DeviceId = app.deviceId
		if err := app.persistState(); err != nil {
			return nil, err
		}
	}

	if app.cfg.ClientToken != "" {
		app.clientToken = app.cfg.ClientToken
	}

	if app.cfg.FlacEnabled && !playplay.Plugin.IsSupported() {
		return nil, fmt.Errorf("FLAC playback requires a PlapPlay implementation")
	}

	if opts.APIServer != nil {
		app.server = opts.APIServer
	} else {
		app.server, _ = NewStubApiServer(app.log)
	}

	if opts.MediaPlayer != nil {
		app.mpris = opts.MediaPlayer
	} else {
		app.mpris = mpris.DummyServer{}
	}

	// Always present, with or without a cache directory: without one it simply
	// remembers for as long as the daemon runs.
	app.tempos = newTempoStore(app.cfg.Cache.Dir)

	if app.cfg.Cache.Enabled && app.cfg.Cache.Dir != "" {
		app.audioCache, err = cache.New(app.log, app.cfg.Cache.Dir, app.cfg.Cache.SizeLimit)
		if err != nil {
			return nil, fmt.Errorf("failed initializing audio cache: %w", err)
		}
	}

	return app, nil
}

func (app *App) SetDeviceName(name string) {
	if app.cfg.DeviceName == name {
		return
	}

	app.cfg.DeviceName = name

	if app.zeroconf != nil {
		app.zeroconf.SetDeviceName(name)
	}
}

// Run starts the daemon. It blocks until ctx is cancelled or an unrecoverable
// error occurs. The credential type configured in cfg.Credentials.Type
// determines which login flow is used.
func (app *App) Run(ctx context.Context) error {
	go func() {
		select {
		case <-ctx.Done():
			_ = app.Close()
		}
	}()

	switch app.cfg.Credentials.Type {
	case "zeroconf":
		// Zeroconf mode unconditionally needs zeroconf to be enabled.
		app.cfg.ZeroconfEnabled = true
		return app.runZeroconf(ctx)
	case "interactive":
		return app.runInteractive(ctx, app.cfg.Credentials.Interactive.CallbackPort)
	case "spotify_token":
		return app.runSpotifyToken(ctx, app.cfg.Credentials.SpotifyToken.Username, app.cfg.Credentials.SpotifyToken.AccessToken)
	default:
		return fmt.Errorf("unknown credentials: %s", app.cfg.Credentials.Type)
	}
}

// Close releases resources held by the daemon. It is safe to call more than once.
func (app *App) Close() error {
	if app.closed {
		return nil
	}
	app.closed = true

	var errs []error
	if app.server != nil {
		if err := app.server.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if app.mpris != nil {
		if err := app.mpris.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if app.zeroconf != nil {
		app.zeroconf.Close()
	}

	return errors.Join(errs...)
}

func (app *App) persistState() error {
	if err := app.stateStore.Save(app.state); err != nil {
		return fmt.Errorf("persisting state: %w", err)
	}
	return nil
}

func (app *App) newAppPlayer(ctx context.Context, creds any) (_ *AppPlayer, err error) {
	appPlayer := &AppPlayer{
		app:             app,
		stop:            make(chan struct{}, 1),
		logout:          app.logoutCh,
		countryCode:     new(string),
		volumeUpdate:    make(chan float32, 1),
		playbackReadyCh: make(chan struct{}),
	}

	appPlayer.prefetchTimer = time.NewTimer(math.MaxInt64)
	appPlayer.prefetchTimer.Stop()

	if appPlayer.sess, err = session.NewSessionFromOptions(ctx, &session.Options{
		Log:         app.log,
		DeviceType:  app.deviceType,
		DeviceId:    app.deviceId,
		ClientToken: app.clientToken,
		Resolver:    app.resolver,
		Client:      app.client,
		StateStore:  app.stateStore,
		Credentials: creds,
	}); err != nil {
		return nil, err
	}

	appPlayer.initState()

	if appPlayer.player, err = player.NewPlayer(&player.Options{
		Spclient: appPlayer.sess.Spclient(),
		AudioKey: appPlayer.sess.AudioKey(),
		Events:   appPlayer.sess.Events(),
		Log:      app.log,

		Cache: app.audioCache,

		FlacEnabled: app.cfg.FlacEnabled,

		NormalisationEnabled:      !app.cfg.NormalisationDisabled,
		NormalisationUseAlbumGain: app.cfg.NormalisationUseAlbumGain,
		NormalisationPregain:      app.cfg.NormalisationPregain,

		CrossfadeDuration: time.Duration(app.cfg.CrossfadeDuration) * time.Millisecond,

		CountryCode: appPlayer.countryCode,

		AudioBackend:              app.cfg.AudioBackend,
		AudioBackendRuntimeSocket: app.cfg.AudioBackendRuntimeSocket,
		AudioDevice:               app.cfg.AudioDevice,
		MixerDevice:               app.cfg.MixerDevice,
		MixerControlName:          app.cfg.MixerControlName,

		AudioBufferTime:  app.cfg.AudioBufferTime,
		AudioPeriodCount: app.cfg.AudioPeriodCount,

		ExternalVolume: app.cfg.ExternalVolume,
		VolumeUpdate:   appPlayer.volumeUpdate,

		AudioOutputPipe:       app.cfg.AudioOutputPipe,
		AudioOutputPipeFormat: app.cfg.AudioOutputPipeFormat,
	},
	); err != nil {
		return nil, fmt.Errorf("failed initializing player: %w", err)
	}

	// The analyser answers for itself from here. What the picture asks for
	// thirty times a second does not need the goroutine that also runs
	// playback, and waiting behind it was where the frames went. See ApiLive.
	app.server.SetLive(appPlayer.player)

	return appPlayer, nil
}

// signInCeiling is the longest this waits between two attempts at signing in.
// A variable so that a test can watch a morning of no network go by in a
// moment.
var signInCeiling = time.Minute

// keepSigningIn signs the device in, and goes on trying for as long as the only
// thing wrong is that Spotify cannot be reached.
//
// Without this a daemon started before the network is up simply ends: measured,
// it takes the port, fails at the resolver, and the process is gone. What
// starts another is the interface, if there is one running, half a minute
// later — so a device started at login on a machine that wakes with its wifi
// still coming up is not there, and nothing says why.
//
// Narrow on purpose. Only a network error is tried again; a refused login is
// not, because waiting cannot produce a password, and neither is anything
// unrecognised, which fails exactly as it did before. A first sign-in needs no
// help from here: it does not fail, it waits, on a browser somebody has to
// visit — see the interactive credentials, and spindle's watchdog, which says
// so out loud.
func (app *App) keepSigningIn(ctx context.Context, signIn func(context.Context) (*AppPlayer, error)) (*AppPlayer, error) {
	appPlayer, err := signIn(ctx)
	if err == nil || !worthAnotherTry(err) {
		return appPlayer, err
	}

	app.log.WithError(err).Warnf("cannot reach Spotify to sign in; trying again until it can be reached")

	// Something has to answer the local API while there is no player to answer
	// it. Every request goes down the channel the player reads, and with nobody
	// reading it each one waits out the server's whole patience before it is
	// told anything at all — ten seconds, for a question asked every second.
	// The zeroconf side has said ErrNoSession in this state all along.
	stop := make(chan struct{})
	stopped := make(chan struct{})
	go app.sayNoSession(stop, stopped)
	defer func() { close(stop); <-stopped }()

	wait := backoff.NewExponentialBackOff()
	wait.MaxInterval = signInCeiling
	wait.MaxElapsedTime = 0 // never give up

	since := time.Now()
	app.signingInSince.Store(since.UnixNano())
	defer app.signingInSince.Store(0)

	for {
		select {
		case <-time.After(wait.NextBackOff()):
		case <-ctx.Done():
			return nil, ctx.Err()
		}

		appPlayer, err := signIn(ctx)
		if err == nil {
			app.log.Infof("signed in after %s of trying", time.Since(since).Round(time.Second))
			return appPlayer, nil
		}
		if !worthAnotherTry(err) {
			return nil, err
		}
		app.log.WithError(err).Debugf("failed signing in")
	}
}

// worthAnotherTry reports that a failed sign-in failed for a reason that may
// not be true a minute from now.
//
// Only the network counts. A login the accesspoint refused is the one thing
// certain not to mend itself, and everything else is left alone deliberately:
// an error nobody here recognises behaves exactly as it did before this
// existed, rather than becoming a loop that never ends and says nothing.
func worthAnotherTry(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	var refused *ap.AccesspointLoginError
	if errors.As(err, &refused) {
		return false
	}

	var unreachable net.Error
	return errors.As(err, &unreachable)
}

// sayNoSession answers the local API while there is no player to answer it, so
// that a request made in this state is told so at once instead of waiting out
// the server's patience. It is the same answer the zeroconf side gives.
func (app *App) sayNoSession(stop <-chan struct{}, stopped chan<- struct{}) {
	defer close(stopped)

	for {
		select {
		case <-stop:
			return
		case req := <-app.server.Receive():
			switch req.Type {
			case ApiRequestTypeRoot:
				req.Reply(&ApiResponseRoot{}, nil)
			case ApiRequestSetDeviceName:
				// The device name is not the session's to know, and answering
				// it here keeps a rename made while the network is down.
				app.SetDeviceName(req.Data.(string))
				req.Reply(nil, nil)
			default:
				req.Reply(nil, ErrNoSession)
			}
		}
	}
}

// SigningIn says how long the device has been trying to sign in, and whether it
// is still trying. Zero and false once it is in — or when it never had to wait.
func (app *App) SigningIn() (time.Duration, bool) {
	at := app.signingInSince.Load()
	if at == 0 {
		return 0, false
	}
	if since := time.Since(time.Unix(0, at)); since > 0 {
		return since, true
	}
	return 0, true
}

func (app *App) runZeroconf(ctx context.Context) error {
	return app.withAppPlayer(ctx, func(ctx context.Context) (*AppPlayer, error) {
		if app.cfg.Credentials.Zeroconf.PersistCredentials && len(app.state.Credentials.Data) > 0 {
			app.log.WithField("username", librespot.ObfuscateUsername(app.state.Credentials.Username)).
				Infof("loading previously persisted zeroconf credentials")
			return app.newAppPlayer(ctx, session.StoredCredentials{
				Username: app.state.Credentials.Username,
				Data:     app.state.Credentials.Data,
			})
		}

		return nil, nil
	})
}

func (app *App) runSpotifyToken(ctx context.Context, username, token string) error {
	return app.withCredentials(ctx, session.SpotifyTokenCredentials{Username: username, Token: token})
}

func (app *App) runInteractive(ctx context.Context, callbackPort int) error {
	return app.withCredentials(ctx, session.InteractiveCredentials{CallbackPort: callbackPort})
}

func (app *App) withCredentials(ctx context.Context, creds any) (err error) {
	return app.withAppPlayer(ctx, func(ctx context.Context) (*AppPlayer, error) {
		if len(app.state.Credentials.Data) > 0 {
			return app.newAppPlayer(ctx, session.StoredCredentials{
				Username: app.state.Credentials.Username,
				Data:     app.state.Credentials.Data,
			})
		} else {
			appPlayer, err := app.newAppPlayer(ctx, creds)
			if err != nil {
				return nil, err
			}

			app.state.Credentials.Username = appPlayer.sess.Username()
			app.state.Credentials.Data = appPlayer.sess.StoredCredentials()

			if err = app.persistState(); err != nil {
				return nil, err
			}

			app.log.WithField("username", librespot.ObfuscateUsername(appPlayer.sess.Username())).
				Debugf("stored credentials")
			return appPlayer, nil
		}
	})
}

func (app *App) withAppPlayer(ctx context.Context, appPlayerFunc func(context.Context) (*AppPlayer, error)) (err error) {
	if !app.cfg.ZeroconfEnabled {
		appPlayer, err := app.keepSigningIn(ctx, appPlayerFunc)
		if err != nil {
			return err
		} else if appPlayer == nil {
			panic("zeroconf is disabled and no credentials are present")
		}

		appPlayer.Run(ctx, app.server.Receive(), app.mpris.Receive())
		return nil
	}

	if err := app.resolver.FetchAll(ctx); err != nil {
		return fmt.Errorf("failed getting endpoints from resolver: %w", err)
	}

	app.zeroconf, err = zeroconf.NewZeroconf(app.log, app.cfg.ZeroconfPort, app.cfg.DeviceName, app.deviceId, app.deviceType, app.cfg.ZeroconfInterfacesToAdvertise, app.cfg.ZeroconfBackend == "avahi")
	if err != nil {
		return fmt.Errorf("failed initializing zeroconf: %w", err)
	}

	var apiCh chan ApiRequest

	currentPlayer, err := appPlayerFunc(ctx)
	if err != nil {
		return err
	}

	if currentPlayer != nil {
		app.log.WithField("username", librespot.ObfuscateUsername(currentPlayer.sess.Username())).
			Debugf("initializing zeroconf session")

		apiCh = make(chan ApiRequest)
		go currentPlayer.Run(ctx, apiCh, app.mpris.Receive())

		app.zeroconf.SetCurrentUser(currentPlayer.sess.Username())
	}

	go func() {
		for {
			select {
			case req := <-app.server.Receive():
				if currentPlayer == nil {
					switch req.Type {
					case ApiRequestTypeRoot:
						req.Reply(&ApiResponseRoot{}, nil)
					case ApiRequestSetDeviceName:
						// The device name drives the zeroconf advertisement, which
						// runs independently of any player session, so handle it
						// even when no session is active.
						app.SetDeviceName(req.Data.(string))
						req.Reply(nil, nil)
					default:
						req.Reply(nil, ErrNoSession)
					}
					break
				}

				apiCh <- req
			}
		}
	}()

	go func() {
		for {
			select {
			case <-ctx.Done():
				if currentPlayer != nil {
					currentPlayer.Close()
					currentPlayer = nil

					close(apiCh)
				}
				return
			case p := <-app.logoutCh:
				if p != currentPlayer {
					continue
				}

				currentPlayer.Close()
				currentPlayer = nil

				close(apiCh)

				newAppPlayer, err := appPlayerFunc(ctx)
				if err != nil {
					app.log.WithError(err).Errorf("failed restoring session after logout")

					app.zeroconf.SetCurrentUser("")
				} else if newAppPlayer == nil {
					app.zeroconf.SetCurrentUser("")
				} else {
					apiCh = make(chan ApiRequest)
					currentPlayer = newAppPlayer

					go newAppPlayer.Run(ctx, apiCh, app.mpris.Receive())

					app.zeroconf.SetCurrentUser(newAppPlayer.sess.Username())

					app.log.WithField("username", librespot.ObfuscateUsername(currentPlayer.sess.Username())).
						Debugf("restored session after logout")
				}
			}
		}
	}()

	return app.zeroconf.Serve(func(req zeroconf.NewUserRequest) bool {
		if currentPlayer != nil {
			currentPlayer.Close()
			currentPlayer = nil

			close(apiCh)
		}

		newAppPlayer, err := app.newAppPlayer(ctx, session.BlobCredentials{
			Username: req.Username,
			Blob:     req.AuthBlob,
		})
		if err != nil {
			app.log.WithError(err).WithField("username", librespot.ObfuscateUsername(req.Username)).
				Errorf("failed creating new session from %s", req.DeviceName)
			return false
		}

		apiCh = make(chan ApiRequest)
		currentPlayer = newAppPlayer

		if app.cfg.Credentials.Zeroconf.PersistCredentials {
			app.state.Credentials.Username = newAppPlayer.sess.Username()
			app.state.Credentials.Data = newAppPlayer.sess.StoredCredentials()

			if err := app.persistState(); err != nil {
				app.log.WithError(err).Errorf("failed persisting zeroconf credentials")
			}

			app.log.WithField("username", librespot.ObfuscateUsername(newAppPlayer.sess.Username())).
				Debugf("persisted zeroconf credentials")
		}

		go newAppPlayer.Run(ctx, apiCh, app.mpris.Receive())
		return true
	})
}
