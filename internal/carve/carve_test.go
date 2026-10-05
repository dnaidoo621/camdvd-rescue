package carve

import (
	"bytes"
	"testing"
)

// pack builds a 2048-byte MPEG-2 pack with the given SCR base.
func pack(scr uint64) []byte {
	b := make([]byte, SectorSize)
	copy(b, packStart)
	b[4] = byte(0x40 | (scr>>30&0x07)<<3 | 0x04 | scr>>28&0x03)
	b[5] = byte(scr >> 20)
	b[6] = byte((scr>>15&0x1F)<<3 | 0x04 | scr>>13&0x03)
	b[7] = byte(scr >> 5)
	b[8] = byte((scr&0x1F)<<3 | 0x04)
	b[9] = 0x01
	b[10], b[11], b[12] = 0x01, 0x89, 0xC3
	b[13] = 0xF8
	return b
}

type image struct{ bytes.Buffer }

func (im *image) packs(n int, startSec float64) {
	for i := range n {
		// About 2.5 packs per 1/25 s frame at camcorder bitrates; exact
		// spacing doesn't matter, only continuity.
		im.Write(pack(uint64((startSec + float64(i)*0.01) * SCRHz)))
	}
}

func (im *image) zeros(n int) { im.Write(make([]byte, n*SectorSize)) }

func scan(t *testing.T, im *image, opt Options) []Clip {
	t.Helper()
	r := bytes.NewReader(im.Bytes())
	clips, err := Scan(r, int64(im.Len()), opt)
	if err != nil {
		t.Fatal(err)
	}
	return clips
}

func TestParseSCRRoundTrip(t *testing.T) {
	for _, v := range []uint64{0, 1, 90000, 1<<33 - 1, 0x123456789 & (1<<33 - 1)} {
		got, ok := ParseSCR(pack(v))
		if !ok || got != v {
			t.Errorf("ParseSCR(pack(%d)) = %d, %v", v, got, ok)
		}
	}
	if _, ok := ParseSCR(make([]byte, SectorSize)); ok {
		t.Error("zero sector parsed as a pack")
	}
	mpeg1 := pack(0)
	mpeg1[4] = 0x21
	if _, ok := ParseSCR(mpeg1); ok {
		t.Error("MPEG-1 pack accepted")
	}
}

func TestSplitOnNonVideoSector(t *testing.T) {
	var im image
	im.zeros(16) // reserved file-system area of an unfinalized disc
	im.packs(300, 0)
	im.zeros(1)
	im.packs(200, 0.5) // clock continues, but a gap still splits
	clips := scan(t, &im, DefaultOptions())
	if len(clips) != 2 {
		t.Fatalf("got %d clips, want 2", len(clips))
	}
	if clips[0].Segments[0] != (Segment{16, 316}) || clips[1].Sectors != 200 {
		t.Errorf("unexpected clips %+v", clips)
	}
	if clips[0].Index != 1 || clips[1].Index != 2 {
		t.Errorf("indexes %d, %d", clips[0].Index, clips[1].Index)
	}
}

func TestSplitOnClockJumps(t *testing.T) {
	var im image
	im.packs(150, 100) // 100.00 .. 101.49 s
	im.packs(150, 5)   // backward: new recording
	im.packs(150, 7)   // +0.5 s: same recording
	im.packs(150, 30)  // +21.5 s: new recording
	clips := scan(t, &im, DefaultOptions())
	if len(clips) != 3 {
		t.Fatalf("got %d clips, want 3: %+v", len(clips), clips)
	}
	if clips[1].Sectors != 300 {
		t.Errorf("middle clip has %d sectors, want 300", clips[1].Sectors)
	}
}

func TestDropShortRuns(t *testing.T) {
	var im image
	im.packs(99, 0)
	im.zeros(1)
	im.packs(100, 50)
	clips := scan(t, &im, DefaultOptions())
	if len(clips) != 1 || clips[0].Sectors != 100 || clips[0].Index != 1 {
		t.Fatalf("got %+v", clips)
	}
}

