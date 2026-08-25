package dealer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/coder/websocket"
	librespot "github.com/devgianlu/go-librespot"
)

const (
	pingInterval = 30 * time.Second
	timeout      = 10 * time.Second
)

var ErrDealerClosed = errors.New("dealer closed")

type Dealer struct {
	log librespot.Logger

	client *http.Client

	addr        librespot.GetAddressFunc
	accessToken librespot.GetLogin5TokenFunc

	conn *websocket.Conn

	ctx    context.Context
	cancel context.CancelFunc

	done         chan struct{}
	closeOnce    sync.Once
	recvLoopOnce sync.Once
	lastPong     time.Time
	lastPongLock sync.Mutex

	// lostAt is when the connection went, in unix nanoseconds, or nought while
	// it is up. It is the one place that says "there is no connection right
	// now": the ping ticker reads it to keep out of the way, and the daemon
	// reads it to say so in its status.
	lostAt atomic.Int64

	// connMu protects conn pointer state.
	connMu sync.RWMutex

	messageReceivers     []messageReceiver
	messageReceiversLock sync.RWMutex

	requestReceivers     map[string]requestReceiver
	requestReceiversLock sync.RWMutex
}

func NewDealer(log librespot.Logger, client *http.Client, dealerAddr librespot.GetAddressFunc, accessToken librespot.GetLogin5TokenFunc) *Dealer {
	ctx, cancel := context.WithCancel(context.Background())
	return &Dealer{
		client: &http.Client{
			Transport:     client.Transport,
			CheckRedirect: client.CheckRedirect,
			Jar:           client.Jar,
			Timeout:       timeout,
		},
		log:              log,
		addr:             dealerAddr,
		accessToken:      accessToken,
		ctx:              ctx,
		cancel:           cancel,
		done:             make(chan struct{}),
		requestReceivers: map[string]requestReceiver{},
	}
}

func (d *Dealer) Connect(ctx context.Context) error {
	d.connMu.Lock()
	defer d.connMu.Unlock()

	select {
	case <-d.done:
		return ErrDealerClosed
	default:
	}

	if d.conn != nil {
		d.log.Debugf("dealer connection already opened")
		return nil
	}

	return d.connect(ctx)
}

// ErrTokenRefused reports that the handshake was answered with a status that
// only the access token can explain. It is a distinct error because the answer
// to it is not to wait: it is to get another token.
var ErrTokenRefused = errors.New("dealer refused the access token")

func (d *Dealer) connect(ctx context.Context) error {
	err := d.dial(ctx, false)
	if !errors.Is(err, ErrTokenRefused) {
		return err
	}

	// The token we hold was refused. Nothing here can tell that from a token
	// that is still good: it is renewed only when our own clock says it has
	// expired, and a token revoked at the other end expires no sooner for
	// being dead. So every retry carries the same refused string, and a
	// connection that could have been re-made in a second is never made again.
	//
	// Measured on this device: seven times over twenty-five days the dealer
	// gave up for good, and every one of the seven was a 401 — never a
	// network error, never a timeout. Asking for a new token costs one
	// Login5 call and is the only answer that can work.
	d.log.WithError(err).Debugf("renewing the dealer access token after it was refused")
	return d.dial(ctx, true)
}

// dial opens the websocket. freshToken forces a new access token rather than
// reusing the one already in hand.
func (d *Dealer) dial(ctx context.Context, freshToken bool) error {
	accessToken, err := d.accessToken(ctx, freshToken)
	if err != nil {
		return fmt.Errorf("failed obtaining dealer access token: %w", err)
	}

	addr := d.addr(ctx)
	conn, resp, err := websocket.Dial(ctx, fmt.Sprintf("wss://%s/?access_token=%s", addr, accessToken), &websocket.DialOptions{
		HTTPClient: d.client,
		HTTPHeader: http.Header{
			"User-Agent": []string{librespot.UserAgent()},
		},
	})
	if err != nil {
		if refusedToken(resp) {
			return fmt.Errorf("%w: %w", ErrTokenRefused, err)
		}
		return err
	}

	if d.conn != nil {
		_ = d.conn.Close(websocket.StatusServiceRestart, "")
	}

	// we assign to d.conn after because if Dial fails we'll have a nil d.conn which we don't want
	d.conn = conn
	d.log.Debug(fmt.Sprintf("connected to %s", addr))

	// remove the read limit
	d.conn.SetReadLimit(math.MaxUint32)

	return nil
}

// refusedToken says whether a failed handshake was the token being refused.
//
// It also closes the body, which nobody else will: a dial that fails hands back
// the first kilobyte of the answer for whoever wants to read it, and a body left
// open is a connection never given back to the pool.
func refusedToken(resp *http.Response) bool {
	if resp == nil {
		return false
	}
	if resp.Body != nil {
		_ = resp.Body.Close()
	}
	// Forbidden as well as unauthorised: both are the other end saying who we
	// claim to be is not good enough, and both are mended the same way. A token
	// is the only credential this handshake carries.
	return resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden
}

