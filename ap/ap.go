package ap

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cenkalti/backoff/v4"
	librespot "github.com/devgianlu/go-librespot"
	"github.com/devgianlu/go-librespot/dh"
	pb "github.com/devgianlu/go-librespot/proto/spotify"
	"golang.org/x/crypto/pbkdf2"
	"golang.org/x/exp/slices"
	"golang.org/x/net/proxy"
	"google.golang.org/protobuf/proto"
)

const pongAckInterval = 120 * time.Second

var ErrAccesspointClosed = errors.New("accesspoint closed")

type AccesspointLoginError struct {
	Message *pb.APLoginFailed
}

func (e *AccesspointLoginError) Error() string {
	return fmt.Sprintf("accesspoint login failed: %s %v", e.Message.ErrorCode.String(), e.Message.ErrorDescription)
}

type Accesspoint struct {
	log librespot.Logger

	addr librespot.GetAddressFunc

	nonce    []byte
	deviceId string

	dh *dh.DiffieHellman

	conn    net.Conn
	encConn *shannonConn

	ctx    context.Context
	cancel context.CancelFunc

	done            chan struct{}
	closeOnce       sync.Once
	recvLoopOnce    sync.Once
	recvChans       map[PacketType][]chan Packet
	recvChansLock   sync.RWMutex
	lastPongAck     time.Time
	lastPongAckLock sync.Mutex

	// lostAt is when the connection went, in unix nanoseconds, or nought while
	// it is up. See the dealer's field of the same name.
	lostAt atomic.Int64

	// connMu protects conn, encConn, and welcome pointer state.
	connMu  sync.RWMutex
	welcome *pb.APWelcome
}

func NewAccesspoint(log librespot.Logger, addr librespot.GetAddressFunc, deviceId string) *Accesspoint {
	ctx, cancel := context.WithCancel(context.Background())
	return &Accesspoint{
		log:       log,
		addr:      addr,
		deviceId:  deviceId,
		ctx:       ctx,
		cancel:    cancel,
		done:      make(chan struct{}),
		recvChans: make(map[PacketType][]chan Packet),
	}
}

func (ap *Accesspoint) Done() <-chan struct{} {
	return ap.done
}

func (ap *Accesspoint) init(ctx context.Context) (err error) {
	// read 16 nonce bytes
	ap.nonce = make([]byte, 16)
	if _, err = rand.Read(ap.nonce); err != nil {
		return fmt.Errorf("failed reading random nonce: %w", err)
	}

	// init diffiehellman parameters
	if ap.dh, err = dh.NewDiffieHellman(); err != nil {
		return fmt.Errorf("failed initializing diffiehellman: %w", err)
	}

	// open connection to accesspoint
	attempts := 0
	for {
		attempts++
		ctx_, cancel := context.WithTimeout(ctx, time.Second*30)
		addr := ap.addr(ctx_)
		conn, err := proxy.Dial(ctx_, "tcp", addr)
		cancel()
		if err == nil {
			// close previous connection if any
			if ap.conn != nil {
				_ = ap.conn.Close()
			}

			// we assign to ap.conn after because if Dial fails we'll have a nil ap.conn which we don't want
			ap.conn = conn
			ap.log.Debugf("connected to %s", addr)
			return nil
		} else if attempts >= 6 {
			// Only try a few times before giving up.
			return fmt.Errorf("failed to connect to AP %v: %w", addr, err)
		}
		// Try again with a different AP.
		ap.log.WithError(err).Warnf("failed to connect to AP %v, retrying with a different AP", addr)
	}
}

func (ap *Accesspoint) ConnectSpotifyToken(ctx context.Context, username, token string) error {
	return ap.Connect(ctx, &pb.LoginCredentials{
		Typ:      pb.AuthenticationType_AUTHENTICATION_SPOTIFY_TOKEN.Enum(),
		Username: proto.String(username),
		AuthData: []byte(token),
	})
}

