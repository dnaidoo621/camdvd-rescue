package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/dnaidoo621/camdvd-rescue/internal/discinfo"
	"github.com/dnaidoo621/camdvd-rescue/internal/library"
	"github.com/dnaidoo621/camdvd-rescue/internal/pipeline"
	"github.com/dnaidoo621/camdvd-rescue/internal/store"
	"github.com/dnaidoo621/camdvd-rescue/internal/tools"
)

// maybeFinalize places the disc's files once every wanted side is imaged and
// processed and the answers are in. The state change to Finalizing is the
// guard, so concurrent callers finalize once.
func (e *Engine) maybeFinalize(id string) {
	_, err := e.update(id, func(d *store.Disc) error {
		if d.State != store.Processing || !d.Answered {
			return errStop
		}
		imaged := 0
		for _, s := range d.Sides {
			if !s.Imaged || !s.Processed {
				return errStop
			}
			imaged++
		}
		if imaged < d.SidesWanted {
			return errStop
		}
		d.State = store.Finalizing
		return nil
	})
	if err != nil {
		return
	}
	ctx, done := e.track(id)
	defer done()
	if err := e.finalize(ctx, id); err != nil {
		if ctx.Err() == nil {
			e.fail(id, "Finishing failed: "+err.Error())
		}
		return
	}
}

func (e *Engine) rules(d *store.Disc) pipeline.DateRules {
	set := e.Settings()
	r := pipeline.DateRules{Zone: set.Location(), ResetDates: set.ResetDates}
	if d.TZ != "" {
		if l, err := time.LoadLocation(d.TZ); err == nil {
			r.Zone = l
		}
	}
	return r
}

// ParseDiscDate reads a disc-level date: yyyy-mm-dd or yyyy-mm-ddThh:mm.
func ParseDiscDate(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	for _, layout := range []string{"2006-01-02T15:04", "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			if layout == "2006-01-02" {
				t = t.Add(12 * time.Hour) // midday, so no zone shifts the day
			}
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("date %q: use yyyy-mm-dd or yyyy-mm-ddThh:mm", s)
}

// resolveDates assigns each clip its date in recording order.
func (e *Engine) resolveDates(d *store.Disc, clips []*store.Clip) {
	recs := make([]time.Time, len(clips))
	for i, c := range clips {
		recs[i] = c.Source.RecTime
	}
	discDate, _ := ParseDiscDate(d.DiscDate)
	dates := pipeline.ResolveDates(e.rules(d), recs, discDate)
	for i, c := range clips {
		c.Date = dates[i]
	}
}

func (e *Engine) tagsFor(d *store.Disc, c *store.Clip) pipeline.Tags {
	title := d.Description
	if title == "" {
		title = d.Folder
	}
	kw := []string{"CamDVD"}
	if d.Description != "" {
		kw = append(kw, d.Description)
	}
	kw = append(kw, "Side "+c.Side)
	return pipeline.Tags{
		When:        c.Date.When,
		Make:        d.Make,
		Model:       d.Model,
		Title:       fmt.Sprintf("%s %s%02d", title, c.Side, c.Num),
		Description: strings.TrimSpace(fmt.Sprintf("%s (disc %s, side %s, recording %d)", d.Description, d.ID, c.Side, c.Num)),
		Keywords:    kw,
		Location:    d.Location,
		Comment:     fmt.Sprintf("camdvd %s %s %d", d.ID, c.Side, c.Num),
	}
}

