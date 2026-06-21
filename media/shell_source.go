package media

import (
	"context"
	"fmt"
	stdio "io"
	"strconv"
	"strings"
	"sync"
	"time"

	gtio "github.com/annihilatorrrr/gotgcall/io"
	"github.com/annihilatorrrr/gotgcall/models"
)

// FromShell parses cmdline as a shell command (handling double-quoted
// arguments) and spawns it directly via exec — NOT via /bin/sh, so shell
// metacharacters in filenames cannot inject commands.
//
// The command must produce Opus-in-OGG on stdout for audio or VP8-in-IVF
// for video. Missing essentials are filled in automatically:
//
//   - input-side fast-probe flags (`-analyzeduration 0`, `-probesize 64k`)
//     are inserted before `-i` if absent — cuts ~1-2 s of startup latency.
//   - output-side flags (`-c:a libopus`, `-f ogg`, opus pacing/codec
//     params, `pipe:1`) for audio, or (`-c:v libvpx`, `-f ivf`, `pipe:1`)
//     for video, are appended if not already present.
//
// Raw PCM output codecs (pcm_s16le, etc.) are still rejected up front
// since the frame readers can't parse them.
func FromShell(cmdline string, track Track) Source {
	tokens := tokenizeShell(cmdline)
	if len(tokens) == 0 {
		return &shellSource{err: fmt.Errorf("%w: empty command", models.ErrFFmpegSpawn), track: track}
	}
	if err := validateOutputCodec(tokens[1:], track); err != nil {
		return &shellSource{err: err, track: track}
	}
	return &shellSource{
		binary: tokens[0],
		args:   ensureFFmpegFlags(tokens[1:], track),
		track:  track,
	}
}

// ensureFFmpegFlags injects the input-side fast-probe flags and the
// output-side opus/VP8 essentials that the frame readers require. Anything
// the caller already passed is left untouched.
func ensureFFmpegFlags(args []string, track Track) []string {
	has := make(map[string]bool, len(args))
	hasPipe := false
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			has[a] = true
		}
		if a == "pipe:1" || a == "-" {
			hasPipe = true
		}
	}
	inputIdx := -1
	for i, a := range args {
		if a == "-i" {
			inputIdx = i
			break
		}
	}
	var inputInject []string
	if inputIdx >= 0 {
		if !has["-analyzeduration"] {
			inputInject = append(inputInject, "-analyzeduration", "0")
		}
		if !has["-probesize"] {
			inputInject = append(inputInject, "-probesize", "64k")
		}
		if !has["-err_detect"] {
			// Tolerate decoder errors on bad input frames rather than aborting
			// — the downstream OGG/IVF parsers can't recover from a half-flushed
			// page.
			inputInject = append(inputInject, "-err_detect", "ignore_err")
		}
	}
	var outputInject []string
	switch {
	case track.Has(TrackAudio):
		if !has["-c:a"] && !has["-acodec"] {
			outputInject = append(outputInject, "-c:a", "libopus")
		}
		if !has["-application"] {
			outputInject = append(outputInject, "-application", "audio")
		}
		if !has["-frame_duration"] {
			outputInject = append(outputInject, "-frame_duration", "20")
		}
		if !has["-page_duration"] {
			outputInject = append(outputInject, "-page_duration", "20000")
		}
		if !has["-mapping_family"] {
			outputInject = append(outputInject, "-mapping_family", "0")
		}
		if !has["-ar"] {
			outputInject = append(outputInject, "-ar", "48000")
		}
		if !has["-ac"] {
			outputInject = append(outputInject, "-ac", "2")
		}
		if !has["-f"] {
			outputInject = append(outputInject, "-f", "ogg")
		}
	case track.Has(TrackVideo):
		if !has["-c:v"] && !has["-vcodec"] {
			outputInject = append(outputInject, "-c:v", "libvpx", "-deadline", "realtime")
		}
		if !has["-f"] {
			outputInject = append(outputInject, "-f", "ivf")
		}
	}
	if len(inputInject) == 0 && len(outputInject) == 0 && hasPipe {
		return args
	}
	out := make([]string, 0, len(args)+len(inputInject)+len(outputInject)+1)
	if inputIdx < 0 {
		out = append(out, args...)
	} else {
		out = append(out, args[:inputIdx]...)
		out = append(out, inputInject...)
		out = append(out, args[inputIdx:]...)
	}
	// Pop pipe:1 if present, append output flags, then re-append pipe:1
	// (or add it for the first time).
	if hasPipe {
		filtered := out[:0]
		for _, a := range out {
			if a == "pipe:1" {
				continue
			}
			filtered = append(filtered, a)
		}
		out = filtered
	}
	out = append(out, outputInject...)
	out = append(out, "pipe:1")
	return out
}