func (ap *Accesspoint) ConnectStored(ctx context.Context, username string, data []byte) error {
	return ap.Connect(ctx, &pb.LoginCredentials{
		Typ:      pb.AuthenticationType_AUTHENTICATION_STORED_SPOTIFY_CREDENTIALS.Enum(),
		Username: proto.String(username),
		AuthData: data,
	})
}

func (ap *Accesspoint) ConnectBlob(ctx context.Context, username string, encryptedBlob64 []byte) error {
	encryptedBlob := make([]byte, base64.StdEncoding.DecodedLen(len(encryptedBlob64)))
	if written, err := base64.StdEncoding.Decode(encryptedBlob, encryptedBlob64); err != nil {
		return fmt.Errorf("failed decoding encrypted blob: %w", err)
	} else {
		encryptedBlob = encryptedBlob[:written]
	}

	secret := sha1.Sum([]byte(ap.deviceId))
	baseKey := pbkdf2.Key(secret[:], []byte(username), 256, 20, sha1.New)

	key := make([]byte, 24)
	copy(key, func() []byte { sum := sha1.Sum(baseKey); return sum[:] }())
	binary.BigEndian.PutUint32(key[20:], 20)

	bc, err := aes.NewCipher(key)
	if err != nil {
		return fmt.Errorf("failed initializing AES cipher: %w", err)
	}

	decryptedBlob := make([]byte, len(encryptedBlob))
	for i := 0; i < len(encryptedBlob)-1; i += aes.BlockSize {
		bc.Decrypt(decryptedBlob[i:], encryptedBlob[i:])
	}

	for i := 0; i < len(decryptedBlob)-16; i++ {
		decryptedBlob[len(decryptedBlob)-i-1] ^= decryptedBlob[len(decryptedBlob)-i-17]
	}

	blob := bytes.NewReader(decryptedBlob)

	// discard first byte
	_, _ = blob.Seek(1, io.SeekCurrent)

	// discard some more bytes
	discardLen, _ := binary.ReadUvarint(blob)
	_, _ = blob.Seek(int64(discardLen), io.SeekCurrent)

	// discard another byte
	_, _ = blob.Seek(1, io.SeekCurrent)

	// read authentication type
	authTyp, _ := binary.ReadUvarint(blob)

	// discard another byte
	_, _ = blob.Seek(1, io.SeekCurrent)

	// read auth data
	authDataLen, _ := binary.ReadUvarint(blob)
	authData := make([]byte, authDataLen)
	_, _ = blob.Read(authData)

	return ap.Connect(ctx, &pb.LoginCredentials{
		Typ:      pb.AuthenticationType(authTyp).Enum(),
		Username: proto.String(username),
		AuthData: authData,
	})
}

func (ap *Accesspoint) Connect(ctx context.Context, creds *pb.LoginCredentials) error {
	ap.connMu.Lock()
	defer ap.connMu.Unlock()

	select {
	case <-ap.done:
		return ErrAccesspointClosed
	default:
	}

	return backoff.Retry(func() error {
		err := ap.connect(ctx, creds)
		if err != nil {
			ap.log.WithError(err).Warnf("failed connecting to accesspoint, retrying")
		}

		return err
	}, backoff.WithContext(backoff.WithMaxRetries(backoff.NewConstantBackOff(500*time.Millisecond), 5), ctx))
}

func (ap *Accesspoint) connect(ctx context.Context, creds *pb.LoginCredentials) error {
	if err := ap.init(ctx); err != nil {
		return err
	}

	if deadline, ok := ctx.Deadline(); ok {
		_ = ap.conn.SetDeadline(deadline)
		defer func() { _ = ap.conn.SetDeadline(time.Time{}) }()
	}

	// perform key exchange with diffiehellman
	exchangeData, err := ap.performKeyExchange()
	if err != nil {
		return fmt.Errorf("failed performing keyexchange: %w", err)
	}

	// solve challenge and complete connection
	if err := ap.solveChallenge(exchangeData); err != nil {
		return fmt.Errorf("failed solving challenge: %w", err)
	}

	// do authentication with credentials
	if err := ap.authenticate(ctx, creds); err != nil {
		return fmt.Errorf("failed authenticating: %w", err)
	}

	return nil
}

