// Package dvdifo reads the title table of a DVD-Video VIDEO_TS.IFO, so each
// title and chapter can be handed to FFmpeg's dvdvideo demuxer.
package dvdifo

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
)

// Title is one entry of the title search pointer table (TT_SRPT).
type Title struct {
	Number   int `json:"number"`   // 1-based title number on the disc
	Chapters int `json:"chapters"` // part-of-title count
	Angles   int `json:"angles"`
	VTS      int `json:"vts"`       // title set holding the title
	VTSTitle int `json:"vts_title"` // title number within that set
}

// ReadFile parses VIDEO_TS.IFO.
func ReadFile(path string) ([]Title, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

// Parse parses the bytes of a VIDEO_TS.IFO.
func Parse(b []byte) ([]Title, error) {
	if len(b) < 0x100 || string(b[:12]) != "DVDVIDEO-VMG" {
		return nil, errors.New("not a VIDEO_TS.IFO (missing DVDVIDEO-VMG)")
	}
	off := int(binary.BigEndian.Uint32(b[0xC4:])) * 2048
	if off == 0 || off+8 > len(b) {
		return nil, fmt.Errorf("title table at %d is outside the %d-byte IFO", off, len(b))
	}
	n := int(binary.BigEndian.Uint16(b[off:]))
	if off+8+n*12 > len(b) {
		return nil, fmt.Errorf("title table claims %d titles, more than the IFO holds", n)
	}
	titles := make([]Title, n)
	for i := range n {
		e := b[off+8+i*12:]
		titles[i] = Title{
			Number:   i + 1,
			Angles:   int(e[1]),
			Chapters: int(binary.BigEndian.Uint16(e[2:])),
			VTS:      int(e[6]),
			VTSTitle: int(e[7]),
		}
	}
	return titles, nil
}
