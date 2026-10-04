package carve

import (
	"bytes"
	"testing"
)

// Disc images are untrusted input: the carver must never panic or loop on
// garbage, and every clip must lie inside the image.
func FuzzScan(f *testing.F) {
	var im image
	im.packs(120, 0)
	im.zeros(2)
	im.packs(110, 50)
	f.Add(im.Bytes())
	f.Add(pack(0))
	f.Add(make([]byte, SectorSize*3))
	f.Fuzz(func(t *testing.T, b []byte) {
		opt := DefaultOptions()
		opt.MinSectors = 1
		clips, err := Scan(bytes.NewReader(b), int64(len(b)), opt)
		if err != nil {
			t.Fatal(err)
		}
		total := int64(len(b)) / SectorSize
		for _, c := range clips {
			for _, s := range c.Segments {
				if s.Start < 0 || s.End > total || s.Start >= s.End {
					t.Fatalf("segment %+v outside %d sectors", s, total)
				}
			}
		}
	})
}

func FuzzParseSCR(f *testing.F) {
	f.Add(pack(90000))
	f.Add([]byte{0, 0, 1, 0xBA})
	f.Fuzz(func(t *testing.T, b []byte) {
		if scr, ok := ParseSCR(b); ok && scr >= 1<<33 {
			t.Fatalf("SCR %d exceeds 33 bits", scr)
		}
	})
}
