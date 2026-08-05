package player

import (
	"math"
	"sync"
)

// Spectrum measures how the energy of what is playing is spread across the
// frequency range.
//
// It runs on the full sample stream rather than the decimated one the waveform
// uses: taking every fifth sample without filtering first folds everything
// above a fifth of the sample rate back down into the audible range, which a
// trace never shows but a spectrum would report as energy that is not there.
type Spectrum struct {
	mu sync.Mutex

	buf  []float64 // the analysis window, filled as samples arrive
	fill int
	win  []float64 // Hann window, precomputed

	bands []float32 // smoothed, 0..1
	edges []int     // bin where each band starts

	// envelope follows the loudest band lately, so the display fills its scale
	// whatever the material. Measured against a live stream, a mix that sounded
	// full used barely a third of the range at a fixed floor.
	envelope float32

	// peaks is the same thing per band: what this part of the range has reached
	// lately, so a cymbal is measured against other cymbals rather than against
	// the kick drum.
	peaks []float32
}

const (
	// spectrumSize is the analysis window. At 44.1kHz this is 46ms and about
	// 21Hz between bins — fine enough to place a bass note, short enough that
	// the display keeps up with the music.
	spectrumSize = 2048

	// SpectrumBands is how many bars come out. Enough to show the shape of a
	// mix without becoming a picket fence on a terminal.
	SpectrumBands = 28

	// The range covered. Below the first there is nothing but rumble, above the
	// last nothing but air, and both would be dead space on screen.
	spectrumLowHz  = 40
	spectrumHighHz = 16000

	// spectrumRangeDb is how much of the music's dynamic range the height
	// stands for, measured down from the loudest band.
	//
	// This is the number that decides whether a meter moves. Mapping a fixed
	// seventy decibels onto the height put every band that was audible at all
	// into the top half and left the difference between a kick and the room it
	// was played in worth two or three cells: measured against Better Off Alone,
	// which is nothing but kick and hats, the lowest band never passed a quarter
	// of its bar. Forty decibels is roughly the range of a mix, so what is loud
	// reaches the top and what is quiet is near the floor.
	spectrumRangeDb = 40

	// spectrumTopRangeDb is the same thing at the top of the range, where it is
	// narrower, because the music is.
	//
	// A kick drum is silence and then a hit, thirty decibels apart. A hi-hat is
	// noise: it rings, it overlaps itself, and between its loudest and its
	// quietest there are ten decibels or so. On a scale wide enough for the kick
	// that is a quarter of the height, which is a band that technically moves and
	// visibly does not. Narrowing the scale with the material is what gives the
	// top of the spectrum the same travel as the bottom, and the numbers here
	// were chosen by replaying recorded band levels through this arithmetic
	// rather than by rebuilding and squinting.
	spectrumTopRangeDb = 22

	// The envelope follows the loudest band, in decibels now rather than in
	// display units: it rises at once and falls slowly, so a quiet passage opens
	// up rather than flattening and a loud one does not clip.
	spectrumEnvFallDb = 0.6

	// spectrumEnvFloorDb stops the gain running away in silence, where the only
	// thing left to amplify is the noise.
	spectrumEnvFloorDb = -55

	// spectrumHeadroom is how much of the scale the loudest band takes, leaving
	// somewhere for something louder to go.
	spectrumHeadroom = 0.98

	// spectrumContrast spreads the bands apart. Above one it pushes the middle
	// down and leaves the peaks where they are, which is what makes a hit read
	// as a hit rather than as a rise.
	spectrumContrast = 1.5

	// Attack fast, release slow: what makes a meter feel like an instrument
	// rather than a graph.
	spectrumAttack  = 0.9
	spectrumRelease = 0.28

	// The range is tilted upwards, because music is not flat and a meter that
	// reports the truth about it looks broken: a mix has most of its energy at
	// the bottom, so an honest spectrum is a slope with the cymbals as a rounding
	// error.
	//
	// Analysers answer this with a slope rather than with a shelf. Pink noise —
	// equal energy in every octave, and what a balanced mix is measured against —
	// falls at three decibels per octave, so a display tilted by the same amount
	// draws it flat; mastering analysers offer between three and four and a half
	// of them, the steeper end for how much low end a modern production carries.
	// Three won here on recorded material: the steeper tilts put the top of the
	// range up against the ceiling, where a band cannot show a hit because it is
	// already at the top. A shelf, which is what this used to be, does the same
	// job to the whole top of the range at once, and a step in the tilt reads as
	// a wall on screen.
	spectrumSlopeDb     = 3.0
	spectrumSlopeFromHz = 160

	// The bottom keeps a lift of its own. The slope leaves it alone by
	// construction, and the lowest bands still need it: what is felt in a kick is
	// below where a small speaker even starts.
	spectrumLowLiftDb = 8
	spectrumLowBands  = 5

	// How much of a band's scale comes from its own recent peak rather than from
	// the loudest band in the mix, at the bottom of the range and at the top.
	//
	// One envelope for the whole spectrum is set by the kick, and beside a kick a
	// hi-hat is nothing: the top of the range then sits wherever the tilt put it
	// and barely moves, which is a wall rather than a meter. Giving a band a
	// share of its own headroom is what makes a cymbal read as loud for a cymbal.
	//
	// The share grows with frequency because that is where the problem is. The
	// bottom of a mix is what the mix is measured by and can be shown against the
	// whole of it; by the top the difference between one cymbal and another is
	// the only difference there is, and a band that keeps a quarter of its scale
	// tied to a kick drum thirty decibels away has thrown away a quarter of its
	// height before it starts.
	spectrumBandShareLow  = 0.4
	spectrumBandShareHigh = 0.85

	// The per-band peak falls slower than the overall one — a band that has gone
	// quiet should sink rather than quietly turn its own gain up — and it is not
	// allowed to fall far below the mix, so a band with nothing in it amplifies
	// nothing.
	spectrumBandFallDb  = 0.3
	spectrumBandFloorDb = 28
)

