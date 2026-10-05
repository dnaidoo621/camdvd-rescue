// Package drive talks to optical drives. PhysicalDrive drives real hardware
// through ioctls and the imaging tools; ImageDrive serves a disc image with
// simulated insert, eject and unreadable sectors, for tests and demo mode.
package drive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/dnaidoo621/camdvd-rescue/internal/discinfo"
)

// SectorSize is the DVD logical sector size.
const SectorSize = 2048

// TrayStatus is what CDROM_DRIVE_STATUS reports.
type TrayStatus int

const (
	StatusNoInfo TrayStatus = iota
	StatusNoDisc
	StatusTrayOpen
	StatusNotReady
	StatusDiscOK
)

func (s TrayStatus) String() string {
	return [...]string{"unknown", "empty", "tray open", "not ready", "disc"}[s]
}

// Info describes a drive for the UI and doctor.
type Info struct {
	ID     string `json:"id"`
	Block  string `json:"block"`
	SG     string `json:"sg"`
	Model  string `json:"model"`
	Demo   bool   `json:"demo"`
	Loader string `json:"loader,omitempty"` // tray or slot, when known
}

// ImageRequest asks for a side to be imaged.
type ImageRequest struct {
	Method  discinfo.ImagingMethod
	Sectors int64 // how much to read; 0 = whole reported capacity
	// Extents are the written ranges of an open session; nil reads all.
	Extents []Range
	Out     string
	Map     string
}

// Progress is reported while imaging.
type Progress struct {
	Done, Total, Bad int64 // bytes
}

// ImageResult summarizes an imaging run.
type ImageResult struct {
	Sectors   int64    `json:"sectors"`
	Bad       Ranges   `json:"bad"`
	CmdLines  []string `json:"cmdlines"`
	Stderr    string   `json:"stderr"`
	StoppedAt int64    `json:"stopped_at,omitempty"` // raw reads: unwritten tail found here
}

// Drive is the hardware boundary.
type Drive interface {
	Info() Info
	Status(ctx context.Context) (TrayStatus, error)
	Eject(ctx context.Context) error
	// Probe reports media and file system before any read.
	Probe(ctx context.Context) (discinfo.Probe, string, error)
	// ReadSectors reads n sectors at lba; raw selects the sg path that open
	// sessions need.
	ReadSectors(ctx context.Context, lba, n int64, raw bool) ([]byte, error)
	Image(ctx context.Context, req ImageRequest, progress func(Progress)) (ImageResult, error)
}

// ErrMediaRemoved is returned when the disc goes away mid-read.
var ErrMediaRemoved = errors.New("disc removed")

// Fingerprint identifies a side without reading all of it: media type,
// written size, and a hash of 1 MB of written data. For an open session that
// sample is the last 1 MB of the last written track, because the start of an
// unfinalized side is an unwritten file-system area that both sides share.
func Fingerprint(ctx context.Context, d Drive, p discinfo.Probe) (string, error) {
	used := p.Media.UsedSectors()
	const n = 512
	lba, cnt, raw := int64(0), int64(n), false
	if p.Media.Open() && p.FSType == "" {
		raw = true
		exts := p.Media.Extents()
		if len(exts) == 0 {
			return "", fmt.Errorf("no written tracks to sample")
		}
		last := exts[len(exts)-1]
		lba = max(last.Start, last.End-n)
		cnt = last.End - lba
	} else if used > 0 {
		cnt = min(cnt, used)
	}
	b, err := d.ReadSectors(ctx, lba, cnt, raw)
	if err != nil {
		return "", fmt.Errorf("read fingerprint sample at sector %d: %w", lba, err)
	}
	h := sha256.New()
	fmt.Fprintf(h, "%s|%d|%d|", p.Media.Type, p.Media.Capacity, used)
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}

