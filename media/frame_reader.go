package media

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	stdio "io"
	"time"

	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/pion/webrtc/v4/pkg/media/ivfreader"
	"github.com/pion/webrtc/v4/pkg/media/oggreader"

	"github.com/annihilatorrrr/gotgcall/models"
)

// frameReader is the internal interface the Streamer pulls from. It
// parses a byte stream (ogg or ivf) and yields one media.Sample per
// call. Closing it must close the underlying byte stream.
type frameReader interface {
	Next(ctx context.Context) (media.Sample, error)
	Close() error
}

// --- Opus -----------------------------------------------------------------

func newOpusFrameReader(r stdio.Reader) (frameReader, error) {
	// One buffered read covers many 20 ms pages instead of 3 read
	// syscalls per page.
	br := bufio.NewReaderSize(r, 8192)
	// pion validates the OpusHead page. It reads exactly one page, so br
	// is left aligned on the next page for nextPage.
	_, head, err := oggreader.NewWith(br)
	if err == nil && head.ChannelMap != 0 && head.StreamCount > 1 {
		// Multistream (surround) packets aren't plain Opus to the SFU;
		// a -c:a copy of such a source would play as garbage.
		err = fmt.Errorf("multistream opus (%d streams) is not supported", head.StreamCount)
	}
	if err != nil {
		if c, ok := r.(stdio.Closer); ok {
			_ = c.Close()
		}
		return nil, fmt.Errorf("%w: ogg parse: %v", models.ErrFile, err)
	}
	return &opusFrameReader{src: r, br: br}, nil
}

type opusFrameReader struct {
	src           stdio.Reader
	br            *bufio.Reader
	payload       []byte // body of the current page
	lacing        []byte // lacing values of the current page not consumed yet
	carry         []byte // a packet continued from the previous page
	off           int    // offset in payload of the next packet
	header        [27]byte
	segments      [255]byte
	skippedHeader bool
	skippedTags   bool
}

// Next returns one Opus packet per call, timed from its own TOC byte, so
// sources passed through with -c:a copy (10/40/60 ms frames, several
// packets per page) pace as correctly as ffmpeg's 20 ms libopus output.
// Data aliases a reused buffer and is only valid until the following
// Next call. Safe for the Streamer: it calls WriteSample synchronously
// and pion's OpusPayloader copies the frame.
func (o *opusFrameReader) Next(ctx context.Context) (media.Sample, error) {
	for {
		if err := ctx.Err(); err != nil {
			return media.Sample{}, err
		}
		pkt, err := o.nextPacket()
		if err != nil {
			return media.Sample{}, err
		}
		if !o.skippedTags && len(pkt) >= 8 {
			switch string(pkt[:8]) {
			case "OpusHead":
				if !o.skippedHeader {
					o.skippedHeader = true
					continue
				}
			case "OpusTags":
				o.skippedTags = true
				continue
			}
		}
		o.skippedHeader, o.skippedTags = true, true
		dur := opusPacketDuration(pkt)
		if dur == 0 {
			continue // empty or malformed: nothing a decoder could play
		}
		return media.Sample{Data: pkt, Duration: dur}, nil
	}
}

// nextPacket returns the next whole Opus packet. A lacing value below
// 255 ends a packet, so one page may hold several packets and a packet
// may continue onto the next page (header flag 0x01). Packets inside one
// page are returned as slices of the page buffer, without copying.
func (o *opusFrameReader) nextPacket() ([]byte, error) {
	o.carry = o.carry[:0]
	partial, orphan := false, false
	for {
		if len(o.lacing) == 0 {
			continued, err := o.nextPage()
			if err != nil {
				return nil, err
			}
			if !continued {
				// A packet cut off by a fresh page can't be completed.
				o.carry, partial, orphan = o.carry[:0], false, false
			} else if !partial {
				// Tail of a packet whose start was never seen.
				orphan = true
			}
			continue
		}
		n, i, done := 0, 0, false
		for i < len(o.lacing) {
			v := o.lacing[i]
			i++
			n += int(v)
			if v < 255 {
				done = true
				break
			}
		}
		chunk := o.payload[o.off : o.off+n]
		o.off += n
		o.lacing = o.lacing[i:]
		switch {
		case orphan:
			orphan = !done
		case !done:
			if len(o.carry)+n > maxOggPacket {
				return nil, fmt.Errorf("%w: ogg packet over %d bytes", models.ErrFile, maxOggPacket)
			}
			o.carry = append(o.carry, chunk...)
			partial = true
		case partial:
			o.carry = append(o.carry, chunk...)
			return o.carry, nil
		default:
			return chunk, nil
		}
	}
}