// validateOutputCodec scans argv for combinations known to produce output
// the frame readers can't parse, and returns a user-actionable error.
func validateOutputCodec(args []string, track Track) error {
	rawAudioFmts := map[string]bool{
		"s16le": true, "s16be": true, "s24le": true, "s32le": true,
		"f32le": true, "f64le": true, "u8": true, "alaw": true, "mulaw": true,
	}
	for i, a := range args {
		next := ""
		if i+1 < len(args) {
			next = args[i+1]
		}
		switch a {
		case "-acodec", "-c:a":
			if strings.HasPrefix(next, "pcm_") && track.Has(TrackAudio) {
				return fmt.Errorf("%w: audio output codec %q produces raw PCM; gotgcall expects libopus in OGG. "+
					"Replace your baseCodec with: -c:a libopus -application audio -frame_duration 20 "+
					"-b:a 64k -ar 48000 -ac 2 -f ogg pipe:1", models.ErrInvalidParams, next)
			}
		case "-f":
			if rawAudioFmts[next] && track.Has(TrackAudio) {
				return fmt.Errorf("%w: audio output container %q is raw PCM; gotgcall expects -f ogg with -c:a libopus", models.ErrInvalidParams, next)
			}
			if next == "rawvideo" && track.Has(TrackVideo) {
				return fmt.Errorf("%w: video output container %q is raw YUV; gotgcall expects -f ivf with -c:v libvpx", models.ErrInvalidParams, next)
			}
		case "-vcodec", "-c:v":
			if next == "rawvideo" && track.Has(TrackVideo) {
				return fmt.Errorf("%w: video output codec %q is raw YUV; gotgcall expects -c:v libvpx -f ivf", models.ErrInvalidParams, next)
			}
		}
	}
	return nil
}

type shellSource struct {
	err    error
	binary string // empty = use configured ffmpegPath()
	args   []string
	track  Track
}

func (s *shellSource) Tracks() Track {
	if s.track == 0 {
		return TrackAudio
	}
	return s.track
}

func (s *shellSource) Open(ctx context.Context) (*Streams, error) {
	return s.openWith(ctx, s.args)
}

// OpenAt spawns the configured ffmpeg command with `-ss <offset>` injected
// before the first `-i`, replacing any existing `-ss` value. Lets
// GroupCall.Pause/Resume preserve playback position for shell sources.
func (s *shellSource) OpenAt(ctx context.Context, offset time.Duration) (*Streams, error) {
	if s.err != nil {
		return nil, s.err
	}
	if offset <= 0 {
		return s.openWith(ctx, s.args)
	}
	return s.openWith(ctx, injectSeek(s.args, offset))
}

func (s *shellSource) openWith(ctx context.Context, args []string) (*Streams, error) {
	if s.err != nil {
		return nil, s.err
	}
	bin := s.binary
	if bin == "" {
		bin = ffmpegPath()
	}
	r, err := gtio.NewShellReader(ctx, bin, args, getLogger(), StderrLogEnabled())
	if err != nil {
		return nil, fmt.Errorf("%w: %v", models.ErrFFmpegSpawn, err)
	}
	st := &Streams{close: r.Close}
	switch {
	case s.Tracks().Has(TrackAudio):
		st.Audio = r
	case s.Tracks().Has(TrackVideo):
		st.Video = r
	default:
		_ = r.Close()
		return nil, fmt.Errorf("%w: no track selected", models.ErrInvalidParams)
	}
	return st, nil
}