// copyLoop images by reading chunks through read, keeping a ddrescue-format
// mapfile so a cancelled run resumes. A failed chunk is retried in smaller
// pieces, down to single sectors; unreadable sectors are zero-filled and
// marked bad.
//
// extents limits reading to the written ranges of an open session (nil reads
// [0, total)); the unwritten gaps between camcorder tracks are left as zeros
// and stay "non-tried" in the map, so they never count as damage. stopAfter
// > 0 ends the last extent at that many consecutive unreadable sectors, the
// unwritten tail when the drive doesn't report the last recorded address.
func copyLoop(ctx context.Context, read func(ctx context.Context, lba, n int64) ([]byte, error),
	total int64, extents []Range, out, mapPath string, stopAfter int64, progress func(Progress)) (ImageResult, error) {

	res := ImageResult{Sectors: total}
	if len(extents) == 0 {
		extents = []Range{{0, total}}
	}
	var want int64
	for _, e := range extents {
		if e.Start < 0 || e.End > total || e.Start >= e.End {
			return res, fmt.Errorf("extent %d-%d outside 0-%d", e.Start, e.End, total)
		}
		want += e.Len()
	}
	f, err := os.OpenFile(out, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return res, err
	}
	defer f.Close()
	m, err := ReadMap(mapPath)
	if err != nil {
		return res, err
	}
	if len(m.Blocks) == 0 {
		m.Set(0, total*SectorSize, NonTried)
	}
	if err := f.Truncate(total * SectorSize); err != nil {
		return res, err
	}

	report := func() {
		if progress != nil {
			done := m.Count(Finished) + m.Count(BadSector)
			progress(Progress{Done: done, Total: want * SectorSize, Bad: m.Count(BadSector)})
		}
	}
	var consecutiveBad int64
	zero := make([]byte, SectorSize)

	readRange := func(lba, n int64) error {
		b, err := read(ctx, lba, n)
		if err == nil && int64(len(b)) == n*SectorSize {
			if _, err := f.WriteAt(b, lba*SectorSize); err != nil {
				return err
			}
			m.Set(lba*SectorSize, n*SectorSize, Finished)
			consecutiveBad = 0
			return nil
		}
		if ctx.Err() != nil || errors.Is(err, ErrMediaRemoved) {
			return err
		}
		return errRetry
	}

	const chunk = 512
	for ei, ext := range extents {
		last := ei == len(extents)-1
		consecutiveBad = 0
		for lba := ext.Start; lba < ext.End; {
			if ctx.Err() != nil {
				_ = m.Write(mapPath)
				return res, ctx.Err()
			}
			if m.Status(lba*SectorSize) == Finished {
				lba++
				for lba < ext.End && lba%chunk != 0 && m.Status(lba*SectorSize) == Finished {
					lba++
				}
				continue
			}
			n := min(chunk-lba%chunk, ext.End-lba)
			// Skip a finished tail inside the chunk on resume.
			for i := int64(1); i < n; i++ {
				if m.Status((lba+i)*SectorSize) == Finished {
					n = i
					break
				}
			}
			err := readRange(lba, n)
			if err == errRetry {
				if n > 32 {
					// Pieces of 32 sectors; failures fall through to singles.
					err = nil
					for s := lba; s < lba+n; s += 32 {
						if e := readRange(s, min(32, lba+n-s)); e == errRetry {
							err = errRetry
						} else if e != nil {
							_ = m.Write(mapPath)
							return res, e
						}
					}
				}
				if err == errRetry {
					// Single sectors: zero-fill what still fails.
					for s := lba; s < lba+n; s++ {
						if m.Status(s*SectorSize) == Finished {
							continue
						}
						e := readRange(s, 1)
						if e == errRetry {
							_, _ = f.WriteAt(zero, s*SectorSize)
							m.Set(s*SectorSize, SectorSize, BadSector)
							consecutiveBad++
							if last && stopAfter > 0 && consecutiveBad >= stopAfter {
								// Unwritten tail: unmark it and stop.
								start := s - consecutiveBad + 1
								m.Set(start*SectorSize, (ext.End-start)*SectorSize, NonTried)
								res.StoppedAt = start
								_ = m.Write(mapPath)
								res.Bad = m.BadSectors(start * SectorSize)
								report()
								return res, nil
							}
						} else if e != nil {
							_ = m.Write(mapPath)
							return res, e
						}
					}
				}
			} else if err != nil {
				_ = m.Write(mapPath)
				return res, err
			}
			lba += n
			if lba%(chunk*8) == 0 || lba >= ext.End {
				if err := m.Write(mapPath); err != nil {
					return res, err
				}
				report()
			}
		}
	}
	if err := m.Write(mapPath); err != nil {
		return res, err
	}
	res.Bad = m.BadSectors(total * SectorSize)
	report()
	return res, nil
}

var errRetry = errors.New("retry")

// readAtFile reads sectors from an open file.
func readAtFile(f *os.File, lba, n int64) ([]byte, error) {
	b := make([]byte, n*SectorSize)
	k, err := f.ReadAt(b, lba*SectorSize)
	if err == io.EOF && k > 0 {
		return b[:k], nil
	}
	return b[:k], err
}
