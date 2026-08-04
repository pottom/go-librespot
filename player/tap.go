package player

import (
	"sync"

	librespot "github.com/devgianlu/go-librespot"
)

// TapSamples is how many samples a tapped frame carries.
//
// It is twice what a display needs, and the surplus is the point: a controller
// drawing a waveform has to start each frame at the same place in the wave, or
// the picture shimmers. The extra samples are the slack it searches for that
// point in.
const TapSamples = 512

// TapWindow is how much of a frame is meant to be drawn. The rest is trigger
// slack.
const TapWindow = 256

// tap copies the samples on their way to the audio device, so that something
// outside the player can see what is being heard.
//
// It sits after mixing, normalisation and crossfade but before the output's own
// volume, which is what a listener would expect: turning the volume down should
// not flatten a waveform display.
//
// Reads happen on the audio thread, so the copy has to be cheap and must never
// block: a slow reader loses frames rather than stalling playback.
type tap struct {
	inner librespot.Float32Reader

	// tempo listens to the same samples. It wants every one of them, not the
	// decimated few the waveform keeps, so it is fed separately.
	tempo    *Tempo
	spectrum *Spectrum
	channels int

	mu    sync.Mutex
	frame []float32 // the most recent frame, mono
	fill  int       // how far into frame the next sample goes
	skip  int       // samples dropped to reach the frame rate
	every int       // one sample kept out of every this many
}

// newTap wraps a reader. rate is the source sample rate and fps how often a
// frame should be complete; together they set how much is thrown away.
func newTap(inner librespot.Float32Reader, rate, channels, fps int) *tap {
	// A drawn window should span roughly one display frame's worth of audio, so
	// the trace moves at the speed of the music rather than racing ahead of it.
	span := rate / fps
	every := max(span/TapWindow, 1)

	return &tap{
		inner:    inner,
		tempo:    newTempo(),
		spectrum: newSpectrum(rate),
		channels: channels,
		frame:    make([]float32, TapSamples),
		every:    every * channels,
	}
}

func (t *tap) Read(p []float32) (int, error) {
	n, err := t.inner.Read(p)
	if n > 0 {
		t.absorb(p[:n])
		t.tempo.feed(p[:n], t.channels)
		t.spectrum.feed(p[:n], t.channels)
	}
	return n, err
}

// absorb takes every nth sample, downmixing the channels it lands on.
func (t *tap) absorb(samples []float32) {
	t.mu.Lock()
	defer t.mu.Unlock()

	for i := 0; i < len(samples); i++ {
		if t.skip > 0 {
			t.skip--
			continue
		}
		t.skip = t.every - 1

		t.frame[t.fill] = samples[i]
		t.fill++
		if t.fill == len(t.frame) {
			t.fill = 0
		}
	}
}

// Frame is a copy of the most recent samples, oldest first. It is safe to call
// from any goroutine, and returns nil before anything has played.
func (t *tap) Frame() []float32 {
	t.mu.Lock()
	defer t.mu.Unlock()

	out := make([]float32, len(t.frame))
	// The buffer is circular, so unwrap it: the write head is the oldest sample.
	copy(out, t.frame[t.fill:])
	copy(out[len(t.frame)-t.fill:], t.frame[:t.fill])
	return out
}

// tapFrameRate is how often a frame is completed, matched to what a display can
// show. Faster would only produce frames nobody draws.
const tapFrameRate = 30

// Waveform is the most recent samples heard, oldest first, or nil when nothing
// has played yet. Safe to call from any goroutine.
func (p *Player) Waveform() []float32 {
	t := p.tap.Load()
	if t == nil {
		return nil
	}
	return t.Frame()
}

// Tempo is the measured beat rate of what is playing, or zero while the
// analyser is still listening or the recording has no steady beat.
func (p *Player) Tempo() float64 {
	t := p.tap.Load()
	if t == nil {
		return 0
	}
	bpm, _ := t.tempo.Result()
	return bpm
}

// Spectrum is how the energy of what is playing is spread across the frequency
// range, lowest first, each 0..1. Nil when nothing has played yet.
func (p *Player) Spectrum() []float32 {
	t := p.tap.Load()
	if t == nil {
		return nil
	}
	return t.spectrum.Bands()
}

// ResetTempo forgets what has been heard, for when the track changes. Without
// it the previous track's beat lingers for as long as the analysis window is
// deep.
func (p *Player) ResetTempo() {
	if t := p.tap.Load(); t != nil {
		t.tempo.Reset()
		t.spectrum.Reset()
	}
}
