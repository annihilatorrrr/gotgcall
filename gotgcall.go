// Package gotgcall is a pure-Go library for streaming audio and video
// into Telegram group calls. The public API mirrors ntgcalls method
// names so bot code translates one-to-one.
//
// The library is blob-only: signaling JSON is exchanged through your
// own MTProto client (typically gogram). Two calls are required:
//
//	params, _ := client.CreateCall(chatID)
//	resp, _   := tg.PhoneJoinGroupCall(... Params: &DataJson{Data: params})
//	client.Connect(chatID, resp.Updates[...].Call.Params.Data)
//	client.SetStreamSources(chatID, gotgcall.FromFile("song.mp3", gotgcall.EncodeOptions{}))
//
// See README.md for the full pattern.
package gotgcall

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/annihilatorrrr/gotgcall/instances"
	"github.com/annihilatorrrr/gotgcall/media"
	"github.com/annihilatorrrr/gotgcall/models"
	"github.com/annihilatorrrr/gotgcall/utils"
	"github.com/annihilatorrrr/gotgcall/wrtc"
)

// NetworkType is re-exported for WithNetworkTypes.
type NetworkType = wrtc.NetworkType

const (
	NetworkTypeUDP4 = wrtc.NetworkTypeUDP4
	NetworkTypeUDP6 = wrtc.NetworkTypeUDP6
	NetworkTypeTCP4 = wrtc.NetworkTypeTCP4
	NetworkTypeTCP6 = wrtc.NetworkTypeTCP6
)

// --- Re-exports for ergonomics -------------------------------------------------

type (
	Source           = media.Source
	SeekableSource   = media.SeekableSource
	MultiShellSource = media.MultiShellSource
	EncodeOptions    = media.EncodeOptions
	Track            = media.Track

	StreamType  = models.StreamType
	Device      = models.Device
	MediaState  = models.MediaState
	NetworkInfo = models.NetworkInfo
	ConnState   = models.ConnState
	CallInfo    = models.CallInfo
)

const (
	TrackAudio = media.TrackAudio
	TrackVideo = media.TrackVideo

	Audio      = models.Audio
	Video      = models.Video
	Microphone = models.Microphone
	Camera     = models.Camera

	Connecting   = models.Connecting
	Connected    = models.Connected
	Disconnected = models.Disconnected
	Failed       = models.Failed
	Closed       = models.Closed
)

var (
	FromFile   = media.FromFile
	FromURL    = media.FromURL
	FromShell  = media.FromShell
	FromShells = media.FromShells
)

// --- Errors (re-export for branchable errors.Is) -------------------------------

var (
	ErrConnectionExists    = models.ErrConnectionExists
	ErrConnectionNotFound  = models.ErrConnectionNotFound
	ErrConnectionFailed    = models.ErrConnectionFailed
	ErrInvalidParams       = models.ErrInvalidParams
	ErrUnsupportedCallMode = models.ErrUnsupportedCallMode
	ErrFFmpegSpawn         = models.ErrFFmpegSpawn
	ErrFFmpegCrashed       = models.ErrFFmpegCrashed
	ErrFile                = models.ErrFile
	ErrClosed              = models.ErrClosed
	ErrInternal            = models.ErrInternal
	ErrNotConnected        = models.ErrNotConnected
	ErrWrongMode           = models.ErrWrongMode
)

// --- Options -------------------------------------------------------------------

type Option func(*config)

type config struct {
	logger           *slog.Logger
	ffmpegPath       string
	networkTypes     []NetworkType
	certPoolSize     int
	dispatchBuf      int
	connectTimeout   time.Duration
	sharedUDPMux     bool
	ffmpegStderrLog  bool
	pionTraceAsDebug bool
	logICECandidates bool
}

func defaultConfig() config {
	return config{
		logger:       slog.New(slog.DiscardHandler),
		ffmpegPath:   "ffmpeg",
		sharedUDPMux: false,
		certPoolSize: 8,
		dispatchBuf:  256,
	}
}

