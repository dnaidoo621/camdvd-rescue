// Package pipeline holds the stages that turn a side image into source clips
// and source clips into tagged MP4s. Each stage is a function over files; the
// job engine sequences them and persists the results.
package pipeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dnaidoo621/camdvd-rescue/internal/carve"
	"github.com/dnaidoo621/camdvd-rescue/internal/drive"
	"github.com/dnaidoo621/camdvd-rescue/internal/dvdifo"
	"github.com/dnaidoo621/camdvd-rescue/internal/tools"
)

// Source is one recording to convert. Kind says how FFmpeg reads it.
type Source struct {
	Kind    string   `json:"kind"` // file, dvdvideo, concat
	Path    string   `json:"path"` // file, or the VIDEO_TS folder
	Title   int      `json:"title,omitempty"`
	Chapter int      `json:"chapter,omitempty"`
	Files   []string `json:"files,omitempty"` // concat members
	Label   string   `json:"label"`           // shown in the UI, e.g. "title 1, chapter 2"
	// RecTime is the recording time from the disc, in camera local time
	// (no zone), when the format stores one.
	RecTime    time.Time `json:"rec_time,omitzero"`
	DateSource string    `json:"date_source,omitempty"`
	BadSectors int64     `json:"bad_sectors,omitempty"`
}

// InputArgs are the FFmpeg arguments that open the source.
func (s Source) InputArgs() []string {
	switch s.Kind {
	case "dvdvideo":
		// The demuxer wants the disc root, not VIDEO_TS itself. -preindex is
		// left off: with a chapter range it loses the duration.
		args := []string{"-f", "dvdvideo", "-title", strconv.Itoa(s.Title)}
		if s.Chapter > 0 { // 0 reads the whole title
			args = append(args, "-chapter_start", strconv.Itoa(s.Chapter), "-chapter_end", strconv.Itoa(s.Chapter))
		}
		return append(args, "-i", filepath.Dir(s.Path))
	case "concat":
		return []string{"-f", "mpeg", "-i", "concat:" + strings.Join(s.Files, "|")}
	}
	return []string{"-i", s.Path}
}

// Listing is what's inside an image's file system.
type Listing struct {
	Paths      []string
	HasVideoTS bool
	HasRTAV    bool
	Output     string
}

// ListImage lists an image's ISO9660/UDF file system with 7-Zip, without
// mounting it.
func ListImage(ctx context.Context, ts tools.Set, img string) (Listing, error) {
	r, err := ts.Run(ctx, tools.Cmd{Name: ts.SevenZip(), Args: []string{"l", "-slt", "-ba", img}})
	l := Listing{Output: tail(r.Stdout+r.Stderr, 4000)}
	if err != nil {
		return l, err
	}
	for _, line := range strings.Split(r.Stdout, "\n") {
		if p, ok := strings.CutPrefix(line, "Path = "); ok {
			p = strings.TrimSpace(p)
			l.Paths = append(l.Paths, p)
			top := strings.ToUpper(strings.SplitN(p, "/", 2)[0])
			l.HasVideoTS = l.HasVideoTS || top == "VIDEO_TS"
			l.HasRTAV = l.HasRTAV || top == "DVD_RTAV"
		}
	}
	if len(l.Paths) == 0 {
		return l, fmt.Errorf("7-Zip found no files in %s", filepath.Base(img))
	}
	return l, nil
}

// ExtractImage pulls the whole file system out of img into dst.
func ExtractImage(ctx context.Context, ts tools.Set, img, dst string) (tools.Result, error) {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return tools.Result{}, err
	}
	return ts.Run(ctx, tools.Cmd{Name: ts.SevenZip(), Args: []string{"x", "-y", "-bd", "-o" + dst, img}})
}

// findDir finds a top-level directory case-insensitively.
func findDir(root, name string) string {
	ents, _ := os.ReadDir(root)
	for _, e := range ents {
		if e.IsDir() && strings.EqualFold(e.Name(), name) {
			return filepath.Join(root, e.Name())
		}
	}
	return ""
}

// FindVideoTS returns the VIDEO_TS folder under root, or "".
func FindVideoTS(root string) string { return findDir(root, "VIDEO_TS") }

// FindRTAV returns the DVD_RTAV folder under root, or "".
func FindRTAV(root string) string { return findDir(root, "DVD_RTAV") }

// DVDVideoSources makes one source per recording from a VIDEO_TS folder. In
// "chapter" mode each chapter is a recording; in "title" mode each title.
// When the IFOs can't be read it falls back to concatenating each title
// set's VOBs.
func DVDVideoSources(videoTS, split string) ([]Source, error) {
	titles, err := dvdifo.ReadFile(filepath.Join(videoTS, ifoName(videoTS)))
	if err != nil || len(titles) == 0 {
		srcs := vobFallback(videoTS)
		if len(srcs) == 0 {
			return nil, fmt.Errorf("no titles in VIDEO_TS (%v) and no VOBs to fall back on", err)
		}
		return srcs, nil
	}
	var out []Source
	for _, t := range titles {
		chapters := t.Chapters
		if split == "title" || chapters < 1 {
			chapters = 1
		}
		for c := 1; c <= chapters; c++ {
			s := Source{Kind: "dvdvideo", Path: videoTS, Title: t.Number, Chapter: c,
				Label: fmt.Sprintf("title %d, chapter %d", t.Number, c)}
			if split == "title" {
				s.Chapter = 0
				s.Label = fmt.Sprintf("title %d", t.Number)
			}
			out = append(out, s)
		}
	}
	return out, nil
}

