package player

import (
	"math"
	"sync"
)

// Which notes are sounding, folded into the twelve of an octave.
//
// The spectrum this daemon already computes cannot answer it. Its window is
// 2048 points, which at 44.1kHz is 21.5Hz between bins, and a semitone at
// middle C is 15.6Hz — under one bin, so two neighbouring notes are one number
// until about 361Hz. That is fine for a meter, which is asked how loud a part
// of the range got, and useless for a keyboard, which is asked which key.
//
// So this is a second analysis over the same samples with a longer window:
// 8192 points is 5.4Hz a bin and semitones separate from about 180Hz, which is
// most of where a tune lives. It costs 186ms of audio, so the answer is about
// 90ms behind the sound — a fifth of a beat at 128, and nothing that draws to
// it is trying to be a metronome.
//
// What it does not claim: to know the tune. Reading a melody out of a finished
// mix is polyphonic transcription, and a guess that is right most of the time
// is worse than none — the times it is wrong are the times somebody is looking
// straight at it. This says only which notes are sounding, which is what a
// keyboard shows.
const (
	// chromaSize is the window, in samples.
	chromaSize = 8192

	// chromaHop is how far the window moves each time: a quarter of itself, so
	// the answer arrives about twenty times a second and a chord change is
	// never sat on the boundary between two windows.
	chromaHop = chromaSize / 4

	// chromaLowHz and chromaHighHz are the range folded in.
	//
	// Below the low end the bins are too far apart to tell a semitone from its
	// neighbour even at this window, and a bass note is a poor guide to the
	// harmony anyway — it is as often the fifth or the third as the root. Above
	// the high end almost everything is a harmonic of something lower, and
	// harmonics of a note are mostly not that note: the third harmonic is a
	// twelfth up, which is a different pitch class entirely.
	chromaLowHz  = 110.0
	chromaHighHz = 1760.0

	// chromaFloor is how much of the strongest a class must reach to count at
	// all, so that silence does not come out as twelve equal notes.
	chromaFloor = 0.05

	// chromaSettle is how fast each class's own long-run level is followed.
	// What it is for is whitening: a drum kit is broadband and lights every
	// class at once, and what is wanted is what stands out from what this
	// record has been doing rather than what is loudest.
	chromaSettle = 0.02

	// chromaFall is how fast the answer is allowed to drop. A note that has
	// been struck rings on, and a class that fell to nothing between two
	// windows would flicker in a way no instrument does.
	chromaFall = 0.35

	// chromaClasses is twelve, and the first of them is C.
	chromaClasses = 12
)

// Chroma folds the spectrum into the twelve pitch classes.
type Chroma struct {
	mu sync.Mutex

	buf  []float64
	win  []float64
	fill int

	// class holds which pitch class each bin belongs to, or -1 for the bins
	// outside the range. Worked out once: it is a logarithm per bin otherwise,
	// twenty times a second.
	class []int8

	now  [chromaClasses]float64 // what is sounding, whitened, 0..1
	long [chromaClasses]float64 // what this record has been doing
	seen bool
}

func newChroma(sampleRate int) *Chroma {
	c := &Chroma{
		buf:   make([]float64, chromaSize),
		win:   make([]float64, chromaSize),
		class: make([]int8, chromaSize/2),
	}

	for i := range c.win {
		c.win[i] = 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(chromaSize-1))
	}

	binHz := float64(sampleRate) / float64(chromaSize)
	for k := range c.class {
		f := float64(k) * binHz
		if f < chromaLowHz || f > chromaHighHz {
			c.class[k] = -1
			continue
		}
		// Semitones above A4, and A is the ninth class counting from C.
		semis := 12 * math.Log2(f/440)
		c.class[k] = int8(((int(math.Round(semis))+9)%12 + 12) % 12)
	}
	return c
}

// feed takes interleaved samples straight from the output path.
func (c *Chroma) feed(samples []float32, channels int) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for i := 0; i < len(samples); i += channels {
		c.buf[c.fill] = float64(samples[i])
		c.fill++
		if c.fill == len(c.buf) {
			c.analyse()
			copy(c.buf, c.buf[chromaHop:])
			c.fill = len(c.buf) - chromaHop
		}
	}
}

// analyse folds one window into the twelve. Callers must hold c.mu.
func (c *Chroma) analyse() {
	re := make([]float64, chromaSize)
	im := make([]float64, chromaSize)
	for i := range re {
		re[i] = c.buf[i] * c.win[i]
	}
	fft(re, im)

	var raw [chromaClasses]float64
	for k, cl := range c.class {
		if cl < 0 {
			continue
		}
		// The energy, not the amplitude: a note is a peak among its neighbours
		// and squaring keeps the peak and drops the skirt around it.
		raw[cl] += re[k]*re[k] + im[k]*im[k]
	}

	// Whitened against what this record has been doing. Without it a record
	// with a loud low string section reads as that string section for four
	// minutes, whatever is played over the top.
	if !c.seen {
		c.long = raw
		c.seen = true
	}
	var most float64
	var out [chromaClasses]float64
	for i := range raw {
		c.long[i] += (raw[i] - c.long[i]) * chromaSettle
		out[i] = max(raw[i]-c.long[i], 0)
		most = max(most, out[i])
	}

	for i := range out {
		v := 0.0
		if most > 0 {
			v = out[i] / most
		}
		if v < chromaFloor {
			v = 0
		}
		// Struck at once, released slowly: an instrument does not stop dead.
		if v > c.now[i] {
			c.now[i] = v
		} else {
			c.now[i] += (v - c.now[i]) * chromaFall
		}
	}
}

// Notes is which of the twelve are sounding, C first, each 0..1. Nil until a
// window has been filled.
func (c *Chroma) Notes() []float32 {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.seen {
		return nil
	}
	out := make([]float32, chromaClasses)
	for i, v := range c.now {
		out[i] = float32(v)
	}
	return out
}

// Reset forgets the record, for when the track changes.
func (c *Chroma) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.fill, c.seen = 0, false
	c.now, c.long = [chromaClasses]float64{}, [chromaClasses]float64{}
	clear(c.buf)
}