// WithLogger sets a structured logger for internal events.
func WithLogger(l *slog.Logger) Option {
	return func(c *config) {
		if l != nil {
			c.logger = l
		}
	}
}

// WithFFmpegPath overrides the ffmpeg binary path (default "ffmpeg").
func WithFFmpegPath(p string) Option {
	return func(c *config) {
		if p != "" {
			c.ffmpegPath = p
		}
	}
}

// WithSharedUDPMux makes all calls share one UDP socket for ICE traffic.
// Useful for high-concurrency setups (100+ simultaneous calls).
func WithSharedUDPMux() Option {
	return func(c *config) { c.sharedUDPMux = true }
}

// WithDTLSCertPool sets the size of the pre-generated DTLS certificate
// pool. Larger pools absorb bigger call-creation bursts without keygen
// latency. 0 disables pre-generation.
func WithDTLSCertPool(n int) Option {
	return func(c *config) { c.certPoolSize = n }
}

// WithDispatchBuffer sizes the event dispatcher's channel. Default 256.
func WithDispatchBuffer(n int) Option {
	return func(c *config) {
		if n > 0 {
			c.dispatchBuf = n
		}
	}
}

// WithNetworkTypes overrides the ICE candidate network-type whitelist.
// Default is UDP4+UDP6 (matching ntgcalls' PORTALLOCATOR_ENABLE_IPV6).
// Telegram's SFU accepts IPv6 candidates and dual-stack hosts get more
// candidate pairs. Add TCP for restrictive environments where UDP is blocked.
//
//	gotgcall.WithNetworkTypes(
//	    gotgcall.NetworkTypeUDP4,
//	    gotgcall.NetworkTypeUDP6,
//	    gotgcall.NetworkTypeTCP4,
//	)
func WithNetworkTypes(types ...NetworkType) Option {
	return func(c *config) {
		if len(types) > 0 {
			c.networkTypes = types
		}
	}
}

// WithConnectTimeout overrides how long SetSource/Resume wait for the WebRTC
// connection to reach Connected before giving up. Default 10s — matches
// ntgcalls' own internal connection timeout. With pion running as
// ICE-CONTROLLED (since v0.6.26) and Telegram's edges responding within
// 50-300ms in healthy networks, 10s is generous. Set higher on
// unstable networks where ICE re-pairing on cross-DC moves takes longer.
func WithConnectTimeout(d time.Duration) Option {
	return func(c *config) {
		if d > 0 {
			c.connectTimeout = d
		}
	}
}

// WithDebugLogs is a convenience that installs a Debug-level text handler
// writing to os.Stderr. Equivalent to:
//
//	WithLogger(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})))
//
// Use this when reporting bugs — debug-level output covers ICE/DTLS state,
// ffmpeg exit codes, streamer pacing, and pion-internal events bridged
// through the new pion→slog adapter.
func WithDebugLogs() Option {
	return func(c *config) {
		c.logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
			Level: slog.LevelDebug,
		}))
	}
}

// WithFFmpegStderrLog tees ffmpeg's stderr output to the library logger at
// Debug level while the process is running. Without this, ffmpeg stderr is
// only surfaced in the final error message (last 512 bytes) when the
// subprocess crashes — useful for crash diagnosis but useless for "ffmpeg
// is running, but I see no audio" symptoms. Enable for verbose diagnosis.
func WithFFmpegStderrLog() Option {
	return func(c *config) { c.ffmpegStderrLog = true }
}

// WithPionTraceLogs remaps pion's Trace-level output (per-ICE-check, per-
// candidate-pair, per-binding-request) to slog.LevelDebug instead of the
// default sub-debug level. Use this when ICE is stuck in "Checking" and you
// need to see exactly which candidate pairs are being tried, which fail, and
// which (if any) get a response from the remote.
//
//	gotgcall.New(gotgcall.WithDebugLogs(), gotgcall.WithPionTraceLogs())
//
// Volume warning: ICE Trace at scale is several hundred lines per call. Use
// for diagnosis, not steady-state production.
func WithPionTraceLogs() Option {
	return func(c *config) { c.pionTraceAsDebug = true }
}

