package daemon

import (
	"context"
	"strings"

	extmetadatapb "github.com/devgianlu/go-librespot/proto/spotify/extendedmetadata"
	metadatapb "github.com/devgianlu/go-librespot/proto/spotify/metadata"
)

// maxQueueMetadata caps how many upcoming tracks are named in one go. The list
// is asked for whenever a controller opens its queue screen, and a context can
// run to hundreds of tracks; nobody reads that far down.
const maxQueueMetadata = 50

// describeQueue fills in the names of the upcoming tracks, in one request.
//
// A failure is not fatal: the queue is still an ordered list of ids, and a
// controller that cannot name a track can still show its place and act on it.
func (p *AppPlayer) describeQueue(ctx context.Context, tracks []ApiResponseQueueTrack) {
	if len(tracks) == 0 || p.prodInfo == nil {
		return
	}
	if len(tracks) > maxQueueMetadata {
		tracks = tracks[:maxQueueMetadata]
	}

	req := &extmetadatapb.BatchedEntityRequest{}
	for _, track := range tracks {
		if track.Uri == "" {
			continue
		}
		req.EntityRequest = append(req.EntityRequest, &extmetadatapb.EntityRequest{
			EntityUri: track.Uri,
			Query: []*extmetadatapb.ExtensionQuery{{
				ExtensionKind: extmetadatapb.ExtensionKind_TRACK_V4,
			}},
		})
	}
	if len(req.EntityRequest) == 0 {
		return
	}

	resp, err := p.sess.Spclient().ExtendedMetadata(ctx, req)
	if err != nil {
		p.app.log.WithError(err).Warnf("failed fetching metadata for %d queued tracks", len(req.EntityRequest))
		return
	}

	byUri := make(map[string]*metadatapb.Track, len(req.EntityRequest))
	for _, item := range resp.ExtendedMetadata {
		if item.ExtensionKind != extmetadatapb.ExtensionKind_TRACK_V4 {
			continue
		}
		for _, data := range item.ExtensionData {
			if data.Header.StatusCode != 200 {
				continue
			}
			var track metadatapb.Track
			if err := data.ExtensionData.UnmarshalTo(&track); err != nil {
				continue
			}
			byUri[data.EntityUri] = &track
		}
	}

	for i := range tracks {
		track, ok := byUri[tracks[i].Uri]
		if !ok {
			continue
		}
		p.describeTrack(&tracks[i], track)
	}
}

// describeTrack copies what a controller needs to draw a row out of the track
// metadata. It mirrors newApiResponseStatusTrack, which does the same for the
// track actually playing.
func (p *AppPlayer) describeTrack(out *ApiResponseQueueTrack, track *metadatapb.Track) {
	artists := make([]string, 0, len(track.Artist))
	for _, artist := range track.Artist {
		if artist.Name != nil {
			artists = append(artists, *artist.Name)
		}
	}

	coverId := getBestImageIdForSize(track.Album.Cover, p.app.cfg.ImageSize)
	if coverId == nil && track.Album.CoverGroup != nil {
		coverId = getBestImageIdForSize(track.Album.CoverGroup.Image, p.app.cfg.ImageSize)
	}

	out.Name = valueOr(track.Name)
	out.ArtistNames = artists
	out.AlbumName = valueOr(track.Album.Name)
	out.AlbumCoverUrl = p.prodInfo.ImageUrl(coverId)
	out.Duration = int(intOr(track.Duration))
	out.ReleaseDate = track.Album.Date.String()
	out.TrackNumber = int(intOr(track.Number))
	out.DiscNumber = int(intOr(track.DiscNumber))
	for _, disc := range track.Album.Disc {
		out.TotalTracks += len(disc.Track)
	}
	if track.Album.Type != nil {
		out.AlbumType = strings.ToLower(track.Album.Type.String())
	}
	out.Popularity = int(intOr(track.Popularity))
}

func valueOr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func intOr(n *int32) int32 {
	if n == nil {
		return 0
	}
	return *n
}
