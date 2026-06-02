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
	// OnMediaStateChange fires when the outgoing media state transitions
	// (Muted, Paused, or VideoStopped changes). Only the transition fires —
	// no-op mutations (Mute when already muted) don't trigger.
	OnMediaStateChange func(state models.MediaState)
}

// GroupCall is the WebRTC call instance for one chat.
type GroupCall struct {
	ev GroupCallEvents

	src       media.Source
	pc        *wrtc.PeerConnection
	log       *slog.Logger
	disp      *utils.Dispatcher
	streams   *media.Streams
	audioStr  *media.Streamer
	videoStr  *media.Streamer
	connected chan struct{}
	srcEncOpt media.EncodeOptions
	chatID    int64
	resumeMs  uint64 // seek offset captured on Pause; injected via SeekableSource.OpenAt on Resume

	mu            sync.RWMutex
	connectedOnce sync.Once
	netState      atomic.Int32 // models.ConnState
	closed        atomic.Bool
	switching     atomic.Bool // true while SetSource is replacing the source; suppresses OnStreamEnd for the old streamer
	connectCalled atomic.Bool
	paused        bool
	muted         bool
	videoOff      bool
}

// NewGroupCall constructs a fresh call. Caller threads pion factory + logger.
func NewGroupCall(chatID int64, factory *wrtc.Factory, disp *utils.Dispatcher, log *slog.Logger, ev GroupCallEvents) (*GroupCall, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	pc, err := wrtc.NewPeerConnection(factory, log)
	if err != nil {
		return nil, err
	}
	gc := &GroupCall{
		chatID:    chatID,
		pc:        pc,
		log:       log.With(slog.Int64("chat", chatID)),
		disp:      disp,
		ev:        ev,
		connected: make(chan struct{}),
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
				gc.fireMediaStateIfChangedLocked(prev)
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

	// Snapshot the state needed for the prepare phase. We RLock briefly
	// just for the read; the actual swap happens under a write Lock in
	// phase 2.
	g.mu.RLock()
	muted := g.muted
	videoOff := g.videoOff
	g.mu.RUnlock()

	// Phase 1 (OUTSIDE g.mu): spawn ffmpeg + read OGG/IVF headers. The
	// ivfreader/oggreader header parse blocks reading from ffmpeg's pipe
	// until ffmpeg flushes its first page (200 ms-1 s on cold start), and
	// holding g.mu across that window stalls every concurrent Pause /
	// Mute / GetState / ElapsedMs caller for the same call.
	streams, audioStr, videoStr, err := g.prepareStreamers(ctx, src, srcEncOpt, muted, videoOff)
	if err != nil {
		return err
	}

	// Gate: wait for WebRTC to reach Connected before starting streamers.
	// Samples written before ICE+DTLS completes are silently dropped by
	// pion (no SRTP binding yet), causing silence on first play. The
	// channel also closes on Failed/Closed so Stop() during the wait
	// doesn't hang for the full timeout.
	if models.ConnState(g.netState.Load()) != models.Connected {
		connectTimer := time.NewTimer(15 * time.Second)
		select {
		case <-g.connected:
			connectTimer.Stop()
		case <-connectTimer.C:
			_ = streams.Close()
			if !g.connectCalled.Load() {
				return fmt.Errorf("%w: timed out waiting for WebRTC — Connect() was never called", models.ErrNotConnected)
			}
			return fmt.Errorf("%w: ICE/DTLS did not reach Connected within 15s", models.ErrNotConnected)
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

	g.switching.Store(true)
	defer g.switching.Store(false)

	prev := g.currentStateLocked()
	g.stopStreamersLocked()
	g.src = src
	g.resumeMs = 0
	g.srcEncOpt = srcEncOpt
	g.streams = streams
	g.audioStr = audioStr
	g.videoStr = videoStr
	paused := g.paused
	if audioStr != nil {
		if paused {
			audioStr.SetPaused(true)
		}
		audioStr.Start()
	}
	if videoStr != nil {
		if paused {
			videoStr.SetPaused(true)
		}
		videoStr.Start()
	}
	g.fireMediaStateIfChangedLocked(prev)
	g.mu.Unlock()
	return nil
}

// prepareStreamers spawns the source's ffmpeg leg(s) and constructs (but
// does NOT Start) the Streamers. Runs outside g.mu so the ffmpeg-header
// wait doesn't block other concurrent operations on the same call.
func (g *GroupCall) prepareStreamers(ctx context.Context, src media.Source, encOpt media.EncodeOptions, muted, videoOff bool) (*media.Streams, *media.Streamer, *media.Streamer, error) {
	streams, err := src.Open(ctx)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("open source: %w", err)
	}
	var audioStr, videoStr *media.Streamer
	if streams.Audio != nil {
		fr, frErr := media.NewOpusFrameReader(streams.Audio)
		if frErr != nil {
			// Promoted to Warn so default-Info logging surfaces "/play
			// silently played no audio" without users having to flip to
			// Debug. The video leg (if any) still plays.
			g.log.Warn("audio track unavailable, skipping", slog.Any("err", frErr))
		} else {
			var s *media.Streamer
			s = media.NewStreamer(ctx, fr, g.pc.AudioTrack(), g.log, func(endErr error) {
				g.handleStreamerEnd(models.Audio, models.Microphone, endErr, s)
			})
			s.SetMuted(muted)
			audioStr = s
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
			var s *media.Streamer
			s = media.NewStreamer(ctx, fr, g.pc.VideoTrack(), g.log, func(endErr error) {
				g.handleStreamerEnd(models.Video, models.Camera, endErr, s)
			})
			s.SetMuted(videoOff)
			videoStr = s
		}
	}
	if audioStr == nil && videoStr == nil {
		_ = streams.Close()
		return nil, nil, nil, fmt.Errorf("%w: source has no playable audio or video stream", models.ErrFile)
	}
	return streams, audioStr, videoStr, nil
}

// startLocked is kept as a thin wrapper used by Resume to rehydrate
// streamers after a Pause-time tear-down. (Today Pause keeps streamers
// alive via SetPaused, so startLocked only ever runs from a Resume that
// finds them nil — typically because SetSource was called while paused.)
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
	g.streams = streams

	var audioOK, videoOK bool
	if streams.Audio != nil {
		fr, frErr := media.NewOpusFrameReader(streams.Audio)
		if frErr != nil {
			g.log.Warn("audio track unavailable, skipping", slog.Any("err", frErr))
		} else {
			var s *media.Streamer
			s = media.NewStreamer(ctx, fr, g.pc.AudioTrack(), g.log, func(endErr error) {
				g.handleStreamerEnd(models.Audio, models.Microphone, endErr, s)
			})
			s.SetMuted(g.muted)
			s.Start()
			g.audioStr = s
			audioOK = true
		}
	}
	if streams.Video != nil {
		fps := g.srcEncOpt.VideoFPS
		if fps <= 0 {
			fps = 30
		}
		fr, frErr := media.NewVP8FrameReader(streams.Video, fps)
		if frErr != nil {
			g.log.Warn("video track unavailable, skipping", slog.Any("err", frErr))
		} else {
			var s *media.Streamer
			s = media.NewStreamer(ctx, fr, g.pc.VideoTrack(), g.log, func(endErr error) {
				g.handleStreamerEnd(models.Video, models.Camera, endErr, s)
			})
			s.SetMuted(g.videoOff)
			s.Start()
			g.videoStr = s
			videoOK = true
		}
	}
	if !audioOK && !videoOK {
		_ = streams.Close()
		g.streams = nil
		return fmt.Errorf("%w: source has no playable audio or video stream", models.ErrFile)
	}
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

// handleStreamerEnd is the per-streamer onEnd callback. Receives the
// streamer pointer captured at construction so the dispatched cleanup
// can verify the ended streamer is still the current one before
// nil-ing the field (concurrent SetSource may have already replaced it).
func (g *GroupCall) handleStreamerEnd(t models.StreamType, d models.Device, err error, str *media.Streamer) {
	closed := g.closed.Load()
	switching := g.switching.Load()
	g.log.Debug("streamer end",
		slog.Any("type", t), slog.Any("device", d), slog.Any("err", err),
		slog.Bool("closed", closed), slog.Bool("switching", switching))
	if closed || switching {
		return
	}
	if g.disp == nil {
		return
	}
	g.disp.Submit(func() {
		if g.closed.Load() || g.switching.Load() {
			return
		}
		g.mu.Lock()
		prev := g.currentStateLocked()
		switch t {
		case models.Audio:
			if g.audioStr == str {
				g.audioStr = nil
			}
		case models.Video:
			if g.videoStr == str {
				g.videoStr = nil
			}
		}
		g.fireMediaStateIfChangedLocked(prev)
		fn := g.ev.OnStreamEnd
		g.mu.Unlock()
		if fn != nil {
			fn(t, d, err)
		}
	})
}

// currentStateLocked computes the MediaState a hypothetical OnMediaState
// change callback would report right now. Caller must hold g.mu (read or
// write — read fields only).
func (g *GroupCall) currentStateLocked() models.MediaState {
	return models.MediaState{
		Muted:        g.muted,
		Paused:       g.paused,
		VideoStopped: g.videoStr == nil,
	}
}

// fireMediaStateIfChangedLocked submits an OnMediaStateChange dispatch
// only if the current state differs from prev. Caller must hold g.mu.
// Dispatch is async via the shared dispatcher so callers can safely
// re-enter Client API from inside the callback.
func (g *GroupCall) fireMediaStateIfChangedLocked(prev models.MediaState) {
	cur := g.currentStateLocked()
	if prev == cur {
		return
	}
	if g.disp == nil || g.ev.OnMediaStateChange == nil {
		return
	}
	g.disp.Submit(func() { g.ev.OnMediaStateChange(cur) })
}

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
	g.fireMediaStateIfChangedLocked(prev)
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
	// If streamers exist (gate-paused), just unblock them. Otherwise the
	// source was never started (e.g. paused before SetStreamSources) — start now.
	if g.audioStr != nil || g.videoStr != nil {
		if g.audioStr != nil {
			g.audioStr.SetPaused(false)
		}
		if g.videoStr != nil {
			g.videoStr.SetPaused(false)
		}
		g.fireMediaStateIfChangedLocked(prev)
		return true, nil
	}
	if g.src == nil {
		g.fireMediaStateIfChangedLocked(prev)
		return true, nil
	}
	err := g.startLocked(context.Background())
	g.fireMediaStateIfChangedLocked(prev)
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
	g.fireMediaStateIfChangedLocked(prev)
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
	g.fireMediaStateIfChangedLocked(prev)
	return true, nil
}

func (g *GroupCall) Stop() error {
	if !g.closed.CompareAndSwap(false, true) {
		return nil
	}
	g.mu.Lock()
	prev := g.currentStateLocked()
	g.stopStreamersLocked()
	g.src = nil
	g.srcEncOpt = media.EncodeOptions{}
	g.resumeMs = 0
	g.paused = false
	g.muted = false
	g.videoOff = false
	g.fireMediaStateIfChangedLocked(prev)
	g.mu.Unlock()
	return g.pc.Close()
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
	return models.MediaState{Muted: g.muted, Paused: g.paused, VideoStopped: g.videoStr == nil}
}

func (g *GroupCall) NetState() models.ConnState {
	return models.ConnState(g.netState.Load())
}

// AudioSSRC is exposed so callers can pass it as the Source param to
// phone.LeaveGroupCall.
func (g *GroupCall) AudioSSRC() uint32 { return g.pc.AudioSSRC() }
