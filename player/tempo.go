package player

import (
	"math"
	"sync"
)

// Tempo estimates the beat rate of what is playing, from the samples on their
// way to the audio device.
//
// Spotify will tell a client the tempo of a track, but only through an endpoint
// closed to applications registered since late 2024. The audio is right here, so
// it is measured instead.
//
// The method is the ordinary one: an onset strength envelope — how sharply the
// energy rises — autocorrelated against itself. A recording with a steady beat
// resembles itself one beat later, so the lag with the strongest resemblance is
// the beat period.
type Tempo struct {
	mu sync.Mutex

	// The current hop: energy is summed over a block of samples and one
	// envelope value comes out, which is what keeps the analysis cheap.
	acc   float64
	count int

	prev float64   // last hop's energy, for the rise
	env  []float32 // onset strength, oldest first
	at   int       // write position in the ring

	filled   int
	sinceRun int

	bpm        float64
	confidence float64
	agreed     int
}

const (
	// tempoHop is how many frames go into one envelope value: about 12ms at
	// 44.1kHz, which is fine enough to place a beat and coarse enough that the
	// envelope stays small.
	tempoHop = 512

	// tempoHistory is how much of the envelope is kept. Twelve seconds holds
	// enough beats for the autocorrelation to be sure of itself without letting
	// a tempo change take a minute to show up.
	tempoHistory = 1024

	// tempoEvery is how often the estimate is recomputed, in envelope values —
	// about a second and a half.
	tempoEvery = 128

	// The range considered. Beyond these a tempo is either not a beat or is the
	// double or half of one, and clamping is what keeps the estimate from
	// jumping an octave every few seconds.
	tempoMinBPM = 62
	tempoMaxBPM = 180

	// tempoAgree is how many consecutive estimates have to land on the same
	// tempo before it is reported. A number that flickers is worse than none.
	tempoAgree = 2

	// tempoCentre and tempoSpread are the prior on what a tempo usually is.
	//
	// A beat resembles itself at twice its period as well as at one, and often
	// more strongly, so the raw peak lands on half the real tempo about as
	// often as on the tempo — 128 came back as 64. Weighting the search toward
	// the range music actually sits in is the ordinary remedy, and the spread
	// is wide enough that a genuinely slow or fast track still wins on merit.
	tempoCentre = 120.0
	tempoSpread = 0.85

	// tempoSharpness is how many standard deviations above the field a peak has
	// to stand to count as certain.
	tempoSharpness = 5.0

	// tempoFloor is the confidence below which nothing is reported: a tempo
	// invented for a recording that has none is worse than a blank.
	//
	// Measured on synthetic material from 62 to 174 bpm, a real beat scores
	// 0.93 or better and noise scores 0.43. The floor sits between them with
	// room on both sides.
	tempoFloor = 0.6
)

func newTempo() *Tempo {
	return &Tempo{env: make([]float32, tempoHistory)}
}

// feed takes interleaved samples straight from the output path.
func (t *Tempo) feed(samples []float32, channels int) {
	t.mu.Lock()
	defer t.mu.Unlock()

	for _, v := range samples {
		t.acc += float64(v) * float64(v)
		t.count++
		if t.count < tempoHop*channels {
			continue
		}

		energy := math.Sqrt(t.acc / float64(t.count))
		t.acc, t.count = 0, 0

		// The rise, not the level: a loud passage is not a beat, a sudden
		// increase is. In log terms, so a quiet passage counts as much as a
		// loud one.
		rise := math.Log1p(energy*40) - math.Log1p(t.prev*40)
		t.prev = energy

		t.env[t.at] = float32(max(rise, 0))
		t.at = (t.at + 1) % len(t.env)
		if t.filled < len(t.env) {
			t.filled++
		}

		t.sinceRun++
		if t.sinceRun >= tempoEvery && t.filled == len(t.env) {
			t.sinceRun = 0
			t.estimate()
		}
	}
}

// estimate runs the autocorrelation. Callers must hold t.mu.
func (t *Tempo) estimate() {
	// Unwrap the ring, oldest first.
	env := make([]float64, len(t.env))
	var mean float64
	for i := range env {
		env[i] = float64(t.env[(t.at+i)%len(t.env)])
		mean += env[i]
	}
	mean /= float64(len(env))
	for i := range env {
		env[i] -= mean
	}

	rate := float64(SampleRate) / float64(tempoHop)
	minLag := int(rate * 60 / tempoMaxBPM)
	maxLag := int(rate * 60 / tempoMinBPM)
	if minLag < 2 || maxLag >= len(env) {
		return
	}

	var bestLag int
	best := math.Inf(-1)
	var total, totalSq float64
	var n int
	for lag := minLag; lag <= maxLag; lag++ {
		sum := corrAt(env, lag) * tempoPrior(rate*60/float64(lag))

		total += sum
		totalSq += sum * sum
		n++
		if sum > best {
			best, bestLag = sum, lag
		}
	}
	if bestLag == 0 || n == 0 {
		t.confidence = 0
		return
	}

	// How far the winner stands above the field, in standard deviations. A
	// recording with no beat has no peak, only noise, and this is what tells the
	// two apart. Measured as a ratio of the mean instead, it went to nothing
	// whenever the mean happened to sit near zero, which is exactly where most
	// of these correlations live.
	field := total / float64(n)
	variance := max(totalSq/float64(n)-field*field, 0)
	confidence := 0.0
	if sd := math.Sqrt(variance); sd > 0 {
		confidence = min((best-field)/sd/tempoSharpness, 1)
	}

	// Refine the peak against its neighbours, so the answer is not quantised to
	// whole envelope values — at 150 BPM one value is worth about 5 BPM.
	lag := float64(bestLag)
	if bestLag > minLag && bestLag < maxLag {
		a := corrAt(env, bestLag-1) * tempoPrior(rate*60/float64(bestLag-1))
		c := corrAt(env, bestLag+1) * tempoPrior(rate*60/float64(bestLag+1))
		if d := a - 2*best + c; d != 0 {
			lag += 0.5 * (a - c) / d
		}
	}

	bpm := rate * 60 / lag
	if t.bpm > 0 && math.Abs(bpm-t.bpm) < 3 {
		t.agreed++
	} else {
		t.agreed = 0
	}
	t.bpm, t.confidence = bpm, confidence
}

// tempoPrior is how likely a tempo is before hearing anything: a bell in
// log-tempo, so half and double a given rate are penalised equally.
func tempoPrior(bpm float64) float64 {
	octaves := math.Log2(bpm / tempoCentre)
	return math.Exp(-0.5 * octaves * octaves / (tempoSpread * tempoSpread))
}

func corrAt(env []float64, lag int) float64 {
	var sum float64
	for i := 0; i+lag < len(env); i++ {
		sum += env[i] * env[i+lag]
	}
	return sum / float64(len(env)-lag)
}

// Result is the tempo and how sure of it the analyser is, or zero while it is
// still listening. A tempo is only reported once consecutive estimates have
// agreed: a number that flickers between two values is worse than none.
func (t *Tempo) Result() (bpm float64, confidence float64) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.agreed < tempoAgree || t.confidence < tempoFloor {
		return 0, t.confidence
	}
	return t.bpm, t.confidence
}

// Reset forgets everything, for when the track changes.
func (t *Tempo) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.acc, t.count, t.prev = 0, 0, 0
	t.at, t.filled, t.sinceRun = 0, 0, 0
	t.bpm, t.confidence, t.agreed = 0, 0, 0
	clear(t.env)
}