func (d *Dealer) Close() {
	d.closeOnce.Do(func() {
		close(d.done)

		// Before cancelling, so the peer still gets a clean going-away frame:
		// cancelling the context tears the websocket down abruptly.
		d.closeConn(websocket.StatusGoingAway)

		d.cancel()
	})
}

func (d *Dealer) startReceiving() {
	d.recvLoopOnce.Do(func() {
		d.log.Tracef("starting dealer recv loop")
		d.resetPongDeadline()
		go d.pingTicker()
		go d.recvLoop()
	})
}

func (d *Dealer) pingTicker() {
	ticker := time.NewTicker(pingInterval)

loop:
	for {
		select {
		case <-d.done:
			break loop
		case <-ticker.C:
			// Nothing to ping and nothing to conclude from the silence: the
			// deadline being read belongs to a socket that has already gone.
			// Left in, this closes the *next* connection the moment one is
			// made, and the reconnecting starts over for no reason.
			if _, lost := d.OutOfTouch(); lost {
				continue
			}

			timePassed := d.timeSinceLastPong()
			if timePassed > pingInterval+timeout {
				d.log.Errorf("did not receive last pong from dealer, %.0fs passed", timePassed.Seconds())

				// closing the connection should make the read on the "recvLoop" fail,
				// continue hoping for a new connection
				d.closeConn(websocket.StatusServiceRestart)
				continue
			}

			ctx, cancel := context.WithTimeout(d.ctx, timeout)
			conn, err := d.writeConn(ctx, websocket.MessageText, []byte("{\"type\":\"ping\"}"))
			cancel()
			d.log.Tracef("sent dealer ping")

			if err != nil {
				select {
				case <-d.done:
					break loop
				default:
				}

				d.log.WithError(err).Warnf("failed sending dealer ping")

				// closing the connection should make the read on the "recvLoop" fail,
				// continue hoping for a new connection
				d.closeConnRef(conn, websocket.StatusServiceRestart)
				continue
			}
		}
	}

	ticker.Stop()
}

func (d *Dealer) recvLoop() {
loop:
	for {
		select {
		case <-d.done:
			break loop
		default:
			// no need to hold the connMu since reconnection happens in this routine
			msgType, messageBytes, err := d.readConn(d.ctx)

			// don't log closed error if we're shutting down
			if err != nil {
				select {
				case <-d.done:
					if websocket.CloseStatus(err) == websocket.StatusGoingAway {
						d.log.Debugf("dealer connection closed")
					}
					break loop
				default:
				}

				d.log.WithError(err).Errorf("failed receiving dealer message")
				break loop
			} else if msgType != websocket.MessageText {
				d.log.WithError(err).Warnf("unsupported message type: %v, len: %d", msgType, len(messageBytes))
				continue
			}

			var message RawMessage
			if err := json.Unmarshal(messageBytes, &message); err != nil {
				d.log.WithError(err).Error("failed unmarshalling dealer message")
				break loop
			}

			switch message.Type {
			case "message":
				d.handleMessage(&message)
				break
			case "request":
				d.handleRequest(&message)
				break
			case "ping":
				// we never receive ping messages
				break
			case "pong":
				d.lastPongLock.Lock()
				d.lastPong = time.Now()
				d.lastPongLock.Unlock()
				d.log.Tracef("received dealer pong")
				break
			default:
				d.log.Warnf("unknown dealer message type: %s", message.Type)
				break
			}
		}
	}

	// always close as we might end up here because of application errors
	d.closeConn(websocket.StatusInternalError)

	select {
	case <-d.done:
	default:
		if d.keepReconnecting() {
			// reconnection was successful, do not close receivers
			return
		}

		// Nothing but the dealer closing ends the trying, so getting here is a
		// shutdown. Close anyway: whoever cancelled the context may not have
		// been Close, and the receivers below must not be left to a reader that
		// still believes the connection is coming back.
		d.Close()
	}

	d.requestReceiversLock.RLock()
	for _, recv := range d.requestReceivers {
		close(recv.c)
	}
	d.requestReceiversLock.RUnlock()

	d.messageReceiversLock.RLock()
	for _, recv := range d.messageReceivers {
		close(recv.c)
	}
	d.messageReceiversLock.RUnlock()

	d.log.Debugf("dealer recv loop stopped")
}

// reconnectCeiling is the longest this waits between two attempts at getting
// the connection back. Long enough that an hour off the network is an hour of
// sixty tries rather than thousands, short enough that somebody who opens the
// lid gets Connect back while they are still looking at the screen.
// A variable rather than a constant so that a test can watch an outage that
// lasts a night happen in a moment.
var reconnectCeiling = time.Minute

