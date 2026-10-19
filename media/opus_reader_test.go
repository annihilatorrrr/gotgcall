package media

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/annihilatorrrr/gotgcall/models"
)

func oggPage(flags byte, lacing, body []byte) []byte {
	p := make([]byte, 27, 27+len(lacing)+len(body))
	copy(p, "OggS")
	p[5] = flags
	p[26] = byte(len(lacing))
	p = append(append(p, lacing...), body...)
	binary.LittleEndian.PutUint32(p[22:26], oggCRC(0, p))
	return p
}

func lace(n int) []byte {
	l := bytes.Repeat([]byte{255}, n/255)
	return append(l, byte(n%255))
}

func opusPacket(toc byte, n int) []byte {
	p := bytes.Repeat([]byte{0xAB}, n)
	p[0] = toc
	return p
}

func TestOpusPacketDuration(t *testing.T) {
	for _, c := range []struct {
		p    []byte
		want time.Duration
	}{
		{nil, 0},
		{[]byte{31 << 3}, 20 * time.Millisecond},      // CELT FB 20 ms
		{[]byte{30 << 3}, 10 * time.Millisecond},      // CELT FB 10 ms
		{[]byte{16 << 3}, 2500 * time.Microsecond},    // CELT NB 2.5 ms
		{[]byte{3 << 3}, 60 * time.Millisecond},       // SILK NB 60 ms
		{[]byte{13 << 3}, 20 * time.Millisecond},      // Hybrid SWB 20 ms
		{[]byte{30<<3 | 1}, 20 * time.Millisecond},    // two 10 ms frames
		{[]byte{31<<3 | 3, 3}, 60 * time.Millisecond}, // code 3, three 20 ms frames
		{[]byte{31<<3 | 3, 0}, 0},                     // code 3 without frames
		{[]byte{31<<3 | 3}, 0},                        // code 3 missing count byte
		{[]byte{3<<3 | 3, 3}, 0},                      // 180 ms is over the 120 ms limit
	} {
		if got := opusPacketDuration(c.p); got != c.want {
			t.Errorf("opusPacketDuration(% x) = %v, want %v", c.p, got, c.want)
		}
	}
}

func TestOpusFrameReaderPackets(t *testing.T) {
	head := append([]byte("OpusHead"), 1, 2, 0x38, 1, 0x80, 0xBB, 0, 0, 0, 0, 0)
	tags := append([]byte("OpusTags"), 0, 0, 0, 0, 0, 0, 0, 0)
	a := opusPacket(30<<3, 300) // 10 ms
	b := opusPacket(30<<3, 40)  // 10 ms, same page as a
	c := opusPacket(31<<3, 600) // 20 ms, split over two pages
	d := opusPacket(3<<3, 50)   // 60 ms
	orphan := opusPacket(31<<3, 70)

	var s []byte
	s = append(s, oggPage(0x02, lace(len(head)), head)...)
	s = append(s, oggPage(0, lace(len(tags)), tags)...)
	s = append(s, oggPage(0, append(lace(len(a)), lace(len(b))...), append(a, b...))...)
	s = append(s, oggPage(0, []byte{255, 255}, c[:510])...)
	s = append(s, oggPage(0x01, append(lace(90), lace(len(d))...), append(c[510:], d...))...)
	s = append(s, oggPage(0x01, append(lace(len(orphan)), lace(len(a))...), append(orphan, a...))...)

	fr, err := newOpusFrameReader(bytes.NewReader(s))
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []struct {
		data []byte
		dur  time.Duration
	}{
		{a, 10 * time.Millisecond},
		{b, 10 * time.Millisecond},
		{c, 20 * time.Millisecond},
		{d, 60 * time.Millisecond},
		{a, 10 * time.Millisecond},
	} {
		got, err := fr.Next(context.Background())
		if err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
		if !bytes.Equal(got.Data, want.data) || got.Duration != want.dur {
			t.Fatalf("packet %d: got %d bytes / %v, want %d bytes / %v", i, len(got.Data), got.Duration, len(want.data), want.dur)
		}
	}
	if _, err = fr.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("after last packet: got %v, want EOF", err)
	}
}

func TestOpusFrameReaderBadChecksum(t *testing.T) {
	head := append([]byte("OpusHead"), 1, 2, 0x38, 1, 0x80, 0xBB, 0, 0, 0, 0, 0)
	p := opusPacket(31<<3, 100)
	bad := oggPage(0, lace(len(p)), p)
	bad[len(bad)-1] ^= 0xFF
	fr, err := newOpusFrameReader(bytes.NewReader(append(oggPage(0x02, lace(len(head)), head), bad...)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = fr.Next(context.Background()); !errors.Is(err, errOggChecksum) {
		t.Fatalf("got %v, want errOggChecksum", err)
	}
}

func TestCopiesAudio(t *testing.T) {
	for _, c := range []struct {
		args []string
		want bool
	}{
		{[]string{"-i", "x", "-c:a", "copy"}, true},
		{[]string{"-i", "x", "-acodec", "copy"}, true},
		{[]string{"-i", "x", "-c", "copy"}, false},
		{[]string{"-i", "x", "-c", "copy", "-c:a", "libopus"}, false},
		{[]string{"-i", "x", "-c:a", "libopus"}, false},
		{[]string{"-i", "copy"}, false},
	} {
		if got := copiesAudio(c.args); got != c.want {
			t.Errorf("copiesAudio(%q) = %v, want %v", c.args, got, c.want)
		}
	}
	args := ensureFFmpegFlags([]string{"-i", "x.webm", "-c:a", "copy"}, TrackAudio)
	for _, f := range []string{"-application", "-frame_duration", "-mapping_family", "-ar", "-ac", "libopus"} {
		for _, a := range args {
			if a == f {
				t.Errorf("copy argv got encoder flag %s: %q", f, args)
			}
		}
	}
}

func TestOpusFrameReaderRejectsMultistream(t *testing.T) {
	stereo := append([]byte("OpusHead"), 1, 2, 0x38, 1, 0x80, 0xBB, 0, 0, 0, 0, 1, 1, 1, 0, 1)
	if _, err := newOpusFrameReader(bytes.NewReader(oggPage(0x02, lace(len(stereo)), stereo))); err != nil {
		t.Fatalf("family 1, one stream: %v", err)
	}
	surround := append([]byte("OpusHead"), 1, 6, 0x38, 1, 0x80, 0xBB, 0, 0, 0, 0, 1, 4, 2, 0, 4, 1, 2, 3, 5)
	if _, err := newOpusFrameReader(bytes.NewReader(oggPage(0x02, lace(len(surround)), surround))); !errors.Is(err, models.ErrFile) {
		t.Fatalf("5.1 multistream: got %v, want ErrFile", err)
	}
}