func (e *Engine) finalize(ctx context.Context, id string) error {
	d, err := e.St.Disc(id)
	if err != nil {
		return err
	}
	clips, err := e.St.Clips(id)
	if err != nil {
		return err
	}
	e.resolveDates(d, clips)

	if d.Folder == "" || !exists(filepath.Join(e.Cfg.Library, d.Folder)) {
		var earliest time.Time
		for _, c := range clips {
			if c.Date.Source == "disc" && (earliest.IsZero() || c.Date.When.Before(earliest)) {
				earliest = c.Date.When
			}
		}
		name := library.FolderName(d.Description, earliest, d.CreatedAt.In(e.rules(d).Zone))
		d.Folder = library.Unique(e.Cfg.Library, name, "", nil)
	}
	folder, err := library.Within(e.Cfg.Library, d.Folder)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(folder, 0o775); err != nil {
		return err
	}
	e.own(folder)
	_, _ = e.update(id, func(x *store.Disc) error { x.Folder = d.Folder; return nil })

	single := d.SidesWanted <= 1 && len(d.Sides) == 1
	var warnings []string
	taken := map[string]bool{}
	for _, c := range clips {
		if c.State == "converted" {
			ext := filepath.Ext(c.Work)
			stem := library.FileStem(d.Description, d.Folder, c.Side, c.Num, single)
			name := library.Unique(folder, stem, ext, taken)
			taken[strings.ToLower(name)] = true
			dst := filepath.Join(folder, name)
			if err := os.Rename(c.Work, dst); err != nil {
				return fmt.Errorf("place %s: %w", name, err)
			}
			c.Output, c.Work, c.State = filepath.Join(d.Folder, name), "", "placed"
			if err := e.tagAndHash(ctx, d, c); err != nil {
				c.Warnings = append(c.Warnings, "tagging failed: "+err.Error())
			}
		} else if c.State == "placed" {
			// Already placed (e.g. side A of a disc that got a side B later):
			// refresh tags, since dates are resolved across the whole disc.
			if err := e.tagAndHash(ctx, d, c); err != nil {
				c.Warnings = append(c.Warnings, "tagging failed: "+err.Error())
			}
		}
		if c.Date.Flag != "" {
			warnings = append(warnings, fmt.Sprintf("%s%02d: %s", c.Side, c.Num, c.Date.Flag))
		} else if c.Date.Source == "none" {
			warnings = append(warnings, fmt.Sprintf("%s%02d: no recording date; set a disc date in the library", c.Side, c.Num))
		}
		for _, w := range c.Warnings {
			warnings = append(warnings, fmt.Sprintf("%s%02d: %s", c.Side, c.Num, w))
		}
		if c.State == "failed" {
			warnings = append(warnings, fmt.Sprintf("%s%02d: %s (source kept)", c.Side, c.Num, c.Error))
		}
		if err := e.St.SaveClip(c); err != nil {
			return err
		}
	}

	// Data discs: copy the files as they are.
	for _, s := range d.Sides {
		if s.Decision.Class.Path() == discinfo.PathCopy {
			dst := folder
			if len(d.Sides) > 1 {
				dst = filepath.Join(folder, "Side "+s.Letter)
			}
			if err := copyTree(e.sourceDir(id, s.Letter), dst); err != nil {
				return fmt.Errorf("copy files: %w", err)
			}
			warnings = append(warnings, "Side "+s.Letter+" is a data disc: files copied without conversion")
		}
	}

	if e.Settings().Combined {
		if name, err := e.combine(ctx, d, clips, folder); err != nil {
			warnings = append(warnings, "combined file: "+err.Error())
		} else if name != "" {
			d.Combined = filepath.Join(d.Folder, name)
		}
	}

	// Retention: keep the image only for unfinalized or damaged sides.
	for _, s := range d.Sides {
		if c := s.Decision.Class; c != discinfo.Unfinalized && c != discinfo.Damaged {
			_ = os.Remove(e.imagePath(id, s.Letter))
		}
	}

	d, err = e.update(id, func(x *store.Disc) error {
		x.Folder, x.Combined = d.Folder, d.Combined
		x.Warnings = dedupe(append(x.Warnings, warnings...))
		x.State, x.Message, x.DoneAt = store.Done, "", time.Now()
		return nil
	})
	if err != nil {
		return err
	}
	return e.writeManifest(id)
}

// tagAndHash writes Photos tags, sets the file time and records the hash.
func (e *Engine) tagAndHash(ctx context.Context, d *store.Disc, c *store.Clip) error {
	p, err := library.Within(e.Cfg.Library, c.Output)
	if err != nil {
		return err
	}
	var terr error
	if strings.EqualFold(filepath.Ext(p), ".mp4") {
		if _, err := pipeline.Tag(ctx, e.Tools, e.tagsFor(d, c), p); err != nil {
			terr = err
		}
	}
	if !c.Date.When.IsZero() {
		_ = os.Chtimes(p, c.Date.When, c.Date.When)
	}
	e.own(p)
	sum, size, err := hashFile(p)
	if err != nil {
		return err
	}
	c.SHA256, c.Size = sum, size
	return terr
}

// Retag recomputes dates and rewrites tags of a finished disc's files after
// its date, zone, camera or place changed. No re-encoding.
func (e *Engine) Retag(ctx context.Context, id string) error {
	d, err := e.St.Disc(id)
	if err != nil {
		return err
	}
	clips, err := e.St.Clips(id)
	if err != nil {
		return err
	}
	e.resolveDates(d, clips)
	var warnings []string
	for _, c := range clips {
		if c.State != "placed" {
			continue
		}
		if err := e.tagAndHash(ctx, d, c); err != nil {
			return err
		}
		if c.Date.Source == "none" {
			warnings = append(warnings, fmt.Sprintf("%s%02d: no recording date; set a disc date in the library", c.Side, c.Num))
		}
		if err := e.St.SaveClip(c); err != nil {
			return err
		}
	}
	_, err = e.update(id, func(x *store.Disc) error {
		var keep []string
		for _, w := range x.Warnings {
			if !strings.Contains(w, "no recording date") {
				keep = append(keep, w)
			}
		}
		x.Warnings = dedupe(append(keep, warnings...))
		return nil
	})
	if err != nil {
		return err
	}
	return e.writeManifest(id)
}

