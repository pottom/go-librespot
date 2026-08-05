package player

import (
	"io"
	"math"
	"testing"
)

// steady is a reader that plays one tone for as long as it is asked to.
type steady struct {
	hz   float64
	at   int
	left int
}

func (s *steady) Read(p []float32) (int, error) {
	if s.left <= 0 {
		return 0, io.EOF
	}

	n := min(len(p), s.left)
	for i := 0; i < n; i += Channels {
		v := float32(math.Sin(2 * math.Pi * s.hz * float64(s.at) / float64(SampleRate)))
		for c := range Channels {
			p[i+c] = v
		}
		s.at++
	}
	s.left -= n
	return n, nil
}

// level is how loud a tapped frame is, which is all these tests ask of it.
func level(frame []float32) float64 {
	var sum float64
	for _, v := range frame {
		sum += float64(v) * float64(v)
	}
	return math.Sqrt(sum / float64(len(frame)))
}

func tapped(hz float64) []float32 {
	t := newTap(&steady{hz: hz, left: SampleRate * Channels}, SampleRate, Channels, tapFrameRate)
	buf := make([]float32, 4096)
	for {
		if _, err := t.Read(buf); err != nil {
			break
		}
	}
	return t.Frame()
}

// The trace is drawn from one sample in every few, and everything above half of
// what is left has to be filtered out before they are thrown away. Without that
// a cymbal — which is mostly energy above eight kilohertz — folds down into the
// range that is drawn and rides on the trace as a wave nobody played.
//
// The tones here are on either side of that line: one in the range the shape of
// a wave lives in, one well above it.
func TestTapFiltersWhatItCannotDraw(t *testing.T) {
	music := level(tapped(1000))
	if music < 0.3 {
		t.Fatalf("a 1kHz tone reads %.3f, want it drawn at close to full strength", music)
	}

	for _, hz := range []float64{6000, 9000, 15000} {
		above := level(tapped(hz))
		down := 20 * math.Log10(above/music)
		t.Logf("%.0fHz is %.1fdB below the 1kHz tone", hz, down)

		if down > -15 {
			t.Errorf("%.0fHz survives the tap at %.1fdB, want it filtered out before the samples are thinned", hz, down)
		}
	}
}

// The window has to average to one, or the trace is quietly scaled by whatever
// it does average to.
func TestTriangleKeepsTheLevel(t *testing.T) {
	var total float32
	for _, w := range triangle(10) {
		total += w
	}
	if math.Abs(float64(total)-1) > 1e-6 {
		t.Errorf("the window sums to %.6f, want 1", total)
	}
}
