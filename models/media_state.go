package models

// MediaState mirrors ntgcalls' onUpgrade(MediaState) shape and the
// Telegram MTProto participant flags (muted / video_paused /
// video_stopped / presentation_paused). The field named "Paused" here
// maps to Telegram's video_paused — it is the "outgoing media is not
// actively flowing" bit, not the user-toggled Pause state on its own.
//
//   - Muted: the bot toggled /mute on its outgoing audio.
//   - Paused: outgoing media is not actively flowing — true whenever
//     Muted OR the call was paused via Pause. Mirrors Telegram's
//     video_paused: set whenever the participant's mic is silent,
//     regardless of *why* it is silent.
//   - VideoStopped: the current source has no video track (true after
//     Play / audio-only; false after VPlay / audio+video).
//   - PresentationPaused: same lifecycle as Paused. The library has no
//     presentation/screen-share source, but the field is emitted so
//     downstream MTProto callers can flip presentation_paused
//     uniformly with the other flags.
type MediaState struct {
	Muted              bool `json:"muted,omitzero"`
	Paused             bool `json:"paused,omitzero"`
	VideoStopped       bool `json:"video_stopped,omitzero"`
	PresentationPaused bool `json:"presentation_paused,omitzero"`
}

type ConnState int

const (
	Connecting ConnState = iota
	Connected
	Disconnected
	Failed
	Closed
)

func (s ConnState) String() string {
	switch s {
	case Connecting:
		return "connecting"
	case Connected:
		return "connected"
	case Disconnected:
		return "disconnected"
	case Failed:
		return "failed"
	case Closed:
		return "closed"
	default:
		return "unknown"
	}
}

type NetworkInfo struct {
	State ConnState `json:"state"`
}

type CallInfo struct {
	CaptureTimeMs uint64 `json:"capture_time_ms"`
}
