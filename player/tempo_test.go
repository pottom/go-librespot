package player

import (
	"math"
	"math/rand"
	"testing"
)

// beat builds a signal with a kick on every beat, which is what the analyser is
// meant to find.
func beat(bpm float64, seconds int) []float32 {
	n := SampleRate * seconds * Channels
	out := make([]float32, n)
	period := 60.0 / bpm * float64(SampleRate)

	rng := rand.New(rand.NewSource(1))
	for i := 0; i < n; i += Channels {
		frame := float64(i / Channels)
		phase := math.Mod(frame, period) / period

		// A kick: a short burst that decays fast.
		env := math.Exp(-phase * period / (float64(SampleRate) * 0.06))
		v := math.Sin(2*math.Pi*55*frame/float64(SampleRate)) * env * 0.7

		// Something continuous underneath, so the energy is never zero.
		v += math.Sin(2*math.Pi*220*frame/float64(SampleRate)) * 0.12
		v += (rng.Float64() - 0.5) * 0.02

		for c := range Channels {
			out[i+c] = float32(v)
		}
	}
	return out
}

// The whole point: a track with a steady beat gets a number, and it is the
// right one. Spotify would have told us this through an endpoint that is closed
// to us, so it is measured from the audio instead.
func TestTempoFindsTheBeat(t *testing.T) {
	for _, want := range []float64{90, 120, 128, 174} {
		analyser := newTempo()
		analyser.feed(beat(want, 20), Channels)

		got, confidence := analyser.Result()
		if got == 0 {
			t.Errorf("%.0f bpm: nothing reported, confidence %.2f", want, confidence)
			continue
		}
		if math.Abs(got-want) > 2 {
			t.Errorf("%.0f bpm: measured %.1f (confidence %.2f)", want, got, confidence)
		}
	}
}

// Noise has no beat, and saying so is the useful answer. A number invented for
// a recording that has no tempo is worse than a blank.
func TestTempoSaysNothingWithoutABeat(t *testing.T) {
	analyser := newTempo()

	n := SampleRate * 20 * Channels
	noise := make([]float32, n)
	rng := rand.New(rand.NewSource(2))
	for i := range noise {
		noise[i] = float32((rng.Float64() - 0.5) * 0.4)
	}
	analyser.feed(noise, Channels)

	if got, confidence := analyser.Result(); got != 0 {
		t.Errorf("measured %.1f bpm from noise, confidence %.2f", got, confidence)
	}
}

// A track change has to wipe the history, or the old tempo lingers into the new
// track for as long as the window is deep.
func TestTempoResetForgets(t *testing.T) {
	analyser := newTempo()
	analyser.feed(beat(128, 20), Channels)
	if got, _ := analyser.Result(); got == 0 {
		t.Fatal("nothing to forget")
	}

	analyser.Reset()
	if got, _ := analyser.Result(); got != 0 {
		t.Errorf("still reporting %.1f bpm after a reset", got)
	}
}
