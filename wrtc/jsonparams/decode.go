package jsonparams

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrUnsupportedMode signals that Telegram's response describes the group
// call as an RTMP livestream ({"rtmp": ...}) or an MTProto broadcast
// stream ({"stream": ...}) rather than a WebRTC call ({"transport": ...}).
// gotgcall has no MTProto segment-stream implementation, so the caller
// must surface this as "not joinable as a voice chat". Mirrors ntgcalls'
// branch in wrtc/src/models/response_payload.cpp:23-30.
var ErrUnsupportedMode = errors.New("call mode unsupported")

// ParseRemote decodes Telegram's response JSON. Lenient: unknown top-level
// keys are ignored. Missing required keys (transport.ufrag/pwd/fingerprints)
// yield a typed error. RTMP/Stream responses yield ErrUnsupportedMode.
func ParseRemote(raw string) (*RemoteParams, error) {
	var probe struct {
		Rtmp   json.RawMessage `json:"rtmp"`
		Stream json.RawMessage `json:"stream"`
	}
	if err := json.Unmarshal([]byte(raw), &probe); err == nil {
		if jsonFieldPresent(probe.Rtmp) {
			return nil, fmt.Errorf("%w: rtmp livestream", ErrUnsupportedMode)
		}
		if jsonFieldPresent(probe.Stream) {
			return nil, fmt.Errorf("%w: mtproto broadcast stream", ErrUnsupportedMode)
		}
	}
	rp := &RemoteParams{}
	if err := json.Unmarshal([]byte(raw), rp); err != nil {
		return nil, fmt.Errorf("decode remote params: %w", err)
	}
	if rp.Transport.Ufrag == "" || rp.Transport.Pwd == "" {
		return nil, fmt.Errorf("remote params missing ice creds")
	}
	if len(rp.Transport.Fingerprints) == 0 {
		return nil, fmt.Errorf("remote params missing fingerprint")
	}
	return rp, nil
}

func jsonFieldPresent(raw json.RawMessage) bool {
	return len(raw) > 0 && !bytes.Equal(raw, []byte("null"))
}
