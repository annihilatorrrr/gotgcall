package instances

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/annihilatorrrr/gotgcall/media"
	"github.com/annihilatorrrr/gotgcall/models"
	"github.com/annihilatorrrr/gotgcall/utils"
	"github.com/annihilatorrrr/gotgcall/wrtc"
)

// GroupCallEvents is the set of callbacks a GroupCall fires through the
// shared dispatcher; the Client wires these to its public OnXxx callbacks.
type GroupCallEvents struct {
	OnStreamEnd        func(t models.StreamType, d models.Device, err error)
	OnConnectionChange func(info models.NetworkInfo)
	// OnUpgrade fires on every outgoing state change that flips a
	// MediaState bit: Pause / Resume / Mute / Unmute (each only when the
	// state actually changed — no-op toggles stay silent), plus PC
	// Failed/Closed while video was active. Stream EOF (audio or video)
	// does not fire — OnStreamEnd already signals that. SetSource and
	// Stop also stay silent: the caller chose the new source / brought
	// the call down and can drive MTProto flags directly in the same
	// code path.
	// Mirror of ntgcalls' onUpgrade(MediaState) pattern.
	OnUpgrade func(state models.MediaState)
}

// GroupCall is the WebRTC call instance for one chat.
type GroupCall struct {
	ev GroupCallEvents

	src            media.Source
	pc             *wrtc.PeerConnection
	log            *slog.Logger
	disp           *utils.Dispatcher
	streams        *media.Streams
	audioStr       *media.Streamer
	videoStr       *media.Streamer
	cycle          *media.Streams // current source cycle, see handleStreamerEnd
	connected      chan struct{}
	srcEncOpt      media.EncodeOptions
	chatID         int64
	connectTimeout time.Duration
	resumeMs       uint64 // seek offset captured on Pause; injected via SeekableSource.OpenAt on Resume

	mu            sync.RWMutex
	connectedOnce sync.Once
	netState      atomic.Int32 // models.ConnState
	closed        atomic.Bool
	connectCalled atomic.Bool
	paused        bool
	muted         bool
	videoOff      bool
	// Per-cycle stream-end state, set by installLocked. A source cycle is
	// identified by its *media.Streams: an end event whose cycle is no
	// longer g.cycle (replaced by SetSource/SeekBy, or already ended by
	// the sibling leg) is stale. hadAudio / hadVideo are captured at
	// cycle-start so the user gets the right events after fields clear.
	hadAudio bool
	hadVideo bool
}

// NewGroupCall constructs a fresh call. Caller threads pion factory + logger.
// connectTimeout controls how long SetSource waits for ICE+DTLS; 0 = 10s
// (matches ntgcalls' internal call_interface.cpp:138 timeout).
func NewGroupCall(chatID int64, factory *wrtc.Factory, disp *utils.Dispatcher, log *slog.Logger, connectTimeout time.Duration, ev GroupCallEvents) (*GroupCall, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if connectTimeout <= 0 {
		connectTimeout = 10 * time.Second
	}
	pc, err := wrtc.NewPeerConnection(factory, log)
	if err != nil {
		return nil, err
	}
	gc := &GroupCall{
		chatID:         chatID,
		pc:             pc,
		log:            log.With(slog.Int64("chat", chatID)),
		disp:           disp,
		ev:             ev,
		connected:      make(chan struct{}),
		connectTimeout: connectTimeout,
	}
	gc.netState.Store(int32(models.Connecting))
	var wasConnected atomic.Bool
	pc.OnConnectionStateChange(func(s models.ConnState) {
		gc.netState.Store(int32(s))
		if s == models.Connected || s == models.Failed || s == models.Closed {
			gc.connectedOnce.Do(func() { close(gc.connected) })
		}
		if s == models.Connected {
			wasConnected.Store(true)
		}

		// Disconnected is a transient ICE state pion can recover from.
		// Streamers keep running so audio resumes when the connection
		// recovers. Suppress the user callback (ntgcalls does the same —
		// it silently logs "Reconnecting" and returns without notifying).
		if s == models.Disconnected {
			gc.log.Debug("ICE disconnected, waiting for recovery")
			return
		}
		// Connecting after already-connected is a transient reconnection
		// attempt. Suppress the user callback to avoid false-alarm churn.
		if s == models.Connecting && wasConnected.Load() {
			gc.log.Debug("ICE reconnecting")
			return
		}

		// When pion declares the PC Failed or Closed, the underlying
		// transport is gone. Tear streamers down so we stop burning CPU.
		if (s == models.Failed || s == models.Closed) && gc.disp != nil {
			gc.disp.Submit(func() {
				gc.mu.Lock()
				prev := gc.currentStateLocked()
				gc.stopStreamersLocked()
				gc.fireUpgradeIfChangedLocked(prev)
				gc.mu.Unlock()
			})
		}
		if gc.disp != nil && gc.ev.OnConnectionChange != nil {
			gc.disp.Submit(func() { gc.ev.OnConnectionChange(models.NetworkInfo{State: s}) })
		}
	})
	return gc, nil
}

