package jobs

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dnaidoo621/camdvd-rescue/internal/library"
	"github.com/dnaidoo621/camdvd-rescue/internal/store"
)

// Folder is a finished disc as the library shows it.
type Folder struct {
	Disc     *store.Disc   `json:"disc"`
	Clips    []*store.Clip `json:"clips"`
	Missing  bool          `json:"missing"` // folder not found on disk
	Duration float64       `json:"duration"`
	Size     int64         `json:"size"`
	Earliest time.Time     `json:"earliest,omitzero"`
}

// Library lists finished discs, newest first.
func (e *Engine) Library() ([]Folder, error) {
	discs, err := e.St.Discs(store.Done)
	if err != nil {
		return nil, err
	}
	var out []Folder
	for _, d := range discs {
		f, err := e.Folder(d.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, *f)
	}
	return out, nil
}

// Folder returns one disc's library view.
func (e *Engine) Folder(id string) (*Folder, error) {
	d, err := e.St.Disc(id)
	if err != nil {
		return nil, err
	}
	clips, err := e.St.Clips(id)
	if err != nil {
		return nil, err
	}
	f := &Folder{Disc: d, Clips: clips}
	if d.Folder != "" {
		if _, err := os.Stat(filepath.Join(e.Cfg.Library, d.Folder)); err != nil {
			f.Missing = true
		}
	}
	for _, c := range clips {
		f.Duration += c.Info.Duration
		f.Size += c.Size
		if !c.Date.When.IsZero() && (f.Earliest.IsZero() || c.Date.When.Before(f.Earliest)) {
			f.Earliest = c.Date.When
		}
	}
	return f, nil
}

// Selection picks what a bulk rename applies to.
type Selection struct {
	Clips       []int64  `json:"clips"`        // individual files
	FolderFiles []string `json:"folder_files"` // every file in these discs
	Folders     []string `json:"folders"`      // the disc folders themselves
	AllFiles    bool     `json:"all_files"`    // every file in the library
}

// Preview is a planned bulk rename, stored so apply uses exactly what the
// user saw.
type Preview struct {
	ID      string          `json:"id"`
	Pattern library.Pattern `json:"pattern"`
	Ops     []library.Op    `json:"ops"`
	Clean   bool            `json:"clean"`
	Changes int             `json:"changes"`
}

func (e *Engine) items(sel Selection) ([]library.Item, error) {
	var items []library.Item
	seen := map[int64]bool{}
	addClip := func(d *store.Disc, c *store.Clip) {
		if seen[c.ID] || c.State != "placed" || c.Output == "" {
			return
		}
		seen[c.ID] = true
		p := filepath.Join(e.Cfg.Library, c.Output)
		desc := d.Description
		if desc == "" {
			desc = d.Folder
		}
		items = append(items, library.Item{Kind: "file", ID: fmt.Sprint(c.ID), Dir: filepath.Dir(p), Name: filepath.Base(p),
			Desc: desc, Side: c.Side, Date: c.Date.When, Duration: c.Info.Duration, Rec: c.Num})
	}
	discIDs := sel.FolderFiles
	if sel.AllFiles {
		discs, err := e.St.Discs(store.Done)
		if err != nil {
			return nil, err
		}
		discIDs = nil
		for i := len(discs) - 1; i >= 0; i-- { // oldest first
			discIDs = append(discIDs, discs[i].ID)
		}
	}
	for _, id := range discIDs {
		d, err := e.St.Disc(id)
		if err != nil {
			return nil, err
		}
		if d.State != store.Done {
			return nil, fmt.Errorf("disc %s isn't finished yet", id)
		}
		clips, _ := e.St.Clips(id)
		for _, c := range clips {
			addClip(d, c)
		}
	}
	for _, cid := range sel.Clips {
		c, err := e.St.Clip(cid)
		if err != nil {
			return nil, err
		}
		d, err := e.St.Disc(c.DiscID)
		if err != nil {
			return nil, err
		}
		addClip(d, c)
	}
	for _, id := range sel.Folders {
		d, err := e.St.Disc(id)
		if err != nil {
			return nil, err
		}
		if d.State != store.Done || d.Folder == "" {
			return nil, fmt.Errorf("disc %s has no finished folder", id)
		}
		items = append(items, library.Item{Kind: "folder", ID: d.ID, Dir: e.Cfg.Library, Name: d.Folder,
			Desc: d.Description, Date: e.earliest(d.ID)})
	}
	return items, nil
}

func (e *Engine) earliest(id string) time.Time {
	f, err := e.Folder(id)
	if err != nil {
		return time.Time{}
	}
	return f.Earliest
}

// PreviewRename plans a bulk rename and stores the plan.
func (e *Engine) PreviewRename(sel Selection, p library.Pattern) (*Preview, error) {
	items, err := e.items(sel)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("nothing selected")
	}
	ops, err := library.Plan(items, p)
	if err != nil {
		return nil, err
	}
	pv := &Preview{ID: store.NewID(), Pattern: p, Ops: ops, Clean: true}
	for _, o := range ops {
		if o.Conflict != "" {
			pv.Clean = false
		}
		if o.Changed() {
			pv.Changes++
		}
	}
	return pv, e.St.SavePreview(pv.ID, pv)
}

