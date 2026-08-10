package player

import (
	"sync"
	"time"

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
	chroma   *Chroma
	channels int

	mu    sync.Mutex
	frame []float32 // the most recent frame, mono
	fill  int       // how far into frame the next sample goes
	every int       // one sample kept out of every this many

	// The stream is averaged before it is thinned out, and these are what that
	// takes: a window of the samples not yet used, and the weights to use them
	// with. Taking every nth sample and throwing the rest away — which is what
	// this did — is decimation without a filter, and everything above half the
	// rate that leaves folds back down into the range that is drawn. On this
	// path that is every cymbal and every breath of tape hiss, arriving on the
	// trace as a wave that was never played.
	window  []float32
	weights []float32
	due     int // source samples until the next one is kept
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
		chroma:   newChroma(rate),
		channels: channels,
		frame:    make([]float32, TapSamples),
		every:    every,
		window:   make([]float32, 2*every),
		weights:  triangle(2 * every),
	}
}

// triangle is the window the samples are averaged through: a Bartlett window
// twice as long as the gap between the samples that are kept.
//
// Measured against tones at 44.1kHz, keeping one sample in five: what is drawn
// — everything up to about two kilohertz, which is where the shape of a wave
// lives — comes through untouched, while six kilohertz is down twenty-four
// decibels and nine is down fifty. Taking every fifth sample instead lets all
// of them through at full strength, folded down on top of the music.
func triangle(n int) []float32 {
	out := make([]float32, n)
	var total float32
	for i := range out {
		out[i] = float32(1 + min(i, n-1-i))
		total += out[i]
	}
	for i := range out {
		out[i] /= total
	}
	return out
}

func (t *tap) Read(p []float32) (int, error) {
	n, err := t.inner.Read(p)
	if n > 0 {
		t.absorb(p[:n])
		t.tempo.feed(p[:n], t.channels)
		t.spectrum.feed(p[:n], t.channels)
		t.chroma.feed(p[:n], t.channels)
	}
	return n, err
}

// absorb downmixes the channels and keeps one averaged sample in every few.
func (t *tap) absorb(samples []float32) {
	t.mu.Lock()
	defer t.mu.Unlock()

	for i := 0; i+t.channels <= len(samples); i += t.channels {
		var sum float32
		for c := range t.channels {
			sum += samples[i+c]
		}

		copy(t.window, t.window[1:])
		t.window[len(t.window)-1] = sum / float32(t.channels)

		if t.due > 0 {
			t.due--
			continue
		}
		t.due = t.every - 1

		// The window's own average, which is the filtering and the thinning in
		// one step: what is kept is what the samples around it were doing, not
		// whichever one the count happened to land on.
		var mixed float32
		for j, w := range t.weights {
			mixed += t.window[j] * w
		}

		t.frame[t.fill] = mixed
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

// Beat is how far apart the beats of what is playing are, and how long ago the
// last one was, or nothing while the analyser is still listening.
//
// In the same time as the spectrum, which is the whole point. What was tried
// first was to report it in the time it will be *heard*: the audio the analyser
// is given has not come out of the device yet — three buffers deep — so a beat
// just found is still ahead of the listener by about seventy milliseconds, and
// taking that off makes the beat land with the sound.
//
// It is the wrong answer, and it was measured to be. The spectrum handed out
// beside it carries no such correction, so what a picture drawn from both got
// was a meter jumping at one moment and a beat marked seventy milliseconds
// later — and an eye compares one part of a picture with another far more
// sharply than it compares a picture with a sound. Measured over three records
// played through the interface at thirty frames a second, the rises in the
// spectrum sat at 0.88 of a beat rather than on it, and correcting for the
// buffer put them exactly on it: the whole error was this, and the analyser
// itself was on the money.
//
// So the picture agrees with itself and leads the sound by the depth of the
// buffer. The other way to have both would be to hold the spectrum back by the
// same amount and match the ear as well — worth doing the day this picture is
// being judged against a room rather than against itself.
func (p *Player) Beat() (period, since time.Duration, confidence float64) {
	t := p.tap.Load()
	if t == nil {
		return 0, 0, 0
	}
	return t.tempo.Beat()
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

// Loudness is where the top of the spectrum's own scale sits, in decibels, so
// that a caller can tell a build from a lull — which the bands cannot say,
// because they are measured against it. Nought when nothing has played yet.
func (p *Player) Loudness() float32 {
	t := p.tap.Load()
	if t == nil {
		return 0
	}
	return t.spectrum.Loudness()
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

// Watch is what the tempo analyser has before the gates Beat reports through.
// See Tempo.Watch — it is there to be recorded, not to be drawn.
func (p *Player) Watch() (bpm, confidence float64, agreed int, period float64) {
	t := p.tap.Load()
	if t == nil {
		return 0, 0, 0, 0
	}
	return t.tempo.Watch()
}

// Notes is which of the twelve pitch classes are sounding, C first, each 0..1.
// Nil until enough has been heard. See Chroma.
func (p *Player) Notes() []float32 {
	t := p.tap.Load()
	if t == nil {
		return nil
	}
	return t.chroma.Notes()
}