func (ap *Accesspoint) Close() {
	ap.closeOnce.Do(func() {
		close(ap.done)
		ap.closeConn()
		ap.cancel()
	})
}

func (ap *Accesspoint) Send(ctx context.Context, pktType PacketType, payload []byte) error {
	ap.connMu.RLock()
	select {
	case <-ap.done:
		ap.connMu.RUnlock()
		return ErrAccesspointClosed
	default:
	}

	encConn := ap.encConn
	ap.connMu.RUnlock()

	if err := encConn.sendPacket(ctx, pktType, payload); err != nil {
		select {
		case <-ap.done:
			return ErrAccesspointClosed
		default:
			return err
		}
	}

	return nil
}

func (ap *Accesspoint) Receive(types ...PacketType) <-chan Packet {
	ch := make(chan Packet)
	ap.connMu.RLock()
	select {
	case <-ap.done:
		ap.connMu.RUnlock()
		close(ch)
		return ch
	default:
	}

	ap.recvChansLock.Lock()
	for _, type_ := range types {
		ll, _ := ap.recvChans[type_]
		ll = append(ll, ch)
		ap.recvChans[type_] = ll
	}
	ap.recvChansLock.Unlock()

	// start the recv loop if necessary
	ap.startReceiving()
	ap.connMu.RUnlock()

	return ch
}

func (ap *Accesspoint) startReceiving() {
	ap.recvLoopOnce.Do(func() {
		ap.log.Tracef("starting accesspoint recv loop")
		ap.resetPongAckDeadline()
		go ap.pongAckTicker()
		go ap.recvLoop()
	})
}

func (ap *Accesspoint) recvLoop() {
loop:
	for {
		select {
		case <-ap.done:
			break loop
		default:
			// no need to hold the connMu since reconnection happens in this routine
			pkt, payload, err := ap.encConn.receivePacket(ap.ctx)
			if err != nil {
				select {
				case <-ap.done:
				default:
					ap.log.WithError(err).Errorf("failed receiving packet")
				}

				break loop
			}

			switch pkt {
			case PacketTypePing:
				ap.log.Tracef("received accesspoint ping")
				if err := ap.Send(ap.ctx, PacketTypePong, payload); err != nil {
					ap.log.WithError(err).Errorf("failed sending Pong packet")
					break loop
				}
			case PacketTypePongAck:
				ap.log.Tracef("received accesspoint pong ack")
				ap.notePongAck()
				continue
			default:
				ap.recvChansLock.RLock()
				ll, _ := ap.recvChans[pkt]
				ap.recvChansLock.RUnlock()

				handled := false
				for _, ch := range ll {
					ch <- Packet{Type: pkt, Payload: payload}
					handled = true
				}

				if !handled {
					ap.log.Debugf("skipping packet %v, len: %d", pkt, len(payload))
				}
			}
		}
	}

	// always close as we might end up here because of application errors
	ap.closeConn()

	select {
	case <-ap.done:
	default:
		if ap.keepReconnecting() {
			// reconnection was successful, do not close receivers
			return
		}

		// Either the accesspoint is closing or it has nothing left to log in
		// with. Both end the same way: the receivers below are closed, and
		// whoever is reading them is told the device has gone deaf.
		ap.Close()
	}

	ap.recvChansLock.RLock()
	defer ap.recvChansLock.RUnlock()

	var closedChannels []chan Packet
	for _, ll := range ap.recvChans {
		for _, ch := range ll {
			// call close on each channel only once
			if !slices.Contains(closedChannels, ch) {
				closedChannels = append(closedChannels, ch)
				close(ch)
			}
		}
	}
}

