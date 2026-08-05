package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	librespot "github.com/devgianlu/go-librespot"
)

// Where Spotify has no timings, somebody else usually does.
//
// Spotify answers with the words for far more tracks than it has timings for,
// and which of the two arrives is decided per track by whoever supplied them.
// LRCLIB is a public database of exactly the missing thing — timed lines,
// keyed by artist, title and length, no account and no key — so a track
// Spotify has only in plain text can still be followed.
//
// It is consulted only when Spotify's answer is not good enough, and what goes
// to it is what identifies a song: the artist, the title, the album and how
// long it runs. It says which words came from where, and the screen repeats it.
const (
	lrclibGet    = "https://lrclib.net/api/get"
	lrclibSearch = "https://lrclib.net/api/search"

	// lrclibName is what this player calls itself there. LRCLIB asks clients to
	// identify themselves and to say where to complain about them.
	lrclibName = "spindle (https://github.com/pottom/spindle)"

	// lrclibTimeout bounds the whole detour. The words are worth a moment and
	// not worth a wait: the screen already has something to show either way.
	lrclibTimeout = 6 * time.Second

	// lrclibSlack is how far a candidate's length may be from the track being
	// played before it is taken to be a different recording. Live versions,
	// radio edits and remasters of one song sit within a few seconds of each
	// other, and lyrics timed against the wrong one drift visibly.
	lrclibSlack = 3 * time.Second
)

// lrclibRecord is one entry there.
type lrclibRecord struct {
	TrackName    string  `json:"trackName"`
	ArtistName   string  `json:"artistName"`
	Duration     float64 `json:"duration"`
	Instrumental bool    `json:"instrumental"`
	PlainLyrics  string  `json:"plainLyrics"`
	SyncedLyrics string  `json:"syncedLyrics"`
}

// lyricsFromLRCLIB looks the current track up and returns whatever it has, or
// nil. A failure is not an error worth carrying up: the caller already has
// Spotify's answer, whatever it was worth.
func (p *AppPlayer) lyricsFromLRCLIB(ctx context.Context, trackId string) *ApiResponseLyrics {
	title, artist, album, duration, ok := p.playingDetails(trackId)
	if !ok || title == "" || artist == "" {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, lrclibTimeout)
	defer cancel()

	// The exact lookup first: it matches on the length as well, so what comes
	// back is the same recording rather than another take of the same song.
	if rec := lrclibExact(ctx, title, artist, album, duration); rec != nil {
		if out := lrclibLyrics(rec); out != nil {
			return out
		}
	}
	if rec := lrclibClosest(ctx, title, artist, duration); rec != nil {
		return lrclibLyrics(rec)
	}
	return nil
}

// playingDetails is what identifies the track to somebody who has never heard
// of Spotify ids. It answers only for the track playing: that is the one the
// screen is asking about, and a lyric matched by name for anything else would
// as often be a different song of the same name.
func (p *AppPlayer) playingDetails(trackId string) (title, artist, album string, duration time.Duration, ok bool) {
	if p.primaryStream == nil || p.primaryStream.Media == nil || !p.primaryStream.Media.IsTrack() {
		return "", "", "", 0, false
	}
	track := p.primaryStream.Media.Track()
	if librespot.SpotifyIdFromGid(librespot.SpotifyIdTypeTrack, track.Gid).Base62() != trackId {
		return "", "", "", 0, false
	}

	if len(track.Artist) > 0 && track.Artist[0].Name != nil {
		artist = *track.Artist[0].Name
	}
	if track.Album != nil && track.Album.Name != nil {
		album = *track.Album.Name
	}
	return track.GetName(), artist, album, time.Duration(track.GetDuration()) * time.Millisecond, true
}

func lrclibExact(ctx context.Context, title, artist, album string, duration time.Duration) *lrclibRecord {
	q := url.Values{}
	q.Set("track_name", title)
	q.Set("artist_name", artist)
	if album != "" {
		q.Set("album_name", album)
	}
	if duration > 0 {
		q.Set("duration", strconv.Itoa(int(duration.Round(time.Second).Seconds())))
	}

	var rec lrclibRecord
	if err := lrclibFetch(ctx, lrclibGet+"?"+q.Encode(), &rec); err != nil {
		return nil
	}
	return &rec
}

