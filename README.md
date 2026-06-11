<h1 align="center">gotgcall</h1>

<p align="center">
  <b>The first drop-in replacement for Telegram Group Calls — with audio and video — in pure Go.</b><br>
  <sub>A drop-in alternative to <a href="https://github.com/pytgcalls/ntgcalls">ntgcalls</a> / <a href="https://github.com/pytgcalls/pytgcalls">pytgcalls</a> — built for Go music bots, livestream bots, and broadcast tooling. No libwebrtc. No cgo. No native build chain.</sub>
</p>

<p align="center">
  <a href="https://pkg.go.dev/github.com/annihilatorrrr/gotgcall"><img src="https://pkg.go.dev/badge/github.com/annihilatorrrr/gotgcall.svg" alt="Go Reference"></a>
  <a href="https://goreportcard.com/report/github.com/annihilatorrrr/gotgcall"><img src="https://goreportcard.com/badge/github.com/annihilatorrrr/gotgcall" alt="Go Report Card"></a>
  <a href="https://app.deepsource.com/gh/annihilatorrrr/gotgcall/"><img src="https://app.deepsource.com/gh/annihilatorrrr/gotgcall.svg/?label=active+issues&show_trend=true&token=M2OsBAzzJt_7f73N5Co3gz9I" alt="DeepSource"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/License-MIT-yellow.svg" alt="License: MIT"></a>
  <a href="#why-pure-go"><img src="https://img.shields.io/badge/cgo-disabled-brightgreen" alt="CGO-free"></a>
  <a href="#performance-vs-ntgcalls"><img src="https://img.shields.io/badge/pion-v4-blueviolet" alt="pion v4"></a>
  <a href="#why-pure-go"><img src="https://img.shields.io/badge/pure--Go-first%20of%20its%20kind-ff6b35" alt="First pure-Go Telegram group call library"></a>
</p>

<!--
Keywords (for search indexers, not rendered to readers):
first pure-Go Telegram group call library, first pure Go ntgcalls alternative,
first pure-Go pytgcalls alternative, first Go Telegram voice chat library,
first pion Telegram group call, only pure-Go Telegram VC library,
Telegram group call, Telegram voice chat, Telegram video chat, pure Go WebRTC,
ntgcalls Go alternative, pytgcalls Go alternative, ntgcalls drop-in Go,
pytgcalls drop-in Go, pion WebRTC Telegram, pion v4 Telegram group call,
Telegram music bot Go, Telegram radio bot Go, Telegram audio streaming Go,
Telegram livestream bot Go, Telegram group video chat Go,
Telegram VC bot Go, Telegram voice chat SDK Go, Telegram group call SDK Go,
gogram voice chat, gogram group call, MTProto-Go group call,
MTProto group call Go, MTProto voice chat Go, blob signaling Telegram call,
Opus VP8 RTP Telegram, Opus 48 kHz Telegram, VP8 IVF Telegram video,
ICE-CONTROLLED pion, native pion stack Telegram, pion ice dtls srtp Telegram,
RTMP push Telegram livestream, Telegram go-live RTMP Go,
phone.GetGroupCallStreamRtmpUrl, phone.JoinGroupCall blob signaling,
static binary Telegram bot, CGO_ENABLED=0 Telegram WebRTC,
ffmpeg pipeline streaming, HLS to Telegram, RTSP to Telegram,
MJPEG to Telegram, screen capture to Telegram, IP camera to Telegram,
YouTube to Telegram voice chat, yt-dlp Telegram bot,
shared UDP mux Telegram, scalable Telegram call backend,
Telegram group call concurrent calls, Telegram bot hosting Go.
-->

```go
client, _ := gotgcall.New()
defer client.Close()

localParams, _ := client.CreateCall(chatID)
remoteParams   := joinViaYourMTProto(localParams)  // gogram / your MTProto stack
client.Connect(chatID, remoteParams)
client.SetStreamSources(chatID, gotgcall.FromFile("song.mp3", gotgcall.EncodeOptions{}))
```

That's a working voice-chat playback bot. Everything else in this README is options on top.

## Highlights

