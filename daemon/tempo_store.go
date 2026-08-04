package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// tempoStore remembers the beat rate of tracks that have been played.
//
// A tempo can only be measured while a track is sounding, so this is the only
// way a controller can show one for a track further down the queue: it has to
// have been heard before. What has not been heard stays blank, which is honest.
type tempoStore struct {
	mu   sync.RWMutex
	path string
	bpm  map[string]float64

	dirty bool
}

// tempoStoreLimit caps the file. Past this the oldest entries are not worth the
// bytes; a library is large but the tracks anyone replays are not.
const tempoStoreLimit = 20000

func newTempoStore(dir string) *tempoStore {
	s := &tempoStore{bpm: map[string]float64{}}
	if dir == "" {
		return s
	}
	s.path = filepath.Join(dir, "tempo.json")

	data, err := os.ReadFile(s.path)
	if err != nil {
		return s
	}
	// A file that cannot be read is treated as empty: the worst that happens is
	// tempos are measured again.
	_ = json.Unmarshal(data, &s.bpm)
	return s
}

// Get is the remembered tempo of a track, or zero.
func (s *tempoStore) Get(uri string) float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.bpm[uri]
}

// Put records a measurement. Writing is left to Flush so that a track playing
// does not touch the disk every time a client asks for the status.
func (s *tempoStore) Put(uri string, bpm float64) {
	if uri == "" || bpm <= 0 {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.bpm[uri] == bpm {
		return
	}
	if len(s.bpm) >= tempoStoreLimit {
		// Nothing clever: the point of the file is that most lookups hit, and a
		// library that large has already had its money's worth.
		clear(s.bpm)
	}
	s.bpm[uri] = bpm
	s.dirty = true
}

// Flush writes the file if anything changed. Failing to write is not worth
// reporting: the tempos will simply be measured again next time.
func (s *tempoStore) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.dirty || s.path == "" {
		return
	}
	s.dirty = false

	data, err := json.Marshal(s.bpm)
	if err != nil {
		return
	}

	// Written beside and renamed, so an interrupted write cannot leave a file
	// that parses as half a library.
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, s.path)
}

// rememberTempo records what has been measured for the track playing, and
// returns it. Recording happens wherever the tempo is asked for, because that
// is where it is known to be current — the analyser has no idea which track it
// is listening to.
func (p *AppPlayer) rememberTempo() float64 {
	bpm := p.player.Tempo()
	if bpm <= 0 || p.state.player.Track == nil {
		return bpm
	}

	p.app.tempos.Put(p.state.player.Track.Uri, bpm)
	p.app.tempos.Flush()
	return bpm
}