func TestBadSectorsBridgedAndCounted(t *testing.T) {
	var im image
	im.packs(150, 0)
	im.zeros(3) // unreadable, zero-filled by imaging
	im.packs(150, 1.6)
	bad := func(lba int64) bool { return lba >= 150 && lba < 153 }
	opt := DefaultOptions()
	opt.Bad = bad
	clips := scan(t, &im, opt)
	if len(clips) != 1 {
		t.Fatalf("got %d clips, want 1", len(clips))
	}
	c := clips[0]
	if c.BadSectors != 3 || c.Sectors != 300 || len(c.Segments) != 2 {
		t.Errorf("got %+v", c)
	}
	var out bytes.Buffer
	if err := Write(bytes.NewReader(im.Bytes()), c, &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 300*SectorSize {
		t.Errorf("wrote %d bytes", out.Len())
	}
	if d := c.Duration(); d < 3.0 || d > 3.2 {
		t.Errorf("duration %.2f", d)
	}
}

func TestBadSectorsDoNotBridgeAClockJump(t *testing.T) {
	var im image
	im.packs(150, 0)
	im.zeros(2)
	im.packs(150, 60)
	opt := DefaultOptions()
	opt.Bad = func(lba int64) bool { return lba == 150 || lba == 151 }
	clips := scan(t, &im, opt)
	if len(clips) != 2 || clips[0].BadSectors != 0 || clips[1].BadSectors != 0 {
		t.Fatalf("got %+v", clips)
	}
}

// packTC is a pack carrying a GOP header with the given timecode, as the
// first pack of each video GOP does.
func packTC(scr uint64, h, m, s, pic uint32) []byte {
	b := pack(scr)
	t := h<<19 | m<<13 | 1<<12 | s<<6 | pic
	v := t << 7
	copy(b[40:], []byte{0, 0, 1, 0xB8, byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
	return b
}

// segment writes n packs starting at clock 0 (as a Hitachi camcorder does
// every ~21.6 s) with GOP timecodes counting from startSec, one GOP per 12
// packs.
func (im *image) segment(n int, startSec float64) {
	for i := range n {
		scr := uint64(float64(i) * 0.04 * SCRHz)
		if i%12 == 0 {
			sec := startSec + float64(i)*0.04
			whole := uint32(sec)
			im.Write(packTC(scr, 0, whole/60, whole%60, uint32((sec-float64(whole))*25)))
		} else {
			im.Write(pack(scr))
		}
	}
}

func TestTimecodeBridgesClockResets(t *testing.T) {
	var im image
	im.segment(540, 0)    // recording 1: 0–21.6 s
	im.segment(540, 21.6) // clock resets, timecode continues: same recording
	im.segment(200, 43.2) // and again
	im.segment(300, 0)    // timecode restarts: recording 2
	clips := scan(t, &im, DefaultOptions())
	if len(clips) != 2 {
		t.Fatalf("got %d clips, want 2: %+v", len(clips), clips)
	}
	if clips[0].Sectors != 1280 || clips[0].Resets != 2 || clips[1].Sectors != 300 {
		t.Errorf("clips %+v", clips)
	}
	if d := clips[0].Duration(); d < 50 || d > 52 {
		t.Errorf("recording 1 lasts %.1f s across resets, want ~51", d)
	}
}

func TestGOPTimecode(t *testing.T) {
	b := packTC(0, 1, 2, 3, 4)
	if got := GOPTimecode(b[14:]); got != ((1*60+2)*60+3)*64+4 {
		t.Errorf("got %d", got)
	}
	if GOPTimecode(make([]byte, 100)) != -1 {
		t.Error("no GOP should be -1")
	}
	bad := packTC(0, 0, 0, 1, 0)
	bad[40+5] &^= 0x08 // clear the marker bit (bit 19 of the time code word)
	if GOPTimecode(bad[14:]) != -1 {
		t.Error("missing marker accepted")
	}
}