// ApplyRename applies a stored, conflict-free preview as one undoable batch.
func (e *Engine) ApplyRename(previewID string) (*store.RenameBatch, error) {
	var pv Preview
	if err := e.St.Preview(previewID, &pv); err != nil {
		return nil, err
	}
	if !pv.Clean {
		return nil, library.ErrConflicts
	}
	// Re-plan against the disk as it is now, so a file that appeared since
	// the preview is caught.
	var items []library.Item
	for _, o := range pv.Ops {
		items = append(items, o.Item)
	}
	ops, err := library.Plan(items, pv.Pattern)
	if err != nil {
		return nil, err
	}
	for i, o := range ops {
		if o.Conflict != "" || o.NewName != pv.Ops[i].NewName {
			return nil, fmt.Errorf("the library changed since the preview; preview again")
		}
	}
	b := &store.RenameBatch{ID: store.NewID(), CreatedAt: time.Now()}
	moves, err := library.Apply(e.Cfg.Library, b.ID, ops)
	if err != nil {
		return nil, err
	}
	for _, m := range moves {
		b.Moves = append(b.Moves, store.Move(m))
	}
	b.Summary = fmt.Sprintf("%d renamed", len(moves))
	if len(moves) > 0 {
		b.Summary += fmt.Sprintf(", e.g. %s → %s", filepath.Base(moves[0].From), filepath.Base(moves[0].To))
	}
	if err := e.followMoves(moves); err != nil {
		return nil, err
	}
	return b, e.St.SaveBatch(b)
}

// UndoRename reverses a batch.
func (e *Engine) UndoRename(batchID string) error {
	b, err := e.St.Batch(batchID)
	if err != nil {
		return err
	}
	if b.Undone {
		return fmt.Errorf("batch already undone")
	}
	var moves []library.Move
	for _, m := range b.Moves {
		moves = append(moves, library.Move(m))
	}
	inv := library.Inverse(moves)
	if err := library.Execute(e.Cfg.Library, "undo-"+b.ID, inv); err != nil {
		return err
	}
	if err := e.followMoves(inv); err != nil {
		return err
	}
	b.Undone = true
	return e.St.SaveBatch(b)
}

// followMoves updates the database and manifests after renames.
func (e *Engine) followMoves(moves []library.Move) error {
	touched := map[string]bool{}
	rel := func(p string) string {
		r, _ := filepath.Rel(e.Cfg.Library, p)
		return r
	}
	for _, m := range moves {
		switch m.Kind {
		case "file":
			var id int64
			fmt.Sscan(m.ID, &id)
			c, err := e.St.Clip(id)
			if err != nil {
				return err
			}
			c.Output = rel(m.To)
			if err := e.St.SaveClip(c); err != nil {
				return err
			}
			touched[c.DiscID] = true
		case "folder":
			oldRel, newRel := rel(m.From), rel(m.To)
			if _, err := e.update(m.ID, func(d *store.Disc) error {
				d.Folder = newRel
				if d.Combined != "" {
					d.Combined = filepath.Join(newRel, filepath.Base(d.Combined))
				}
				return nil
			}); err != nil {
				return err
			}
			clips, _ := e.St.Clips(m.ID)
			for _, c := range clips {
				if strings.HasPrefix(c.Output, oldRel+string(filepath.Separator)) {
					c.Output = filepath.Join(newRel, strings.TrimPrefix(c.Output, oldRel+string(filepath.Separator)))
					if err := e.St.SaveClip(c); err != nil {
						return err
					}
				}
			}
			touched[m.ID] = true
		}
	}
	for id := range touched {
		_ = e.writeManifest(id)
		e.Hub.Publish("job-"+id, "")
	}
	e.Hub.Publish("library", "")
	return nil
}

// RenameFolder renames one disc folder as an undoable batch.
func (e *Engine) RenameFolder(id, name string) (*store.RenameBatch, error) {
	name = library.Sanitize(name)
	if name == "" {
		return nil, fmt.Errorf("folder name is empty")
	}
	pv, err := e.PreviewRename(Selection{Folders: []string{id}}, library.Pattern{Template: strings.ReplaceAll(name, "{", "(")})
	if err != nil {
		return nil, err
	}
	if !pv.Clean {
		return nil, fmt.Errorf("%s", pv.Ops[0].Conflict)
	}
	if pv.Changes == 0 {
		return nil, nil
	}
	return e.ApplyRename(pv.ID)
}

// DiscEdit changes a finished disc's description and metadata.
type DiscEdit struct {
	Description *string `json:"description"`
	Folder      *string `json:"folder"`
	Make        *string `json:"make"`
	Model       *string `json:"model"`
	TZ          *string `json:"tz"`
	DiscDate    *string `json:"disc_date"`
	Location    *string `json:"location"`
	Imported    *bool   `json:"imported"`
}

