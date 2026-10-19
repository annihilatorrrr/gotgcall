package native

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4/pkg/media"
)

// RTPWriter is the minimum surface a Track needs from a write path —
// satisfied by *srtp.WriteStreamSRTP for session-based use and by our
// own srtpWriter shim for the encrypt-only path.
type RTPWriter interface {
	WriteRTP(header *rtp.Header, payload []byte) (int, error)
}

// Kind disambiguates audio and video tracks at construction.
type Kind int

const (
	KindAudio Kind = iota
	KindVideo
)

// Track is the send-only RTP writer for one media kind. WriteSample
// packetises a single encoded frame (Opus or VP8) into one or more RTP
// packets, stamps required header extensions, and emits each via the
// shared SRTP write stream. There are no per-track goroutines — every
// call to WriteSample / GeneratePadding executes synchronously on the
// caller's goroutine (the streamer pacer).
//
// Concurrency:
//   - writeStream is published once (Connect → AttachWriteStream) and
//     thereafter read-only from many goroutines. Stored in an
//     atomic.Pointer so the hot path reads it lock-free.
//   - writeMu serialises writePackets calls within a single Track so
//     the streamer goroutine and the keepalive goroutine cannot mutate
//     absSendBuf / extBuf concurrently (SetExtension stores the slice by
//     reference; pion/rtp marshals it during the subsequent SRTP write,
//     and EncryptRTP re-parses the header back into extBuf, so both are
//     in-flight until WriteRTP returns).
//   - The mutex is per-Track. Audio and video Tracks have independent
//     mutexes; the shared SRTP context further down still serializes
//     encryption across both via Stack.srtpWriteMu.
type Track struct {
	packetizer  rtp.Packetizer
	writeStream atomic.Pointer[writerSlot]
	mid         string
	absSendBuf  []byte
	midBuf      []byte
	extBuf      [3]rtp.Extension // backing array for every packet's Extensions
	writeMu     sync.Mutex
	kind        Kind
	ssrc        uint32
	clockRate   uint32
	firstAudio  atomic.Bool
	pt          uint8
}

// writerSlot wraps the RTPWriter interface so it can sit inside an
// atomic.Pointer (which needs a struct, not an interface).
type writerSlot struct{ w RTPWriter }

// NewTrack constructs the packetiser side of a track. The SRTP write
// stream is wired in later via Track.AttachWriteStream once the SRTP
// session is up.
func NewTrack(kind Kind, ssrc uint32) *Track {
	t := &Track{
		kind:       kind,
		ssrc:       ssrc,
		absSendBuf: make([]byte, 3),
	}
	switch kind {
	case KindAudio:
		t.pt = 111
		t.clockRate = 48000
		t.mid = "0"
		t.packetizer = rtp.NewPacketizerWithOptions(
			1200,
			&codecs.OpusPayloader{},
			rtp.NewRandomSequencer(),
			t.clockRate,
			rtp.WithSSRC(ssrc),
			rtp.WithPayloadType(t.pt),
		)
		t.firstAudio.Store(true)
	case KindVideo:
		t.pt = 100
		t.clockRate = 90000
		t.mid = "1"
		t.packetizer = rtp.NewPacketizerWithOptions(
			1200,
			&codecs.VP8Payloader{EnablePictureID: true},
			rtp.NewRandomSequencer(),
			t.clockRate,
			rtp.WithSSRC(ssrc),
			rtp.WithPayloadType(t.pt),
		)
	}
	t.midBuf = []byte(t.mid)
	return t
}

// AttachWriteStream binds the SRTP write stream so subsequent
// WriteSample calls actually go out on the wire. Before this, the
// packetiser is built and waiting; samples written pre-attachment
// silently no-op (matches old TrackLocalStaticSample behavior
// pre-PeerConnection-Connected).
func (t *Track) AttachWriteStream(ws RTPWriter) {
	t.writeStream.Store(&writerSlot{w: ws})
}

// SSRC reports the SSRC this track packetises into.
func (t *Track) SSRC() uint32 { return t.ssrc }

// WriteSample packetises and sends one encoded frame.
func (t *Track) WriteSample(s media.Sample) error {
	// Integer math: the float form truncates e.g. 60 ms to 2879 ticks.
	samples := uint32(s.Duration * time.Duration(t.clockRate) / time.Second)
	pkts := t.packetizer.Packetize(s.Data, samples)
	if len(pkts) == 0 {
		return nil
	}
	return t.writePackets(pkts)
}

// GeneratePadding emits N RTP padding packets — used by the keepalive
// loop to keep Telegram's SFU video SSRC binding warm. No payload is
// generated; the SFU strips the padding before forwarding.
func (t *Track) GeneratePadding(count uint32) error {
	pkts := t.packetizer.GeneratePadding(count)
	if len(pkts) == 0 {
		return nil
	}
	return t.writePackets(pkts)
}

func (t *Track) writePackets(pkts []*rtp.Packet) error {
	slot := t.writeStream.Load()
	if slot == nil {
		return nil
	}
	ws := slot.w

	t.writeMu.Lock()
	defer t.writeMu.Unlock()

	// Compute abs-send-time once per writePackets call: every packet from
	// a single sample shares the same send instant within the resolution
	// we care about. Reusing one 3-byte buffer across packets within the
	// mutex avoids per-packet heap traffic; pion/rtp marshals the slice
	// into the wire header during WriteRTP (still inside this lock), so
	// reuse is safe against concurrent writers on the SAME Track.
	now := time.Now()
	abs := (uint64(now.Unix())<<18 | uint64(now.Nanosecond())*uint64(1<<18)/uint64(1e9)) & 0x00FFFFFF
	t.absSendBuf[0] = byte(abs >> 16)
	t.absSendBuf[1] = byte(abs >> 8)
	t.absSendBuf[2] = byte(abs)

	for i, pkt := range pkts {
		t.stampExtensions(&pkt.Header, i == 0)
		if _, err := ws.WriteRTP(&pkt.Header, pkt.Payload); err != nil {
			return err
		}
	}
	return nil
}

// stampExtensions writes the per-packet RTP header extensions Telegram
// requires. Audio packets carry ssrc-audio-level + abs-send-time + sdes-mid;
// video packets carry abs-send-time + sdes-mid. RFC-7587-correct marker
// bit handling: audio sets marker=true on the very first packet emitted
// from the track (silence boundary), false thereafter.
func (t *Track) stampExtensions(h *rtp.Header, firstOfFrame bool) {
	h.Extension = false
	h.Extensions = t.extBuf[:0]

	if t.kind == KindAudio {
		// audio-level: voice-activity bit (0x80) | level in -dBov (20).
		_ = h.SetExtension(ExtAudioLevelID, audioLevelConstBuf)
	}
	_ = h.SetExtension(ExtAbsSendTimeID, t.absSendBuf)
	_ = h.SetExtension(ExtSdesMidID, t.midBuf)

	if t.kind == KindAudio {
		if firstOfFrame && t.firstAudio.CompareAndSwap(true, false) {
			h.Marker = true
		} else {
			h.Marker = false
		}
	}
}

// audioLevelConstBuf is the constant ssrc-audio-level payload:
// voice-activity bit (0x80) | level in -dBov (fixed at -20 dBov).
// Shared across all tracks — it never mutates.
var audioLevelConstBuf = []byte{0x80 | 20}
