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
	spectrumContrast = 1.25

	// Attack fast, release slow: what makes a meter feel like an instrument
	// rather than a graph.
	spectrumAttack  = 0.9
	spectrumRelease = 0.28

	// The ends of the range are lifted, because music is not flat and a meter
	// that reports the truth about it looks broken: a mix has most of its energy
	// in the middle, so an honest spectrum is a hill with the kick and the
	// cymbals as foothills. Both ends are raised until what the ear picks out —
	// the beat underneath and the hats on top — is what the eye picks out too.
	// The tilt is fixed and cosmetic, and it is the only cosmetic thing here.
	spectrumLowLiftDb  = 11
	spectrumLowBands   = 6
	spectrumHighLiftDb = 14
	spectrumHighFrom   = 17
)

func newSpectrum(sampleRate int) *Spectrum {
	s := &Spectrum{
		buf:   make([]float64, spectrumSize),
		win:   make([]float64, spectrumSize),
		bands: make([]float32, SpectrumBands),
		edges: make([]int, SpectrumBands+1),
	}

	// Hann, so a tone that does not fit a whole number of cycles in the window
	// does not smear across every bin.
	for i := range s.win {
		s.win[i] = 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(spectrumSize-1))
	}

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

	floor := float64(s.envelope) - spectrumRangeDb
	for b := range s.bands {
		level := (loudest[b] - floor) / spectrumRangeDb
		level = min(max(level, 0), 1)
		level = math.Pow(level, spectrumContrast) * spectrumHeadroom

		rate := spectrumRelease
		if float32(level) > s.bands[b] {
			rate = spectrumAttack
		}
		s.bands[b] += (float32(level) - s.bands[b]) * float32(rate)
	}
}

// spectrumTiltDb lifts the ends of the range. See the constants: it is what
// makes the beat and the cymbals read on a screen, and it is deliberately not
// the truth about the signal.
func spectrumTiltDb(band int) float64 {
	if band < spectrumLowBands {
		return spectrumLowLiftDb * float64(spectrumLowBands-band) / spectrumLowBands
	}
	if band >= spectrumHighFrom {
		span := float64(SpectrumBands - spectrumHighFrom)
		return spectrumHighLiftDb * float64(band-spectrumHighFrom+1) / span
	}
	return 0
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
