package player

import (
	"math"
	"testing"
)

// tone builds a steady sine at a given frequency.
func tone(hz float64, seconds int) []float32 {
	n := SampleRate * seconds * Channels
	out := make([]float32, n)
	for i := 0; i < n; i += Channels {
		v := float32(0.6 * math.Sin(2*math.Pi*hz*float64(i/Channels)/float64(SampleRate)))
		for c := range Channels {
			out[i+c] = v
		}
	}
	return out
}

// loudest is the band with the most energy.
func loudest(bands []float32) int {
	at := 0
	for i, v := range bands {
		if v > bands[at] {
			at = i
		}
	}
	return at
}

// A tone has to land in the band that covers it, and a higher tone in a higher
// band: bass on the left, treble on the right, as every analyser reads.
func TestSpectrumPlacesTones(t *testing.T) {
	var last = -1
	for _, hz := range []float64{80, 400, 2000, 8000} {
		s := newSpectrum(SampleRate)
		s.feed(tone(hz, 1), Channels)

		at := loudest(s.Bands())
		if at <= last {
			t.Errorf("%.0fHz landed in band %d, no higher than the %.0fHz one", hz, at, hz/5)
		}
		last = at
	}
}

// The bands are spaced by octave, so each covers about the same musical
// distance rather than the same number of hertz.
func TestSpectrumBandsAreOctaveSpaced(t *testing.T) {
	s := newSpectrum(SampleRate)
	binHz := float64(SampleRate) / float64(spectrumSize)

	var ratios []float64
	for i := 1; i < len(s.edges); i++ {
		lo, hi := float64(s.edges[i-1])*binHz, float64(s.edges[i])*binHz
		if lo > spectrumLowHz*2 && hi > lo {
			ratios = append(ratios, hi/lo)
		}
	}
	if len(ratios) < 10 {
		t.Fatal("not enough bands to judge the spacing")
	}
	for _, r := range ratios {
		if r < 1.15 || r > 1.45 {
			t.Errorf("a band spans a ratio of %.2f, want them evenly spaced in octaves", r)
		}
	}
}

// Silence reads as silence rather than as a floor of noise.
func TestSpectrumIsQuietInSilence(t *testing.T) {
	s := newSpectrum(SampleRate)
	s.feed(make([]float32, SampleRate*Channels), Channels)

	for i, v := range s.Bands() {
		if v > 0.05 {
			t.Errorf("band %d reads %.2f in silence", i, v)
		}
	}
}

// The transform has to be right, or every band is quietly wrong. A single bin's
// worth of sine must come back as a single spike.
func TestFFTFindsASingleBin(t *testing.T) {
	const n, bin = 64, 7
	re, im := make([]float64, n), make([]float64, n)
	for i := range re {
		re[i] = math.Cos(2 * math.Pi * bin * float64(i) / n)
	}
	fft(re, im)

	for i := 0; i <= n/2; i++ {
		mag := math.Hypot(re[i], im[i])
		want := 0.0
		if i == bin {
			want = n / 2
		}
		if math.Abs(mag-want) > 1e-6 {
			t.Errorf("bin %d has magnitude %.6f, want %.1f", i, mag, want)
		}
	}
}
