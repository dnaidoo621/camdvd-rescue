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
//
// It never stops a rescue: if the sample is unreadable (a recording cut off
// mid-write leaves a damaged tail) it steps back towards the start, and as a
// last resort identifies the side by its track table alone.
func Fingerprint(ctx context.Context, d Drive, p discinfo.Probe) (string, error) {
	used := p.Media.UsedSectors()
	const n = 512
	start, end, raw := int64(0), min(int64(n), max(used, n)), false
	if p.Media.Open() && p.FSType == "" {
		raw = true
		exts := p.Media.Extents()
		if len(exts) > 0 {
			last := exts[len(exts)-1]
			start, end = last.Start, last.End
		}
	} else if used > 0 {
		end = used
	}
	h := sha256.New()
	fmt.Fprintf(h, "%s|%d|%d|", p.Media.Type, p.Media.Capacity, used)
	if raw {
		lba := max(start, end-n)
		for try := 0; try < 16 && lba >= start; try++ {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			b, err := d.ReadSectors(ctx, lba, min(n, end-lba), true)
			if errors.Is(err, ErrMediaRemoved) {
				return "", err
			}
			if err == nil {
				fmt.Fprintf(h, "%d|", lba)
				h.Write(b)
				return hex.EncodeToString(h.Sum(nil))[:16], nil
			}
			lba -= 4 * n // 4 MB further back
		}
		// Nothing near the end reads: the track table still tells sides apart.
		for _, t := range p.Media.Tracks {
			fmt.Fprintf(h, "%s|%d|%d|%d|", t.State, t.Start, t.Size, t.LastRecorded)
		}
		return "m" + hex.EncodeToString(h.Sum(nil))[:15], nil
	}
	b, err := d.ReadSectors(ctx, 0, min(int64(n), end), false)
	if err != nil {
		return "", fmt.Errorf("read fingerprint sample: %w", err)
	}
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}

// ScrapeMax is the largest unreadable gap read sector by sector; bigger
// gaps are zero-filled after their edges are found. A failed read can cost
// a drive several seconds of retries, so scraping a dead area is slow and
// rarely recovers anything.
var ScrapeMax int64 = 32