func (*GroupCall) Mode() string { return "webrtc" }

func (g *GroupCall) CreateLocalParams() (string, error) {
	if g.closed.Load() {
		return "", models.ErrClosed
	}
	return g.pc.LocalParams()
}

func (g *GroupCall) Connect(remoteJSON string) error {
	if g.closed.Load() {
		return models.ErrClosed
	}
	g.connectCalled.Store(true)
	g.log.Debug("Connect: setting remote description")
	return g.pc.Connect(remoteJSON)
}

func (g *GroupCall) SetSource(ctx context.Context, src media.Source) error {
	if g.closed.Load() {
		return models.ErrClosed
	}

	// Source-owned encode opts. FromShell sources don't expose them — the
	// prepared video streamer falls back to 30 FPS.
	srcEncOpt := media.EncodeOptions{}
	if sp, ok := src.(media.SourcePath); ok {
		srcEncOpt = sp.EncodeOpts()
	}

	// Phase 1 (OUTSIDE g.mu): spawn ffmpeg + read OGG/IVF headers. The
	// ivfreader/oggreader header parse blocks reading from ffmpeg's pipe
	// until ffmpeg flushes its first page (200 ms-1 s on cold start), and
	// holding g.mu across that window stalls every concurrent Pause /
	// Mute / GetState / ElapsedMs caller for the same call.
	streams, err := src.Open(ctx)
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	audioStr, videoStr, err := g.prepareStreamers(ctx, streams, srcEncOpt)
	if err != nil {
		return err
	}

	// Gate: wait for WebRTC to reach Connected before starting streamers.
	// Samples written before ICE+DTLS completes are silently dropped by
	// pion (no SRTP binding yet), causing silence on first play. The
	// channel also closes on Failed/Closed so Stop() during the wait
	// doesn't hang for the full timeout.
	if models.ConnState(g.netState.Load()) != models.Connected {
		connectTimer := time.NewTimer(g.connectTimeout)
		select {
		case <-g.connected:
			connectTimer.Stop()
		case <-connectTimer.C:
			_ = streams.Close()
			if !g.connectCalled.Load() {
				return fmt.Errorf("%w: timed out waiting for WebRTC — Connect() was never called", models.ErrNotConnected)
			}
			state := models.ConnState(g.netState.Load())
			g.log.Warn("connect gate timed out",
				slog.String("state", state.String()),
				slog.Duration("timeout", g.connectTimeout),
				slog.String("hint", "enable WithDebugLogs()+WithICECandidateLogs() to see candidate exchange details"))
			return fmt.Errorf("%w: ICE/DTLS did not reach Connected within %s (stuck in %s)", models.ErrNotConnected, g.connectTimeout, state)
		case <-ctx.Done():
			connectTimer.Stop()
			_ = streams.Close()
			return ctx.Err()
		}
		if state := models.ConnState(g.netState.Load()); state != models.Connected {
			_ = streams.Close()
			return fmt.Errorf("%w: connection %s during setup", models.ErrConnectionFailed, state)
		}
	}

	// Phase 2 (UNDER g.mu): tear down old streamers, install new ones,
	// start them under the lock so any concurrent Stop/Pause sees a
	// coherent state.
	g.mu.Lock()

	if g.closed.Load() {
		g.mu.Unlock()
		// Streamers prepared but never Started — close the streams
		// (which kills ffmpeg). Streamer.Stop would deadlock because
		// it waits on a done chan that's only closed by the never-spawned run goroutine.
		_ = streams.Close()
		return models.ErrClosed
	}

	// The old cycle's end events turn stale once g.streams changes, so
	// replacing a source never fires OnStreamEnd for it.
	g.stopStreamersLocked()
	g.src = src
	g.resumeMs = 0
	g.srcEncOpt = srcEncOpt
	g.installLocked(streams, audioStr, videoStr)
	g.mu.Unlock()
	// Intentionally no OnUpgrade fire here — SetSource is user-initiated,
	// the caller already knows they're switching to a video source (same
	// principle as Stop). Spontaneous transitions (video leg EOF, ICE
	// failure) still fire.
	return nil
}