func newSpectrum(sampleRate int) *Spectrum {
	s := &Spectrum{
		buf:   make([]float64, spectrumSize),
		win:   make([]float64, spectrumSize),
		bands: make([]float32, SpectrumBands),
		peaks: make([]float32, SpectrumBands),
		edges: make([]int, SpectrumBands+1),
	}

	// Hann, so a tone that does not fit a whole number of cycles in the window
	// does not smear across every bin.
	for i := range s.win {
		s.win[i] = 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(spectrumSize-1))
	}

	s.rest()

	// Bands spaced by octave, not by hertz: an octave is what the ear hears as
	// an equal step, and a linear axis would crowd the whole of music into the
	// left tenth of the screen.
	binHz := float64(sampleRate) / float64(spectrumSize)
	for i := range s.edges {
		f := spectrumLowHz * math.Pow(spectrumHighHz/spectrumLowHz, float64(i)/float64(SpectrumBands))
		s.edges[i] = min(max(int(f/binHz), 1), spectrumSize/2-1)
	}
	return s
}

// feed takes interleaved samples straight from the output path.
func (s *Spectrum) feed(samples []float32, channels int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := 0; i < len(samples); i += channels {
		s.buf[s.fill] = float64(samples[i])
		s.fill++
		if s.fill == len(s.buf) {
			s.analyse()
			// Half the window is kept, so successive frames overlap and a
			// transient is never missed between them.
			copy(s.buf, s.buf[len(s.buf)/2:])
			s.fill = len(s.buf) / 2
		}
	}
}

// analyse turns the window into band levels. Callers must hold s.mu.
func (s *Spectrum) analyse() {
	re := make([]float64, spectrumSize)
	im := make([]float64, spectrumSize)
	for i := range re {
		re[i] = s.buf[i] * s.win[i]
	}
	fft(re, im)

	// The loudest bin in a band rather than the average of them.
	//
	// A band near the top covers dozens of bins, and a cymbal is a few of them:
	// averaging spreads that hit across everything it did not touch and leaves
	// a number that barely moves. What a meter is asked is how loud this part
	// of the range got, and the answer is the loudest thing in it.
	loudest := make([]float64, len(s.bands))
	for b := range s.bands {
		lo, hi := s.edges[b], s.edges[b+1]
		if hi <= lo {
			hi = lo + 1
		}

		var power float64
		for i := lo; i < hi && i < len(re)/2; i++ {
			power = max(power, re[i]*re[i]+im[i]*im[i])
		}

		// Normalised by the window: the transform is unscaled, so without this
		// a bin's magnitude grows with the analysis size and every band pins
		// to the top of the scale whatever is playing.
		power /= float64(spectrumSize) * float64(spectrumSize)

		loudest[b] = 10*math.Log10(power+1e-12) + spectrumTiltDb(b)
	}

	// The envelope is where the top of the scale sits, and it is in decibels:
	// dividing display units by each other, which is what this used to do,
	// compares two numbers that have already been squeezed onto a height and
	// leaves everything bunched together near the top.
	top := loudest[0]
	for _, db := range loudest {
		top = max(top, db)
	}
	s.envelope = float32(max(max(top, float64(s.envelope)-spectrumEnvFallDb), spectrumEnvFloorDb))

	for b := range s.bands {
		// Where the top of this band's scale sits: partly the loudest thing in
		// the mix, partly the loudest this band itself has been. The first keeps
		// the picture honest about which part of the range carries the music, the
		// second is what lets the quiet parts of it move at all.
		peak := max(loudest[b], float64(s.peaks[b])-spectrumBandFallDb)
		peak = max(peak, float64(s.envelope)-spectrumBandFloorDb)
		s.peaks[b] = float32(peak)

		share := spectrumShareAt(b)
		ref := (1-share)*float64(s.envelope) + share*peak

		span := spectrumRangeAt(b)
		level := (loudest[b] - (ref - span)) / span
		level = min(max(level, 0), 1)
		level = math.Pow(level, spectrumContrast) * spectrumHeadroom

		rate := spectrumRelease
		if float32(level) > s.bands[b] {
			rate = spectrumAttack
		}
		s.bands[b] += (float32(level) - s.bands[b]) * float32(rate)
	}
}

