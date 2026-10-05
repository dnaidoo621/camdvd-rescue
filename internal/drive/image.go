package drive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/dnaidoo621/camdvd-rescue/internal/discinfo"
	"github.com/dnaidoo621/camdvd-rescue/internal/tools"
)

// ImageDrive is a drive whose "disc" is an image file. A sidecar
// <image>.media.json stands in for dvd+rw-mediainfo, and <image>.bad.json
// lists sectors that fail to read, so every pipeline path can run without
// hardware.
type ImageDrive struct {
	id    string
	dir   string // where Images() looks
	tools tools.Set

	mu      sync.Mutex
	current string
}

// NewImageDrive serves images from dir.
func NewImageDrive(id, dir string, ts tools.Set) *ImageDrive {
	return &ImageDrive{id: id, dir: dir, tools: ts}
}

func (d *ImageDrive) Info() Info {
	return Info{ID: d.id, Block: d.dir, Model: "Image drive (demo)", Demo: true, Loader: "tray"}
}

// Images lists the images available to insert.
func (d *ImageDrive) Images() []string {
	m, _ := filepath.Glob(filepath.Join(d.dir, "*.img"))
	for i := range m {
		m[i] = filepath.Base(m[i])
	}
	sort.Strings(m)
	return m
}

// Insert loads one of Images() by name. It never takes a path, so a request
// can't make the drive read an arbitrary file.
func (d *ImageDrive) Insert(name string) error {
	for _, n := range d.Images() {
		if n == name {
			return d.load(filepath.Join(d.dir, n))
		}
	}
	return fmt.Errorf("no image %q in %s", name, d.dir)
}

// InsertPath loads an image by absolute path. For the CLI and tests only;
// never wire it to a request.
func (d *ImageDrive) InsertPath(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("%q is not an absolute path", path)
	}
	return d.load(path)
}

func (d *ImageDrive) load(p string) error {
	if _, err := os.Stat(p); err != nil {
		return err
	}
	d.mu.Lock()
	d.current = p
	d.mu.Unlock()
	return nil
}

// Current returns the loaded image, or "".
func (d *ImageDrive) Current() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.current
}

func (d *ImageDrive) Status(context.Context) (TrayStatus, error) {
	if d.Current() == "" {
		return StatusNoDisc, nil
	}
	return StatusDiscOK, nil
}

func (d *ImageDrive) Eject(context.Context) error {
	d.mu.Lock()
	d.current = ""
	d.mu.Unlock()
	return nil
}

type sidecar struct {
	Type         string           `json:"type"`
	DiscStatus   string           `json:"disc_status"`
	SessionState string           `json:"session_state"`
	NextWritable int64            `json:"next_writable"`
	Tracks       []discinfo.Track `json:"tracks"`
}

func (d *ImageDrive) Probe(ctx context.Context) (discinfo.Probe, string, error) {
	cur := d.Current()
	if cur == "" {
		return discinfo.Probe{Media: discinfo.Media{NoMedia: true}}, "no image loaded", nil
	}
	fi, err := os.Stat(cur)
	if err != nil {
		return discinfo.Probe{}, "", err
	}
	sc := sidecar{Type: "DVD-R Sequential", DiscStatus: "complete", SessionState: "complete"}
	if b, err := os.ReadFile(cur + ".media.json"); err == nil {
		if err := json.Unmarshal(b, &sc); err != nil {
			return discinfo.Probe{}, "", fmt.Errorf("%s.media.json: %w", cur, err)
		}
	}
	m := discinfo.Media{Type: sc.Type, DiscStatus: sc.DiscStatus, SessionState: sc.SessionState, NextWritable: sc.NextWritable, Tracks: sc.Tracks}
	if !m.Open() {
		m.Capacity = fi.Size() / SectorSize
	}
	p := discinfo.Probe{Media: m}
	evidence := fmt.Sprintf("image %s\nmedia: %+v\n", filepath.Base(cur), m)
	if fi.Size() > 0 {
		out, _ := d.tools.Output(ctx, "blkid", "-p", "-o", "value", "-s", "TYPE", cur)
		p.FSType = strings.TrimSpace(out)
		evidence += "blkid TYPE=" + p.FSType + "\n"
	}
	return p, evidence, nil
}

func (d *ImageDrive) bad(cur string) Ranges {
	var rs Ranges
	if b, err := os.ReadFile(cur + ".bad.json"); err == nil {
		_ = json.Unmarshal(b, &rs)
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i].Start < rs[j].Start })
	return rs
}

func (d *ImageDrive) reader(cur string) (*os.File, func(ctx context.Context, lba, n int64) ([]byte, error), error) {
	f, err := os.Open(cur)
	if err != nil {
		return nil, nil, err
	}
	bad := d.bad(cur)
	return f, func(ctx context.Context, lba, n int64) ([]byte, error) {
		if d.Current() != cur {
			return nil, ErrMediaRemoved
		}
		for s := lba; s < lba+n; s++ {
			if bad.Contains(s) {
				return nil, fmt.Errorf("sector %d: medium error (simulated)", s)
			}
		}
		return readAtFile(f, lba, n)
	}, nil
}

func (d *ImageDrive) ReadSectors(ctx context.Context, lba, n int64, raw bool) ([]byte, error) {
	cur := d.Current()
	if cur == "" {
		return nil, ErrMediaRemoved
	}
	f, read, err := d.reader(cur)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return read(ctx, lba, n)
}

func (d *ImageDrive) Image(ctx context.Context, req ImageRequest, progress func(Progress)) (ImageResult, error) {
	cur := d.Current()
	if cur == "" {
		return ImageResult{}, ErrMediaRemoved
	}
	f, read, err := d.reader(cur)
	if err != nil {
		return ImageResult{}, err
	}
	defer f.Close()
	total := req.Sectors
	if total <= 0 {
		fi, err := f.Stat()
		if err != nil {
			return ImageResult{}, err
		}
		total = fi.Size() / SectorSize
	}
	if total == 0 {
		return ImageResult{}, errors.New("image is empty")
	}
	var stop int64
	if req.Method == discinfo.ImageRaw {
		stop = 4096
	}
	res, err := copyLoop(ctx, read, total, req.Extents, req.Out, req.Map, stop, progress)
	res.CmdLines = []string{fmt.Sprintf("image-drive copy %s -> %s (%d sectors, %s)", filepath.Base(cur), req.Out, total, req.Method)}
	return res, err
}