// firstPageTimeout bounds the wait for a source's first OGG/IVF page. A
// hung ffmpeg (e.g. a stalled HTTP input without -rw_timeout) would
// otherwise block SetSource, or Resume/SeekBy under g.mu, forever.
const firstPageTimeout = 30 * time.Second

// prepareStreamers parses the headers of freshly opened streams and
// constructs (but does NOT Start) the Streamers. Runs outside g.mu from
// SetSource so the ffmpeg-header wait doesn't block other concurrent
// operations on the same call. Closes streams on error.
func (g *GroupCall) prepareStreamers(ctx context.Context, streams *media.Streams, encOpt media.EncodeOptions) (*media.Streamer, *media.Streamer, error) {
	stall := time.AfterFunc(firstPageTimeout, func() { _ = streams.Close() })
	var audioStr, videoStr *media.Streamer
	if streams.Audio != nil {
		fr, frErr := media.NewOpusFrameReader(streams.Audio)
		if frErr != nil {
			// Promoted to Warn so default-Info logging surfaces "/play
			// silently played no audio" without users having to flip to
			// Debug. The video leg (if any) still plays.
			g.log.Warn("audio track unavailable, skipping", slog.Any("err", frErr))
		} else {
			audioStr = media.NewStreamer(ctx, fr, g.pc.AudioTrack(), g.log, func(endErr error) {
				g.handleStreamerEnd(models.Audio, models.Microphone, endErr, streams)
			})
		}
	}
	if streams.Video != nil {
		fps := encOpt.VideoFPS
		if fps <= 0 {
			fps = 30
		}
		fr, frErr := media.NewVP8FrameReader(streams.Video, fps)
		if frErr != nil {
			// Promoted from Debug — likely root cause of "I called
			// /vplay and video never started". Visible at default Info.
			g.log.Warn("video track unavailable, skipping", slog.Any("err", frErr))
		} else {
			videoStr = media.NewStreamer(ctx, fr, g.pc.VideoTrack(), g.log, func(endErr error) {
				g.handleStreamerEnd(models.Video, models.Camera, endErr, streams)
			})
		}
	}
	if !stall.Stop() {
		return nil, nil, fmt.Errorf("%w: no media from the source within %s", models.ErrFile, firstPageTimeout)
	}
	if audioStr == nil && videoStr == nil {
		_ = streams.Close()
		return nil, nil, fmt.Errorf("%w: source has no playable audio or video stream", models.ErrFile)
	}
	return audioStr, videoStr, nil
}

// installLocked makes streams the current source cycle and starts its
// streamers with the call's current pause / mute state, so a Mute that
// landed while SetSource was preparing is not lost. Caller holds g.mu.
func (g *GroupCall) installLocked(streams *media.Streams, audioStr, videoStr *media.Streamer) {
	g.streams = streams
	g.cycle = streams
	g.audioStr = audioStr
	g.videoStr = videoStr
	g.hadAudio = audioStr != nil
	g.hadVideo = videoStr != nil
	if audioStr != nil {
		audioStr.SetMuted(g.muted)
		audioStr.SetPaused(g.paused)
		audioStr.Start()
	}
	if videoStr != nil {
		videoStr.SetMuted(g.videoOff)
		videoStr.SetPaused(g.paused)
		videoStr.Start()
	}
}

