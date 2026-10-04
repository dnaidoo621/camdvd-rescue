package drive

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

// Map is a GNU ddrescue mapfile: which byte ranges were read, failed or not
// yet tried. ddrescue writes it for block-device imaging; the raw reader
// writes the same format so resume and bad-sector reporting share one parser.
type Map struct {
	Blocks []Block
}

// Block statuses, as ddrescue defines them.
const (
	NonTried   = '?'
	NonTrimmed = '*'
	NonScraped = '/'
	BadSector  = '-'
	Finished   = '+'
)

// Block is a byte range with one status.
type Block struct {
	Pos, Size int64
	Status    byte
}

// ReadMap parses a mapfile; a missing file is an empty map.
func ReadMap(path string) (*Map, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return &Map{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	m := &Map{}
	sc := bufio.NewScanner(f)
	statusLine := true
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' {
			continue
		}
		fs := strings.Fields(line)
		if statusLine { // "current_pos current_status [current_pass]"
			statusLine = false
			continue
		}
		if len(fs) < 3 || len(fs[2]) != 1 {
			return nil, fmt.Errorf("mapfile %s: bad line %q", path, line)
		}
		pos, err1 := strconv.ParseInt(fs[0], 0, 64)
		size, err2 := strconv.ParseInt(fs[1], 0, 64)
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("mapfile %s: bad line %q", path, line)
		}
		m.Blocks = append(m.Blocks, Block{pos, size, fs[2][0]})
	}
	return m, sc.Err()
}

// Write saves the map atomically.
func (m *Map) Write(path string) error {
	m.normalize()
	var b strings.Builder
	b.WriteString("# Mapfile. Created by camdvd (ddrescue format)\n")
	b.WriteString("# current_pos  current_status  current_pass\n0x00000000     ?               1\n")
	b.WriteString("#      pos        size  status\n")
	for _, bl := range m.Blocks {
		fmt.Fprintf(&b, "0x%08X  0x%08X  %c\n", bl.Pos, bl.Size, bl.Status)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Set marks [pos, pos+size) with status, splitting blocks as needed.
func (m *Map) Set(pos, size int64, status byte) {
	if size <= 0 {
		return
	}
	end := pos + size
	var out []Block
	for _, bl := range m.Blocks {
		bEnd := bl.Pos + bl.Size
		if bEnd <= pos || bl.Pos >= end {
			out = append(out, bl)
			continue
		}
		if bl.Pos < pos {
			out = append(out, Block{bl.Pos, pos - bl.Pos, bl.Status})
		}
		if bEnd > end {
			out = append(out, Block{end, bEnd - end, bl.Status})
		}
	}
	out = append(out, Block{pos, size, status})
	m.Blocks = out
	m.normalize()
}

func (m *Map) normalize() {
	sort.Slice(m.Blocks, func(i, j int) bool { return m.Blocks[i].Pos < m.Blocks[j].Pos })
	var out []Block
	for _, bl := range m.Blocks {
		if bl.Size <= 0 {
			continue
		}
		if n := len(out); n > 0 && out[n-1].Status == bl.Status && out[n-1].Pos+out[n-1].Size == bl.Pos {
			out[n-1].Size += bl.Size
			continue
		}
		out = append(out, bl)
	}
	m.Blocks = out
}

// Count sums the bytes with status s.
func (m *Map) Count(s byte) int64 {
	var n int64
	for _, bl := range m.Blocks {
		if bl.Status == s {
			n += bl.Size
		}
	}
	return n
}

// Status returns the status of the byte at pos ('?' if unmapped).
func (m *Map) Status(pos int64) byte {
	i := sort.Search(len(m.Blocks), func(i int) bool { return m.Blocks[i].Pos+m.Blocks[i].Size > pos })
	if i < len(m.Blocks) && m.Blocks[i].Pos <= pos {
		return m.Blocks[i].Status
	}
	return NonTried
}

// BadSectors lists the unreadable sector ranges (anything not finished
// inside [0, limit) bytes), as half-open sector ranges.
func (m *Map) BadSectors(limit int64) []Range {
	var out []Range
	for _, bl := range m.Blocks {
		if bl.Status == Finished || bl.Status == NonTried || bl.Pos >= limit {
			continue
		}
		end := min(bl.Pos+bl.Size, limit)
		out = append(out, Range{bl.Pos / SectorSize, (end + SectorSize - 1) / SectorSize})
	}
	return out
}

// Range is a half-open sector range.
type Range struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

// Len is the number of sectors.
func (r Range) Len() int64 { return r.End - r.Start }

// Ranges answers "is this sector bad?" quickly.
type Ranges []Range

// Contains reports whether lba falls in any range. Ranges must be sorted.
func (rs Ranges) Contains(lba int64) bool {
	i := sort.Search(len(rs), func(i int) bool { return rs[i].End > lba })
	return i < len(rs) && rs[i].Start <= lba
}

// Total sums the sectors.
func (rs Ranges) Total() int64 {
	var n int64
	for _, r := range rs {
		n += r.Len()
	}
	return n
}