// reconnectCeiling is the longest this waits between two attempts at getting
// the connection back. See the dealer's constant of the same name.
// A variable rather than a constant so that a test can watch an outage that
// lasts a night happen in a moment.
var reconnectCeiling = time.Minute

// keepReconnecting tries to log back in to the accesspoint until it succeeds,
// the accesspoint is closed, or it turns out there is nothing to log in with.
// It reports whether it succeeded.
//
// A bounded retry was here before: fifteen minutes and then the receive
// channels closed, which is the device going deaf for good — playing on, still
// answering its own API, and out of everybody's reach until the process is
// restarted. A laptop shut for the night comes back to a working network and
// deserves a working device.
func (ap *Accesspoint) keepReconnecting() bool {
	wait := backoff.NewExponentialBackOff()
	wait.MaxInterval = reconnectCeiling
	wait.MaxElapsedTime = 0 // never give up

	ap.lostAt.Store(time.Now().UnixNano())
	defer ap.lostAt.Store(0)

	told := false
	for {
		// Per attempt rather than across the whole loop: connMu is what every
		// send waits on, and the audio key request the player makes to start a
		// track is one of them. Held for an hour, the daemon is frozen for an
		// hour rather than failing a track and saying so.
		ap.connMu.Lock()
		err := ap.reconnect()
		ap.connMu.Unlock()

		if err == nil {
			if since, _ := ap.OutOfTouch(); told {
				ap.log.Infof("the accesspoint is back after %s", since.Round(time.Second))
			}
			return true
		}

		// Nothing to log in with. Trying again cannot help and would go on for
		// ever, so this is the one failure that ends the trying.
		var permanent *backoff.PermanentError
		if errors.As(err, &permanent) {
			ap.log.WithError(err).Errorf("cannot reconnect the accesspoint")
			return false
		}

		if !told {
			ap.log.WithError(err).Warnf("lost the accesspoint connection; trying again until it comes back")
			told = true
		} else {
			ap.log.WithError(err).Debugf("failed reconnecting accesspoint")
		}

		select {
		case <-time.After(wait.NextBackOff()):
		case <-ap.ctx.Done():
			return false
		}
	}
}

// OutOfTouch says how long the accesspoint has been without a connection, and
// whether it is without one at all. Zero and false while it is connected.
func (ap *Accesspoint) OutOfTouch() (time.Duration, bool) {
	at := ap.lostAt.Load()
	if at == 0 {
		return 0, false
	}

	// Never negative, whatever the clock has been told since.
	if since := time.Since(time.Unix(0, at)); since > 0 {
		return since, true
	}
	return 0, true
}

func (ap *Accesspoint) pongAckTicker() {
	ticker := time.NewTicker(pongAckInterval)

loop:
	for {
		select {
		case <-ap.done:
			break loop
		case <-ticker.C:
			// Nothing to conclude from the silence while there is no
			// connection: the deadline being read belongs to a socket that has
			// already gone, and closing on it lands on the next one.
			if _, lost := ap.OutOfTouch(); lost {
				continue
			}

			timePassed := ap.timeSinceLastPongAck()
			if timePassed > pongAckInterval {
				ap.log.Errorf("did not receive last pong ack from accesspoint, %.0fs passed", timePassed.Seconds())

				// closing the connection should make the read on the "recvLoop" fail,
				// continue hoping for a new connection
				ap.closeConn()
				continue
			}
		}
	}

	ticker.Stop()
}

func (ap *Accesspoint) reconnect() (err error) {
	if ap.welcome == nil {
		return backoff.Permanent(fmt.Errorf("cannot reconnect without APWelcome"))
	}

	if err = ap.connect(ap.ctx, &pb.LoginCredentials{
		Typ:      ap.welcome.ReusableAuthCredentialsType,
		Username: ap.welcome.CanonicalUsername,
		AuthData: ap.welcome.ReusableAuthCredentials,
	}); err != nil {
		return err
	}

	ap.resetPongAckDeadline()

	// if we are here the "recvLoop" has already died, restart it
	go ap.recvLoop()

	ap.log.Debugf("re-established accesspoint connection")
	return nil
}

