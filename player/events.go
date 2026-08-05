package player

import (
	librespot "github.com/devgianlu/go-librespot"
	"github.com/devgianlu/go-librespot/audio"
	connectpb "github.com/devgianlu/go-librespot/proto/spotify/connectstate"
	"github.com/devgianlu/go-librespot/proto/spotify/metadata"
)

type EventType int

const (
	EventTypePlay EventType = iota
	EventTypeResume
	EventTypePause
	EventTypeStop
	EventTypeNotPlaying
)

type Event struct {
	Type EventType

	// Failed marks a stop that nobody asked for: the output device gave up
	// rather than being closed. The two look the same from here and are not
	// the same at all — one is a track ending or a command being obeyed, the
	// other is the music stopping while everything above still believes it is
	// playing.
	Failed bool
}

type EventManager interface {
	PreStreamLoadNew(playbackId []byte, spotId librespot.SpotifyId, mediaPosition int64)
	PostStreamResolveAudioFile(playbackId []byte, targetBitrate int32, media *librespot.Media, file *metadata.AudioFile)
	PostStreamRequestAudioKey(playbackId []byte)
	PostStreamResolveStorage(playbackId []byte)
	PostStreamInitHttpChunkReader(playbackId []byte, reader *audio.HttpChunkedReader)

	OnPrimaryStreamUnload(stream *Stream, pos int64)
	PostPrimaryStreamLoad(stream *Stream, paused bool)

	OnPlayerPlay(stream *Stream, ctxUri string, shuffle bool, playOrigin *connectpb.PlayOrigin, track *connectpb.ProvidedTrack, pos int64)
	OnPlayerResume(stream *Stream, pos int64)
	OnPlayerPause(stream *Stream, ctxUri string, shuffle bool, playOrigin *connectpb.PlayOrigin, track *connectpb.ProvidedTrack, pos int64)
	OnPlayerSeek(stream *Stream, oldPos, newPos int64)
	OnPlayerSkipForward(stream *Stream, pos int64, skipTo bool)
	OnPlayerSkipBackward(stream *Stream, pos int64)
	OnPlayerEnd(stream *Stream, pos int64)

	Close()
}
