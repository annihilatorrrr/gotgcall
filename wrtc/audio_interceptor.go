package wrtc

import (
	"strings"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
)

const (
	audioLevelURI       = "urn:ietf:params:rtp-hdrext:ssrc-audio-level"
	absSendTimeURI      = "http://www.webrtc.org/experiments/rtp-hdrext/abs-send-time"
	transportCCURI      = "http://www.ietf.org/id/draft-holmer-rmcat-transport-wide-cc-extensions-01"
	sdesMidURI          = "urn:ietf:params:rtp-hdrext:sdes:mid"
	videoOrientationURI = "urn:3gpp:video-orientation"
)

// audioLevelInterceptorFactory builds interceptors that stamp the
// ssrc-audio-level (RFC 6464) and abs-send-time extensions on every
// outgoing audio RTP packet. Telegram's SFU silently drops streams that
// don't carry audio-level (it treats them as silence and stops forwarding
// them to listeners); pion's defaults do not stamp these for us.
type audioLevelInterceptorFactory struct{}

func (*audioLevelInterceptorFactory) NewInterceptor(string) (interceptor.Interceptor, error) {
	return &audioLevelInterceptor{}, nil
}

type audioLevelStream struct {
	// absSendBuf is the per-stream 3-byte buffer for abs-send-time.
	// pion's SetExtension copies the slice into the header during marshal
	// (synchronous with Write), so reusing the buffer across packets is
	// safe — the bytes are out on the wire before our next iteration.
	absSendBuf    []byte
	audioLevelID  uint8
	absSendTimeID uint8
	hasAudioLevel bool
	hasAbsSend    bool
}

// audioLevelConstBuf is the constant ssrc-audio-level payload: voice-activity
// bit (0x80) | level in -dBov (0..127), fixed at -20 dBov. Shared across all
// streams since it never mutates.
var audioLevelConstBuf = []byte{0x80 | 20}

type audioLevelInterceptor struct {
	interceptor.NoOp
}

func (*audioLevelInterceptor) BindLocalStream(info *interceptor.StreamInfo, writer interceptor.RTPWriter) interceptor.RTPWriter {
	s := &audioLevelStream{absSendBuf: make([]byte, 3)}
	for _, ext := range info.RTPHeaderExtensions {
		switch ext.URI {
		case audioLevelURI:
			s.audioLevelID = uint8(ext.ID)
			s.hasAudioLevel = true
		case absSendTimeURI:
			s.absSendTimeID = uint8(ext.ID)
			s.hasAbsSend = true
		}
	}
	return interceptor.RTPWriterFunc(func(header *rtp.Header, payload []byte, attrs interceptor.Attributes) (int, error) {
		if s.hasAudioLevel {
			_ = header.SetExtension(s.audioLevelID, audioLevelConstBuf)
		}
		if s.hasAbsSend {
			// 24-bit fixed-point seconds since epoch * 2^18, big-endian.
			// Write into the per-stream buffer (no per-packet alloc); pion's
			// SetExtension copies it into the header during marshal.
			now := time.Now()
			abs := (uint64(now.Unix())<<18 | uint64(now.Nanosecond())*uint64(1<<18)/uint64(1e9)) & 0x00FFFFFF
			s.absSendBuf[0] = byte(abs >> 16)
			s.absSendBuf[1] = byte(abs >> 8)
			s.absSendBuf[2] = byte(abs)
			_ = header.SetExtension(s.absSendTimeID, s.absSendBuf)
		}
		return writer.Write(header, payload, attrs)
	})
}

// markerClearInterceptorFactory fixes RTP marker on outbound audio.
// Pion's packetizer marks every single-payload Opus packet, but per
// RFC 7587 the marker should only be set on the first packet after
// silence. The first packet of each binding gets marker=true so the
// SFU recognises the start of audio; all subsequent packets get
// marker=false to avoid jitter-buffer resync on every frame.
type markerClearInterceptorFactory struct{}

func (*markerClearInterceptorFactory) NewInterceptor(string) (interceptor.Interceptor, error) {
	return &markerClearInterceptor{}, nil
}

type markerClearInterceptor struct {
	interceptor.NoOp
}

func (*markerClearInterceptor) BindLocalStream(info *interceptor.StreamInfo, writer interceptor.RTPWriter) interceptor.RTPWriter {
	if !strings.HasPrefix(info.MimeType, "audio/") {
		return writer
	}
	first := true
	return interceptor.RTPWriterFunc(func(header *rtp.Header, payload []byte, attrs interceptor.Attributes) (int, error) {
		if first {
			header.Marker = true
			first = false
		} else {
			header.Marker = false
		}
		return writer.Write(header, payload, attrs)
	})
}
