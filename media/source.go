package media

import (
	"context"
	stdio "io"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/annihilatorrrr/gotgcall/models"
)

// Track is a bitmask selecting which tracks a Source provides.
type Track int

const (
	TrackAudio Track = 1 << iota
	TrackVideo
)

func (t Track) Has(x Track) bool { return t&x != 0 }

// Streams is the encoded output of a Source: ogg/Opus audio and/or IVF/VP8
// video. A nil reader means that track is absent. Close releases any
// underlying ffmpeg processes and pipes.
type Streams struct {
	Audio stdio.Reader
	Video stdio.Reader
	close func() error
}

func (s *Streams) Close() error {
	if s == nil || s.close == nil {
		return nil
	}
	return s.close()
}

// Source is the public input abstraction. A Source is created lazily;
// Open spawns whatever processes are needed and returns the encoded
// audio+video byte streams.
type Source interface {
	Tracks() Track
	Open(ctx context.Context) (*Streams, error)
}

// SeekableSource is a Source that can begin playback at an offset. Only
// file/URL transcode sources implement it; pre-encoded passthrough sources
// and stdin-fed reader sources do not.
type SeekableSource interface {
	Source
	OpenAt(ctx context.Context, offset time.Duration) (*Streams, error)
}

// EncodeOptions tunes the ffmpeg encode for transcoding sources. Zero
// values become sensible defaults.
type EncodeOptions struct {
	VideoBitrateKbps int
	VideoWidth       int
	VideoHeight      int
	VideoFPS         int
	AudioBitrateKbps int
	AudioChannels    int
	// Tracks limits which tracks to produce. Zero means audio+video.
	Tracks Track
}

func (o EncodeOptions) WithDefaults() EncodeOptions { return o.withDefaults() }

func (o EncodeOptions) withDefaults() EncodeOptions {
	if o.VideoBitrateKbps == 0 {
		o.VideoBitrateKbps = 800
	}
	if o.VideoWidth == 0 {
		o.VideoWidth = 1280
	}
	if o.VideoHeight == 0 {
		o.VideoHeight = 720
	}
	if o.VideoFPS == 0 {
		o.VideoFPS = 30
	}
	if o.AudioBitrateKbps == 0 {
		// 128 kbps Opus is the floor for music: below it the encoder strips
		// high-frequency detail (perceived as "hiss") and tightens the bass
		// range (perceived as "thin"). 64 kbps was the previous default —
		// fine for voice/conferencing, terrible for a music bot. Telegram's
		// fmtp negotiates up to 510 kbps so any bump here is safe wire-wise.
		o.AudioBitrateKbps = 128
	}
	if o.AudioChannels == 0 {
		o.AudioChannels = models.DefaultChannelCount
	}
	if o.Tracks == 0 {
		o.Tracks = TrackAudio
	}
	// Requesting video implies audio too — a video file is a video file
	// with audio. Open both ffmpeg legs; if one of the streams is missing,
	// the frame-reader gracefully skips it.
	if o.Tracks.Has(TrackVideo) {
		o.Tracks |= TrackAudio
	}
	return o
}

// ffmpeg binary path; configurable via SetFFmpegPath. Default "ffmpeg".
var ffmpegBinary atomic.Value

func init() { ffmpegBinary.Store("ffmpeg") }

// SetFFmpegPath overrides the binary used for transcoding. Empty resets to "ffmpeg".
func SetFFmpegPath(p string) {
	if p == "" {
		p = "ffmpeg"
	}
	ffmpegBinary.Store(p)
}

// FFmpegPath returns the ffmpeg binary configured via SetFFmpegPath.
func FFmpegPath() string { return ffmpegPath() }

func ffmpegPath() string {
	if v := ffmpegBinary.Load(); v != nil {
		return v.(string)
	}
	return "ffmpeg"
}

// Package-level logger used by ffmpeg-spawning sources for stderr/exit
// diagnostics. Defaults to discard; SetLogger plumbs the Client's logger in.
var mediaLogger atomic.Pointer[slog.Logger]

// SetLogger sets the logger used by media-package ffmpeg shell readers.
// Pass nil to disable logging.
func SetLogger(l *slog.Logger) {
	if l == nil {
		l = slog.New(slog.DiscardHandler)
	}
	mediaLogger.Store(l)
}

func getLogger() *slog.Logger {
	if l := mediaLogger.Load(); l != nil {
		return l
	}
	return slog.New(slog.DiscardHandler)
}

// stderrLog gates whether ShellReader tees the live ffmpeg stderr stream
// into the package logger at Debug level. Off by default — the last 512
// bytes of stderr are always wrapped into the exit error, which suffices
// for crash diagnosis. Enable via gotgcall.WithFFmpegStderrLog() when you
// need to see ffmpeg's warnings ("missing reference frame", "non-monotonic
// dts", etc.) live while the stream runs.
var stderrLog atomic.Bool

// SetStderrLog toggles the live-tee of ffmpeg stderr to the package logger.
func SetStderrLog(on bool) { stderrLog.Store(on) }

// StderrLogEnabled reports the current setting (read by io.ShellReader).
func StderrLogEnabled() bool { return stderrLog.Load() }