// startLocked reopens g.src (at resumeMs when seekable) and installs it
// as a new cycle. Used by Resume when no streamers exist (e.g. after an
// ICE-failure teardown) and by SeekBy. Caller holds g.mu.
func (g *GroupCall) startLocked(ctx context.Context) error {
	if g.src == nil {
		return nil
	}
	var (
		streams *media.Streams
		err     error
	)
	if seekable, ok := g.src.(media.SeekableSource); ok && g.resumeMs > 0 {
		streams, err = seekable.OpenAt(ctx, time.Duration(g.resumeMs)*time.Millisecond)
	} else {
		streams, err = g.src.Open(ctx)
	}
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	audioStr, videoStr, err := g.prepareStreamers(ctx, streams, g.srcEncOpt)
	if err != nil {
		return err
	}
	g.installLocked(streams, audioStr, videoStr)
	return nil
}

func (g *GroupCall) stopStreamersLocked() {
	if g.audioStr != nil {
		g.audioStr.Stop()
		g.audioStr = nil
	}
	if g.videoStr != nil {
		g.videoStr.Stop()
		g.videoStr = nil
	}
	if g.streams != nil {
		_ = g.streams.Close()
		g.streams = nil
	}
}

// handleStreamerEnd is the per-streamer onEnd callback. cycle is the
// *media.Streams the streamer was built from; the dispatched closure only
// fires if that cycle is still current, so ends from a replaced source
// or the second leg of the same source are dropped.
//
// For a video+audio source, the first leg to EOF wins the right to fire.
// The sibling streamer is force-stopped and its own EOF is stale. The
// user gets two OnStreamEnd events in fixed order — Video then Audio —
// regardless of which leg ended first. Audio-only sources fire a single
// Audio event; video-only sources fire a single Video event.
func (g *GroupCall) handleStreamerEnd(t models.StreamType, d models.Device, err error, cycle *media.Streams) {
	closed := g.closed.Load()
	g.log.Debug("streamer end",
		slog.Any("type", t), slog.Any("device", d), slog.Any("err", err),
		slog.Bool("closed", closed))
	if closed || g.disp == nil {
		return
	}
	g.disp.Submit(func() {
		if g.closed.Load() {
			return
		}
		g.mu.Lock()
		if g.cycle != cycle {
			g.mu.Unlock()
			return
		}
		hadAudio := g.hadAudio
		hadVideo := g.hadVideo
		// The streamer that ended naturally needs no Stop call; its
		// sibling is stopped below, outside the lock.
		sibling := g.videoStr
		if t == models.Video {
			sibling = g.audioStr
		}
		g.audioStr, g.videoStr, g.streams, g.cycle = nil, nil, nil, nil
		// The source is finished: a later Pause/Resume must not replay it.
		g.src, g.resumeMs = nil, 0
		// EOF doesn't fire OnUpgrade — OnStreamEnd already signals
		// end-of-stream and the caller drives any MTProto state change
		// from there.
		fn := g.ev.OnStreamEnd
		g.mu.Unlock()

		// Streamer.Stop blocks until its run goroutine exits; that exit
		// dispatches an end event for this cycle, which is now stale.
		if sibling != nil {
			sibling.Stop()
		}
		_ = cycle.Close()

		if fn == nil {
			return
		}
		if hadVideo {
			fn(models.Video, models.Camera, err)
		}
		if hadAudio {
			fn(models.Audio, models.Microphone, err)
		}
	})
}