// WithICECandidateLogs logs every locally-gathered ICE candidate (host /
// srflx / relay, address, port, foundation) at Debug level via the
// PeerConnection's OnICECandidate hook. Pairs well with WithPionTraceLogs
// for "why is ICE failing" diagnosis: this option shows what we offered,
// pion-trace shows which pairs were tried, and the remote answer's
// candidate list (parsed in jsonparams) shows what Telegram returned.
func WithICECandidateLogs() Option {
	return func(c *config) { c.logICECandidates = true }
}

// WithVerboseConnectionLogs is a one-flag bundle for diagnosing
// "ICE/DTLS did not reach Connected within Ns" failures. It enables:
//   - Debug-level slog handler to stderr (WithDebugLogs)
//   - Per-candidate gather logging (WithICECandidateLogs)
//   - Pion's per-binding-request / per-pair-check trace at Debug
//     (WithPionTraceLogs)
//
// Use when reporting a stuck-in-Connecting bug — the resulting log
// contains every signal the library can surface about the ICE state
// machine. Library still also emits an Info-level checking-phase
// snapshot every ~5 seconds without this flag, so most issues can be
// triaged from a non-Debug run; flip this when those Info lines aren't
// enough.
func WithVerboseConnectionLogs() Option {
	return func(c *config) {
		c.logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
			Level: slog.LevelDebug,
		}))
		c.logICECandidates = true
		c.pionTraceAsDebug = true
	}
}

// --- Client --------------------------------------------------------------------

// Client multiplexes many concurrent group calls behind a single
// process-wide handle. Safe for concurrent use.
type Client struct {
	factory            *wrtc.Factory
	disp               *utils.Dispatcher
	onStreamEnd        func(chatID int64, t StreamType, d Device, err error)
	onConnectionChange func(chatID int64, info NetworkInfo)
	onUpgrade          func(chatID int64, state MediaState)
	calls              sync.Map // map[int64]instances.Call
	createMu           sync.Map // map[int64]*sync.Mutex — gates CreateCall/StartRTMP per chat
	cfg                config
	cbMu               sync.RWMutex
	closed             atomic.Bool
}

