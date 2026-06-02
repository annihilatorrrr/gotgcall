package wrtc

import (
	"strings"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
)

// midStampInterceptorFactory builds interceptors that stamp the sdes-mid
// (urn:ietf:params:rtp-hdrext:sdes:mid) extension onto every outgoing RTP
// packet. pion v4 negotiates this extension in SDP but its built-in
// interceptors don't emit it — verified against
// github.com/pion/interceptor@v0.1.45, where only the TWCC interceptor
// stamps RTP header extensions.
//
// Telegram's SFU may use sdes-mid for BUNDLE demux of incoming participant
// media (matching how every other modern WebRTC SFU identifies bundled
// streams). Today we rely on the FID ssrc-groups in the JOIN payload to
// declare our video SSRC, but stamping mid is cheap belt-and-suspenders.
//
// mid values are derived from MimeType because our codec map is fixed
// (audio added before video in NewPeerConnection → mid 0 for audio, mid 1
// for video). If the transceiver order ever changes, update the switch
// below in lockstep.
type midStampInterceptorFactory struct{}

func (*midStampInterceptorFactory) NewInterceptor(string) (interceptor.Interceptor, error) {
	return &midStampInterceptor{}, nil
}

type midStampInterceptor struct {
	interceptor.NoOp
}

func (*midStampInterceptor) BindLocalStream(info *interceptor.StreamInfo, writer interceptor.RTPWriter) interceptor.RTPWriter {
	var midExtID uint8
	for _, ext := range info.RTPHeaderExtensions {
		if ext.URI == sdesMidURI {
			midExtID = uint8(ext.ID)
			break
		}
	}
	if midExtID == 0 {
		return writer
	}
	var midVal []byte
	switch {
	case strings.HasPrefix(info.MimeType, "audio/"):
		midVal = []byte("0")
	case strings.HasPrefix(info.MimeType, "video/"):
		midVal = []byte("1")
	}
	if midVal == nil {
		return writer
	}
	return interceptor.RTPWriterFunc(func(header *rtp.Header, payload []byte, attrs interceptor.Attributes) (int, error) {
		_ = header.SetExtension(midExtID, midVal)
		return writer.Write(header, payload, attrs)
	})
}
