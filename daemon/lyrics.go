package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
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

	resp, err := p.sess.Spclient().Request(ctx, "GET",
		"/color-lyrics/v2/track/"+trackId,
		url.Values{"format": []string{"json"}, "market": []string{"from_token"}},
		http.Header{"app-platform": []string{"WebPlayer"}},
		nil)
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
