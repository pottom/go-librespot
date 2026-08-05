package daemon

import (
	"context"
	"time"

	librespot "github.com/devgianlu/go-librespot"
	"github.com/devgianlu/go-librespot/dealer"
	"github.com/devgianlu/go-librespot/player"
	connectpb "github.com/devgianlu/go-librespot/proto/spotify/connectstate"
	"github.com/devgianlu/go-librespot/tracks"
)

type State struct {
	active      bool
	activeSince time.Time

	device *connectpb.DeviceInfo
	player *connectpb.PlayerState

	tracks  *tracks.List
	queueID uint64

	lastCommand           *dealer.RequestPayload
	lastTransferTimestamp int64
}

// Set the IsPaused flag, and also the PlaybackSpeed as well.
// PlaybackSpeed must be 0 when paused, or Spotify Android will have subtle
// bugs.
func (s *State) setPaused(val bool) {
	s.player.IsPaused = val
	if val {
		s.player.PlaybackSpeed = 0
	} else {
		s.player.PlaybackSpeed = 1
	}
}

func (s *State) setActive(val bool) {
	if val {
		if s.active {
			return
		}

		s.active = true
		s.activeSince = time.Now()
	} else {
		s.active = false
		s.activeSince = time.Time{}
	}
}

func (s *State) reset() {
	s.active = false
	s.activeSince = time.Time{}
	s.player = &connectpb.PlayerState{
		IsSystemInitiated: true,
		PlaybackSpeed:     1,
		PlayOrigin:        &connectpb.PlayOrigin{},
		Suppressions:      &connectpb.Suppressions{},
		Options:           &connectpb.ContextPlayerOptions{},
	}
}

// maxReasonableElapsed is the longest gap between two updates that can still be
// playback rather than a device left alone. Beyond it the timestamp is stale —
// a previous session, a sleeping machine, or an output that died while the
// state went on saying it was playing — and the time that passed is not time
// the track advanced by.
const maxReasonableElapsed = 10 * 60 * 1000 // ten minutes, in milliseconds

func (s *State) trackPosition() int64 {
	// If paused or not actually playing, use raw position value
	if s.player.IsPaused || !s.player.IsPlaying {
		return s.clampedPosition(s.player.PositionAsOfTimestamp)
	}

	// Calculate dynamic position only if playback is actually active
	now := time.Now().UnixMilli()
	elapsed := now - s.player.Timestamp

	if elapsed > maxReasonableElapsed || elapsed < 0 {
		return s.clampedPosition(s.player.PositionAsOfTimestamp)
	}

	calculated := s.player.PositionAsOfTimestamp + elapsed
	// Ensure position is non-negative (shouldn't happen, but defensive)
	if calculated < 0 {
		return s.clampedPosition(s.player.PositionAsOfTimestamp)
	}

	return s.clampedPosition(calculated)
}

// clampedPosition keeps a position inside the track it belongs to.
//
// A position past the end is not a position: loading a stream there reads no
// samples at all, the output fails on the empty read, and the device sits there
// looking like it has frozen. That is exactly what an inflated timestamp
// produced — a track of three minutes resumed seven hours in — so nothing that
// asks where playback is gets an answer outside the track.
func (s *State) clampedPosition(position int64) int64 {
	if position < 0 {
		return 0
	}
	if d := s.player.Duration; d > 0 && position > d {
		return 0
	}
	return position
}

// Update timestamp, and updating the player position timestamp according to how
// much time has passed since the last update.
func (s *State) updateTimestamp() {
	// Use single timestamp throughout, for consistency.
	now := time.Now()

	// How many milliseconds the playback has advanced since the last update to
	// PositionAsOfTimestamp.
	advancedTimeMillis := now.UnixMilli() - s.player.Timestamp

	// A gap too long to be playback is a device that was left alone: asleep,
	// or with an output that died while the state went on saying it played.
	// The clock moved; the track did not. Advancing by it is what turned a
	// three-minute track into a position seven hours in.
	if advancedTimeMillis > maxReasonableElapsed || advancedTimeMillis < 0 {
		advancedTimeMillis = 0
	}

	// How far the playback position has advanced during that time.
	// (For example, PlaybackSpeed is 0 when paused so the position doesn't
	// change).
	advancedPositionMillis := int64(float64(advancedTimeMillis) * s.player.PlaybackSpeed)

	// Update the timestamps accordingly.
	s.player.PositionAsOfTimestamp += advancedPositionMillis
	s.player.Timestamp = now.UnixMilli()
}