// FromShells builds a MultiShellSource from two separate ffmpeg commands
// — one for audio, one for video. Either string may be empty to skip
// that track. Mirrors ntgcalls' MediaDescription(microphone, camera)
// pattern for users who want full control over both legs.
//
// Each cmd goes through the same auto-flag injection as FromShell, so a
// minimal `ffmpeg -i movie.mp4` works for either leg.
//
// The concrete return type lets callers chain optional configuration
// like WithParallelSpawn before passing the value as a Source. Existing
// `var s Source = FromShells(...)` call sites still compile unchanged
// because *MultiShellSource satisfies Source.
func FromShells(audioCmd, videoCmd string) *MultiShellSource {
	if audioCmd == "" && videoCmd == "" {
		return &MultiShellSource{err: fmt.Errorf("%w: both commands empty", models.ErrInvalidParams)}
	}
	s := &MultiShellSource{}
	if audioCmd != "" {
		tokens := tokenizeShell(audioCmd)
		if len(tokens) == 0 {
			return &MultiShellSource{err: fmt.Errorf("%w: empty audio command", models.ErrFFmpegSpawn)}
		}
		if err := validateOutputCodec(tokens[1:], TrackAudio); err != nil {
			return &MultiShellSource{err: err}
		}
		s.audioBin = tokens[0]
		s.audioArgs = ensureFFmpegFlags(tokens[1:], TrackAudio)
	}
	if videoCmd != "" {
		tokens := tokenizeShell(videoCmd)
		if len(tokens) == 0 {
			return &MultiShellSource{err: fmt.Errorf("%w: empty video command", models.ErrFFmpegSpawn)}
		}
		if err := validateOutputCodec(tokens[1:], TrackVideo); err != nil {
			return &MultiShellSource{err: err}
		}
		s.videoBin = tokens[0]
		s.videoArgs = ensureFFmpegFlags(tokens[1:], TrackVideo)
	}
	return s
}

// MultiShellSource is the concrete return type of FromShells. It
// satisfies both Source and SeekableSource. Use WithParallelSpawn to
// opt into spawning the two ffmpeg legs concurrently at Open/OpenAt
// time.
type MultiShellSource struct {
	err       error
	audioBin  string
	videoBin  string
	audioArgs []string
	videoArgs []string
	parallel  bool
}

// WithParallelSpawn opts into starting both ffmpeg legs concurrently
// when Open or OpenAt fires. Default is sequential (audio then video),
// which avoids CDN per-IP concurrency throttles when both legs read the
// same HTTP/HLS URL. Enable this only when the two legs read
// independent inputs (separate files, separate camera/mic devices) or
// when you've verified your source can handle concurrent opens. Only
// takes effect when both legs are non-empty; single-leg sources stay
// sequential since there's nothing to parallelize.
//
// Returns the receiver for chaining: `FromShells(a, v).WithParallelSpawn()`.
func (s *MultiShellSource) WithParallelSpawn() *MultiShellSource {
	s.parallel = true
	return s
}

func (s *MultiShellSource) Tracks() Track {
	var t Track
	if s.audioArgs != nil {
		t |= TrackAudio
	}
	if s.videoArgs != nil {
		t |= TrackVideo
	}
	return t
}

func (s *MultiShellSource) Open(ctx context.Context) (*Streams, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.openWith(ctx, s.audioArgs, s.videoArgs)
}

// OpenAt spawns both legs with `-ss <offset>` injected before the first
// `-i` of each non-empty leg, mirroring shellSource.OpenAt. Either leg
// may be empty; that leg is skipped. Makes FromShells satisfy
// SeekableSource so GroupCall.SeekBy and startLocked's resume-at-offset
// path work for dual-leg (vplay) sources.
func (s *MultiShellSource) OpenAt(ctx context.Context, offset time.Duration) (*Streams, error) {
	if s.err != nil {
		return nil, s.err
	}
	if offset <= 0 {
		return s.openWith(ctx, s.audioArgs, s.videoArgs)
	}
	audio := s.audioArgs
	if audio != nil {
		audio = injectSeek(audio, offset)
	}
	video := s.videoArgs
	if video != nil {
		video = injectSeek(video, offset)
	}
	return s.openWith(ctx, audio, video)
}

func (s *MultiShellSource) openWith(ctx context.Context, audioArgs, videoArgs []string) (*Streams, error) {
	if s.parallel && audioArgs != nil && videoArgs != nil {
		return s.openParallel(ctx, audioArgs, videoArgs)
	}
	return s.openSequential(ctx, audioArgs, videoArgs)
}