// lrclibClosest searches by name and takes the nearest recording of the right
// length that actually carries timings.
func lrclibClosest(ctx context.Context, title, artist string, duration time.Duration) *lrclibRecord {
	q := url.Values{}
	q.Set("track_name", title)
	q.Set("artist_name", artist)

	var found []lrclibRecord
	if err := lrclibFetch(ctx, lrclibSearch+"?"+q.Encode(), &found); err != nil {
		return nil
	}

	var best *lrclibRecord
	bestOff := time.Duration(math.MaxInt64)
	for i := range found {
		if found[i].SyncedLyrics == "" {
			continue
		}
		if duration <= 0 {
			return &found[i]
		}

		off := time.Duration(math.Abs(found[i].Duration*float64(time.Second) - float64(duration)))
		if off <= lrclibSlack && off < bestOff {
			best, bestOff = &found[i], off
		}
	}
	return best
}

func lrclibFetch(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", lrclibName)
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("lrclib answered %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// lrclibLyrics turns a record into an answer, preferring the timed lines. An
// instrumental is a record with no words at all, and saying so is better than
// searching on and matching somebody else's song.
func lrclibLyrics(rec *lrclibRecord) *ApiResponseLyrics {
	if rec == nil || rec.Instrumental {
		return nil
	}

	if lines := parseLRC(rec.SyncedLyrics); len(lines) > 0 {
		return &ApiResponseLyrics{Synced: true, Provider: "LRCLIB", Lines: lines}
	}

	plain := strings.Split(strings.TrimSpace(rec.PlainLyrics), "\n")
	if len(plain) == 0 || rec.PlainLyrics == "" {
		return nil
	}
	lines := make([]ApiResponseLyric, 0, len(plain))
	for _, words := range plain {
		lines = append(lines, ApiResponseLyric{Words: strings.TrimSpace(words)})
	}
	return &ApiResponseLyrics{Provider: "LRCLIB", Lines: lines}
}

// parseLRC reads the format the timed lines come in: one or more [mm:ss.xx]
// stamps, then the words. A stamp with no words is a pause, and is kept —
// the screen draws it as the gap it is.
//
// Lines with no stamp are the file's own headers ([ar: …], [by: …]) and are
// dropped: they are about the lyric, not part of it.
func parseLRC(text string) []ApiResponseLyric {
	var out []ApiResponseLyric
	for _, line := range strings.Split(text, "\n") {
		rest := strings.TrimSpace(line)

		var stamps []int64
		for strings.HasPrefix(rest, "[") {
			end := strings.Index(rest, "]")
			if end < 0 {
				break
			}
			at, ok := parseLRCStamp(rest[1:end])
			rest = strings.TrimSpace(rest[end+1:])
			if !ok {
				stamps = nil
				break
			}
			stamps = append(stamps, at)
		}

		for _, at := range stamps {
			out = append(out, ApiResponseLyric{At: at, Words: rest})
		}
	}
	return out
}

// parseLRCStamp reads mm:ss, mm:ss.xx or mm:ss.xxx into milliseconds.
func parseLRCStamp(s string) (int64, bool) {
	mins, rest, ok := strings.Cut(s, ":")
	if !ok {
		return 0, false
	}
	m, err := strconv.Atoi(mins)
	if err != nil {
		return 0, false
	}

	secs, frac, hasFrac := strings.Cut(rest, ".")
	sec, err := strconv.Atoi(secs)
	if err != nil {
		return 0, false
	}

	ms := 0
	if hasFrac {
		// Two digits are hundredths and three are thousandths, which is how the
		// format is written in the wild.
		digits, err := strconv.Atoi(frac)
		if err != nil {
			return 0, false
		}
		switch len(frac) {
		case 1:
			ms = digits * 100
		case 2:
			ms = digits * 10
		default:
			ms = digits
		}
	}
	return int64(m)*60_000 + int64(sec)*1_000 + int64(ms), true
}
