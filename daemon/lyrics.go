package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// lyricsEndpoint is where Spotify's own client reads them from. It is not a
// documented API and may move; a failure here costs the words, nothing else.
const (
	lyricsEndpoint  = "https://spclient.wg.spotify.com/color-lyrics/v2/track/"
	lyricsUserAgent = "spindle"
)

// ApiResponseLyrics is the words of a track against the clock.
//
// Spotify has no public endpoint for these. The one its own client uses is
// reachable with the session already open here, and it is keyed by track id, so
// there is no guessing which song the words belong to — matching by title and
// artist finds a different song of the same name often enough to be worse than
// showing nothing.
type ApiResponseLyrics struct {
	// Synced says whether the lines carry timings. Without them there is a
	// lyric but nothing to follow along with.
	Synced   bool               `json:"synced"`
	Language string             `json:"language"`
	Provider string             `json:"provider"`
	Lines    []ApiResponseLyric `json:"lines"`
}

type ApiResponseLyric struct {
	// At is when the line is sung, in milliseconds from the start of the track.
	At    int64  `json:"at"`
	Words string `json:"words"`
}

// lyricsResponse is the shape the internal endpoint answers with.
type lyricsResponse struct {
	Lyrics struct {
		SyncType string `json:"syncType"`
		Language string `json:"language"`
		Provider string `json:"providerDisplayName"`
		Lines    []struct {
			StartTimeMs string `json:"startTimeMs"`
			Words       string `json:"words"`
		} `json:"lines"`
	} `json:"lyrics"`
}

// lyricsFor fetches the words for a track, or nil when there are none. A track
// without lyrics is the ordinary case, not a failure.
func (p *AppPlayer) lyricsFor(ctx context.Context, trackId string) (*ApiResponseLyrics, error) {
	if trackId == "" {
		return nil, nil
	}

	// Asked for directly rather than through the Spclient helper: that signs
	// requests with a token this endpoint refuses, while the access token the
	// session hands out is the one it accepts.
	token, err := p.sess.Spclient().GetAccessToken(ctx, false)
	if err != nil {
		return nil, fmt.Errorf("failed getting token for lyrics: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		lyricsEndpoint+trackId+"?format=json&market=from_token", nil)
	if err != nil {
		return nil, fmt.Errorf("failed building lyrics request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	// Without this the endpoint answers as though the client were not one that
	// shows lyrics at all.
	req.Header.Set("app-platform", "WebPlayer")
	req.Header.Set("Accept", "application/json")
	// Measured: the endpoint answers 403 to Go's default user agent and 200 to
	// anything else, this one included. Naming ourselves is both the fix and
	// the honest thing to send.
	req.Header.Set("User-Agent", lyricsUserAgent)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed fetching lyrics: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("invalid status code from lyrics: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed reading lyrics: %w", err)
	}

	var raw lyricsResponse
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("failed unmarshalling lyrics: %w", err)
	}

	out := &ApiResponseLyrics{
		Synced:   raw.Lyrics.SyncType == "LINE_SYNCED",
		Language: raw.Lyrics.Language,
		Provider: raw.Lyrics.Provider,
		Lines:    make([]ApiResponseLyric, 0, len(raw.Lyrics.Lines)),
	}
	for _, line := range raw.Lyrics.Lines {
		var at int64
		_, _ = fmt.Sscanf(line.StartTimeMs, "%d", &at)
		out.Lines = append(out.Lines, ApiResponseLyric{At: at, Words: line.Words})
	}
	return out, nil
}