// nextPage reads one page into the reused buffers and reports whether
// it continues a packet from the previous page. It is
// oggreader.ParseNextPage without the per-page allocations. The CRC
// check is kept so a corrupt or misaligned stream still errors out
// instead of streaming garbage (nothing here decodes the Opus).
func (o *opusFrameReader) nextPage() (continued bool, err error) {
	h := o.header[:]
	if _, err = stdio.ReadFull(o.br, h); err != nil {
		return false, err
	}
	segs := o.segments[:h[26]]
	if _, err = stdio.ReadFull(o.br, segs); err != nil {
		return false, err
	}
	size := 0
	for _, s := range segs {
		size += int(s)
	}
	if cap(o.payload) < size {
		o.payload = make([]byte, size)
	}
	payload := o.payload[:size]
	if _, err = stdio.ReadFull(o.br, payload); err != nil {
		return false, err
	}
	want := binary.LittleEndian.Uint32(h[22:26])
	clear(h[22:26]) // the CRC field counts as zero in its own checksum
	if oggCRC(oggCRC(oggCRC(0, h), segs), payload) != want {
		return false, errOggChecksum
	}
	o.payload, o.lacing, o.off = payload, segs, 0
	return h[5]&0x01 != 0, nil
}

// opusFrameSize is the frame length of each TOC config (RFC 6716 §3.1).
var opusFrameSize = func() (t [32]time.Duration) {
	for c := range t {
		switch {
		case c < 12: // SILK
			t[c] = [4]time.Duration{10, 20, 40, 60}[c&3] * time.Millisecond
		case c < 16: // Hybrid
			t[c] = [2]time.Duration{10, 20}[c&1] * time.Millisecond
		default: // CELT
			t[c] = [4]time.Duration{2500, 5000, 10000, 20000}[c&3] * time.Microsecond
		}
	}
	return t
}()

// opusPacketDuration is the playback length of one Opus packet from its
// TOC byte (RFC 6716 §3.1-3.2), or 0 if it is empty or malformed.
func opusPacketDuration(p []byte) time.Duration {
	if len(p) == 0 {
		return 0
	}
	frame := opusFrameSize[p[0]>>3]
	var d time.Duration
	switch p[0] & 3 {
	case 0:
		d = frame
	case 1, 2:
		d = 2 * frame
	default:
		if len(p) < 2 {
			return 0
		}
		d = time.Duration(p[1]&0x3f) * frame
	}
	if d > 120*time.Millisecond {
		return 0
	}
	return d
}

var errOggChecksum = errors.New("expected and actual checksum do not match")

// maxOggPacket caps a packet assembled across pages. OpusTags with cover
// art can run to a few MB; anything larger is a broken stream.
const maxOggPacket = 16 << 20

// oggCRCTable is the Ogg CRC-32 (poly 0x04c11db7, unreflected, so
// hash/crc32 can't be used), built once per process.
var oggCRCTable = func() (t [256]uint32) {
	for i := range t {
		r := uint32(i) << 24
		for range 8 {
			if r&0x80000000 != 0 {
				r = r<<1 ^ 0x04c11db7
			} else {
				r <<= 1
			}
		}
		t[i] = r
	}
	return t
}()

func oggCRC(crc uint32, b []byte) uint32 {
	for _, v := range b {
		crc = crc<<8 ^ oggCRCTable[byte(crc>>24)^v]
	}
	return crc
}

func (o *opusFrameReader) Close() error {
	if c, ok := o.src.(stdio.Closer); ok {
		return c.Close()
	}
	return nil
}

// --- VP8 ------------------------------------------------------------------

func newVP8FrameReader(r stdio.Reader, fps int) (frameReader, error) {
	if fps <= 0 {
		fps = 30
	}
	ivf, _, err := ivfreader.NewWith(r)
	if err != nil {
		if c, ok := r.(stdio.Closer); ok {
			_ = c.Close()
		}
		return nil, fmt.Errorf("%w: ivf parse: %v", models.ErrFile, err)
	}
	return &vp8FrameReader{src: r, ivf: ivf, frameDur: time.Second / time.Duration(fps)}, nil
}

type vp8FrameReader struct {
	src      stdio.Reader
	ivf      *ivfreader.IVFReader
	frameDur time.Duration
}

func (v *vp8FrameReader) Next(ctx context.Context) (media.Sample, error) {
	if err := ctx.Err(); err != nil {
		return media.Sample{}, err
	}
	payload, _, err := v.ivf.ParseNextFrame()
	if err != nil {
		return media.Sample{}, err
	}
	return media.Sample{Data: payload, Duration: v.frameDur}, nil
}

func (v *vp8FrameReader) Close() error {
	if c, ok := v.src.(stdio.Closer); ok {
		return c.Close()
	}
	return nil
}

// NewOpusFrameReader exposes the internal opus reader for callers that
// have a raw ogg byte stream (e.g. instances/group_call). A returned
// Sample's Data aliases the reader's buffers and is only valid until the
// next Next call; copy it to keep it longer.
func NewOpusFrameReader(r stdio.Reader) (FrameReader, error) {
	fr, err := newOpusFrameReader(r)
	return fr, err
}

// NewVP8FrameReader exposes the internal vp8 reader.
func NewVP8FrameReader(r stdio.Reader, fps int) (FrameReader, error) {
	fr, err := newVP8FrameReader(r, fps)
	return fr, err
}

// FrameReader is the exported alias for the internal frameReader so
// other packages in the module can hold one.
type FrameReader = frameReader