// EditDisc applies a DiscEdit; tag fields are rewritten into the files.
func (e *Engine) EditDisc(ctx context.Context, id string, ed DiscEdit) error {
	if ed.DiscDate != nil {
		if _, err := ParseDiscDate(*ed.DiscDate); err != nil {
			return err
		}
	}
	if ed.TZ != nil && *ed.TZ != "" {
		if _, err := time.LoadLocation(*ed.TZ); err != nil {
			return fmt.Errorf("unknown time zone %q", *ed.TZ)
		}
	}
	if ed.Location != nil && *ed.Location != "" && !validISO6709(*ed.Location) {
		return fmt.Errorf("location %q: use ISO 6709 like +29.8587+031.0218/", *ed.Location)
	}
	retag := false
	d, err := e.update(id, func(d *store.Disc) error {
		set := func(dst *string, v *string) {
			if v != nil && strings.TrimSpace(*v) != *dst {
				*dst = strings.TrimSpace(*v)
				retag = true
			}
		}
		set(&d.Description, ed.Description)
		set(&d.Make, ed.Make)
		set(&d.Model, ed.Model)
		set(&d.TZ, ed.TZ)
		set(&d.DiscDate, ed.DiscDate)
		set(&d.Location, ed.Location)
		if ed.Imported != nil {
			d.Imported = *ed.Imported
		}
		return nil
	})
	if err != nil {
		return err
	}
	if ed.Folder != nil && library.Sanitize(*ed.Folder) != d.Folder && d.State == store.Done {
		if _, err := e.RenameFolder(id, *ed.Folder); err != nil {
			return err
		}
	}
	if retag && d.State == store.Done {
		// Written in the background: rewriting every file takes minutes.
		e.requestRetag(id)
	}
	return nil
}

func validISO6709(s string) bool {
	if !strings.HasSuffix(s, "/") || len(s) < 4 || (s[0] != '+' && s[0] != '-') {
		return false
	}
	for _, r := range s[1 : len(s)-1] {
		if !(r >= '0' && r <= '9' || r == '.' || r == '+' || r == '-') {
			return false
		}
	}
	return true
}

// RescanReport says what a rescan found.
type RescanReport struct {
	Relinked []string `json:"relinked"`
	Missing  []string `json:"missing"`
	Unknown  []string `json:"unknown"`
}

// Rescan re-links files and folders renamed outside the app (e.g. over
// SMB) by size and hash, and lists files it doesn't know. It never deletes.
func (e *Engine) Rescan(ctx context.Context) (*RescanReport, error) {
	bySize := map[int64][]string{}
	known := map[string]bool{}
	err := filepath.WalkDir(e.Cfg.Library, func(p string, de fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if de.IsDir() && de.Name() == ".camdvd" {
			return filepath.SkipDir
		}
		if de.Type().IsRegular() {
			if fi, err := de.Info(); err == nil {
				bySize[fi.Size()] = append(bySize[fi.Size()], p)
			}
		}
		return ctx.Err()
	})
	if err != nil {
		return nil, err
	}
	discs, err := e.St.Discs(store.Done)
	if err != nil {
		return nil, err
	}
	rep := &RescanReport{}
	for _, d := range discs {
		clips, _ := e.St.Clips(d.ID)
		newFolder := ""
		for _, c := range clips {
			if c.State != "placed" {
				continue
			}
			p := filepath.Join(e.Cfg.Library, c.Output)
			if _, err := os.Stat(p); err == nil {
				known[p] = true
				continue
			}
			found := ""
			for _, cand := range bySize[c.Size] {
				if known[cand] {
					continue
				}
				if sum, _, err := hashFile(cand); err == nil && sum == c.SHA256 {
					found = cand
					break
				}
			}
			if found == "" {
				rep.Missing = append(rep.Missing, c.Output)
				continue
			}
			known[found] = true
			r, _ := filepath.Rel(e.Cfg.Library, found)
			rep.Relinked = append(rep.Relinked, c.Output+" → "+r)
			c.Output = r
			_ = e.St.SaveClip(c)
			if dir := filepath.Dir(r); dir != d.Folder {
				newFolder = dir
			}
		}
		if newFolder != "" {
			if _, err := os.Stat(filepath.Join(e.Cfg.Library, d.Folder)); err != nil {
				_, _ = e.update(d.ID, func(d *store.Disc) error { d.Folder = newFolder; return nil })
			}
		}
		if d.Combined != "" {
			known[filepath.Join(e.Cfg.Library, d.Combined)] = true
		}
		_ = e.writeManifest(d.ID)
	}
	for _, paths := range bySize {
		for _, p := range paths {
			if !known[p] {
				r, _ := filepath.Rel(e.Cfg.Library, p)
				rep.Unknown = append(rep.Unknown, r)
			}
		}
	}
	sort.Strings(rep.Unknown)
	e.Hub.Publish("library", "")
	return rep, nil
}
