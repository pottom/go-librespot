package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"

	librespot "github.com/devgianlu/go-librespot"
	connectpb "github.com/devgianlu/go-librespot/proto/spotify/connectstate"
	"github.com/devgianlu/go-librespot/spclient"
)

// maxContextTracks caps one answer. A context can run to thousands of tracks,
// and naming them all would be a long request for a screen that shows twenty.
const maxContextTracks = 100

// contextTracks lists what a playlist, album or artist holds, without playing
// it and without disturbing whatever is playing.
//
// It exists because the Web API will not answer for these: a client id
// registered after Spotify's clampdown is refused the tracks of any playlist,
// including the user's own. The session here is a player rather than an
// application, and the protocol it speaks has no such rule — so a controller
// that could play a playlist but never list it can now do both.
func (p *AppPlayer) contextTracks(ctx context.Context, uri string, offset, limit int) (*ApiResponseContext, error) {
	if uri == "" {
		return nil, ErrBadRequest
	}
	offset = max(offset, 0)
	limit = min(max(limit, 1), maxContextTracks)

	resolver, err := spclient.NewContextResolver(ctx, p.app.log, p.sess.Spclient(), &connectpb.Context{Uri: uri})
	if err != nil {
		return nil, fmt.Errorf("failed resolving context %s: %w", uri, err)
	}

	// The pages are walked from the first: a context page is not addressable by
	// track number, only by following the one before it.
	var (
		rows []ApiResponseQueueTrack
		seen int
	)
	for idx := 0; seen < offset+limit; idx++ {
		page, err := resolver.Page(ctx, idx)
		if errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("failed fetching context page %d: %w", idx, err)
		}

		for _, track := range page {
			if seen++; seen <= offset {
				continue
			}
			if len(rows) == limit {
				break
			}
			rows = append(rows, ApiResponseQueueTrack{
				Uri: contextTrackUri(resolver.Type(), track),
				Uid: track.Uid,
			})
		}
	}

	p.describeQueue(ctx, rows)
	for i := range rows {
		rows[i].Tempo = p.app.tempos.Get(rows[i].Uri)
	}
	return &ApiResponseContext{Uri: uri, Offset: offset, Tracks: rows}, nil
}

// contextTrackUri is the track's name in the only form the rest of the world
// uses. A context page carries a gid, a uri, or both, depending on where the
// page came from.
func contextTrackUri(typ librespot.SpotifyIdType, track *connectpb.ContextTrack) string {
	if len(track.Uri) > 0 {
		return track.Uri
	}
	if len(track.Gid) > 0 {
		return librespot.SpotifyIdFromGid(typ, track.Gid).Uri()
	}
	return ""
}
