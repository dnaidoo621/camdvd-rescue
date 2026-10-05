// Package udf reads files out of a UDF (ECMA-167 / OSTA UDF 1.02–2.01) disc
// image without mounting it. It exists for DVD-RAM camcorder discs, whose
// UDF 2.00 file systems 7-Zip can list but not extract. It supports type 1
// (physical) partition maps, which is what DVD-RAM and DVD±RW use; virtual,
// sparable and metadata partitions are refused with an error.
package udf

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf16"
)

const sectorSize = 2048

// Descriptor tag identifiers.
const (
	tagPD   = 5
	tagLVD  = 6
	tagTD   = 8
	tagFSD  = 256
	tagFID  = 257
	tagAED  = 258
	tagFE   = 261
	tagEFE  = 266
	tagAVDP = 2
)

// FS is an opened UDF file system.
type FS struct {
	r         io.ReaderAt
	blockSize int64
	partStart map[uint16]int64 // partition reference -> first sector
	root      longAD
}

// Entry is a file or directory.
type Entry struct {
	Path    string
	Dir     bool
	Size    int64
	ModTime time.Time
	extents []extent
	inline  []byte // embedded data (allocation type 3)
}

type extent struct {
	sector int64 // absolute sector; -1 for unrecorded (reads as zeros)
	length int64 // bytes
}

type longAD struct {
	length  uint32
	lbn     uint32
	partRef uint16
}

// ErrUnsupported marks UDF features this reader doesn't handle.
var ErrUnsupported = errors.New("unsupported UDF feature")

// Open reads the volume structure.
func Open(r io.ReaderAt) (*FS, error) {
	fs := &FS{r: r, blockSize: sectorSize, partStart: map[uint16]int64{}}
	avdp, err := fs.sector(256)
	if err != nil {
		return nil, err
	}
	if tagID(avdp) != tagAVDP {
		return nil, errors.New("no UDF anchor at sector 256")
	}
	vdsLen := int64(le32(avdp, 16))
	vdsLoc := int64(le32(avdp, 20))

	partByNumber := map[uint16]int64{}
	var maps []uint16 // partition reference -> partition number
	var fsd longAD
	foundLVD := false
	for i := int64(0); i < vdsLen/sectorSize && i < 64; i++ {
		d, err := fs.sector(vdsLoc + i)
		if err != nil {
			return nil, err
		}
		switch tagID(d) {
		case tagPD:
			partByNumber[le16(d, 22)] = int64(le32(d, 188))
		case tagLVD:
			foundLVD = true
			fs.blockSize = int64(le32(d, 212))
			if fs.blockSize != sectorSize {
				return nil, fmt.Errorf("%w: logical block size %d", ErrUnsupported, fs.blockSize)
			}
			fsd = readLongAD(d, 248)
			mtl, n := int(le32(d, 264)), int(le32(d, 268))
			off := 440
			for j := 0; j < n && off < 440+mtl && off+2 <= len(d); j++ {
				typ, l := d[off], int(d[off+1])
				if typ != 1 {
					return nil, fmt.Errorf("%w: partition map type %d (virtual, sparable or metadata)", ErrUnsupported, typ)
				}
				maps = append(maps, le16(d, off+4))
				off += l
			}
		case tagTD:
			i = vdsLen // done
		}
	}
	if !foundLVD || len(maps) == 0 {
		return nil, errors.New("no logical volume descriptor")
	}
	for ref, num := range maps {
		start, ok := partByNumber[num]
		if !ok {
			return nil, fmt.Errorf("partition %d has no descriptor", num)
		}
		fs.partStart[uint16(ref)] = start
	}

	d, err := fs.block(fsd.partRef, fsd.lbn)
	if err != nil {
		return nil, err
	}
	if tagID(d) != tagFSD {
		return nil, fmt.Errorf("no file set descriptor at %d", fsd.lbn)
	}
	fs.root = readLongAD(d, 400)
	return fs, nil
}

func (fs *FS) sector(n int64) ([]byte, error) {
	b := make([]byte, sectorSize)
	if _, err := fs.r.ReadAt(b, n*sectorSize); err != nil {
		return nil, fmt.Errorf("read sector %d: %w", n, err)
	}
	return b, nil
}