// keepReconnecting tries to re-establish the connection until it succeeds or
// the dealer is closed, and reports whether it succeeded.
//
// It used to be a bounded retry: fifteen minutes, and then the receivers were
// closed for good and Spotify Connect was gone until the process was started
// again. Measured on one device, that happened seven times in twenty-five days,
// and every single one was the access token being refused rather than anything
// wrong with the network — see connect. There is nothing to be gained by
// stopping: a device nobody can reach is worth exactly what a device still
// trying is worth, and only one of the two mends itself.
func (d *Dealer) keepReconnecting() bool {
	wait := backoff.NewExponentialBackOff()
	wait.MaxInterval = reconnectCeiling
	wait.MaxElapsedTime = 0 // never give up

	d.lostAt.Store(time.Now().UnixNano())
	defer d.lostAt.Store(0)

	told := false
	for {
		// The lock is taken for each attempt rather than held across the whole
		// loop. Everything that writes to the socket waits on it, and one of
		// those is the goroutine that answers Spotify's requests: held for an
		// hour, they wait an hour, and a daemon that is merely off the network
		// looks like a daemon that has crashed.
		d.connMu.Lock()
		err := d.reconnect()
		d.connMu.Unlock()

		if err == nil {
			if since, _ := d.OutOfTouch(); told {
				d.log.Infof("the dealer is back after %s", since.Round(time.Second))
			}
			return true
		}

		if !told {
			// Once, and then quietly. A line a minute for an outage that lasts
			// the night buries everything else in the log; the status field is
			// what says it is still going on.
			d.log.WithError(err).Warnf("lost the dealer connection; trying again until it comes back")
			told = true
		} else {
			d.log.WithError(err).Debugf("failed reconnecting dealer")
		}

		select {
		case <-time.After(wait.NextBackOff()):
		case <-d.ctx.Done():
			return false
		}
	}
}

// OutOfTouch says how long the dealer has been without a connection, and
// whether it is without one at all. Zero and false while it is connected.
func (d *Dealer) OutOfTouch() (time.Duration, bool) {
	at := d.lostAt.Load()
	if at == 0 {
		return 0, false
	}

	// Never negative, whatever the clock has been told since.
	if since := time.Since(time.Unix(0, at)); since > 0 {
		return since, true
	}
	return 0, true
}

func (d *Dealer) sendReply(key string, success bool) error {
	reply := Reply{Type: "reply", Key: key}
	reply.Payload.Success = success

	replyBytes, err := json.Marshal(reply)
	if err != nil {
		return fmt.Errorf("failed marshalling reply: %w", err)
	}

	ctx, cancel := context.WithTimeout(d.ctx, timeout)
	_, err = d.writeConn(ctx, websocket.MessageText, replyBytes)
	cancel()
	if err != nil {
		return fmt.Errorf("failed sending dealer reply: %w", err)
	}

	return nil
}

func (d *Dealer) reconnect() error {
	if err := d.connect(d.ctx); err != nil {
		return err
	}

	d.resetPongDeadline()
	// restart the recv loop
	go d.recvLoop()

	d.log.Debugf("re-established dealer connection")
	return nil
}

func (d *Dealer) resetPongDeadline() {
	d.lastPongLock.Lock()
	d.lastPong = time.Now().Add(pingInterval)
	d.lastPongLock.Unlock()
}

func (d *Dealer) timeSinceLastPong() time.Duration {
	d.lastPongLock.Lock()
	defer d.lastPongLock.Unlock()
	return time.Since(d.lastPong)
}

func (d *Dealer) closeConn(status websocket.StatusCode) {
	d.connMu.RLock()
	conn := d.conn
	d.connMu.RUnlock()

	d.closeConnRef(conn, status)
}

func (d *Dealer) closeConnRef(conn *websocket.Conn, status websocket.StatusCode) {
	if conn != nil {
		_ = conn.Close(status, "")
	}
}

func (d *Dealer) writeConn(ctx context.Context, typ websocket.MessageType, payload []byte) (*websocket.Conn, error) {
	d.connMu.RLock()
	select {
	case <-d.done:
		d.connMu.RUnlock()
		return nil, ErrDealerClosed
	default:
	}

	conn := d.conn
	d.connMu.RUnlock()

	if conn == nil {
		return nil, fmt.Errorf("dealer connection not established")
	}

	err := conn.Write(ctx, typ, payload)
	if err != nil {
		select {
		case <-d.done:
			return conn, ErrDealerClosed
		default:
		}
	}

	return conn, err
}

func (d *Dealer) readConn(ctx context.Context) (websocket.MessageType, []byte, error) {
	d.connMu.RLock()
	conn := d.conn
	d.connMu.RUnlock()

	if conn == nil {
		return 0, nil, fmt.Errorf("dealer connection not established")
	}

	return conn.Read(ctx)
}