func (s *MultiShellSource) openSequential(ctx context.Context, audioArgs, videoArgs []string) (*Streams, error) {
	var closers []stdio.Closer
	closeAll := func() error {
		var firstErr error
		for _, c := range closers {
			if err := c.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		return firstErr
	}
	st := &Streams{close: closeAll}
	if audioArgs != nil {
		r, err := s.spawnLeg(ctx, s.audioBin, audioArgs)
		if err != nil {
			_ = closeAll()
			return nil, fmt.Errorf("%w: audio: %v", models.ErrFFmpegSpawn, err)
		}
		closers = append(closers, r)
		st.Audio = r
	}
	if videoArgs != nil {
		r, err := s.spawnLeg(ctx, s.videoBin, videoArgs)
		if err != nil {
			_ = closeAll()
			return nil, fmt.Errorf("%w: video: %v", models.ErrFFmpegSpawn, err)
		}
		closers = append(closers, r)
		st.Video = r
	}
	return st, nil
}

// openParallel spawns both legs concurrently. Pre-condition: both
// audioArgs and videoArgs are non-nil (caller verified). If either leg
// fails the other is closed; audio's error takes precedence when both
// fail, matching openSequential's reporting order so callers don't see
// a behavior change just because they flipped the flag.
func (s *MultiShellSource) openParallel(ctx context.Context, audioArgs, videoArgs []string) (*Streams, error) {
	var (
		wg                 sync.WaitGroup
		audioR, videoR     *gtio.ShellReader
		audioErr, videoErr error
	)
	wg.Go(func() {
		audioR, audioErr = s.spawnLeg(ctx, s.audioBin, audioArgs)
	})
	wg.Go(func() {
		videoR, videoErr = s.spawnLeg(ctx, s.videoBin, videoArgs)
	})
	wg.Wait()

	if audioErr != nil {
		if videoR != nil {
			_ = videoR.Close()
		}
		return nil, fmt.Errorf("%w: audio: %v", models.ErrFFmpegSpawn, audioErr)
	}
	if videoErr != nil {
		if audioR != nil {
			_ = audioR.Close()
		}
		return nil, fmt.Errorf("%w: video: %v", models.ErrFFmpegSpawn, videoErr)
	}
	closers := []stdio.Closer{audioR, videoR}
	closeAll := func() error {
		var firstErr error
		for _, c := range closers {
			if err := c.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		return firstErr
	}
	return &Streams{close: closeAll, Audio: audioR, Video: videoR}, nil
}

func (*MultiShellSource) spawnLeg(ctx context.Context, bin string, args []string) (*gtio.ShellReader, error) {
	if bin == "" {
		bin = ffmpegPath()
	}
	return gtio.NewShellReader(ctx, bin, args, getLogger(), StderrLogEnabled())
}

// injectSeek returns args with `-ss <offset>` placed immediately before the
// first `-i`. Any existing `-ss <value>` pair (input-side) is removed first.
// If no `-i` is found, `-ss` is prepended.
func injectSeek(args []string, offset time.Duration) []string {
	offsetStr := strconv.FormatFloat(offset.Seconds(), 'f', 3, 64)
	cleaned := make([]string, 0, len(args)+2)
	inputIdx := -1
	skip := false
	for i, a := range args {
		if skip {
			skip = false
			continue
		}
		// Drop only the input-side -ss (one appearing before -i).
		if a == "-ss" && inputIdx < 0 && i+1 < len(args) {
			skip = true
			continue
		}
		if a == "-i" && inputIdx < 0 {
			inputIdx = len(cleaned)
		}
		cleaned = append(cleaned, a)
	}
	if inputIdx < 0 {
		return append([]string{"-ss", offsetStr}, cleaned...)
	}
	out := make([]string, 0, len(cleaned)+2)
	out = append(out, cleaned[:inputIdx]...)
	out = append(out, "-ss", offsetStr)
	out = append(out, cleaned[inputIdx:]...)
	return out
}

// tokenizeShell splits a shell-like string into argv tokens. Supports
// double-quoted segments with spaces and — inside double quotes only —
// the escape sequences \" (literal ") and \\ (literal \). Any other
// backslash sequence is emitted verbatim so existing callers that embed
// literal backslashes in strings like a User-Agent (`Mozilla\5.0 ...`)
// keep working unchanged. Single quotes are treated as literal characters.
// Whitespace outside quotes separates tokens.
//
// The \" support lets callers pass filenames that themselves contain "
// (e.g. Telegram audio with `(From "Foo")` in the title) without the
// embedded quote toggling inQuote and slicing the path mid-string.
func tokenizeShell(s string) []string {
	var out []string
	var cur strings.Builder
	inQuote := false
	flush := func() {
		if cur.Len() == 0 {
			return
		}
		out = append(out, cur.String())
		cur.Reset()
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\\' && i+1 < len(s) {
			next := s[i+1]
			if next == '"' || next == '\\' {
				cur.WriteByte(next)
				i++
				continue
			}
		}
		switch {
		case c == '"':
			inQuote = !inQuote
		case (c == ' ' || c == '\t' || c == '\n') && !inQuote:
			flush()
		default:
			cur.WriteByte(c)
		}
	}
	flush()
	return out
}

// shellReaderCloser adapts gtio.ShellReader's Close to the close-func
// shape Streams expects. Kept here in case Open wants to wrap; currently
// st.close = r.Close suffices.
var _ stdio.ReadCloser = (*gtio.ShellReader)(nil)