func (fs *FS) abs(partRef uint16, lbn uint32) (int64, error) {
	start, ok := fs.partStart[partRef]
	if !ok {
		return 0, fmt.Errorf("unknown partition reference %d", partRef)
	}
	return start + int64(lbn), nil
}

func (fs *FS) block(partRef uint16, lbn uint32) ([]byte, error) {
	s, err := fs.abs(partRef, lbn)
	if err != nil {
		return nil, err
	}
	return fs.sector(s)
}

// entry reads a (extended) file entry.
func (fs *FS) entry(icb longAD, p string) (*Entry, error) {
	d, err := fs.block(icb.partRef, icb.lbn)
	if err != nil {
		return nil, err
	}
	var lEA, lAD, base, timeOff int
	switch tagID(d) {
	case tagFE:
		lEA, lAD, base, timeOff = int(le32(d, 168)), int(le32(d, 172)), 176, 84
	case tagEFE:
		lEA, lAD, base, timeOff = int(le32(d, 208)), int(le32(d, 212)), 216, 92
	default:
		return nil, fmt.Errorf("%s: no file entry at %d (tag %d)", p, icb.lbn, tagID(d))
	}
	if base+lEA+lAD > len(d) {
		return nil, fmt.Errorf("%s: file entry overflows its block", p)
	}
	e := &Entry{
		Path:    p,
		Dir:     d[16+11] == 4, // ICB file type 4: directory
		Size:    int64(binary.LittleEndian.Uint64(d[56:])),
		ModTime: timestamp(d[timeOff : timeOff+12]),
	}
	flags := le16(d, 16+18)
	ads := d[base+lEA : base+lEA+lAD]
	switch flags & 7 {
	case 0, 1:
		if err := fs.readADs(e, ads, flags&7 == 1, icb.partRef, 0); err != nil {
			return nil, err
		}
	case 3:
		e.inline = append([]byte(nil), ads...)
	default:
		return nil, fmt.Errorf("%w: %s uses extended allocation descriptors", ErrUnsupported, p)
	}
	return e, nil
}

// readADs appends extents from short (long=false) or long allocation
// descriptors, following allocation extent continuations.
func (fs *FS) readADs(e *Entry, ads []byte, long bool, partRef uint16, depth int) error {
	if depth > 32 {
		return errors.New("allocation descriptor chain too long")
	}
	size := 8
	if long {
		size = 16
	}
	for off := 0; off+size <= len(ads); off += size {
		raw := le32(ads, off)
		length, typ := int64(raw&0x3FFFFFFF), raw>>30
		if length == 0 {
			break
		}
		lbn := le32(ads, off+4)
		ref := partRef
		if long {
			ref = le16(ads, off+8)
		}
		switch typ {
		case 0: // recorded
			s, err := fs.abs(ref, lbn)
			if err != nil {
				return err
			}
			e.extents = append(e.extents, extent{s, length})
		case 1, 2: // allocated or not, but not recorded: zeros
			e.extents = append(e.extents, extent{-1, length})
		case 3: // continuation: an allocation extent descriptor
			d, err := fs.block(ref, lbn)
			if err != nil {
				return err
			}
			if tagID(d) != tagAED {
				return fmt.Errorf("%s: bad allocation extent at %d", e.Path, lbn)
			}
			l := int(le32(d, 20))
			if 24+l > len(d) {
				return fmt.Errorf("%s: allocation extent overflows", e.Path)
			}
			return fs.readADs(e, d[24:24+l], long, ref, depth+1)
		}
	}
	return nil
}