func (s *State) playOrigin() string {
	return s.player.PlayOrigin.FeatureIdentifier
}

func (p *AppPlayer) initState() {
	p.state = &State{
		lastCommand: nil,
		device: &connectpb.DeviceInfo{
			CanPlay:               true,
			Volume:                player.MaxStateVolume,
			Name:                  p.app.cfg.DeviceName,
			DeviceId:              p.app.deviceId,
			DeviceType:            p.app.deviceType,
			DeviceSoftwareVersion: librespot.VersionString(),
			ClientId:              librespot.ClientIdHex,
			SpircVersion:          "3.2.6",
			Capabilities: &connectpb.Capabilities{
				CanBePlayer:                true,
				RestrictToLocal:            false,
				GaiaEqConnectId:            true,
				SupportsLogout:             p.app.cfg.ZeroconfEnabled,
				IsObservable:               true,
				VolumeSteps:                int32(p.app.cfg.VolumeSteps),
				SupportedTypes:             []string{"audio/track", "audio/episode"},
				CommandAcks:                true,
				SupportsRename:             false,
				Hidden:                     false,
				DisableVolume:              false,
				ConnectDisabled:            false,
				SupportsPlaylistV2:         true,
				IsControllable:             true,
				SupportsExternalEpisodes:   false, // TODO: support external episodes
				SupportsSetBackendMetadata: true,
				SupportsTransferCommand:    true,
				SupportsCommandRequest:     true,
				IsVoiceEnabled:             false,
				NeedsFullPlayerState:       false,
				SupportsGzipPushes:         true,
				SupportsSetOptionsCommand:  true,
				SupportsHifi:               nil, // TODO: nice to have?
				ConnectCapabilities:        "",
			},
		},
	}
	p.state.reset()
}

// statePutMinInterval is the minimum spacing between connect-state PUTs.
const statePutMinInterval = 200 * time.Millisecond

// updateState PUTs the latest connect-state, at most one per statePutMinInterval: immediately
// and synchronously when the budget allows, else deferred to the timer so a burst coalesces.
func (p *AppPlayer) updateState(ctx context.Context) {
	p.stateDirty = true
	if p.statePutScheduled {
		return
	}
	if wait := statePutMinInterval - time.Since(p.lastStatePut); wait > 0 {
		p.statePutScheduled = true
		p.stateTimer.Reset(wait)
		return
	}
	p.flushState(ctx)
}

func (p *AppPlayer) putConnectState(ctx context.Context, reason connectpb.PutStateReason) error {
	if reason == connectpb.PutStateReason_BECAME_INACTIVE {
		return p.sess.Spclient().PutConnectStateInactive(ctx, p.spotConnId, false)
	}

	putStateReq := &connectpb.PutStateRequest{
		ClientSideTimestamp: uint64(time.Now().UnixMilli()),
		MemberType:          connectpb.MemberType_CONNECT_STATE,
		PutStateReason:      reason,
	}

	if t := p.state.activeSince; !t.IsZero() {
		putStateReq.StartedPlayingAt = uint64(t.UnixMilli())
	}
	if t := p.player.HasBeenPlayingFor(); t > 0 {
		putStateReq.HasBeenPlayingForMs = uint64(t.Milliseconds())
	}

	putStateReq.IsActive = p.state.active
	putStateReq.Device = &connectpb.Device{
		DeviceInfo:  p.state.device,
		PlayerState: p.state.player,
	}

	if p.state.lastCommand != nil {
		putStateReq.LastCommandMessageId = p.state.lastCommand.MessageId
		putStateReq.LastCommandSentByDeviceId = p.state.lastCommand.SentByDeviceId
	}

	// finally send the state update
	return p.sess.Spclient().PutConnectState(ctx, p.spotConnId, putStateReq)
}
