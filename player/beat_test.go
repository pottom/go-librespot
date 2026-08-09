//go:build test_unit

package player

import (
	"math"
	"math/rand"
	"testing"
	"time"
)

// hiss is a recording with no beat in it at all.
func hiss(seconds int) []float32 {
	out := make([]float32, SampleRate*seconds*Channels)
	rng := rand.New(rand.NewSource(2))
	for i := range out {
		out[i] = float32((rng.Float64() - 0.5) * 0.4)
	}
	return out
}

// The analyser says when the beats are, not only how often.
//
// The period is half of keeping time; the other half is where the beats fall.
// A picture given only the period can move at the right rate and still be on
// the off-beat all night.
func TestTheBeatIsPlaced(t *testing.T) {
	for _, bpm := range []float64{90, 120, 128, 174} {
		tempo := newTempo()
		tempo.feed(beat(bpm, 30), Channels)

		period, since, confidence := tempo.Beat()
		if period == 0 {
			t.Errorf("%.0f bpm: no beat placed, confidence %.2f", bpm, confidence)
			continue
		}

		apart := time.Duration(60.0 / bpm * float64(time.Second))
		if off := math.Abs(float64(period-apart)) / float64(apart); off > 0.03 {
			t.Errorf("%.0f bpm: the beats are %s apart, want %s", bpm, period, apart)
		}

		// Where the kick is: the material is fed for a whole number of seconds
		// and the kick is on the beat, so the last one falls a known fraction
		// of a period before the end.
		beats := 30 / (60.0 / bpm)
		want := math.Mod(beats, 1)
		if want == 0 {
			want = 1
		}

		off := float64(since)/float64(period) - want
		if off < -0.5 {
			off += 1
		}
		t.Logf("%.0f bpm: beats %s apart, the last one %s ago — %.3f of a period from the kick",
			bpm, period.Round(time.Millisecond), since.Round(time.Millisecond), off)

		// Measured on this material: the beat comes back a hundredth to a tenth
		// of a period late, because the energy rise it is found from peaks
		// after the transient that caused it rather than on it. Fifteen
		// milliseconds at 120, thirty at 174 — under what an eye can catch, and
		// under what the audio output's own buffer will add on top.
		if math.Abs(off) > 0.12 {
			t.Errorf("%.0f bpm: the last beat is %.3f of a period from where the kick is", bpm, off)
		}
	}
}

// And it says nothing about a recording that has no beat, rather than inventing
// one to keep time with.
func TestNoBeatIsPlacedWithoutOne(t *testing.T) {
	tempo := newTempo()
	tempo.feed(hiss(30), Channels)

	if period, since, confidence := tempo.Beat(); period != 0 {
		t.Errorf("noise was given beats %s apart (%s ago), confidence %.2f", period, since, confidence)
	}
}