func (ap *Accesspoint) resetPongAckDeadline() {
	ap.lastPongAckLock.Lock()
	ap.lastPongAck = time.Now().Add(pongAckInterval)
	ap.lastPongAckLock.Unlock()
}

func (ap *Accesspoint) notePongAck() {
	ap.lastPongAckLock.Lock()
	ap.lastPongAck = time.Now()
	ap.lastPongAckLock.Unlock()
}

func (ap *Accesspoint) timeSinceLastPongAck() time.Duration {
	ap.lastPongAckLock.Lock()
	defer ap.lastPongAckLock.Unlock()
	return time.Since(ap.lastPongAck)
}

func (ap *Accesspoint) closeConn() {
	ap.connMu.RLock()
	conn := ap.conn
	ap.connMu.RUnlock()

	if conn != nil {
		_ = conn.Close()
	}
}

func (ap *Accesspoint) performKeyExchange() ([]byte, error) {
	// accumulate transferred data for challenge
	cc := &connAccumulator{Conn: ap.conn}

	var productFlags []pb.ProductFlags
	if librespot.VersionNumberString() == "dev" {
		productFlags = []pb.ProductFlags{pb.ProductFlags_PRODUCT_FLAG_DEV_BUILD}
	} else {
		productFlags = []pb.ProductFlags{pb.ProductFlags_PRODUCT_FLAG_NONE}
	}

	// send ClientHello message
	if err := writeMessage(cc, true, &pb.ClientHello{
		BuildInfo: &pb.BuildInfo{
			Product:      pb.Product_PRODUCT_CLIENT.Enum(),
			ProductFlags: productFlags,
			Platform:     librespot.GetPlatform().Enum(),
			Version:      proto.Uint64(librespot.SpotifyVersionCode),
		},
		CryptosuitesSupported: []pb.Cryptosuite{pb.Cryptosuite_CRYPTO_SUITE_SHANNON},
		ClientNonce:           ap.nonce,
		Padding:               []byte{0x1e},
		LoginCryptoHello: &pb.LoginCryptoHelloUnion{
			DiffieHellman: &pb.LoginCryptoDiffieHellmanHello{
				Gc:              ap.dh.PublicKeyBytes(),
				ServerKeysKnown: proto.Uint32(1),
			},
		},
	}); err != nil {
		return nil, fmt.Errorf("failed writing ClientHello message: %w", err)
	}

	// receive APResponseMessage message
	var apResponse pb.APResponseMessage
	if err := readMessage(cc, -1, &apResponse); err != nil {
		return nil, fmt.Errorf("failed reading APResponseMessage message: %w", err)
	}

	// verify signature
	if !verifySignature(apResponse.Challenge.LoginCryptoChallenge.DiffieHellman.Gs, apResponse.Challenge.LoginCryptoChallenge.DiffieHellman.GsSignature) {
		return nil, fmt.Errorf("failed verifying signature")
	}

	// exchange keys and compute shared secret
	ap.dh.Exchange(apResponse.Challenge.LoginCryptoChallenge.DiffieHellman.Gs)

	ap.log.Debugf("completed keyexchange")
	return cc.Dump(), nil
}