// currentStateLocked computes the MediaState a hypothetical OnUpgrade
// callback would report right now. Caller must hold g.mu (read or
// write — read fields only).
//
// Paused and PresentationPaused both follow g.muted||g.paused — the
// outgoing media is not flowing whenever the user muted or paused, so
// the MTProto-mirrored "paused" flags flip together. The library has
// no presentation source, so PresentationPaused has no independent
// state — exposing it keeps downstream MTProto code uniform.
func (g *GroupCall) currentStateLocked() models.MediaState {
	silent := g.muted || g.paused
	return models.MediaState{
		Muted:              g.muted,
		Paused:             silent,
		VideoStopped:       g.videoStr == nil,
		PresentationPaused: silent,
	}
}

// fireUpgradeIfChangedLocked submits an OnUpgrade dispatch only if the
// current state differs from prev. Caller must hold g.mu. Dispatch is
// async via the shared dispatcher so callers can safely re-enter Client
// API from inside the callback.
//
// The nil-check on disp/OnUpgrade runs *before* the second
// currentStateLocked call — when no listener is wired up, we skip the
// struct literal entirely. Cheap, but every Mute/Pause/Resume/Unmute
// goes through here.
func (g *GroupCall) fireUpgradeIfChangedLocked(prev models.MediaState) {
	if g.disp == nil || g.ev.OnUpgrade == nil {
		return
	}
	cur := g.currentStateLocked()
	if prev == cur {
		return
	}
	g.disp.Submit(func() { g.ev.OnUpgrade(cur) })
}

