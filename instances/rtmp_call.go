package instances

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	gtio "github.com/annihilatorrrr/gotgcall/io"
	"github.com/annihilatorrrr/gotgcall/media"
	"github.com/annihilatorrrr/gotgcall/models"
	"github.com/annihilatorrrr/gotgcall/utils"
)

// RTMPCall pushes audio+video to a Telegram-issued RTMP URL via a single
// ffmpeg process. No pion involvement.
type RTMPCall struct {
	startedAt time.Time
	ev        GroupCallEvents

	src       media.Source
	srcPath   media.SourcePath
	log       *slog.Logger
	disp      *utils.Dispatcher
	cmd       *gtio.ShellReader
	cmdCancel context.CancelFunc
	rtmpURL   string
	chatID    int64
	resumeMs  uint64 // seek offset captured on Pause
	elapsedMs atomic.Uint64

	mu     sync.Mutex
	closed atomic.Bool
	paused bool
	muted  bool
}

func NewRTMPCall(chatID int64, rtmpURL string, disp *utils.Dispatcher, log *slog.Logger, ev GroupCallEvents) *RTMPCall {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &RTMPCall{
		chatID:  chatID,
		rtmpURL: rtmpURL,
		log:     log.With(slog.Int64("chat", chatID), slog.String("mode", "rtmp")),
		disp:    disp,
		ev:      ev,
	}
}

func (*RTMPCall) Mode() string { return "rtmp" }

// CreateLocalParams WebRTC-only methods return ErrWrongMode for RTMP calls.
func (*RTMPCall) CreateLocalParams() (string, error) { return "", models.ErrWrongMode }
func (*RTMPCall) Connect(string) error               { return models.ErrWrongMode }

func (r *RTMPCall) SetSource(ctx context.Context, src media.Source) error {
	if r.closed.Load() {
		return models.ErrClosed
	}
	pathed, ok := src.(media.SourcePath)
	if !ok {
		return fmt.Errorf("%w: RTMP mode requires a path-based source (FromFile/FromURL)", models.ErrWrongMode)
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	r.stopFFmpegLocked()
	r.src = src
	r.srcPath = pathed
	r.resumeMs = 0
	r.elapsedMs.Store(0)

	if r.paused {
		return nil
	}
	return r.spawnLocked(ctx, 0)
}

func (r *RTMPCall) spawnLocked(ctx context.Context, seekMs uint64) error {
	if r.srcPath == nil {
		return nil
	}
	opt := r.srcPath.EncodeOpts().WithDefaults()
	args := buildRTMPArgs(r.srcPath, seekMs, opt, r.rtmpURL)
	ctx, cancel := context.WithCancel(ctx)
	cmd, err := gtio.NewShellReader(ctx, "ffmpeg", args, r.log, media.StderrLogEnabled())
	if err != nil {
		cancel()
		return err
	}
	// Wire the lifecycle event directly into the ShellReader's existing reap
	// goroutine via SetOnExit instead of spawning a separate watchEnd goroutine
	// per call. Saves one goroutine per active RTMP call.
	cmd.SetOnExit(func(exitErr error) {
		if r.closed.Load() {
			return
		}
		if r.disp != nil && r.ev.OnStreamEnd != nil {
			r.disp.Submit(func() {
				r.ev.OnStreamEnd(models.Audio, models.Microphone, exitErr)
			})
		}
	})
	r.cmd = cmd
	r.cmdCancel = cancel
	r.startedAt = time.Now().Add(-time.Duration(seekMs) * time.Millisecond)
	return nil
}

func (r *RTMPCall) stopFFmpegLocked() {
	if r.cmd != nil {
		if r.cmdCancel != nil {
			r.cmdCancel()
		}
		_ = r.cmd.Close()
		r.cmd = nil
		r.cmdCancel = nil
	}
}

func (r *RTMPCall) Pause() (bool, error) {
	if r.closed.Load() {
		return false, models.ErrClosed
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.paused {
		return false, nil
	}
	r.paused = true
	r.resumeMs = uint64(time.Since(r.startedAt) / time.Millisecond)
	r.elapsedMs.Store(r.resumeMs)
	r.stopFFmpegLocked()
	return true, nil
}

func (r *RTMPCall) Resume() (bool, error) {
	if r.closed.Load() {
		return false, models.ErrClosed
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.paused {
		return false, nil
	}
	r.paused = false
	return true, r.spawnLocked(context.Background(), r.resumeMs)
}

func (r *RTMPCall) Mute() (bool, error)   { return r.notSupportedToggle(&r.muted, true) }
func (r *RTMPCall) Unmute() (bool, error) { return r.notSupportedToggle(&r.muted, false) }

func (r *RTMPCall) notSupportedToggle(flag *bool, target bool) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if *flag == target {
		return false, nil
	}
	*flag = target
	// RTMP push has no fine-grained mute. We log and fake state.
	r.log.Debug("rtmp_call: mute/unmute is best-effort (no SFU control)")
	return true, nil
}

func (r *RTMPCall) Stop() error {
	if !r.closed.CompareAndSwap(false, true) {
		return nil
	}
	r.mu.Lock()
	r.stopFFmpegLocked()
	r.src = nil
	r.srcPath = nil
	r.resumeMs = 0
	r.elapsedMs.Store(0)
	r.paused = false
	r.muted = false
	r.mu.Unlock()
	return nil
}

func (r *RTMPCall) ElapsedMs() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.paused {
		return r.resumeMs
	}
	if r.cmd == nil {
		return 0
	}
	return uint64(time.Since(r.startedAt) / time.Millisecond)
}

func (r *RTMPCall) State() models.MediaState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return models.MediaState{Muted: r.muted, Paused: r.paused}
}

func (r *RTMPCall) NetState() models.ConnState {
	if r.closed.Load() {
		return models.Closed
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cmd == nil {
		return models.Connecting
	}
	return models.Connected
}

// buildRTMPArgs assembles a single ffmpeg argv that reads from srcPath,
// transcodes to H.264+AAC, and pushes FLV to rtmpURL.
func buildRTMPArgs(srcPath media.SourcePath, seekMs uint64, opt media.EncodeOptions, rtmpURL string) []string {
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-re"}
	if seekMs > 0 {
		args = append(args, "-ss", fmt.Sprintf("%.3f", float64(seekMs)/1000.0))
	}
	args = append(args, srcPath.InputArgs()...)
	args = append(args,
		"-c:v", "libx264",
		"-preset", "veryfast",
		"-tune", "zerolatency",
		"-b:v", fmt.Sprintf("%dk", opt.VideoBitrateKbps),
		"-r", strconv.Itoa(opt.VideoFPS),
		"-vf", fmt.Sprintf("scale=%d:%d", opt.VideoWidth, opt.VideoHeight),
		"-pix_fmt", "yuv420p",
		"-g", strconv.Itoa(opt.VideoFPS*2),
		"-c:a", "aac",
		"-b:a", fmt.Sprintf("%dk", opt.AudioBitrateKbps),
		"-ar", "44100",
		"-ac", strconv.Itoa(opt.AudioChannels),
		"-f", "flv",
		rtmpURL,
	)
	return args
}
