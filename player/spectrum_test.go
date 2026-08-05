package player

import (
	"math"
	"math/rand"
	"testing"
)

// pink builds noise that falls at three decibels per octave — the same energy
// in every octave, which is what a mix is measured against and what an analyser
// is calibrated with. Paul Kellett's filter: six one-pole sections whose sum
// holds the slope to a few tenths of a decibel across the audible range.
func pink(seconds int, seed int64) []float32 {
	rng := rand.New(rand.NewSource(seed))
	n := SampleRate * seconds * Channels
	out := make([]float32, n)

	var b0, b1, b2, b3, b4, b5, b6 float64
	for i := 0; i < n; i += Channels {
		white := rng.Float64()*2 - 1
		b0 = 0.99886*b0 + white*0.0555179
		b1 = 0.99332*b1 + white*0.0750759
		b2 = 0.96900*b2 + white*0.1538520
		b3 = 0.86650*b3 + white*0.3104856
		b4 = 0.55000*b4 + white*0.5329522
		b5 = -0.7616*b5 - white*0.0168980
		v := float32((b0 + b1 + b2 + b3 + b4 + b5 + b6 + white*0.5362) * 0.11)
		b6 = white * 0.115926
		for c := range Channels {
			out[i+c] = v
		}
	}
	return out
}

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

// Pink noise is the one signal whose picture is not a matter of taste: it
// carries the same energy in every octave, so an analyser tilted the way this
// one is has to draw it as a line. The tilt, the band spacing and the
// normalisation all show up here — if any of them drifts, the line bends.
func TestSpectrumDrawsPinkNoiseFlat(t *testing.T) {
	s := newSpectrum(SampleRate)
	s.feed(pink(4, 1), Channels)

	bands := s.Bands()

	// The lowest bands are lifted on purpose and are not part of the claim: a
	// kick is meant to stand out, and the line is judged from where the tilt
	// stops being a decision and starts being arithmetic.
	lowest, highest := bands[spectrumLowBands], bands[spectrumLowBands]
	var sum float32
	for i, level := range bands {
		if i >= spectrumLowBands {
			lowest, highest = min(lowest, level), max(highest, level)
			sum += level
		}
		t.Logf("band %2d  %6.0fHz  %.2f", i, spectrumBandHz(i), level)
	}

	mean := sum / float32(len(bands)-spectrumLowBands)
	t.Logf("mean %.2f, from %.2f to %.2f", mean, lowest, highest)

	if spread := highest - lowest; spread > 0.28 {
		t.Errorf("pink noise spreads over %.2f of the height, want a flatter line", spread)
	}
	if mean < 0.3 || mean > 0.9 {
		t.Errorf("pink noise sits at %.2f, want it near the middle of the scale", mean)
	}
}

// drumPattern builds the signal this display exists for: a kick on every beat and a
// hi-hat between them. The two are what somebody listening picks out, so they
// are what the eye has to be able to pick out.
func drumPattern(seconds int) []float32 {
	n := SampleRate * seconds * Channels
	out := make([]float32, n)
	rng := rand.New(rand.NewSource(7))

	step := SampleRate / 4 // 120 beats a minute, hats on the eighths
	for i := 0; i < n; i += Channels {
		at := (i / Channels) % step
		since := float64(at) / float64(SampleRate)

		var v float64
		if (i/Channels)%(step*2) < step {
			// The kick: sixty hertz, gone in a fifth of a second.
			v += 0.9 * math.Sin(2*math.Pi*60*since) * math.Exp(-since*18)
		}
		// The hat: noise through a crude high pass, gone in a twentieth.
		v += 0.25 * (rng.Float64()*2 - 1) * math.Exp(-since*90)

		for c := range Channels {
			out[i+c] = float32(v)
		}
	}
	return out
}

// The whole point of the meter: what the ear picks out, the eye picks out. On a
// kick-and-hat pattern the top of the range has to move about as much as the
// bottom does — the cymbals are quieter than the drum and always will be, but a
// band that barely moves is a wall, and a wall says nothing about the music.
func TestSpectrumMovesAcrossTheRange(t *testing.T) {
	s := newSpectrum(SampleRate)
	signal := drumPattern(6)

	lows := make([]float32, SpectrumBands)
	highs := make([]float32, SpectrumBands)
	for i := range lows {
		lows[i] = 1
	}

	// Fed in windows rather than all at once, so the travel measured is the
	// travel somebody would see frame by frame.
	const chunk = 1024 * Channels
	for at := 0; at+chunk <= len(signal); at += chunk {
		s.feed(signal[at:at+chunk], Channels)
		if at < SampleRate*Channels { // let the envelope find the material
			continue
		}
		for i, level := range s.Bands() {
			lows[i], highs[i] = min(lows[i], level), max(highs[i], level)
		}
	}

	travel := func(from, to int) float32 {
		var sum float32
		for i := from; i < to; i++ {
			sum += highs[i] - lows[i]
		}
		return sum / float32(to-from)
	}

	bottom, top := travel(0, 8), travel(17, SpectrumBands)
	t.Logf("travel: bottom %.2f, top %.2f", bottom, top)
	for i := range lows {
		t.Logf("band %2d  %6.0fHz  %.2f..%.2f", i, spectrumBandHz(i), lows[i], highs[i])
	}

	if top < 0.3 {
		t.Errorf("the top of the range travels %.2f, want it visibly moving", top)
	}
	if top < bottom*0.6 {
		t.Errorf("the top travels %.2f against the bottom's %.2f, want them comparable", top, bottom)
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

// A tone has to read as a level, not as a wall. The transform is unscaled, so
// without normalising by the window every band pinned to the top of the scale
// whatever was playing — which looks exactly like nothing at all.
func TestSpectrumLevelsAreNotPinned(t *testing.T) {
	s := newSpectrum(SampleRate)
	s.feed(tone(400, 1), Channels)

	bands := s.Bands()
	at := loudest(bands)
	if bands[at] < 0.2 {
		t.Errorf("the tone reads %.2f, want it clearly visible", bands[at])
	}

	// Everything away from the tone has to be far below it.
	var pinned int
	for i, v := range bands {
		if i != at && v > bands[at]*0.6 {
			pinned++
		}
	}
	if pinned > 2 {
		t.Errorf("%d bands sit near the tone's level, want a peak rather than a wall", pinned)
	}
}