func (ap *Accesspoint) solveChallenge(exchangeData []byte) error {
	macData := make([]byte, 0, sha1.Size*5)

	mac := hmac.New(sha1.New, ap.dh.SharedSecretBytes())
	for i := byte(1); i < 6; i++ {
		mac.Reset()
		mac.Write(exchangeData)
		mac.Write([]byte{i})
		macData = mac.Sum(macData)
	}

	mac = hmac.New(sha1.New, macData[:20])
	mac.Write(exchangeData)

	if err := writeMessage(ap.conn, false, &pb.ClientResponsePlaintext{
		PowResponse:    &pb.PoWResponseUnion{},
		CryptoResponse: &pb.CryptoResponseUnion{},
		LoginCryptoResponse: &pb.LoginCryptoResponseUnion{
			DiffieHellman: &pb.LoginCryptoDiffieHellmanResponse{
				Hmac: mac.Sum(nil),
			},
		},
	}); err != nil {
		return fmt.Errorf("failed writing ClientResponsePlaintext message: %w", err)
	}

	// we are not sure if the challenge is actually completed, we check it in authenticate
	ap.encConn = newShannonConn(ap.conn, macData[20:52], macData[52:84])
	ap.log.Debug("completed challenge")
	return nil
}

func (ap *Accesspoint) authenticate(ctx context.Context, credentials *pb.LoginCredentials) error {
	if ap.encConn == nil {
		panic("accesspoint not connected")
	}

	// assemble ClientResponseEncrypted message
	payload, err := proto.Marshal(&pb.ClientResponseEncrypted{
		LoginCredentials: credentials,
		VersionString:    proto.String(librespot.VersionString()),
		SystemInfo: &pb.SystemInfo{
			Os:                      librespot.GetOS().Enum(),
			CpuFamily:               librespot.GetCpuFamily().Enum(),
			SystemInformationString: proto.String(librespot.SystemInfoString()),
			DeviceId:                proto.String(ap.deviceId),
		},
	})
	if err != nil {
		return fmt.Errorf("failed marshalling ClientResponseEncrypted message: %w", err)
	}

	// send Login packet
	if err := ap.encConn.sendPacket(ctx, PacketTypeLogin, payload); err != nil {
		return fmt.Errorf("failed sending Login packet: %w", err)
	}

	// check if we received an APResponseMessage from the challenge
	var challengeResp pb.APResponseMessage
	if peekBytes, err := ap.encConn.peekUnencrypted(9); err != nil {
		return fmt.Errorf("failed peeking unencrypted bytes: %w", err)
	} else if err = readMessage(bytes.NewReader(peekBytes), 9, &challengeResp); err == nil {
		return &AccesspointLoginError{Message: challengeResp.LoginFailed}
	}

	// receive APWelcome or AuthFailure
	recvPkt, recvPayload, err := ap.encConn.receivePacket(ctx)
	if err != nil {
		return fmt.Errorf("failed recevining Login response packet: %w", err)
	}

	if recvPkt == PacketTypeAPWelcome {
		var welcome pb.APWelcome
		if err := proto.Unmarshal(recvPayload, &welcome); err != nil {
			return fmt.Errorf("failed unmarshalling APWelcome message: %w", err)
		}

		ap.welcome = &welcome
		ap.log.WithField("username", librespot.ObfuscateUsername(*welcome.CanonicalUsername)).
			Infof("authenticated AP")

		return nil
	} else if recvPkt == PacketTypeAuthFailure {
		var loginFailed pb.APLoginFailed
		if err := proto.Unmarshal(recvPayload, &loginFailed); err != nil {
			return fmt.Errorf("failed unmarshalling APLoginFailed message: %w", err)
		}

		return &AccesspointLoginError{Message: &loginFailed}
	} else {
		return fmt.Errorf("unexpected command after Login packet: %x", recvPkt)
	}
}

func (ap *Accesspoint) Username() string {
	ap.connMu.RLock()
	defer ap.connMu.RUnlock()

	if ap.welcome == nil {
		panic("accesspoint not authenticated")
	}

	return *ap.welcome.CanonicalUsername
}

func (ap *Accesspoint) StoredCredentials() []byte {
	ap.connMu.RLock()
	defer ap.connMu.RUnlock()

	if ap.welcome == nil {
		panic("accesspoint not authenticated")
	}

	return ap.welcome.ReusableAuthCredentials
}