// copyLoop images by reading through read, keeping a ddrescue-format mapfile
// so a cancelled run resumes. Like ddrescue it works in passes:
//
//  1. copy: read 512-sector chunks, skipping any that fail;
//  2. trim: from each failed area's edges, read inwards until the first
//     failure, so readable data next to damage isn't lost;
//  3. scrape: read the remaining gaps sector by sector when they're small
//     (ScrapeMax); larger ones are marked bad and left as zeros.
//
// extents limits reading to the written ranges of an open session (nil reads
// [0, total)); the unwritten gaps between camcorder tracks stay "non-tried"
// and never count as damage. stopAfter > 0 treats an unreadable area of at
// least that many sectors at the end of the last extent as the unwritten
// tail (when the drive doesn't report the last recorded address).
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
			done := m.Count(Finished) + m.Count(BadSector) + m.Count(NonTrimmed) + m.Count(NonScraped)
			progress(Progress{Done: min(done, want*SectorSize), Total: want * SectorSize, Bad: m.Count(BadSector)})
		}
	}
	status := func(lba int64) byte { return m.Status(lba * SectorSize) }
	mark := func(lba, n int64, st byte) { m.Set(lba*SectorSize, n*SectorSize, st) }
	save := func() error { report(); return m.Write(mapPath) }

	// tryRead reads n sectors; ok=false means unreadable (not an abort).
	tryRead := func(lba, n int64) (bool, error) {
		b, err := read(ctx, lba, n)
		if err == nil && int64(len(b)) == n*SectorSize {
			if _, err := f.WriteAt(b, lba*SectorSize); err != nil {
				return false, err
			}
			mark(lba, n, Finished)
			return true, nil
		}
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		if errors.Is(err, ErrMediaRemoved) {
			return false, err
		}
		return false, nil
	}
	abort := func(err error) (ImageResult, error) {
		_ = m.Write(mapPath)
		return res, err
	}

	// Pass 1: copy.
	const chunk = 512
	for _, ext := range extents {
		for lba := ext.Start; lba < ext.End; {
			if ctx.Err() != nil {
				return abort(ctx.Err())
			}
			if status(lba) != NonTried {
				lba++
				continue
			}
			n := min(chunk-lba%chunk, ext.End-lba)
			for i := int64(1); i < n; i++ { // stop at anything already handled
				if status(lba+i) != NonTried {
					n = i
					break
				}
			}
			ok, err := tryRead(lba, n)
			if err != nil {
				return abort(err)
			}
			if !ok {
				mark(lba, n, NonTrimmed)
			}
			lba += n
			if lba%(chunk*8) == 0 || lba >= ext.End || !ok {
				if err := save(); err != nil {
					return res, err
				}
			}
		}
	}

	// Pass 2: trim each failed area from both edges, then sweep what's left
	// of large ones for readable islands (one 16-sector probe every 256
	// sectors); an island splits the area, whose new edges are trimmed in
	// the next round. Two separate scratches a chunk apart are both handled
	// this way, while a dead area costs only a few failed reads.
	trim := func(ext Range, last bool, r Range) (stop bool, err error) {
		a, b := r.Start, r.End
		singles := false // after a 16-sector read fails, approach the damage one sector at a time
		for a < b {      // leading edge
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			if !singles && b-a >= 16 {
				ok, err := tryRead(a, 16)
				if err != nil {
					return false, err
				}
				if ok {
					a += 16
					continue
				}
				singles = true
			}
			ok, err := tryRead(a, 1)
			if err != nil {
				return false, err
			}
			if !ok {
				break
			}
			a++
		}
		if last && stopAfter > 0 && b == ext.End && b-a >= stopAfter {
			// The unwritten tail: not damage, and not worth reading.
			mark(a, b-a, NonTried)
			res.StoppedAt = a
			return true, nil
		}
		for b > a+1 { // trailing edge
			ok, err := tryRead(b-1, 1)
			if err != nil {
				return false, err
			}
			if !ok {
				break
			}
			b--
		}
		if b > a {
			mark(a, b-a, NonScraped)
		}
		return false, nil
	}
	swept := map[int64]bool{} // probe positions already tried
	for round := 0; round < 64; round++ {
		changed := false
		for ei, ext := range extents {
			for _, r := range m.regions(ext, NonTrimmed) {
				stop, err := trim(ext, ei == len(extents)-1, r)
				if err != nil {
					return abort(err)
				}
				if stop {
					_ = save()
					res.Bad = m.BadSectors(res.StoppedAt * SectorSize)
					return res, nil
				}
				changed = true
			}
			for _, r := range m.regions(ext, NonScraped) {
				if r.Len() <= ScrapeMax {
					continue
				}
				for p := r.Start + 16; p+16 <= r.End-16; p += 256 {
					if swept[p] {
						continue
					}
					swept[p] = true
					if ctx.Err() != nil {
						return abort(ctx.Err())
					}
					ok, err := tryRead(p, 16)
					if err != nil {
						return abort(err)
					}
					if ok {
						// An island: what's left on either side gets trimmed.
						if p > r.Start {
							mark(r.Start, p-r.Start, NonTrimmed)
						}
						if r.End > p+16 {
							mark(p+16, r.End-p-16, NonTrimmed)
						}
						changed = true
						break
					}
				}
			}
		}
		if err := save(); err != nil {
			return res, err
		}
		if !changed {
			break
		}
	}

	// Pass 3: scrape small gaps sector by sector; give up on large ones.
	// Bad sectors need no zero-fill: the image starts as a sparse file of
	// zeros and only good reads are ever written to it.
	for _, ext := range extents {
		for _, r := range m.regions(ext, NonScraped) {
			if r.Len() > ScrapeMax {
				mark(r.Start, r.Len(), BadSector)
				continue
			}
			for s := r.Start; s < r.End; s++ {
				if ctx.Err() != nil {
					return abort(ctx.Err())
				}
				ok, err := tryRead(s, 1)
				if err != nil {
					return abort(err)
				}
				if !ok {
					mark(s, 1, BadSector)
				}
			}
		}
	}
	if err := save(); err != nil {
		return res, err
	}
	res.Bad = m.BadSectors(total * SectorSize)
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