// Manifest is the disc.json written next to the disc's working files.
type Manifest struct {
	Version int           `json:"version"`
	Written time.Time     `json:"written"`
	Disc    *store.Disc   `json:"disc"`
	Clips   []*store.Clip `json:"clips"`
}

func (e *Engine) writeManifest(id string) error {
	d, err := e.St.Disc(id)
	if err != nil {
		return err
	}
	clips, err := e.St.Clips(id)
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(Manifest{Version: 1, Written: time.Now(), Disc: d, Clips: clips}, "", "  ")
	p := filepath.Join(e.workDir(id), "disc.json")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// combine writes one MP4 for the disc with a chapter per recording.
func (e *Engine) combine(ctx context.Context, d *store.Disc, clips []*store.Clip, folder string) (string, error) {
	var parts []*store.Clip
	for _, c := range clips {
		if c.State == "placed" && strings.EqualFold(filepath.Ext(c.Output), ".mp4") {
			parts = append(parts, c)
		}
	}
	if len(parts) < 2 {
		return "", nil
	}
	work := filepath.Join(e.workDir(d.ID), "combine")
	if err := os.MkdirAll(work, 0o755); err != nil {
		return "", err
	}
	var list, meta strings.Builder
	meta.WriteString(";FFMETADATA1\n")
	var start float64
	for _, c := range parts {
		p := filepath.Join(e.Cfg.Library, c.Output)
		fmt.Fprintf(&list, "file '%s'\n", strings.ReplaceAll(p, "'", `'\''`))
		end := start + c.Info.Duration
		fmt.Fprintf(&meta, "[CHAPTER]\nTIMEBASE=1/1000\nSTART=%d\nEND=%d\ntitle=%s%02d\n", int64(start*1000), int64(end*1000), c.Side, c.Num)
		start = end
	}
	lp, mp := filepath.Join(work, "list.txt"), filepath.Join(work, "chapters.txt")
	_ = os.WriteFile(lp, []byte(list.String()), 0o644)
	_ = os.WriteFile(mp, []byte(meta.String()), 0o644)
	stem := library.Sanitize(d.Description)
	if stem == "" {
		stem = d.Folder
	}
	name := stem + " - full.mp4"
	dst := filepath.Join(folder, name)
	tmp := filepath.Join(work, "full.mp4")
	_, err := e.Tools.Run(ctx, tools.Cmd{Name: "ffmpeg", Args: []string{"-hide_banner", "-nostdin", "-y", "-v", "error",
		"-f", "concat", "-safe", "0", "-i", lp, "-i", mp, "-map", "0", "-map_metadata", "1", "-map_chapters", "1",
		"-c", "copy", "-movflags", "+faststart", tmp}})
	if err != nil {
		return "", err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return "", err
	}
	first := parts[0]
	_, _ = pipeline.Tag(ctx, e.Tools, e.tagsFor(d, first), dst)
	e.own(dst)
	return name, nil
}

// CleanDisc deletes a finished disc's image and sources. Its MP4s stay.
func (e *Engine) CleanDisc(ctx context.Context, id string) error {
	d, err := e.St.Disc(id)
	if err != nil {
		return err
	}
	if d.State != store.Done {
		return fmt.Errorf("only finished discs can be cleaned up")
	}
	clips, _ := e.St.Clips(id)
	for _, c := range clips {
		if c.State != "placed" {
			continue
		}
		p, err := library.Within(e.Cfg.Library, c.Output)
		if err != nil {
			return err
		}
		in, err := pipeline.Probe(ctx, e.Tools, pipeline.Source{Kind: "file", Path: p})
		if err != nil || in.Duration <= 0 {
			return fmt.Errorf("%s doesn't verify, so its source is kept", c.Output)
		}
	}
	for _, sub := range []string{"image", "source", "out", "combine"} {
		if err := os.RemoveAll(filepath.Join(e.workDir(id), sub)); err != nil {
			return err
		}
	}
	_, err = e.update(id, func(d *store.Disc) error { d.Cleaned = true; return nil })
	return err
}

// autoClean removes images and sources of verified discs after the
// configured number of days. Off unless enabled in settings.
func (e *Engine) autoClean(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if days := e.Settings().AutoClean; days > 0 {
			discs, _ := e.St.Discs(store.Done)
			for _, d := range discs {
				if !d.Cleaned && time.Since(d.DoneAt) > time.Duration(days)*24*time.Hour && !e.Running(d.ID) {
					if err := e.CleanDisc(ctx, d.ID); err != nil {
						e.Log.Warn("auto-clean skipped", "disc", d.ID, "err", err)
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func hashFile(p string) (string, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, de fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if de.IsDir() {
			return os.MkdirAll(target, 0o775)
		}
		if !de.Type().IsRegular() {
			return nil
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o664)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	})
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func dedupe(s []string) []string {
	var out []string
	for _, x := range s {
		if !slices.Contains(out, x) {
			out = append(out, x)
		}
	}
	return out
}
