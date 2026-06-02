package media

import (
	"context"
	"errors"
	stdio "io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4/pkg/media"
)

// Writer consumes encoded samples. Implemented by pion's TrackLocalStaticSample
// and by test fakes.
type Writer interface {
	WriteSample(s media.Sample) error
}

// stallTimeout bounds how long a single src.Next() read can block before
// the streamer force-closes the source. Catches ffmpeg processes stuck on
// network I/O (e.g., HTTP source that dropped mid-stream) that would
// otherwise hang the streamer indefinitely — OnStreamEnd never fires, the
// bot never advances to the next song.
const stallTimeout = 30 * time.Second

// Streamer pulls Samples from a FrameReader at the sample's natural cadence
// and pushes them to a Writer. Mute (audio) skips WriteSample but keeps
// the clock advancing. Pause (set via SetPaused) blocks the pull loop on a
// channel without tearing down the underlying ffmpeg process — the OS pipe
// buffer absorbs ~1s of OGG bytes while paused; on resume the loop wakes
// and drains them at real-time pace.
type Streamer struct {
	src    FrameReader
	writer Writer

	ctx context.Context
	log *slog.Logger

	cancel context.CancelFunc
	done   chan struct{}

	// resume signals a paused→unpaused transition. Buffered, size 1.
	resume chan struct{}

	onEnd  func(err error)
	msSent atomic.Uint64

	once sync.Once

	muted      atomic.Bool
	pausedGate atomic.Bool
}

func NewStreamer(parent context.Context, src FrameReader, writer Writer, log *slog.Logger, onEnd func(error)) *Streamer {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	ctx, cancel := context.WithCancel(parent)
	return &Streamer{
		src:    src,
		writer: writer,
		log:    log,
		ctx:    ctx,
		cancel: cancel,
		done:   make(chan struct{}),
		resume: make(chan struct{}, 1),
		onEnd:  onEnd,
	}
}

// SetPaused gates the run loop without canceling the context or closing the
// source. While paused the pull loop blocks on a channel; ffmpeg keeps
// running and its stdout pipe buffers the next ~1s of frames. On resume
// the loop wakes and resumes at real time.
func (s *Streamer) SetPaused(p bool) {
	if p {
		s.pausedGate.Store(true)
		s.log.Debug("streamer: SetPaused", slog.Bool("to", true))
		return
	}
	if s.pausedGate.CompareAndSwap(true, false) {
		s.log.Debug("streamer: SetPaused", slog.Bool("to", false))
		// Wake the loop. Non-blocking — if a wake is already queued, fine.
		select {
		case s.resume <- struct{}{}:
		default:
		}
	}
}

// IsPaused reports whether the gate is currently blocking the loop.
func (s *Streamer) IsPaused() bool { return s.pausedGate.Load() }

// gate blocks while paused. Returns false if the context is canceled while
// waiting, so the loop can exit cleanly.
func (s *Streamer) gate() bool {
	if !s.pausedGate.Load() {
		return s.ctx.Err() == nil
	}
	// Drop any stale resume token left from a previous cycle.
	select {
	case <-s.resume:
	default:
	}
	for s.pausedGate.Load() {
		select {
		case <-s.resume:
		case <-s.ctx.Done():
			return false
		}
	}
	return s.ctx.Err() == nil
}

// Start kicks off the pacing goroutine. Returns immediately. The run loop
// services its own ctx cancellation via channel select — no separate
// cancel-watcher goroutine is needed.
func (s *Streamer) Start() { go s.run() }

func (s *Streamer) run() {
	defer close(s.done)
	defer func() {
		if r := recover(); r != nil {
			s.log.Error("streamer: panic", slog.Any("recover", r))
			s.fireEnd(panicErr{v: r})
		}
		s.log.Debug("streamer: run exit", slog.Uint64("msSent", s.msSent.Load()))
	}()
	s.log.Debug("streamer: run start")

	// One timer for the entire loop — reset each iteration. Go 1.23+
	// Reset semantics make this safe without manual drain.
	t := time.NewTimer(0)
	if !t.Stop() {
		<-t.C
	}
	defer t.Stop()

	next := time.Now()
	stallTimer := time.AfterFunc(time.Hour, func() {
		s.log.Warn("streamer: source read stalled, force-closing")
		_ = s.src.Close()
	})
	stallTimer.Stop()
	defer stallTimer.Stop()
	for {
		if err := s.ctx.Err(); err != nil {
			s.fireEnd(err)
			return
		}
		if !s.gate() {
			s.fireEnd(s.ctx.Err())
			return
		}
		if gateWake := time.Now(); gateWake.After(next) {
			next = gateWake
		}
		stallTimer.Reset(stallTimeout)
		sample, err := s.src.Next(s.ctx)
		stallTimer.Stop()
		if err != nil {
			s.log.Debug("streamer: src.Next err", slog.Any("err", err), slog.Uint64("msSent", s.msSent.Load()))
			s.fireEnd(err)
			return
		}
		if !s.muted.Load() {
			if writeErr := s.writer.WriteSample(sample); writeErr != nil && !errors.Is(writeErr, stdio.ErrClosedPipe) {
				s.log.Debug("streamer: write sample failed", slog.Any("err", writeErr))
				s.fireEnd(writeErr)
				return
			}
		}
		s.msSent.Add(uint64(sample.Duration / time.Millisecond))

		next = next.Add(sample.Duration)
		wait := time.Until(next)
		if wait < -100*time.Millisecond {
			next = time.Now()
			continue
		}
		if wait <= 0 {
			continue
		}
		t.Reset(wait)
		select {
		case <-t.C:
		case <-s.ctx.Done():
			if !t.Stop() {
				<-t.C
			}
			s.fireEnd(s.ctx.Err())
			return
		}
	}
}

func (s *Streamer) fireEnd(err error) {
	if s.onEnd == nil {
		return
	}
	if errors.Is(err, context.Canceled) {
		err = nil
	}
	s.onEnd(err)
}

// Stop cancels the run loop and closes the underlying frame reader. Safe
// to call from any goroutine. Blocks until the run loop exits.
func (s *Streamer) Stop() {
	s.once.Do(func() {
		s.cancel()
		_ = s.src.Close()
	})
	<-s.done
}

// Done is closed when the streamer has finished (EOF, error, or Stop).
func (s *Streamer) Done() <-chan struct{} { return s.done }

func (s *Streamer) SetMuted(m bool) { s.muted.Store(m) }
func (s *Streamer) Muted() bool     { return s.muted.Load() }

// ElapsedMs returns cumulative ms of samples handed to the pacing loop.
func (s *Streamer) ElapsedMs() uint64 { return s.msSent.Load() }

type panicErr struct{ v any }

func (panicErr) Error() string { return "streamer panic" }