func ifoName(dir string) string {
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.EqualFold(e.Name(), "VIDEO_TS.IFO") {
			return e.Name()
		}
	}
	return "VIDEO_TS.IFO"
}

var reVOB = regexp.MustCompile(`(?i)^VTS_(\d\d)_([1-9])\.VOB$`)

func vobFallback(dir string) []Source {
	ents, _ := os.ReadDir(dir)
	sets := map[string][]string{}
	for _, e := range ents {
		if m := reVOB.FindStringSubmatch(e.Name()); m != nil {
			sets[m[1]] = append(sets[m[1]], filepath.Join(dir, e.Name()))
		}
	}
	keys := make([]string, 0, len(sets))
	for k := range sets {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []Source
	for _, k := range keys {
		sort.Strings(sets[k])
		out = append(out, Source{Kind: "concat", Files: sets[k], Path: sets[k][0], Label: "title set " + k + " (VOBs joined)"})
	}
	return out
}

var (
	reVRNum  = regexp.MustCompile(`^num\s*:\s*(\d+)`)
	reVRDate = regexp.MustCompile(`^date\s*:\s*(\d{4}-\d\d-\d\d \d\d:\d\d:\d\d)`)
)

// DVDVRSources splits VR_MOVIE.VRO into one VOB per recording with dvd-vr,
// keeping each recording's date and time from VR_MANGR.IFO.
func DVDVRSources(ctx context.Context, ts tools.Set, rtav, outDir string) ([]Source, tools.Result, error) {
	find := func(name string) string {
		ents, _ := os.ReadDir(rtav)
		for _, e := range ents {
			if strings.EqualFold(e.Name(), name) {
				return filepath.Join(rtav, e.Name())
			}
		}
		return ""
	}
	ifo, vro := find("VR_MANGR.IFO"), find("VR_MOVIE.VRO")
	if ifo == "" || vro == "" {
		return nil, tools.Result{}, fmt.Errorf("DVD_RTAV lacks VR_MANGR.IFO or VR_MOVIE.VRO")
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, tools.Result{}, err
	}
	r, err := ts.Run(ctx, tools.Cmd{Name: "dvd-vr", Args: []string{"-n", "vr", ifo, vro}, Dir: outDir})
	if err != nil {
		return nil, r, err
	}
	dates := ParseDVDVRDates(r.Stdout + "\n" + r.Stderr)
	var out []Source
	files, _ := filepath.Glob(filepath.Join(outDir, "vr#*.vob"))
	sort.Strings(files)
	for _, f := range files {
		n, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(filepath.Base(f), "vr#"), ".vob"))
		s := Source{Kind: "file", Path: f, Label: fmt.Sprintf("recording %d", n)}
		if t, ok := dates[n]; ok {
			s.RecTime, s.DateSource = t, "dvd-vr"
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil, r, fmt.Errorf("dvd-vr extracted no recordings")
	}
	return out, r, nil
}

// ParseDVDVRDates maps program number to recording time from dvd-vr output.
func ParseDVDVRDates(out string) map[int]time.Time {
	dates := map[int]time.Time{}
	num := 0
	tools.ScanLines(out, func(line string) {
		line = strings.TrimSpace(line)
		if m := reVRNum.FindStringSubmatch(line); m != nil {
			num, _ = strconv.Atoi(m[1])
		} else if m := reVRDate.FindStringSubmatch(line); m != nil && num > 0 {
			if t, err := time.Parse("2006-01-02 15:04:05", m[1]); err == nil {
				dates[num] = t
			}
		}
	})
	return dates
}

// CarveSources scans an image for MPEG-2 packs and writes one .mpg per
// recording to outDir. Bad sectors come from the imaging mapfile.
func CarveSources(img, mapPath, outDir string) ([]Source, []carve.Clip, error) {
	f, err := os.Open(img)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	opt := carve.DefaultOptions()
	if m, err := drive.ReadMap(mapPath); err == nil {
		bad := drive.Ranges(m.BadSectors(fi.Size()))
		opt.Bad = bad.Contains
	}
	clips, err := carve.Scan(f, fi.Size(), opt)
	if err != nil {
		return nil, nil, err
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, nil, err
	}
	var out []Source
	for _, c := range clips {
		p := filepath.Join(outDir, fmt.Sprintf("carved_%03d.mpg", c.Index))
		w, err := os.Create(p)
		if err != nil {
			return nil, nil, err
		}
		if err := carve.Write(f, c, w); err != nil {
			w.Close()
			return nil, nil, err
		}
		if err := w.Close(); err != nil {
			return nil, nil, err
		}
		out = append(out, Source{Kind: "file", Path: p, Label: fmt.Sprintf("carved recording %d (~%.0f s)", c.Index, c.Duration()), BadSectors: c.BadSectors})
	}
	return out, clips, nil
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}
