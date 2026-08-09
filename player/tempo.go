package player

import (
	"math"
	"sync"
	"time"
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

	filled     int
	sinceRun   int
	sincePlace int

	bpm        float64
	confidence float64
	agreed     int

	// hops is how many envelope values have gone by since the analyser started,
	// and beat where the last beat fell on that count — a fractional position,
	// because a beat does not land on a whole envelope value any more than a
	// tempo is a whole number.
	//
	// The period alone says how often; this says when, which is the difference
	// between a picture that reacts to the music and one that plays with it.
	hops   float64
	beat   float64
	period float64
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

	// phaseWindow is how much of the envelope the beats are placed against, in
	// values: about four seconds. See phaseAt.
	phaseWindow = 344

	// phaseEvery is how often they are placed again, in values: about a third
	// of a second.
	phaseEvery = 32

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
		t.hops++
		if t.filled < len(t.env) {
			t.filled++
		}

		t.sinceRun++
		if t.sinceRun >= tempoEvery && t.filled == len(t.env) {
			t.sinceRun = 0
			t.estimate()
		}

		// The beats are placed far oftener than the tempo is measured. Twelve
		// seconds of envelope is what it takes to be sure how fast a record is;
		// where the next beat falls is a question about the last few seconds,
		// and asking it once every second and a half meant extrapolating a
		// position across a second and a half of a period that is a fraction of
		// a percent out. Which is a fraction of a beat late, every time.
		t.sincePlace++
		if t.sincePlace >= phaseEvery && t.period > 0 {
			t.sincePlace = 0
			t.phaseAt(t.tail(), t.period)
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

	t.period = lag
	t.phaseAt(t.tail(), lag)

	bpm := rate * 60 / lag
	if t.bpm > 0 && math.Abs(bpm-t.bpm) < 3 {
		t.agreed++
	} else {
		t.agreed = 0
	}
	t.bpm, t.confidence = bpm, confidence
}

// tail is the newest of the envelope, oldest first, with the mean taken out —
// the same shape estimate works on, over the window the beats are placed
// against. Callers must hold t.mu.
func (t *Tempo) tail() []float64 {
	n := min(phaseWindow, t.filled)
	out := make([]float64, n)

	var mean float64
	for i := range out {
		out[i] = float64(t.env[(t.at-n+i+len(t.env)*2)%len(t.env)])
		mean += out[i]
	}
	if n > 0 {
		mean /= float64(n)
	}
	for i := range out {
		out[i] -= mean
	}
	return out
}

// phaseAt finds where the beats fall, given how far apart they are.
//
// The period says how often, which is half of keeping time; this is the other
// half. A comb is laid over the envelope — one tooth every period — and slid
// along until the teeth sit on as much onset as they can. Where they land is
// where the beats are.
//
// Sub-hop, by taking the centre of mass of the onset around each tooth: at 120
// bpm one envelope value is a fortieth of a beat, which is audible as a stumble
// if every beat is quantised to it.
//
// Callers must hold t.mu.
func (t *Tempo) phaseAt(env []float64, lag float64) {
	if lag < 2 || int(lag) >= len(env) {
		return
	}

	// Only the end of the window. The period is measured over twelve seconds
	// because that is what it takes to be sure of it, and it comes back a
	// fraction of a percent out — which is nothing for a rate and a great deal
	// for a position, because walking a comb from one end of twelve seconds to
	// the other multiplies that fraction by every beat it steps over. At 174
	// bpm it came out a fifth of a beat late. Four seconds is a dozen beats to
	// fit against and a third of the drift.
	if from := len(env) - phaseWindow; from > 0 && float64(phaseWindow) > lag*3 {
		env = env[from:]
	}

	best, at := math.Inf(-1), 0.0
	for off := 0.0; off < lag; off++ {
		var sum, weighted, weight float64
		var teeth int
		for x := off; x < float64(len(env)); x += lag {
			teeth++
			// The tooth and its neighbours, so a beat that fell between two
			// values is not missed by the one it fell between.
			for d := -1.0; d <= 1; d++ {
				i := int(x + d)
				if i < 0 || i >= len(env) || env[i] <= 0 {
					continue
				}
				sum += env[i]
				weighted += env[i] * d
				weight += env[i]
			}
		}
		if teeth == 0 {
			continue
		}

		// Per tooth, not per comb: an offset near the start fits one more tooth
		// into the same window than an offset near the end does, and judged on
		// the total that extra tooth is worth more than being right. Which is
		// how a comb laid over a steady beat came back half a period out.
		if sum /= float64(teeth); sum > best {
			best = sum
			at = off
			if weight > 0 {
				at += weighted / weight
			}
		}
	}

	// The comb was laid over the envelope oldest first, so the last tooth
	// before the end is the last beat heard. Counted against hops, which is the
	// clock everything else reads it against.
	last := at
	for last+lag < float64(len(env)) {
		last += lag
	}
	t.beat = t.hops - (float64(len(env)) - 1 - last)
	t.period = lag
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

// Beat is how far apart the beats are and how long ago the last one fell, or
// nothing while the analyser is still listening.
//
// The pair is what a picture needs to keep time rather than to react: the
// period says how often to move and the offset says when, and between the two
// anything that draws can work out where the next beat is without asking again.
func (t *Tempo) Beat() (period, since time.Duration, confidence float64) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.agreed < tempoAgree || t.confidence < tempoFloor || t.period <= 0 {
		return 0, 0, t.confidence
	}

	hop := float64(time.Second) * float64(tempoHop) / float64(SampleRate)
	gone := t.hops - t.beat
	period = time.Duration(t.period * hop)

	// Wrapped into the period: the last beat is the last one, not the one the
	// estimate happened to land on a second and a half ago.
	if period > 0 {
		gone = math.Mod(gone, t.period)
		if gone < 0 {
			gone += t.period
		}
	}
	return period, time.Duration(gone * hop), t.confidence
}

// Reset forgets everything, for when the track changes.
func (t *Tempo) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.acc, t.count, t.prev = 0, 0, 0
	t.at, t.filled, t.sinceRun, t.sincePlace = 0, 0, 0, 0
	t.bpm, t.confidence, t.agreed = 0, 0, 0
	t.hops, t.beat, t.period = 0, 0, 0
	clear(t.env)
}