// Pause / Resume / Mute / Unmute fire OnUpgrade on real transitions so
// callers can drive the matching MTProto participant flags off the
// callback. No-op toggles (already in the target state) stay silent —
// fireUpgradeIfChangedLocked skips when prev == cur. Stop and
// SetStreamSources remain silent: Stop tears the call down (no peer
// left to mirror to) and SetStreamSources is the caller's own
// transition (they already know the new VideoStopped). Spontaneous
// transitions (video leg EOF mid-stream, ICE Failed/Closed) still
// fire from handleStreamerEnd / the PC state callback.
func (g *GroupCall) Pause() (bool, error) {
	if g.closed.Load() {
		return false, models.ErrClosed
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.paused {
		return false, nil
	}
	prev := g.currentStateLocked()
	g.paused = true
	// Block the pull loop on the streamer's gate without killing ffmpeg.
	// The OS pipe absorbs the next ~1s of frames; Resume wakes the loop.
	if g.audioStr != nil {
		g.audioStr.SetPaused(true)
	}
	if g.videoStr != nil {
		g.videoStr.SetPaused(true)
	}
	g.fireUpgradeIfChangedLocked(prev)
	return true, nil
}

func (g *GroupCall) Resume() (bool, error) {
	if g.closed.Load() {
		return false, models.ErrClosed
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.paused {
		return false, nil
	}
	prev := g.currentStateLocked()
	g.paused = false
	// If streamers exist (gate-paused), just unblock them. Otherwise, the
	// source was never started (e.g. paused before SetStreamSources) — start now.
	if g.audioStr != nil || g.videoStr != nil {
		if g.audioStr != nil {
			g.audioStr.SetPaused(false)
		}
		if g.videoStr != nil {
			g.videoStr.SetPaused(false)
		}
		g.fireUpgradeIfChangedLocked(prev)
		return true, nil
	}
	if g.src == nil {
		g.fireUpgradeIfChangedLocked(prev)
		return true, nil
	}
	err := g.startLocked(context.Background())
	g.fireUpgradeIfChangedLocked(prev)
	return true, err
}

func (g *GroupCall) Mute() (bool, error) {
	if g.closed.Load() {
		return false, models.ErrClosed
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.muted {
		return false, nil
	}
	prev := g.currentStateLocked()
	g.muted = true
	if g.audioStr != nil {
		g.audioStr.SetMuted(true)
	}
	g.fireUpgradeIfChangedLocked(prev)
	return true, nil
}

func (g *GroupCall) Unmute() (bool, error) {
	if g.closed.Load() {
		return false, models.ErrClosed
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.muted {
		return false, nil
	}
	prev := g.currentStateLocked()
	g.muted = false
	if g.audioStr != nil {
		g.audioStr.SetMuted(false)
	}
	g.fireUpgradeIfChangedLocked(prev)
	return true, nil
}

func (g *GroupCall) Stop() error {
	if !g.closed.CompareAndSwap(false, true) {
		return nil
	}
	g.mu.Lock()
	g.stopStreamersLocked()
	g.src = nil
	g.srcEncOpt = media.EncodeOptions{}
	g.resumeMs = 0
	g.paused = false
	g.muted = false
	g.videoOff = false
	g.mu.Unlock()
	// Intentionally no fireUpgradeIfChangedLocked here — Stop is
	// user-initiated, the caller already knows the call is gone (same
	// principle as OnStreamEnd being skipped on Stop). Firing
	// videoStopped=true after a vplay→Stop would be inconsistent with
	// play→Stop (which fires nothing because the state never changed).
	return g.pc.Close()
}

// SeekBy shifts playback by deltaMs relative to the current position
// (positive forward, negative backward). If the resulting position is
// below 0 the seek is treated as an EOF: streamers are stopped and
// OnStreamEnd fires through the normal handleStreamerEnd path. Forward
// overshoots past source duration are detected naturally — ffmpeg's
// `-ss` lands past the end, the OGG/IVF reader sees zero frames, and
// the streamer EOFs on its own.
//
// Returns ErrSeekUnsupported if the source does not implement
// media.SeekableSource and ErrNoSource if no source is currently
// playing. Does not fire OnUpgrade — the user initiated the move
// (same principle as SetSource).
func (g *GroupCall) SeekBy(deltaMs int64) error {
	if g.closed.Load() {
		return models.ErrClosed
	}
	g.mu.Lock()
	if g.src == nil {
		g.mu.Unlock()
		return models.ErrNoSource
	}
	if _, ok := g.src.(media.SeekableSource); !ok {
		g.mu.Unlock()
		return models.ErrSeekUnsupported
	}
	if deltaMs == 0 {
		g.mu.Unlock()
		return nil
	}
	var streamerMs uint64
	switch {
	case g.audioStr != nil:
		streamerMs = g.audioStr.ElapsedMs()
	case g.videoStr != nil:
		streamerMs = g.videoStr.ElapsedMs()
	}
	target := int64(g.resumeMs+streamerMs) + deltaMs
	if target < 0 {
		// Underflow ends the source like EOF: stop it and fire
		// OnStreamEnd explicitly (clearing the cycle makes the stopped
		// legs' own end events stale).
		hadAudio, hadVideo := g.hadAudio, g.hadVideo
		g.stopStreamersLocked()
		g.cycle = nil
		g.src, g.resumeMs = nil, 0
		fn := g.ev.OnStreamEnd
		g.mu.Unlock()
		if fn != nil && g.disp != nil {
			g.disp.Submit(func() {
				if g.closed.Load() {
					return
				}
				if hadVideo {
					fn(models.Video, models.Camera, nil)
				}
				if hadAudio {
					fn(models.Audio, models.Microphone, nil)
				}
			})
		}
		return nil
	}
	// Normal seek: kill the old streamers (their end events go stale
	// with the cycle), advance resumeMs, reopen via OpenAt.
	g.stopStreamersLocked()
	g.cycle = nil
	g.resumeMs = uint64(target)
	err := g.startLocked(context.Background())
	g.mu.Unlock()
	return err
}

func (g *GroupCall) ElapsedMs() uint64 {
	g.mu.RLock()
	str := g.audioStr
	if str == nil {
		str = g.videoStr
	}
	base := g.resumeMs
	g.mu.RUnlock()
	if str == nil {
		return base
	}
	return base + str.ElapsedMs()
}

func (g *GroupCall) State() models.MediaState {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.currentStateLocked()
}

func (g *GroupCall) NetState() models.ConnState {
	return models.ConnState(g.netState.Load())
}

// AudioSSRC is exposed so callers can pass it as the Source param to
// phone.LeaveGroupCall.
func (g *GroupCall) AudioSSRC() uint32 { return g.pc.AudioSSRC() }