// acquireCreate serialises the construction phase of CreateCall and
// StartRTMP for a single chat. Returns the unlock function. The per-chat
// mutex is kept in the map for the lifetime of the process (sizeof
// sync.Mutex per unique chatID ever started — negligible).
func (c *Client) acquireCreate(chatID int64) func() {
	v, _ := c.createMu.LoadOrStore(chatID, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// New constructs a Client with the given options. Fails fast if the ffmpeg
// binary isn't on PATH (or wherever WithFFmpegPath points) so callers see
// the error at startup rather than on first stream.
func New(opts ...Option) (*Client, error) {
	cfg := defaultConfig()
	for _, opt := range opts {
		opt(&cfg)
	}
	if _, err := exec.LookPath(cfg.ffmpegPath); err != nil {
		return nil, fmt.Errorf("ffmpeg binary not found at %q: %w — install ffmpeg or override with WithFFmpegPath",
			cfg.ffmpegPath, err)
	}
	media.SetFFmpegPath(cfg.ffmpegPath)
	media.SetLogger(cfg.logger)
	media.SetStderrLog(cfg.ffmpegStderrLog)

	factory, err := wrtc.NewFactory(wrtc.FactoryOptions{
		Logger:           cfg.logger,
		SharedUDPMux:     cfg.sharedUDPMux,
		CertPoolSize:     cfg.certPoolSize,
		NetworkTypes:     cfg.networkTypes,
		PionTraceAsDebug: cfg.pionTraceAsDebug,
		LogICECandidates: cfg.logICECandidates,
	})
	if err != nil {
		return nil, err
	}
	return &Client{
		cfg:     cfg,
		factory: factory,
		disp:    utils.NewDispatcher(cfg.dispatchBuf, cfg.logger),
	}, nil
}

// Close stops every call and releases resources. Idempotent.
func (c *Client) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	c.calls.Range(func(_, v any) bool {
		_ = v.(instances.Call).Stop()
		return true
	})
	if c.factory != nil {
		_ = c.factory.Close()
	}
	if c.disp != nil {
		c.disp.Close()
	}
	return nil
}

// --- Lifecycle: WebRTC mode ----------------------------------------------------

// CreateCall starts a new WebRTC group-call instance for chatID and
// returns the JSON params the caller must pass to phone.JoinGroupCall.
//
// Concurrent CreateCall / StartRTMP calls for the same chat are
// serialized; the first one wins, others get ErrConnectionExists
// without allocating a pion PeerConnection.
func (c *Client) CreateCall(chatID int64) (string, error) {
	if c.closed.Load() {
		return "", ErrClosed
	}
	unlock := c.acquireCreate(chatID)
	defer unlock()
	if c.callIsLive(chatID) {
		return "", ErrConnectionExists
	}
	gc, err := instances.NewGroupCall(chatID, c.factory, c.disp, c.cfg.logger, c.cfg.connectTimeout, c.eventsFor(chatID))
	if err != nil {
		return "", err
	}
	c.calls.Store(chatID, instances.Call(gc))
	params, err := gc.CreateLocalParams()
	if err != nil {
		c.reap(chatID)
		return "", err
	}
	return params, nil
}

// Connect finishes the WebRTC handshake using Telegram's response JSON.
// On error the call is auto-reaped so the caller can immediately retry
// CreateCall without coordinating a separate Stop.
func (c *Client) Connect(chatID int64, telegramParams string) error {
	call, err := c.lookup(chatID)
	if err != nil {
		return err
	}
	if err = call.Connect(telegramParams); err != nil {
		c.reap(chatID)
		return err
	}
	return nil
}

// --- Lifecycle: RTMP mode ------------------------------------------------------

// StartRTMP creates an RTMP-push call for chatID. The caller obtains
// rtmpURL via phone.GetGroupCallStreamRtmpUrl gogram-side. Serialised
// with CreateCall via the same per-chat creation mutex.
func (c *Client) StartRTMP(chatID int64, rtmpURL string) error {
	if c.closed.Load() {
		return ErrClosed
	}
	unlock := c.acquireCreate(chatID)
	defer unlock()
	if c.callIsLive(chatID) {
		return ErrConnectionExists
	}
	rc := instances.NewRTMPCall(chatID, rtmpURL, c.disp, c.cfg.logger, c.eventsFor(chatID))
	c.calls.Store(chatID, instances.Call(rc))
	return nil
}

// callIsLive reports whether an existing call entry for chatID is still
// usable. If the entry is in a terminal state (Failed / Closed) — for
// example after pion declared ICE failed permanently or the previous
// SetSource returned "connection closed during setup" — it is reaped
// here so the caller can immediately create a fresh call instead of
// being told ErrConnectionExists for a corpse. Callers hold the per-chat create mutex via acquireCreate.
func (c *Client) callIsLive(chatID int64) bool {
	v, ok := c.calls.Load(chatID)
	if !ok {
		return false
	}
	prev := v.(instances.Call)
	if state := prev.NetState(); state == models.Failed || state == models.Closed {
		c.calls.Delete(chatID)
		_ = prev.Stop()
		return false
	}
	return true
}

// reap removes the per-chat call entry and tears it down. Used by every
// setup-phase API (CreateCall, Connect, SetStreamSources) when the call
// returns an error so the caller doesn't need to remember to invoke Stop
// separately. Safe if the entry is already gone (e.g. concurrent Stop).
//
// Intentionally does NOT touch createMu: a concurrent goroutine may be
// parked on mu.Lock() from acquireCreate having already resolved the
// pointer. Deleting the map entry would let the next acquireCreate
// LoadOrStore a fresh mutex, and the two goroutines would then hold
// different mutexes for the same chat and race inside CreateCall.
func (c *Client) reap(chatID int64) {
	v, ok := c.calls.LoadAndDelete(chatID)
	if !ok {
		return
	}
	_ = v.(instances.Call).Stop()
}

// --- Lifecycle: source control --------------------------------------------------

// SetStreamSources installs or replaces the streaming source for chatID.
// Encode options (FPS, tracks, bitrates) ride along with the Source — set
// them on the constructor (FromFile/FromURL).
//
// On error the call is auto-reaped (closed and removed from the per-client
// registry) so the caller can immediately retry CreateCall without first
// invoking Stop. This covers the failure modes the user would otherwise
// need to clean up by hand: ICE/DTLS gate timeout, "connection closed
// during setup", and ffmpeg / source-open errors.
func (c *Client) SetStreamSources(chatID int64, src Source) error {
	call, err := c.lookup(chatID)
	if err != nil {
		return err
	}
	if err = call.SetSource(context.Background(), src); err != nil {
		c.reap(chatID)
		return err
	}
	return nil
}

func (c *Client) Pause(chatID int64) (bool, error) {
	call, err := c.lookup(chatID)
	if err != nil {
		return false, err
	}
	return call.Pause()
}

func (c *Client) Resume(chatID int64) (bool, error) {
	call, err := c.lookup(chatID)
	if err != nil {
		return false, err
	}
	return call.Resume()
}

func (c *Client) Mute(chatID int64) (bool, error) {
	call, err := c.lookup(chatID)
	if err != nil {
		return false, err
	}
	return call.Mute()
}

func (c *Client) Unmute(chatID int64) (bool, error) {
	call, err := c.lookup(chatID)
	if err != nil {
		return false, err
	}
	return call.Unmute()
}

// SeekBy shifts playback by deltaMs (signed; positive forward, negative
// backward) relative to the current position. Underflow below 0 fires
// OnStreamEnd instead of seeking — caller's auto-skip-to-next logic
// can drive off the same callback as natural end-of-stream. Forward
// overshoots past source duration are detected by ffmpeg yielding zero
// frames after the seek (also natural OnStreamEnd path).
//
// Returns ErrSeekUnsupported if the active source is not seekable
// (live FromShell commands that ignore -ss fall here), ErrNoSource if
// no source is currently playing, and ErrConnectionNotFound if there's
// no call for chatID.
func (c *Client) SeekBy(chatID int64, deltaMs int64) error {
	call, err := c.lookup(chatID)
	if err != nil {
		return err
	}
	return call.SeekBy(deltaMs)
}

// Stop tears down the call and clears the per-chat call entry. The
// per-chat create-mutex is intentionally kept (see reap) so a concurrent
// CreateCall parked on mu.Lock() doesn't end up racing a later one on a
// fresh mutex. The mutex memory is negligible (sizeof sync.Mutex per
// chatID ever used).
func (c *Client) Stop(chatID int64) error {
	call, err := c.lookup(chatID)
	if err != nil {
		return err
	}
	c.calls.Delete(chatID)
	return call.Stop()
}

// --- Introspection -------------------------------------------------------------

// Time returns elapsed ms of media pushed.
func (c *Client) Time(chatID int64) (uint64, error) {
	call, err := c.lookup(chatID)
	if err != nil {
		return 0, err
	}
	return call.ElapsedMs(), nil
}

// GetState returns the current media-state (mute/pause flags).
func (c *Client) GetState(chatID int64) (MediaState, error) {
	call, err := c.lookup(chatID)
	if err != nil {
		return MediaState{}, err
	}
	return call.State(), nil
}

// Calls returns a snapshot of all active calls.
func (c *Client) Calls() map[int64]CallInfo {
	out := make(map[int64]CallInfo)
	c.calls.Range(func(k, v any) bool {
		id := k.(int64)
		call := v.(instances.Call)
		out[id] = CallInfo{CaptureTimeMs: call.ElapsedMs()}
		return true
	})
	return out
}

// AudioSSRC returns the audio SSRC of a WebRTC call. Pass as Source to
// phone.LeaveGroupCall. Returns ErrWrongMode for RTMP calls.
func (c *Client) AudioSSRC(chatID int64) (uint32, error) {
	call, err := c.lookup(chatID)
	if err != nil {
		return 0, err
	}
	gc, ok := call.(*instances.GroupCall)
	if !ok {
		return 0, ErrWrongMode
	}
	return gc.AudioSSRC(), nil
}

// --- Callbacks -----------------------------------------------------------------

// OnStreamEnd registers a callback fired when a track ends from EOF or
// ffmpeg crash. Manual Stop / SetSource don't fire — the caller already
// knows they initiated those.
//
// For video+audio sources (vplay) the callback fires twice in fixed
// order: first StreamType=Video, then StreamType=Audio. Audio-only and
// video-only sources fire once. Called on the dispatcher goroutine so
// it is safe to re-enter the Client API from within.
func (c *Client) OnStreamEnd(fn func(chatID int64, t StreamType, d Device, err error)) {
	c.cbMu.Lock()
	c.onStreamEnd = fn
	c.cbMu.Unlock()
}

// OnConnectionChange registers a callback for ICE/DTLS state transitions.
func (c *Client) OnConnectionChange(fn func(chatID int64, info NetworkInfo)) {
	c.cbMu.Lock()
	c.onConnectionChange = fn
	c.cbMu.Unlock()
}

// OnUpgrade registers a callback fired whenever the outgoing
// MediaState flips a bit. Mirror of ntgcalls' onUpgrade(MediaState).
//
// Fires on:
//   - Mute / Unmute — flips Muted (and Paused / PresentationPaused
//     follow, since the mic is no longer producing audio).
//   - Pause / Resume — flips Paused and PresentationPaused while
//     Muted stays put.
//   - A video leg ending mid-stream (EOF / ffmpeg crash) — VideoStopped
//     flips false→true. Audio-only sources had VideoStopped=true
//     already, so they don't fire here.
//   - The WebRTC PC transitioning to Failed/Closed while video was
//     active — same VideoStopped flip as the EOF case.
//
// Does NOT fire on:
//   - SetStreamSources — the caller chose the new source and already
//     knows the resulting VideoStopped (true for Play / audio-only,
//     false for VPlay / audio+video). A same-shape re-source (e.g.
//     Play → Play) would have prev == cur anyway.
//   - Stop — the call is gone, there is nothing left to mirror.
//   - No-op toggles (Mute when already muted, etc.) — the helper
//     skips dispatch when prev == cur.
//
// Fires on the dispatcher goroutine, so it is safe to re-enter the
// Client API from inside the callback.
func (c *Client) OnUpgrade(fn func(chatID int64, state MediaState)) {
	c.cbMu.Lock()
	c.onUpgrade = fn
	c.cbMu.Unlock()
}

// --- internals -----------------------------------------------------------------

func (c *Client) lookup(chatID int64) (instances.Call, error) {
	if c.closed.Load() {
		return nil, ErrClosed
	}
	v, ok := c.calls.Load(chatID)
	if !ok {
		return nil, ErrConnectionNotFound
	}
	return v.(instances.Call), nil
}

func (c *Client) eventsFor(chatID int64) instances.GroupCallEvents {
	return instances.GroupCallEvents{
		OnStreamEnd: func(t models.StreamType, d models.Device, err error) {
			c.cbMu.RLock()
			fn := c.onStreamEnd
			c.cbMu.RUnlock()
			if fn != nil {
				fn(chatID, t, d, err)
			}
		},
		OnConnectionChange: func(info models.NetworkInfo) {
			c.cbMu.RLock()
			fn := c.onConnectionChange
			c.cbMu.RUnlock()
			if fn != nil {
				fn(chatID, info)
			}
		},
		OnUpgrade: func(state models.MediaState) {
			c.cbMu.RLock()
			fn := c.onUpgrade
			c.cbMu.RUnlock()
			if fn != nil {
				fn(chatID, state)
			}
		},
	}
}
