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
	cmd, err := gtio.NewShellReader(ctx, media.FFmpegPath(), args, r.log, media.StderrLogEnabled())
	if err != nil {
		cancel()
		return err
	}
	// Wire the lifecycle event directly into the ShellReader's existing reap
	// goroutine via SetOnExit instead of spawning a separate watchEnd goroutine
	// per call. Saves one goroutine per active RTMP call.
	cmd.SetOnExit(func(exitErr error) {
		// A canceled spawn context means we killed ffmpeg ourselves
		// (Pause, SetSource, SeekBy, Stop): that is not a stream end.
		if r.closed.Load() || ctx.Err() != nil || r.disp == nil {
			return
		}
		// r.mu is taken on the dispatcher, never here: SetOnExit runs
		// this hook synchronously when ffmpeg already exited, and
		// spawnLocked holds r.mu at that point.
		r.disp.Submit(func() {
			r.mu.Lock()
			current := r.cmd == cmd && ctx.Err() == nil
			if current {
				// The source is finished: a later Resume must not replay it.
				r.src, r.srcPath = nil, nil
			}
			r.mu.Unlock()
			if current && !r.closed.Load() && r.ev.OnStreamEnd != nil {
				r.ev.OnStreamEnd(models.Audio, models.Microphone, exitErr)
			}
		})
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
	prev := r.currentStateLocked()
	r.paused = true
	r.resumeMs = uint64(time.Since(r.startedAt) / time.Millisecond)
	r.elapsedMs.Store(r.resumeMs)
	r.stopFFmpegLocked()
	r.fireUpgradeIfChangedLocked(prev)
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
	prev := r.currentStateLocked()
	r.paused = false
	err := r.spawnLocked(context.Background(), r.resumeMs)
	r.fireUpgradeIfChangedLocked(prev)
	return true, err
}

func (r *RTMPCall) Mute() (bool, error)   { return r.notSupportedToggle(&r.muted, true) }
func (r *RTMPCall) Unmute() (bool, error) { return r.notSupportedToggle(&r.muted, false) }

func (r *RTMPCall) notSupportedToggle(flag *bool, target bool) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if *flag == target {
		return false, nil
	}
	prev := r.currentStateLocked()
	*flag = target
	// RTMP push has no fine-grained mute. We log and fake state.
	r.log.Debug("rtmp_call: mute/unmute is best-effort (no SFU control)")
	r.fireUpgradeIfChangedLocked(prev)
	return true, nil
}

// SeekBy shifts playback by deltaMs relative to the current position
// (positive forward, negative backward). Underflow below 0 stops ffmpeg
// and fires OnStreamEnd. Normal seek stops the old ffmpeg (its exit is
// ignored because we canceled it), advances resumeMs, and respawns at
// the new offset.
func (r *RTMPCall) SeekBy(deltaMs int64) error {
	if r.closed.Load() {
		return models.ErrClosed
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.srcPath == nil {
		return models.ErrNoSource
	}
	var curMs uint64
	switch {
	case r.paused:
		curMs = r.resumeMs
	case r.cmd != nil:
		curMs = uint64(time.Since(r.startedAt) / time.Millisecond)
	}
	target := int64(curMs) + deltaMs
	if target < 0 {
		r.stopFFmpegLocked()
		r.src, r.srcPath = nil, nil
		r.resumeMs = 0
		r.elapsedMs.Store(0)
		if r.disp != nil && r.ev.OnStreamEnd != nil {
			r.disp.Submit(func() {
				if !r.closed.Load() {
					r.ev.OnStreamEnd(models.Audio, models.Microphone, nil)
				}
			})
		}
		return nil
	}
	r.stopFFmpegLocked()
	r.resumeMs = uint64(target)
	r.elapsedMs.Store(r.resumeMs)
	if r.paused {
		return nil
	}
	return r.spawnLocked(context.Background(), r.resumeMs)
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
	return r.currentStateLocked()
}

// currentStateLocked builds the MediaState for the RTMP path. RTMP
// always pushes H.264 video (VideoStopped is permanently false), and
// the audio is "silent" in both the Muted and Paused senses, so
// Paused / PresentationPaused mirror muted||paused — same shape as
// GroupCall.currentStateLocked.
func (r *RTMPCall) currentStateLocked() models.MediaState {
	silent := r.muted || r.paused
	return models.MediaState{
		Muted:              r.muted,
		Paused:             silent,
		VideoStopped:       false,
		PresentationPaused: silent,
	}
}

// fireUpgradeIfChangedLocked mirrors GroupCall's helper: dispatch
// OnUpgrade asynchronously when prev != cur. Submit never blocks, so
// it is safe to call while holding r.mu. The disp/OnUpgrade nil-check
// runs first so we skip the second currentStateLocked when nothing
// is wired up.
func (r *RTMPCall) fireUpgradeIfChangedLocked(prev models.MediaState) {
	if r.disp == nil || r.ev.OnUpgrade == nil {
		return
	}
	cur := r.currentStateLocked()
	if prev == cur {
		return
	}
	r.disp.Submit(func() { r.ev.OnUpgrade(cur) })
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
