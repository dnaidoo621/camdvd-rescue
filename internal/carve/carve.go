// Package carve recovers MPEG-2 program-stream recordings from a raw DVD
// image that has no usable file system (an unfinalized camcorder disc).
//
// A DVD video stream is a sequence of 2048-byte packs, each starting with an
// MPEG-2 pack header (00 00 01 BA) on a sector boundary. Consecutive packs form
// a run. A recording ends where the run is broken by a non-video sector or the
// stream clock (SCR) jumps, because each camcorder recording restarts its clock.
package carve

import (
	"bytes"
	"errors"
	"fmt"
	"io"
)

// SectorSize is the DVD logical sector size.
const SectorSize = 2048

// SCRHz is the frequency of the MPEG system clock base.
const SCRHz = 90000

// Options controls how runs of packs are split into clips.
type Options struct {
	// MaxJump is the largest forward SCR step, in seconds, that still
	// continues a clip. Any backward step always starts a new clip.
	MaxJump float64
	// MinSectors drops clips shorter than this many sectors.
	MinSectors int64
	// Bad reports whether a sector was unreadable and zero-filled. Bad
	// sectors inside a recording don't split it; the clip is flagged instead.
	Bad func(lba int64) bool
}

// DefaultOptions are the rules from the spec: split on a jump over 10 s,
// drop runs under 100 sectors.
func DefaultOptions() Options {
	return Options{MaxJump: 10, MinSectors: 100}
}

// Segment is a half-open sector range [Start, End).
type Segment struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

// Clip is one recovered recording.
type Clip struct {
	Index      int       `json:"index"`
	Segments   []Segment `json:"segments"`
	Sectors    int64     `json:"sectors"`
	FirstSCR   uint64    `json:"first_scr"`
	LastSCR    uint64    `json:"last_scr"`
	BadSectors int64     `json:"bad_sectors"`
}

// Duration is the clip length estimated from its stream clock.
func (c Clip) Duration() float64 {
	if c.LastSCR <= c.FirstSCR {
		return 0
	}
	return float64(c.LastSCR-c.FirstSCR) / SCRHz
}

var packStart = []byte{0x00, 0x00, 0x01, 0xBA}

// ParseSCR returns the 33-bit SCR base of an MPEG-2 pack header at the start
// of b, and false if b doesn't start with one.
func ParseSCR(b []byte) (uint64, bool) {
	if len(b) < 14 || !bytes.Equal(b[:4], packStart) {
		return 0, false
	}
	b4, b5, b6, b7, b8 := uint64(b[4]), uint64(b[5]), uint64(b[6]), uint64(b[7]), uint64(b[8])
	// MPEG-2 packs mark the first SCR byte with '01'; MPEG-1 uses '0010'.
	if b4>>6 != 1 {
		return 0, false
	}
	// Marker bits must be set, or this is a stray start code in other data.
	if b4&0x04 == 0 || b6&0x04 == 0 || b8&0x04 == 0 || b[9]&0x01 == 0 {
		return 0, false
	}
	scr := (b4>>3&0x07)<<30 | (b4&0x03)<<28 | b5<<20 | (b6>>3&0x1F)<<15 | (b6&0x03)<<13 | b7<<5 | b8>>3
	return scr, true
}

// Scan reads size bytes from r sector by sector and returns the clips found.
func Scan(r io.ReaderAt, size int64, opt Options) ([]Clip, error) {
	if opt.MaxJump <= 0 {
		opt.MaxJump = DefaultOptions().MaxJump
	}
	maxJump := uint64(opt.MaxJump * SCRHz)
	bad := opt.Bad
	if bad == nil {
		bad = func(int64) bool { return false }
	}

	var clips []Clip
	var cur *Clip
	// pendingBad holds bad sectors seen inside the current clip that may be
	// bridged if the stream resumes with a continuous clock.
	var pendingBad int64
	var lastSCR uint64

	closeClip := func() {
		if cur != nil && cur.Sectors >= opt.MinSectors {
			cur.Index = len(clips) + 1
			clips = append(clips, *cur)
		}
		cur = nil
		pendingBad = 0
	}
	addSector := func(lba int64) {
		n := len(cur.Segments)
		if n > 0 && cur.Segments[n-1].End == lba {
			cur.Segments[n-1].End++
		} else {
			cur.Segments = append(cur.Segments, Segment{lba, lba + 1})
		}
		cur.Sectors++
	}

	const chunk = 512
	buf := make([]byte, chunk*SectorSize)
	total := size / SectorSize
	for base := int64(0); base < total; base += chunk {
		n := min(chunk, total-base)
		if _, err := r.ReadAt(buf[:n*SectorSize], base*SectorSize); err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("read sector %d: %w", base, err)
		}
		for i := int64(0); i < n; i++ {
			lba := base + i
			sec := buf[i*SectorSize : (i+1)*SectorSize]
			scr, ok := ParseSCR(sec)
			if !ok {
				if cur != nil && bad(lba) {
					pendingBad++
					continue
				}
				closeClip()
				continue
			}
			if cur != nil {
				backward := scr < lastSCR
				jump := scr > lastSCR && scr-lastSCR > maxJump
				if backward || jump {
					closeClip()
				}
			}
			if cur == nil {
				cur = &Clip{FirstSCR: scr}
			}
			cur.BadSectors += pendingBad
			pendingBad = 0
			addSector(lba)
			cur.LastSCR = scr
			lastSCR = scr
		}
	}
	closeClip()
	return clips, nil
}

// Write copies a clip's sectors from r to w.
func Write(r io.ReaderAt, c Clip, w io.Writer) error {
	for _, s := range c.Segments {
		sr := io.NewSectionReader(r, s.Start*SectorSize, (s.End-s.Start)*SectorSize)
		if _, err := io.Copy(w, sr); err != nil {
			return err
		}
	}
	return nil
}