- **Single static binary.** `CGO_ENABLED=0 go build` → `scp` → run. No libwebrtc, no glibc, no C++ toolchain — `ffmpeg` is the only runtime dependency.
- **Fast connect.** Reaches the SFU in tens of milliseconds. Built on [pion v4](https://github.com/pion/webrtc) under the hood.
- **Blob-only signalling.** The library never imports `gogram` or any MTProto code. Use any MTProto Go client you like.
- **ntgcalls-shaped API.** `CreateCall` / `Connect` / `SetStreamSources` / `Pause` / `Resume` / `Mute` / `SeekBy` / `Stop` — existing bot code translates line-for-line.
- **Three source modes.** `FromFile`, `FromURL`, `FromShell` — anything ffmpeg can decode is fair game (HLS, RTSP, RTMP, MJPEG, screen capture, …).
- **WebRTC + RTMP push.** Group voice/video chats *and* "go live" RTMP broadcasts via one client.
- **Scales to tens of thousands of calls** per process with `WithSharedUDPMux` + raised FD limits.

## At a glance

| | |
| --- | --- |
| **Language** | Pure Go (`CGO_ENABLED=0`) |
| **Min Go version** | 1.26 |
| **Codecs** | Opus (audio) · VP8 (video) |
| **Signalling** | Blob JSON — bring your own MTProto layer |
| **Runtime dep** | `ffmpeg` on `PATH` (or `WithFFmpegPath`) |
| **Modes** | WebRTC group call · RTMP livestream push |
| **License** | MIT |

> **Status — Stable.** Built for my own bots; the API is intentionally close to ntgcalls so existing code translates with minimal change. Breaking changes are tagged in releases.

<details>
<summary><b>Table of contents</b></summary>

- [Install](#install) · [Architecture](#architecture-at-a-glance) · [Quick start](#quick-start)
- **Sources** — [`FromFile` / `FromURL`](#fromfile--fromurl) · [`FromShell`](#fromshell--single-custom-ffmpeg-leg) ([audio recipes](#audio-recipes) · [video recipes](#video-recipes)) · [`FromShells`](#fromshells--dual-ffmpeg-legs) ([dual-leg recipes](#dual-leg-recipes)) · [Gotchas](#shell-source-gotchas) · [`EncodeOptions`](#encodeoptions)
- **Client** — [Options](#client-options) · [Debug logs](#enabling-debug-logs) · [UDP mux & scaling](#udp-mux--scaling)
- **Lifecycle** — [WebRTC mode](#webrtc-mode) · [RTMP mode](#rtmp-mode) · [Pause / Resume / Mute](#pause--resume--mute) · [Seek](#seek) · [Callbacks](#callbacks) · [Server-side state changes](#server-side-media-state-changes-admin-mute-video-off)
- **Reference** — [Errors](#errors) · [Concurrency model](#concurrency-model) · [Goroutine budget](#goroutine-budget) · [Networking](#networking) · [A/V sync](#av-sync) · [Pitfalls](#pitfalls)
- **Performance** — [Tuning](#performance-tuning) · [Memory](#memory-usage) · [Scaling ballparks](#concurrency--scaling-ballparks) · [vs ntgcalls](#performance-vs-ntgcalls)
- [Why pure Go](#why-pure-go) · [FAQ](#faq) · [See also](#see-also) · [License](#license)

</details>

## Install

```sh
go get github.com/annihilatorrrr/gotgcall
```

`ffmpeg` must be on `PATH` at runtime (or set `gotgcall.WithFFmpegPath("/path/to/ffmpeg")`). `New()` fails fast if the binary isn't found, so the error surfaces at startup rather than on the first stream.

Requires Go 1.26+ (uses `errors.AsType[T]` and a few stdlib features added in 1.26).

## Architecture at a glance

```
   ┌────────────┐    blob JSON     ┌─────────────────────┐
   │   Client   │ ◀──────────────▶ │   Your MTProto      │
   │ (gotgcall) │                  │   layer (gogram, …) │
   └────────────┘                  └─────────────────────┘
         │
         ├──▶  GroupCall   (WebRTC: audio + video)
         └──▶  RTMPCall    (RTMP push: "go live")
                  │
                  ▼
            Telegram SFU
```

**Blob-only signalling.** `CreateCall(chatID)` returns a JSON string; you hand it to `phone.JoinGroupCall` via your own MTProto stack, then feed the response back via `Connect(chatID, respJSON)`. The library never imports `gogram` or any MTProto code, so it stays MTProto-version-independent.

**Send-only audio + video.** Outgoing Opus + VP8. The library doesn't receive incoming media — group calls are one-way from the bot's perspective.

**ffmpeg is the encoder.** ffmpeg is invoked as a subprocess for decoding and encoding; nothing is linked into the Go binary. That's how `CGO_ENABLED=0` is possible.

## Quick start

```go
client, err := gotgcall.New()
if err != nil { log.Fatal(err) }
defer client.Close()

client.OnStreamEnd(func(chat int64, t gotgcall.StreamType, d gotgcall.Device, err error) {
    log.Printf("stream end: %v", err)
})
client.OnConnectionChange(func(chat int64, info gotgcall.NetworkInfo) {
    log.Printf("conn state: %s", info.State)
})
client.OnUpgrade(func(chat int64, state gotgcall.MediaState) {
    // Fires on Mute / Unmute / Pause / Resume and on spontaneous
    // transitions (video leg dying mid-stream, ICE Failed/Closed
    // while video was active). SetStreamSources and Stop stay silent
    // — the caller already knows the new state.
    //
    // state fields mirror Telegram's MTProto participant flags
    // (Paused maps to video_paused — "media not flowing"):
    //   Muted              — explicit mute toggle
    //   Paused             — Muted || the call was paused
    //   VideoStopped       — true for Play (audio-only), false for VPlay
    //   PresentationPaused — same lifecycle as Paused (no presentation
    //                        source in this library)
})

// 1. Local-side JSON.
localParams, _ := client.CreateCall(chatID)

// 2. Drive Telegram via your MTProto layer (gogram, etc.).
//    Pass localParams to phone.JoinGroupCall; read the response.
remoteParams := joinViaYourMTProto(localParams)

// 3. Finish the WebRTC handshake.
client.Connect(chatID, remoteParams)

// 4. Stream.
client.SetStreamSources(chatID, gotgcall.FromFile("song.mp3", gotgcall.EncodeOptions{}))

// 5. Pause / resume / mute / change source any time.
client.Pause(chatID)
client.Resume(chatID)
client.SetStreamSources(chatID, gotgcall.FromURL("https://stream.example.com/radio.m3u8", gotgcall.EncodeOptions{}))

// 6. Stop tears down the call.
client.Stop(chatID)
```

See [`examples/bot/`](examples/bot) for a runnable skeleton against gogram (own `go.mod` so the example doesn't taint the library's dependency tree).

## Sources

All sources target **Opus-in-OGG** (audio) and/or **VP8-in-IVF** (video) on ffmpeg's stdout. The library will not accept raw PCM/YUV — the frame readers can't parse them.

### `FromFile` / `FromURL`

```go
gotgcall.FromFile("song.mp3", gotgcall.EncodeOptions{})
gotgcall.FromURL("https://stream.example.com/...", gotgcall.EncodeOptions{})
```

Anything ffmpeg can decode is fair game — mp3, m4a, flac, ogg, opus, wav, webm, mp4, mkv, mov, m3u8 (HLS), live RTMP/RTSP, etc.

Defaults to **audio only**, regardless of what the container holds. Opt in to video extraction:

```go
client.SetStreamSources(chatID, gotgcall.FromFile("movie.mp4", gotgcall.EncodeOptions{
    Tracks: gotgcall.TrackAudio | gotgcall.TrackVideo,
    // Or just TrackVideo — TrackVideo implies TrackAudio (a video file is a
    // video file with audio).
}))
```

Fast-start probing (`-analyzeduration 0 -probesize 64k`) is on by default for every source — cuts ~1-2 s off ffmpeg's startup latency vs the stock defaults (5 s + 5 MB). HLS sources additionally get `-user_agent`, `-protocol_whitelist file,http,https,tcp,tls`, `-rw_timeout 10s`, `-http_persistent 1`; HTTP/HTTPS sources get `-reconnect 1 -reconnect_at_eof 1 -reconnect_streamed 1 -reconnect_delay_max 5 -timeout 10s` so transient network blips don't kill the stream.

Both `FromFile` and `FromURL` return seekable sources. `Pause` records the elapsed offset and `Resume` re-spawns ffmpeg with `-ss <offset>` injected before the input.

### `FromShell` — single custom ffmpeg leg

```go
gotgcall.FromShell(`ffmpeg -i "song.mp3"`, gotgcall.TrackAudio)
```

`FromShell` parses the cmdline as a shell-like argv (handles double-quoted args, plus `\"` and `\\` escape sequences for filenames containing literal `"` or `\` — e.g. a Telegram audio titled `(From "Foo")` that would otherwise slice the path mid-string when the embedded quote toggled the quote state) and spawns it **directly via `exec`**, NOT via `/bin/sh`. Shell metacharacters in filenames can't inject commands; use `%q` for filenames.

**Auto-injected if missing** (so the minimal command above just works):

| Position | Flags |
| --- | --- |
| Before `-i` | `-analyzeduration 0 -probesize 64k -err_detect ignore_err` |
| Audio out  | `-c:a libopus -application audio -frame_duration 20 -page_duration 20000 -mapping_family 0 -ar 48000 -ac 2 -f ogg` |
| Video out  | `-c:v libvpx -deadline realtime -f ivf` |
| Last token | `pipe:1` |

**Not auto-injected** (specify yourself if you need them): `-b:a` / `-b:v`, `-vn` / `-an`, `-map`, `-re`, HLS reconnect flags (`-user_agent`, `-protocol_whitelist`, `-reconnect *`), HTTP `-headers`, `-stream_loop`, hardware accel. The auto-fill is conservative — anything you pass is left alone.

A single `FromShell` produces one output (audio OR video). Raw PCM/YUV output codecs (`-c:a pcm_*`, `-f rawvideo`, …) are rejected up front with a pointer at the correct flags.

#### Audio recipes

All examples below are `FromShell(<cmd>, gotgcall.TrackAudio)`. The `<cmd>` is shown as a Go raw string literal.

**Tempo change (atempo)** — pitch-preserving speed-up/slow-down. Stack multiple `atempo` filters for ratios outside `[0.5, 2.0]`:

```go
`ffmpeg -i "song.mp3" -af "atempo=1.25"`
`ffmpeg -i "song.mp3" -af "atempo=2.0,atempo=1.25"`   // = 2.5x
```

**Loudness normalization (EBU R128)** — broadcast-grade levelling. Two-pass is more accurate; one-pass is fine for live streams:

```go
`ffmpeg -i "song.mp3" -af "loudnorm=I=-16:LRA=11:TP=-1.5"`
```

**Volume / gain** — linear or dB:

```go
`ffmpeg -i "song.mp3" -af "volume=1.5"`        // +50 %
`ffmpeg -i "song.mp3" -af "volume=-6dB"`       // -6 dB
```

**Bass / treble shelf** — simple two-band EQ:

```go
`ffmpeg -i "song.mp3" -af "bass=g=6,treble=g=2"`
```

**Pitch shift (semitones)** — resample + atempo trick; `1.06` ≈ +1 semitone, `0.944` ≈ -1:

```go
`ffmpeg -i "song.mp3" -af "asetrate=48000*1.06,aresample=48000,atempo=1/1.06"`
```

**Fade in / out**:

```go
`ffmpeg -i "song.mp3" -af "afade=t=in:d=2"`
`ffmpeg -i "song.mp3" -af "afade=t=out:st=180:d=5"`
```

**Mix two sources (amix)** — overlay background ambience under music:

```go
`ffmpeg -i "music.mp3" -i "ambient.wav" -filter_complex "amix=inputs=2:duration=longest:weights=1 0.3"`
```

**Seek to start position** — initial play offset; note that Pause/Resume's `-ss` injection replaces this on resume (you control the *first* play position only):

```go
`ffmpeg -ss 90 -i "song.mp3"`
```

**Infinite loop** — replay forever:

```go
`ffmpeg -stream_loop -1 -i "jingle.mp3"`
```

**Concat playlist (concat protocol)** — gapless join of identically-encoded files:

```go
`ffmpeg -i "concat:track01.mp3|track02.mp3|track03.mp3"`
```

For mixed-format playlists use the concat *demuxer* with a list file:

```go
`ffmpeg -f concat -safe 0 -i "playlist.txt"`
```

**HLS / live radio with reconnect + custom UA** — `FromShell` does NOT inject the HLS-specific flags that `FromURL` does; add them yourself if your source needs them:

```go
`ffmpeg -user_agent "Mozilla/5.0" -reconnect 1 -reconnect_at_eof 1 ` +
`-reconnect_streamed 1 -reconnect_delay_max 5 -rw_timeout 10000000 ` +
`-protocol_whitelist "file,http,https,tcp,tls" ` +
`-i "https://stream.example.com/radio.m3u8"`
```

**HTTP with custom headers / cookies** — inject Referer / Cookie / Authorization on the input:

```go
`ffmpeg -headers "Referer: https://example.com\r\nCookie: session=abc\r\n" ` +
`-i "https://example.com/protected.mp3"`
```

(`\r\n` here is **literal** four characters in the Go raw string — ffmpeg's `-headers` parses them as CRLF separators between header lines.)

**RTSP / RTMP / SRT input** — `FromShell` is the right escape hatch when you need transport flags:

```go
`ffmpeg -rtsp_transport tcp -i "rtsp://camera.local/live"`
`ffmpeg -i "srt://ingest.example.com:9000?mode=caller"`
```

#### Video recipes

All examples below are `FromShell(<cmd>, gotgcall.TrackVideo)`. Telegram requires VP8 — `libvpx` is the only video encoder that works end-to-end, so most recipes here are filter-side, not codec-side.

**Scale + framerate + bitrate**:

```go
`ffmpeg -i "movie.mp4" -vf "scale=1280:720" -r 30 -b:v 1500k`
```

**Letterbox a vertical / odd-aspect source to 720p**:

```go
`ffmpeg -i "vertical.mp4" -vf "scale=1280:-2:force_original_aspect_ratio=decrease,` +
`pad=1280:720:(ow-iw)/2:(oh-ih)/2:black"`
```

**Watermark / logo overlay**:

```go
`ffmpeg -i "movie.mp4" -i "logo.png" -filter_complex "overlay=W-w-20:20"`
```

**Burned-in timestamp (drawtext)** — useful for security-camera feeds:

```go
`ffmpeg -i "movie.mp4" -vf "drawtext=text='%{localtime}':fontcolor=white:fontsize=24:` +
`box=1:boxcolor=black@0.5:boxborderw=5:x=10:y=10"`
```

**RTSP IP camera** — TCP transport survives lossy Wi-Fi better than the UDP default:

```go
`ffmpeg -rtsp_transport tcp -i "rtsp://user:pass@192.168.1.10/Streaming/Channels/101"`
```

**Live screen capture**:

```go
// Linux (X11):
`ffmpeg -f x11grab -framerate 30 -video_size 1920x1080 -i ":0.0"`

// Windows:
`ffmpeg -f gdigrab -framerate 30 -i "desktop"`

// macOS (avfoundation index from -f avfoundation -list_devices true -i ""):
`ffmpeg -f avfoundation -framerate 30 -i "1:none"`
```

### `FromShells` — dual ffmpeg legs

For ntgcalls-style "microphone + camera" patterns where you want full control over both legs:

```go
gotgcall.FromShells(
    `ffmpeg -i "movie.mp4"`,                                // audio leg
    `ffmpeg -i "movie.mp4" -vf "scale=1280:720" -b:v 1500k`, // video leg
)
```

Each cmd goes through the same auto-flag injection as `FromShell`. Either string may be empty to skip that track.

For the convenience path use `FromFile`/`FromURL` with `Tracks: TrackVideo` and let the library construct both ffmpeg commands for you.

`FromShells` returns `*MultiShellSource`, which satisfies both `Source` and `SeekableSource` — `client.SeekBy(chatID, deltaMs)` works for dual-leg sources, killing both ffmpegs and re-spawning with `-ss <offset>` injected into each leg.

**Sequential vs parallel spawn.** By default both legs spawn sequentially (audio then video). When both legs read the same URL, this avoids tripping CDN per-IP concurrency throttles. Opt into concurrent spawn when the legs read independent inputs (separate files, separate camera/mic devices):

```go
gotgcall.FromShells(audioCmd, videoCmd).WithParallelSpawn()
```

Single-leg sources ignore the flag — there's nothing to parallelize.

#### Dual-leg recipes

**Audio file over a static cover image** — "music with art":

```go
gotgcall.FromShells(
    `ffmpeg -i "song.mp3"`,
    `ffmpeg -loop 1 -framerate 1 -i "cover.jpg" -vf "scale=1280:720" -r 1 -b:v 200k`,
)
```

**Different sources per leg** — radio audio + live webcam:

```go
gotgcall.FromShells(
    `ffmpeg -i "https://stream.example.com/radio.mp3"`,
    `ffmpeg -f v4l2 -framerate 30 -video_size 1280x720 -i "/dev/video0"`,
)
```

**A/V sync under time-distortion** — when speeding up audio with `atempo`, scale video PTS by the same factor or the legs drift apart:

```go
gotgcall.FromShells(
    `ffmpeg -i "movie.mp4" -af "atempo=1.25"`,
    `ffmpeg -i "movie.mp4" -vf "setpts=PTS/1.25,scale=1280:720" -r 30 -b:v 1500k`,
)
```

#### Shell-source gotchas

- **No shell features.** The argv is exec'd directly, so `$VAR`, `${VAR}`, `*.mp3`, `$(cmd)`, `cmd1 | cmd2`, `cmd1 && cmd2`, `>` redirects, and `~` expansion are all **literal characters**. Substitute env vars in Go before composing the string.
- **No `/dev/stdin` source.** `FromShell` has no way to pipe bytes in from your Go process; `ffmpeg -i pipe:0` would just block. Spawn external producers (yt-dlp, etc.) yourself and write the file to disk first, or have them stream to a URL you can then `-i`.
- **Quoting.** Use double quotes for arguments with spaces; `\"` for a literal `"` inside; `\\` for a literal `\`. Single quotes are not quote characters — they're literal apostrophes (filenames like `Don't Stop.mp3` work as-is, no quoting needed unless there's a space).
- **HLS/HTTP convenience flags don't apply.** `FromFile`/`FromURL` inject `-user_agent`, `-reconnect *`, `-protocol_whitelist`, `-rw_timeout` automatically; `FromShell` does not. Add them yourself when streaming m3u8 / unreliable HTTP.
- **Hardware encoders rarely help.** Telegram only accepts VP8, and very few platforms have a VP8 hardware encoder (some Intel iGPUs have `vp8_vaapi`; most NVENC/QSV builds don't). Stick with `libvpx`.
- **`-c:a copy` / `-c:v copy` is brittle.** Even if the source is already Opus or VP8, pacing depends on per-frame metadata the OGG/IVF muxers add — `copy` paths often miss the page/keyframe cadence the streamer expects. Re-encode is the safe default.
- **Auto-fill is per-flag, not all-or-nothing.** Each flag is checked independently — `-c:a libopus -b:a 192k` keeps your bitrate and still fills in `-application`, `-frame_duration`, `-page_duration`, `-mapping_family`, `-ar`, `-ac`, `-f`. The only setting that gets *rejected* is a raw PCM/YUV output codec, with an error pointing at the right replacement.
- **Inspecting the realized argv.** gotgcall doesn't currently log the post-injection argv. Turn on `WithFFmpegStderrLog()` and you'll see ffmpeg's own "Input #0 …" / "Stream mapping" output, which confirms what it parsed and which streams it picked.

### `EncodeOptions`

```go
type EncodeOptions struct {
    VideoBitrateKbps int   // default 800
    VideoWidth       int   // default 1280
    VideoHeight      int   // default 720
    VideoFPS         int   // default 30
    AudioBitrateKbps int   // default 128 (music-grade; bump to 192+ for transparent quality, Telegram fmtp accepts up to 510)
    AudioChannels    int   // default 2
    Tracks           Track // default TrackAudio; TrackVideo implies +TrackAudio
}
```

Set on the constructor (`FromFile`/`FromURL`); rides with the Source. `FromShell` / `FromShells` ignore `EncodeOptions` because you control ffmpeg directly.

## Client options

```go
gotgcall.New(
    gotgcall.WithFFmpegPath("/opt/ffmpeg/bin/ffmpeg"),  // override binary lookup
    gotgcall.WithLogger(slog.Default()),                // structured logger
    gotgcall.WithDebugLogs(),                           // shortcut: text handler @ Debug level to stderr
    gotgcall.WithFFmpegStderrLog(),                     // tee ffmpeg stderr → debug log
    gotgcall.WithSharedUDPMux(),                        // one UDP socket for all calls
    gotgcall.WithDTLSCertPool(16),                      // pre-generate N DTLS certs
    gotgcall.WithDispatchBuffer(512),                   // event-dispatcher queue size
    gotgcall.WithNetworkTypes(                          // enable IPv6/TCP for restrictive nets
        gotgcall.NetworkTypeUDP4,
        gotgcall.NetworkTypeUDP6,
        gotgcall.NetworkTypeTCP4,
    ),
)
```

| Option | Default | Notes |
| --- | --- | --- |
| `WithFFmpegPath` | `"ffmpeg"` | `New()` fails fast if the binary is missing. |
| `WithLogger` | **discard** (no logs at all) | Pass a `*slog.Logger` to receive gotgcall events plus ffmpeg stderr/exit. Without this, every log call — Info, Warn, Error — is silently dropped. |
| `WithDebugLogs` | off | Convenience shortcut for debug-level slog to stderr. Use when reporting bugs. |
| `WithFFmpegStderrLog` | off | Tees ffmpeg stderr line-by-line into the logger. Helpful for "stream runs but I hear nothing" diagnostics. |
| `WithSharedUDPMux` | off | Multiplex every call through one UDP socket. See [UDP mux scaling](#udp-mux--scaling). |
| `WithDTLSCertPool` | 8 | Pre-generate N DTLS certs so `CreateCall` doesn't stall during bursts. 0 = disabled. |
| `WithDispatchBuffer` | 256 | Callback queue size. Raise to absorb bursts of state changes. |
| `WithNetworkTypes` | UDP4+UDP6 | Override the candidate network-type whitelist. Add TCP for environments where UDP is blocked. |
| `WithConnectTimeout` | 10 s | How long `SetSource` / `Resume` wait for the call to be ready. |
| `WithVerboseConnectionLogs` | off | Debug slog + per-candidate logs. Use when reporting a stuck-in-Connecting bug. |

### Enabling debug logs

> `gotgcall.New()` with no logger option produces **no logs at all** — not Info, not Warn, not Error. Logging is opt-in so the library never spams your stdout/stderr unexpectedly. Pass `WithLogger`, `WithDebugLogs`, or `WithVerboseConnectionLogs` to turn it on.

For maximum verbosity when reporting a bug:

```go
client, err := gotgcall.New(
    gotgcall.WithVerboseConnectionLogs(), // ICE + DTLS + per-candidate trace
    gotgcall.WithFFmpegStderrLog(),       // ffmpeg stderr line-by-line
)
```

### UDP mux & scaling

The README said "use `WithSharedUDPMux` at 100+ calls". That was a conservative guess — the real picture:

**Default (one socket per call):**
- 1 UDP socket = 1 file descriptor + 1 ephemeral port per call.
- Linux defaults: `ulimit -n 1024` (raise to 65535), ephemeral port range `32768–60999` (~28000 usable).
- Practical ceiling **without** any tuning: ~900 calls (bounded by FDs, leaving room for other FDs).
- After `ulimit -n 65535` and `net.ipv4.ip_local_port_range="1024 65000"`: **tens of thousands** of calls on a beefy server.
- Benefit: kernel-level UDP receive-queue per call, parallelism scales with CPU cores naturally.

**`WithSharedUDPMux` (one socket total):**
- 1 UDP socket, 1 FD, 1 port for the entire process — FD/port limits stop mattering.
- All traffic funnels through one socket → kernel UDP buffer might become contended at extreme rates.
- Per-socket UDP throughput on modern Linux: easily 1–10 Gbps. At ~50 kbps per voice call, that's **20 000–200 000 concurrent voice calls** through one socket before throughput becomes the bottleneck.
- Best for huge call counts where FD/port pressure is the limiting factor, or where firewall rules need to pin a single port.

**Rule of thumb:**
- < 1000 calls: per-call sockets is fine, simpler, and gives you natural per-call kernel-queue isolation.
- 1000–10000 calls: either works; `WithSharedUDPMux` simplifies sysctl tuning.
- 10000+ calls: `WithSharedUDPMux` is the easier path; tune the kernel UDP receive buffer (`net.core.rmem_max`, `net.core.rmem_default`).

**Note:** `client.Stop(chatID)` closes only that call's WebRTC stack (and the per-call socket if not using the shared mux). The shared mux survives every `Stop` and is only closed when you call `client.Close()` on the parent client. So you can spin calls up and down freely without leaking or thrashing the shared socket.

## Lifecycle

### WebRTC mode

The default. Use for normal group voice/video.

```go
localParams, err := client.CreateCall(chatID)
// → send localParams to phone.JoinGroupCall; read remoteParams from response.
err = client.Connect(chatID, remoteParams)
err = client.SetStreamSources(chatID, gotgcall.FromFile("song.mp3", gotgcall.EncodeOptions{}))
// …
err = client.Stop(chatID)
```

- `CreateCall` returns `ErrConnectionExists` only if a **live** call for that chat exists. Failed/Closed calls are reaped automatically — retries on a dead chat just work.
- `Connect` before `CreateCall` returns `ErrConnectionNotFound`. Re-calling `Connect` updates the remote params.
- After `Stop` you can re-use the same `chatID` cleanly.
- `client.AudioSSRC(chatID)` returns the audio SSRC for `phone.LeaveGroupCall`'s `Source` field. RTMP calls return `ErrWrongMode`.

### RTMP mode

For "go live" / host-style broadcasts. Obtain the URL via `phone.GetGroupCallStreamRtmpUrl`:

```go
err := client.StartRTMP(chatID, rtmpURL)
err  = client.SetStreamSources(chatID, gotgcall.FromFile("movie.mp4", gotgcall.EncodeOptions{}))
// Pause/Resume/Stop work identically. Mute/Unmute are best-effort (RTMP push has
// no per-track control); the lib tracks state but doesn't drop frames.
```

RTMP transcodes to H.264 + AAC. Pause/Resume in RTMP mode incurs a brief silence (~100–300 ms) on resume because Telegram's RTMP ingest closes silent streams; WebRTC mode pauses silently.

## Pause / Resume / Mute

```go
ok, err := client.Pause(chatID)   // false if already paused
ok, err  = client.Resume(chatID)
ok, err  = client.Mute(chatID)    // mute audio track; video keeps going
ok, err  = client.Unmute(chatID)
```

- **WebRTC Pause/Resume:** silent — no audible gap on resume.
- **RTMP Pause/Resume:** a brief ~100–300 ms gap on resume (Telegram's RTMP ingest closes silent streams).
- **Mute** silences the audio track; video keeps going.
- `SetStreamSources` can be called any time. While paused, the new source is recorded and starts at offset 0 on Resume.

## Seek

```go
err := client.SeekBy(chatID, +30_000) // forward 30s
err  = client.SeekBy(chatID, -10_000) // back 10s
```

`SeekBy(chatID, deltaMs)` is **relative** to the current position. Positive jumps forward, negative jumps backward. Internally it kills ffmpeg and respawns at the new offset via `SeekableSource.OpenAt` — same machinery Resume uses, just with a user-chosen target.

- **Out-of-range → EOF.** Underflow below 0 fires `OnStreamEnd` instead of seeking. Forward overshoots past the source duration are detected naturally — ffmpeg yields zero frames after `-ss` and the streamer EOFs on its own. Both paths land your "play next track" logic on the same callback.
- **Errors.** `ErrNoSource` when nothing is playing, `ErrSeekUnsupported` when the active source doesn't implement `SeekableSource` (today every built-in source does — `FromFile` / `FromURL` / `FromShell` all inject `-ss`).
- **No `OnUpgrade` fire.** `SeekBy` is user-initiated; the caller already knows they moved.
- **Works while paused.** Position updates immediately; Resume picks up at the new offset.
- For absolute seeks: `client.SeekBy(chat, targetMs - int64(client.Time(chat)))` — the lib intentionally doesn't expose a `SeekTo` (one line at the caller side).

## Callbacks

```go
client.OnStreamEnd(func(chat int64, t StreamType, d Device, err error) {
    // Fires on natural EOF (err == nil) or ffmpeg crash (err != nil).
    // Manual Stop / SetSource don't fire — the caller already knows.
    // For video+audio sources fires twice: first Video, then Audio.
})

client.OnConnectionChange(func(chat int64, info NetworkInfo) {
    // info.State: Connecting | Connected | Disconnected | Failed | Closed | Timeout
})

client.OnUpgrade(func(chat int64, state MediaState) {
    // Mirror of ntgcalls' onUpgrade(MediaState). Fires on Mute /
    // Unmute / Pause / Resume and on spontaneous transitions (a video
    // leg ending mid-stream via EOF or ffmpeg crash, or the WebRTC
    // PC reaching Failed/Closed while video was active).
    //
    // SetStreamSources and Stop stay silent: the caller chose the new
    // source / brought the call down and can mirror MTProto in the
    // same code path. No-op toggles (e.g. Mute when already muted)
    // are also silent.
    //
    // MediaState fields (Paused maps to Telegram's video_paused —
    // i.e. "media not flowing"):
    //   Muted              — explicit mute toggle
    //   Paused             — Muted || internally-paused
    //   VideoStopped       — true for Play (audio-only), false for VPlay
    //   PresentationPaused — same as Paused (no presentation source
    //                        in this library)
})
```

All callbacks fire on a single dispatcher goroutine, so you can safely re-enter the API from inside (e.g. call `client.Stop(chat)` from inside `OnStreamEnd`). If your callback panics it is recovered and logged; the dispatcher keeps running.

If the dispatch queue fills up (slow consumer), the dispatcher drops the **oldest** queued event and logs a warning. Tune with `WithDispatchBuffer`.

## Server-side media-state changes (admin mute, video off)

The library is blob-only and never sees MTProto updates. When Telegram tells you the bot was admin-muted (via your `UpdateGroupCallParticipants` handler), react directly:

```go
tg.AddRawHandler(&telegram.UpdateGroupCallParticipants{}, func(u telegram.Update, _ *telegram.Client) error {
    upd := u.(*telegram.UpdateGroupCallParticipants)
    for _, p := range upd.Participants {
        // compare p.Peer to your own user id, then:
        if p.Muted {
            client.Pause(chatID)
        } else if p.CanSelfUnmute {
            client.Resume(chatID)
        }
    }
    return nil
})
```

The `OnUpgrade(MediaState)` callback fires for **outgoing** state changes — Mute / Unmute / Pause / Resume plus spontaneous video-leg EOF or ICE Failed/Closed. Server-side mute / video-stop from Telegram is delivered only via your MTProto `UpdateGroupCallParticipants` handler — gotgcall stays out of MTProto by design.

## Errors

All errors are sentinels — branch with `errors.Is`:

| Error | Returned when |
| --- | --- |
| `ErrConnectionExists` | `CreateCall` / `StartRTMP` for a chatID that already has a **live** call. Failed/Closed calls are auto-reaped, so retries on a dead chat just work. |
| `ErrConnectionNotFound` | Any method called with an unknown chatID, or after `Stop`. |
| `ErrConnectionTimeout` | Reserved for future use. ICE-failure currently surfaces via `OnConnectionChange(Failed)`. |
| `ErrConnectionFailed` | Reserved for branching; ICE-failure currently surfaces via `OnConnectionChange(Failed)`. |
| `ErrInvalidParams` | Malformed remote JSON in `Connect`, or `FromShell` with empty/invalid command. |
| `ErrFFmpegSpawn` | ffmpeg couldn't start (binary missing / permission denied / OS resource exhaustion). |
| `ErrFFmpegCrashed` | ffmpeg exited non-zero. Wrapped error carries `exit=<code>` and the last 512 bytes of stderr. |
| `ErrFile` | Source contained no playable audio or video stream. |
| `ErrClosed` | Any method called after `Client.Close()`. |
| `ErrNotConnected` | `SetSource` timed out waiting for the call to reach Connected (10 s default; override with `WithConnectTimeout`). |
| `ErrInternal` | Wrapping for internal errors that shouldn't normally occur. |
| `ErrWrongMode` | WebRTC-only method called on an RTMP call (or vice versa). |

## Concurrency model

- One `*Client` per process multiplexes any number of group calls.
- All public methods are safe for concurrent use.
- Concurrent `CreateCall` / `StartRTMP` for the same chat are deduped — the first wins, others get `ErrConnectionExists` without doing any allocation.
- After `Stop`, the same `chatID` can be re-used cleanly.
- Callbacks fire on a single dispatcher goroutine, so you can safely re-enter the API from inside (`client.Stop(chat)` from `OnStreamEnd` is fine).

## Goroutine budget

Deliberately frugal:

- **3 shared per process** — keepalive ticker, callback dispatcher, DTLS cert pool refill.
- **3 per live call** — audio streamer, video streamer, and one inbound drainer.
- **1 per ffmpeg subprocess** — waits for the process to exit and surfaces the error.
- pion adds ~5–8 of its own per call (ICE/DTLS/SRTP internals) — upstream territory.

Scales linearly with live calls; nothing is allocated per-source-switch or per-frame.

## Networking

- **Transport:** UDP4 + UDP6 by default. Override with `WithNetworkTypes(...)` to restrict or add TCP.
- **STUN / TURN:** not exposed — host candidates only, matching ntgcalls. Telegram's edge learns our post-NAT source peer-reflexively as ICE-CONTROLLED, so STUN adds nothing for this flow.
- **Interface filter:** virtual / VPN interfaces (Docker bridges, WSL, VMware, Tailscale, ZeroTier, OpenVPN, etc.) are skipped automatically. Override is not exposed; report a bug if your interface name is being filtered incorrectly.
- **UDP mux:** default = one socket per call. Pass `WithSharedUDPMux()` to multiplex all calls through one `udp4:0` socket (recommended once you're above ~1 000 concurrent calls — see [UDP mux & scaling](#udp-mux--scaling)).
- **Connect gate:** `SetSource` waits up to 10 s for the call to reach Connected before returning `ErrNotConnected`. Override with `WithConnectTimeout(...)`.
- **ICE timeouts:** internal — 10 s disconnect grace, 30 s failed, 2 s keepalive. Not user-tunable; matches ntgcalls.

## Performance tuning

- **Cert pool** (`WithDTLSCertPool`): default 8; raise for very bursty workloads so `CreateCall` doesn't block on keygen.
- **Dispatch buffer** (`WithDispatchBuffer`): default 256. Raise if you see drop warnings under bursty callback fan-out.
- **Shared UDP mux** (`WithSharedUDPMux`): cuts FD use once you're above ~1 000 concurrent calls.
- **Fast cold-start:** `FromFile` / `FromURL` already inject `-analyzeduration 0 -probesize 64k` to cut ~1–2 s from ffmpeg startup. Add the same flags in your `FromShell` commands if cold-start matters.

### Memory usage

Measured per-process on Linux/amd64, Go 1.26, `GOGC=100`. RSS includes ffmpeg subprocesses. Round figures — your workload will move them ±30 %.

| State | Go heap | ffmpeg RSS (per call) | Total per call |
| --- | --- | --- | --- |
| Idle (no calls) | ~6–8 MB | — | — |
| One audio-only call | +~1–2 MB | ~6–10 MB | ~7–12 MB |
| One audio+video call (720p30) | +~2–3 MB | ~25–40 MB (1 ffmpeg/leg) | ~50–80 MB |
| One RTMP push | +~1 MB | ~20–35 MB | ~20–35 MB |

Audio-only is the cheap path. The 25–40 MB number for video is ffmpeg's encoder state, not gotgcall.

### Concurrency / scaling ballparks

| Concurrent calls | Recommended tuning |
| --- | --- |
| 1–100 | Defaults. Don't touch anything. |
| 100–1 000 | `WithSharedUDPMux()`. Raise FD limit (`ulimit -n 65535`). |
| 1 000–10 000 | Above + `WithDTLSCertPool(64)`, `WithDispatchBuffer(4096)`. Pin GOMAXPROCS. Watch ffmpeg total RSS — this is the bottleneck. |
| 10 000+ | Above + shard across processes; ffmpeg memory dominates at this scale. |

## A/V sync

- Audio and video legs share a wall-clock baseline within microseconds and pace by per-frame duration; drift does not accumulate.
- **Don't apply different time-distortion filters to the two legs** — e.g. `atempo=1.25` on audio without `setpts=PTS/1.25` on video — they will desync linearly.
- In RTMP mode, sync is ffmpeg's responsibility (single muxed push).

## Pitfalls

- **Requesting video on an audio-only source.** Don't pass `Tracks: TrackVideo` unless the container actually has video; you'll get `ErrFile`.
- **Raw PCM/YUV codecs.** `FromShell` rejects raw output up front with `ErrInvalidParams`.
- **`SetSource` blocks until the call is ready** (10 s default). On failure: `ErrNotConnected`.
- **Pause in RTMP mode** causes a brief silence on resume — see [RTMP mode](#rtmp-mode).

## Performance vs ntgcalls

Both use the same codecs at the same bitrates against the same SFU, so wire bandwidth is identical. The differences are operational.

**Apples-to-apples note.** Both stacks run ffmpeg as a subprocess — the difference is *where the encoder lives*. ntgcalls pipes raw `pcm_s16le` / YUV into libwebrtc and encodes Opus / VP8 *in-process*; gotgcall has ffmpeg emit pre-encoded Opus (OGG) / VP8 (IVF) and the library just packetises + SRTPs. Total encoding work is the same — gotgcall just moves it out of your bot process where you can pin it with `-threads 1`.

### CPU per call (audio-only, steady state)

| Component         | ntgcalls                                              | gotgcall                                                  |
| ----------------- | ----------------------------------------------------- | --------------------------------------------------------- |
| Library itself    | ~1.5–2.5 % (Opus encode + RTP + SRTP + jitter)        | **under 1 %** (RTP packetise + SRTP only)                 |
| ffmpeg subprocess | ~0.5–1 % (decode + resample to PCM, no encoder)       | ~1–2 % (decode + resample + Opus encode)                  |
| **Total**         | **~2–3.5 %**                                          | **~1.5–3 %**                                              |

### CPU per call (audio + 720p30 video)

| Component         | ntgcalls                                              | gotgcall                                                  |
| ----------------- | ----------------------------------------------------- | --------------------------------------------------------- |
| Library itself    | ~6–12 % (VP8 + Opus encode + pacer + SRTP)            | **under 1 %** (RTP packetise + SRTP only)                 |
| ffmpeg subprocess | ~3–5 % (decode + YUV output, no encoder)              | ~5–10 % (decode + VP8 + Opus encode)                      |
| **Total**         | **~9–17 %**                                           | **~6–11 %**                                               |

### Memory per call

| Component         | ntgcalls                              | gotgcall                                                  |
| ----------------- | ------------------------------------- | --------------------------------------------------------- |
| Library itself    | ~15–25 MB (libwebrtc state)           | **~1–3 MB** Go heap                                       |
| ffmpeg subprocess | ~5–8 MB (audio) · ~20–30 MB (+video)  | ~6–10 MB (audio) · ~25–40 MB (audio+video)                |
| **Total**         | **~20–33 MB · ~35–55 MB (+video)**    | **~7–13 MB · ~26–43 MB (+video)**                         |

### Everything else

| Dimension                    | ntgcalls (libwebrtc, C++)                     | gotgcall (pure Go)                                           |
| ---------------------------- | --------------------------------------------- | ------------------------------------------------------------ |
| Cold-start to first packet   | ~50–150 ms                                    | ~80–300 ms                                                   |
| Cross-compile / deploy       | libwebrtc + glibc + C++ toolchain + cgo       | `CGO_ENABLED=0 go build` → single static binary → scp → run  |
| Binary size                  | ~20–30 MB                                     | ~12–18 MB                                                    |
| Pause/resume                 | Sub-ms                                        | WebRTC: sub-ms · RTMP: ~100–300 ms gap                       |
| Concurrent calls per process | ~hundreds without tuning                      | Tens of thousands with `WithSharedUDPMux` + raised FDs       |
| Hot-reload of encoder logic  | Recompile + redeploy                          | Swap an ffmpeg flag string at runtime                        |

**The library itself is leaner in gotgcall** — well under a percent of CPU and a few MB of heap per call. The full-pipeline number is higher because ffmpeg is counted; that subprocess cost is bounded (`-threads 1`), inspectable (`ps`, `top`), and isolated (an ffmpeg crash doesn't take the bot down).

**Trade-offs:**

- ntgcalls is leaner per call (no subprocess overhead).
- gotgcall is dramatically easier to deploy and customise (static binary, ffmpeg-flag flexibility).
- For typical music bots (10–500 concurrent calls), the per-call difference is invisible.
- For 10 000+ concurrent calls, ntgcalls' lower memory footprint matters; `WithSharedUDPMux` closes part of that gap.

Numbers are order-of-magnitude estimates — benchmark your workload.

## Why pure Go

`gotgcall` is — at the time of writing — the **first pure-Go library that joins Telegram group calls end-to-end with audio and video**. Every other option in the Go ecosystem until now required wrapping libwebrtc through ntgcalls + cgo + a C++ toolchain.

ntgcalls works fine but pulls in libwebrtc + glibc + a C++ build chain and has a lot of surprises like `panic: segment fault` issues with CGo. Cross-compiling music bots becomes a maintenance burden. `gotgcall` builds with `CGO_ENABLED=0` to a single static binary on every supported platform.

## FAQ

<details>
<summary><b>Is this a port of ntgcalls / pytgcalls to Go?</b></summary>

No — it's an independent implementation with a deliberately ntgcalls-shaped API so existing bot code translates almost line-for-line. ntgcalls wraps libwebrtc (C++); `gotgcall` uses [pion](https://github.com/pion/webrtc), the pure-Go WebRTC stack.
</details>

<details>
<summary><b>Does it work with gogram, MTProto-Go, or other MTProto libraries?</b></summary>

Yes — any of them. The library is blob-only: it produces and consumes JSON strings; you handle the MTProto layer (`phone.JoinGroupCall` / `phone.LeaveGroupCall`) in your bot using whichever MTProto Go library you prefer. The `examples/bot/` directory has a runnable skeleton against [gogram](https://github.com/amarnathcjd/gogram).
</details>

<details>
<summary><b>Can I use this for a Telegram music bot?</b></summary>

That's the primary use case. See [`examples/bot/`](examples/bot) and the [`FromShell` audio recipes](#audio-recipes) for atempo, loudness normalisation, equalizer, fade, mix, and live-radio HLS pipelines. `FromShell` cannot pipe bytes in from another Go process (no stdin source) — fetch with yt-dlp / similar tools to a file or URL first, then point `FromFile` / `FromURL` / `FromShell` at it.
</details>

<details>
<summary><b>Does it support video chats / livestreams / RTMP push?</b></summary>

Yes — three modes:

1. **WebRTC group video.** Send-only audio + video into a normal voice/video chat.
2. **RTMP push.** "Go live" broadcasts to a channel via Telegram's RTMP ingest URL — see [RTMP mode](#rtmp-mode).
3. **Custom ffmpeg.** `FromShell` / `FromShells` lets you point at any decodable container or live source — HLS, RTSP, MJPEG, screen capture, IP camera, etc.
</details>

<details>
<summary><b>Does it support TGCalls / MTProto E2E voice calls?</b></summary>

No — only group calls and channel RTMP livestreams. 1-on-1 MTProto voice/video calls (TGCalls) require a different signalling path that this library does not currently target.
</details>

<details>
<summary><b>What Go version is required?</b></summary>

Go 1.26 or newer (uses `errors.AsType[T]` and a few stdlib refinements added in 1.26).
</details>

<details>
<summary><b>Does it run on Windows?</b></summary>

Yes. Pure-Go means no Make/gcc/clang. Pause/Resume in WebRTC mode uses a channel gate (works on every OS); RTMP mode uses kill+restart-with-`-ss` (also OS-agnostic — `SIGSTOP` would be killed by Telegram's RTMP ingest timeout anyway).
</details>

<details>
<summary><b>How many concurrent calls can one process handle?</b></summary>

The library has no hardcoded limit. The practical ceiling is ffmpeg subprocess count + ICE socket count. Use `WithSharedUDPMux()` to collapse all calls onto one UDP socket once you're above ~100 concurrent calls. See [UDP mux & scaling](#udp-mux--scaling).
</details>

<details>
<summary><b>Where do I report bugs?</b></summary>

Open an issue with logs from `WithVerboseConnectionLogs()` + `WithFFmpegStderrLog()` — that combination covers streamer state, ffmpeg exit, ICE transitions, DTLS, and per-candidate trace.
</details>

## See also

- [pion/webrtc](https://github.com/pion/webrtc) — pure-Go WebRTC stack (the layer underneath).
- [amarnathcjd/gogram](https://github.com/amarnathcjd/gogram) — pure-Go MTProto client used in the example.
- [pytgcalls/ntgcalls](https://github.com/pytgcalls/ntgcalls) — the C++ library this is an alternative to.

## License

MIT — see [LICENSE](LICENSE).