// spectrumTiltDb is the display slope: how much a band is lifted for where it
// sits in the range. See the constants — it is deliberately not the truth about
// the signal, it is what makes a mix read as level rather than as a ramp.
func spectrumTiltDb(band int) float64 {
	tilt := 0.0
	if hz := spectrumBandHz(band); hz > spectrumSlopeFromHz {
		tilt = spectrumSlopeDb * math.Log2(hz/spectrumSlopeFromHz)
	}
	if band < spectrumLowBands {
		tilt += spectrumLowLiftDb * float64(spectrumLowBands-band) / spectrumLowBands
	}
	return tilt
}

// spectrumShareAt is how much of a band's scale is its own, growing with
// frequency. See the constants.
func spectrumShareAt(band int) float64 {
	at := float64(band) / float64(SpectrumBands-1)
	return spectrumBandShareLow + (spectrumBandShareHigh-spectrumBandShareLow)*at
}

// spectrumRangeAt is how many decibels a band's height stands for, narrowing
// with frequency because the music does. See spectrumTopRangeDb.
func spectrumRangeAt(band int) float64 {
	at := float64(band) / float64(SpectrumBands-1)
	return spectrumRangeDb + (spectrumTopRangeDb-spectrumRangeDb)*at
}

// spectrumBandHz is the middle of a band, which is where its tilt is taken.
func spectrumBandHz(band int) float64 {
	step := (float64(band) + 0.5) / float64(SpectrumBands)
	return spectrumLowHz * math.Pow(spectrumHighHz/spectrumLowHz, step)
}

// rest puts every band's peak at the quietest the display goes, so the first
// seconds of a track are measured rather than spent falling from silence.
// Callers must hold s.mu, or hold the spectrum alone.
func (s *Spectrum) rest() {
	for i := range s.peaks {
		s.peaks[i] = spectrumEnvFloorDb
	}
	s.envelope = spectrumEnvFloorDb
}

// Bands is the current spectrum, lowest frequency first, each 0..1.
func (s *Spectrum) Bands() []float32 {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]float32, len(s.bands))
	copy(out, s.bands)
	return out
}

// Reset forgets what has been heard, for when the track changes.
func (s *Spectrum) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.fill = 0
	clear(s.bands)
	s.rest()
}

// fft transforms in place, radix-2 Cooley-Tukey. The standard library has no
// transform and the one thing needed here is eighty lines, which is cheaper
// than a dependency.
func fft(re, im []float64) {
	n := len(re)

	// Bit-reversal permutation.
	for i, j := 1, 0; i < n; i++ {
		bit := n >> 1
		for ; j&bit != 0; bit >>= 1 {
			j ^= bit
		}
		j |= bit
		if i < j {
			re[i], re[j] = re[j], re[i]
			im[i], im[j] = im[j], im[i]
		}
	}

	for size := 2; size <= n; size <<= 1 {
		angle := -2 * math.Pi / float64(size)
		wr, wi := math.Cos(angle), math.Sin(angle)
		for start := 0; start < n; start += size {
			cr, ci := 1.0, 0.0
			for k := 0; k < size/2; k++ {
				i, j := start+k, start+k+size/2
				tr := re[j]*cr - im[j]*ci
				ti := re[j]*ci + im[j]*cr
				re[j], im[j] = re[i]-tr, im[i]-ti
				re[i], im[i] = re[i]+tr, im[i]+ti
				cr, ci = cr*wr-ci*wi, cr*wi+ci*wr
			}
		}
	}
}
