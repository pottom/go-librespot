package daemon

import (
	"testing"
	"time"

	connectpb "github.com/devgianlu/go-librespot/proto/spotify/connectstate"
)

// A device left alone — asleep, or with an output that died while the state
// went on saying it played — comes back with a timestamp hours old. The clock
// moved; the track did not, and advancing the position by that gap is what
// resumed a three-minute track seven hours in and froze the output on a stream
// with nothing left to read.
func TestAStaleTimestampDoesNotAdvanceTheTrack(t *testing.T) {
	s := &State{player: &connectpb.PlayerState{
		IsPlaying:             true,
		PlaybackSpeed:         1,
		Duration:              214883,
		PositionAsOfTimestamp: 60000,
		Timestamp:             time.Now().Add(-7 * time.Hour).UnixMilli(),
	}}

	s.updateTimestamp()
	if got := s.player.PositionAsOfTimestamp; got != 60000 {
		t.Errorf("the position moved to %dms over a gap of seven hours, want it left at 60000ms", got)
	}
}

// A short gap is playback, and does advance it.
func TestAFreshTimestampAdvancesTheTrack(t *testing.T) {
	s := &State{player: &connectpb.PlayerState{
		IsPlaying:             true,
		PlaybackSpeed:         1,
		Duration:              214883,
		PositionAsOfTimestamp: 1000,
		Timestamp:             time.Now().Add(-2 * time.Second).UnixMilli(),
	}}

	s.updateTimestamp()
	if got := s.player.PositionAsOfTimestamp; got < 2500 || got > 3500 {
		t.Errorf("the position moved to %dms over two seconds, want about 3000ms", got)
	}
}

// Nothing that asks where playback is may answer past the end of the track: a
// stream created there reads no samples at all, and the output dies on the
// empty read instead of the track starting over.
func TestThePositionNeverLeavesTheTrack(t *testing.T) {
	s := &State{player: &connectpb.PlayerState{
		IsPaused:              true,
		Duration:              214883,
		PositionAsOfTimestamp: 25407478,
		Timestamp:             time.Now().UnixMilli(),
	}}

	if got := s.trackPosition(); got != 0 {
		t.Errorf("trackPosition = %dms past the end of a %dms track, want the beginning", got, s.player.Duration)
	}
}
