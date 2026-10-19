package media

import (
	"context"
	stdio "io"
	"testing"
	"time"

	"github.com/pion/webrtc/v4/pkg/media"
)

type fakeFrames struct{ left int }

func (f *fakeFrames) Next(context.Context) (media.Sample, error) {
	if f.left == 0 {
		return media.Sample{}, stdio.EOF
	}
	f.left--
	return media.Sample{Data: []byte{0}, Duration: 5 * time.Millisecond}, nil
}

func (*fakeFrames) Close() error { return nil }

type nopWriter struct{}

func (nopWriter) WriteSample(media.Sample) error { return nil }

// Pacing must track wall time: never faster than the samples' total
// duration, and timer lateness must not accumulate into a slow drift.
func TestStreamerPacesInRealTime(t *testing.T) {
	const n = 200 // 1 s of 5 ms samples
	ended := make(chan time.Time, 1)
	s := NewStreamer(context.Background(), &fakeFrames{left: n}, nopWriter{}, nil, func(error) { ended <- time.Now() })
	start := time.Now()
	s.Start()
	elapsed := (<-ended).Sub(start)
	t.Logf("%d x 5 ms samples took %v", n, elapsed)
	if elapsed < n*5*time.Millisecond || elapsed > 1060*time.Millisecond {
		t.Fatalf("%d x 5 ms samples took %v, want about 1s", n, elapsed)
	}
	if got := s.ElapsedMs(); got != n*5 {
		t.Fatalf("ElapsedMs = %d, want %d", got, n*5)
	}
}