// Open returns a reader over an entry's contents.
func (fs *FS) Open(e *Entry) io.Reader {
	if e.inline != nil {
		return io.LimitReader(strings.NewReader(string(e.inline)), e.Size)
	}
	var rs []io.Reader
	left := e.Size
	for _, x := range e.extents {
		if left <= 0 {
			break
		}
		n := min(x.length, left)
		if x.sector < 0 {
			rs = append(rs, io.LimitReader(zeros{}, n))
		} else {
			rs = append(rs, io.NewSectionReader(fs.r, x.sector*sectorSize, n))
		}
		left -= n
	}
	if left > 0 { // descriptors shorter than the file: pad with zeros
		rs = append(rs, io.LimitReader(zeros{}, left))
	}
	return io.MultiReader(rs...)
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// Walk visits every entry under the root, depth first, directories before
// their contents.
func (fs *FS) Walk(fn func(*Entry) error) error {
	root, err := fs.entry(fs.root, "")
	if err != nil {
		return err
	}
	seen := map[int64]bool{}
	return fs.walk(root, fn, seen, 0)
}

func (fs *FS) walk(dir *Entry, fn func(*Entry) error, seen map[int64]bool, depth int) error {
	if depth > 32 {
		return errors.New("directory tree too deep")
	}
	data, err := io.ReadAll(io.LimitReader(fs.Open(dir), 16<<20))
	if err != nil {
		return err
	}
	for off := 0; off+38 <= len(data); {
		if le16(data, off) != tagFID {
			break
		}
		chars := data[off+18]
		lFI := int(data[off+19])
		icb := readLongAD(data, off+20)
		lIU := int(le16(data, off+36))
		next := off + (38+lIU+lFI+3)&^3
		if off+38+lIU+lFI > len(data) {
			return fmt.Errorf("%s: truncated directory", dir.Path)
		}
		name := dstring(data[off+38+lIU : off+38+lIU+lFI])
		off = next
		if chars&(1<<3) != 0 || chars&(1<<2) != 0 { // parent or deleted
			continue
		}
		if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") {
			return fmt.Errorf("%s: unsafe file name %q", dir.Path, name)
		}
		e, err := fs.entry(icb, path.Join(dir.Path, name))
		if err != nil {
			return err
		}
		if err := fn(e); err != nil {
			return err
		}
		if e.Dir {
			key, _ := fs.abs(icb.partRef, icb.lbn)
			if seen[key] {
				continue // a loop in a damaged directory tree
			}
			seen[key] = true
			if err := fs.walk(e, fn, seen, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

// Extract writes the whole tree under dst, keeping modification times.
func (fs *FS) Extract(dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	var dirs []*Entry
	err := fs.Walk(func(e *Entry) error {
		p := filepath.Join(dst, filepath.FromSlash(e.Path))
		if e.Dir {
			dirs = append(dirs, e)
			return os.MkdirAll(p, 0o755)
		}
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, fs.Open(e)); err != nil {
			f.Close()
			return fmt.Errorf("%s: %w", e.Path, err)
		}
		if err := f.Close(); err != nil {
			return err
		}
		if !e.ModTime.IsZero() {
			_ = os.Chtimes(p, e.ModTime, e.ModTime)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if t := dirs[i].ModTime; !t.IsZero() {
			_ = os.Chtimes(filepath.Join(dst, filepath.FromSlash(dirs[i].Path)), t, t)
		}
	}
	return nil
}

func tagID(b []byte) uint16         { return le16(b, 0) }
func le16(b []byte, off int) uint16 { return binary.LittleEndian.Uint16(b[off:]) }
func le32(b []byte, off int) uint32 { return binary.LittleEndian.Uint32(b[off:]) }

func readLongAD(b []byte, off int) longAD {
	return longAD{length: le32(b, off), lbn: le32(b, off+4), partRef: le16(b, off+8)}
}

// dstring decodes an OSTA CS0 d-characters field (compression id 8 or 16).
func dstring(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	switch b[0] {
	case 8:
		r := make([]rune, 0, len(b)-1)
		for _, c := range b[1:] {
			r = append(r, rune(c))
		}
		return string(r)
	case 16:
		u := make([]uint16, 0, (len(b)-1)/2)
		for i := 1; i+1 < len(b); i += 2 {
			u = append(u, uint16(b[i])<<8|uint16(b[i+1]))
		}
		return string(utf16.Decode(u))
	}
	return ""
}

// timestamp decodes an ECMA-167 timestamp (type 1, local time with offset).
func timestamp(b []byte) time.Time {
	tz := int16(le16(b, 0)<<4) >> 4 // 12-bit signed minutes
	year := int(le16(b, 2))
	if year == 0 {
		return time.Time{}
	}
	loc := time.UTC
	if tz != -2047 {
		loc = time.FixedZone("", int(tz)*60)
	}
	return time.Date(year, time.Month(b[4]), int(b[5]), int(b[6]), int(b[7]), int(b[8]), int(b[9])*10_000_000, loc)
}
