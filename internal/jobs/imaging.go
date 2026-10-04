package jobs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/dnaidoo621/camdvd-rescue/internal/discinfo"
	"github.com/dnaidoo621/camdvd-rescue/internal/drive"
	"github.com/dnaidoo621/camdvd-rescue/internal/store"
)

// onInsert handles a disc arriving in a drive. It runs with the drive
// marked busy.
func (e *Engine) onInsert(dc *driveCtl) {
	ctx := e.ctx
	if reason := e.Blocked(); reason != "" {
		e.notice(dc, "Disc detected, but CamDVD can't process discs until the self-check passes: "+reason)
		return
	}
	e.notice(dc, "Disc detected, probing…")
	probe, evidence, err := dc.d.Probe(ctx)
	if err != nil {
		e.notice(dc, "Couldn't probe the disc: "+err.Error())
		return
	}
	if probe.Media.NoMedia {
		e.notice(dc, "")
		return
	}

	if hid := e.holder(dc); hid != "" {
		d, err := e.St.Disc(hid)
		if err == nil {
			e.continueHolder(ctx, dc, d, probe, evidence)
			return
		}
		e.setHolder(dc, "")
	}
	e.startNew(ctx, dc, probe, evidence)
}

// startNew begins a job for a newly inserted disc.
func (e *Engine) startNew(ctx context.Context, dc *driveCtl, probe discinfo.Probe, evidence string) {
	dec, method, ok := discinfo.PreClassify(probe)
	if !ok {
		e.notice(dc, dec.Reason+". Ejecting.")
		_ = e.eject(dc)
		return
	}
	fp, err := drive.Fingerprint(ctx, dc.d, probe)
	if err != nil {
		e.notice(dc, "Couldn't read the disc: "+err.Error())
		return
	}
	set := e.Settings()
	d := &store.Disc{
		ID:      store.NewDiscID(),
		DriveID: dc.d.Info().ID,
		State:   store.Probing,
		Sides:   []*store.Side{{Letter: "A", Fingerprint: fp, Probe: probe, Evidence: evidence, Method: method}},
		Make:    set.Make, Model: set.Model, TZ: set.TimeZone,
	}
	if set.Batch {
		d.Answered, d.SidesWanted, d.Description = true, max(1, set.BatchSides), set.BatchDesc
	}
	if err := e.St.CreateDisc(d); err != nil {
		e.notice(dc, "Database error: "+err.Error())
		return
	}
	e.setHolder(dc, d.ID)
	e.notice(dc, "")
	e.Hub.Publish("jobs", "")

	// Already imported? Ask before reading it again.
	if prev, _ := e.St.DiscsByFingerprint(fp); len(prev) > 0 {
		for _, p := range prev {
			if p.ID != d.ID && p.State == store.Done {
				_, _ = e.update(d.ID, func(d *store.Disc) error {
					d.State = store.Duplicate
					d.DuplicateOf = p.ID
					d.Message = fmt.Sprintf("Imported on %s as %q. Import again?", p.CreatedAt.Format("2 Jan 2006"), p.Folder)
					return nil
				})
				return
			}
		}
	}
	_, _ = e.update(d.ID, func(d *store.Disc) error { d.State = store.ImagingA; return nil })
	e.imageSide(dc, d.ID, "A")
}

// continueHolder handles an insert while a job owns the drive: side B after
// a flip, or the same disc returning after removal or a restart.
func (e *Engine) continueHolder(ctx context.Context, dc *driveCtl, d *store.Disc, probe discinfo.Probe, evidence string) {
	fp, err := drive.Fingerprint(ctx, dc.d, probe)
	if err != nil {
		e.notice(dc, "Couldn't read the disc: "+err.Error())
		return
	}
	switch d.State {
	case store.AwaitingFlip:
		_, method, ok := discinfo.PreClassify(probe)
		if !ok {
			e.notice(dc, "That side reads as blank or unsupported. Insert side B of the same disc, or press Cancel.")
			_ = e.eject(dc)
			return
		}
		if a := d.Side("A"); a != nil && a.Fingerprint == fp {
			e.notice(dc, "This looks like side A again, flip it over.")
			_ = e.eject(dc)
			return
		}
		e.notice(dc, "")
		_, _ = e.update(d.ID, func(d *store.Disc) error {
			if b := d.Side("B"); b != nil && !b.Imaged && b.Fingerprint != fp {
				// A different side B than before: start it over.
				_ = os.Remove(e.imagePath(d.ID, "B"))
				_ = os.Remove(e.mapPath(d.ID, "B"))
				d.Sides = d.Sides[:1]
			}
			if d.Side("B") == nil {
				d.Sides = append(d.Sides, &store.Side{Letter: "B", Fingerprint: fp, Probe: probe, Evidence: evidence, Method: method})
			}
			d.State, d.Message = store.ImagingB, ""
			return nil
		})
		e.imageSide(dc, d.ID, "B")
		return
	}

	// Resume imaging a side that matches what's in the drive.
	for _, s := range d.Sides {
		if s.Imaged {
			continue
		}
		if s.Fingerprint != fp {
			e.notice(dc, fmt.Sprintf("This isn't the disc job %s is waiting for. Insert that disc, or cancel the job to free the drive.", d.ID))
			_ = e.eject(dc)
			return
		}
		e.notice(dc, "")
		letter := s.Letter
		_, _ = e.update(d.ID, func(d *store.Disc) error {
			d.State, d.Message = store.ImagingA, ""
			if letter == "B" {
				d.State = store.ImagingB
			}
			return nil
		})
		e.imageSide(dc, d.ID, letter)
		return
	}
	e.notice(dc, fmt.Sprintf("Drive is reserved by job %s (%s).", d.ID, d.State.Label()))
}

// imageSide reads one side into the disc's work folder, then hands it to
// processing and decides what the drive does next.
func (e *Engine) imageSide(dc *driveCtl, id, letter string) {
	ctx, done := e.track(id)
	defer done()
	d, err := e.St.Disc(id)
	if err != nil {
		return
	}
	s := d.Side(letter)
	total := s.Probe.Media.UsedSectors()
	if msg := e.needSpace(total * drive.SectorSize * 3); msg != "" {
		_, _ = e.update(id, func(d *store.Disc) error { d.State, d.Message = store.Paused, msg; return nil })
		return
	}
	img, mp := e.imagePath(id, letter), e.mapPath(id, letter)
	if err := os.MkdirAll(filepath.Dir(img), 0o755); err != nil {
		e.fail(id, err.Error())
		return
	}
	stageIdx := e.beginStage(id, letter, "image", fmt.Sprintf("Reading side %s with %s", letter, s.Method))
	req := drive.ImageRequest{Method: s.Method, Out: img, Map: mp}
	if s.Method == discinfo.ImageRaw || total > 0 {
		req.Sectors = total
	}
	res, err := dc.d.Image(ctx, req, func(p drive.Progress) {
		if p.Total == 0 || !e.Hub.Throttled("img-"+id, "", 750*time.Millisecond) {
			return
		}
		_, _ = e.St.UpdateDisc(id, func(d *store.Disc) error {
			sd := d.Side(letter)
			sd.ImageDone = float64(p.Done) / float64(p.Total)
			sd.Stages[stageIdx].Progress = sd.ImageDone
			sd.Stages[stageIdx].Message = fmt.Sprintf("%.0f of %.0f MB, %d unreadable sectors", float64(p.Done)/1e6, float64(p.Total)/1e6, p.Bad/drive.SectorSize)
			return nil
		})
		e.Hub.Publish("job-"+id, "")
	})
	switch {
	case errors.Is(err, drive.ErrMediaRemoved):
		e.endStage(id, letter, stageIdx, "failed", res.CmdLines, res.Stderr, "Disc removed during imaging")
		_, _ = e.update(id, func(d *store.Disc) error {
			d.State, d.Message = store.Paused, "Disc removed during imaging. Insert the same disc to continue where it stopped."
			return nil
		})
		return
	case ctx.Err() != nil:
		e.endStage(id, letter, stageIdx, "failed", res.CmdLines, res.Stderr, "Cancelled")
		return
	case err != nil:
		e.endStage(id, letter, stageIdx, "failed", res.CmdLines, res.Stderr, err.Error())
		e.fail(id, fmt.Sprintf("Imaging side %s failed: %v", letter, err))
		_ = e.eject(dc)
		e.setHolder(dc, "")
		return
	}
	msg := fmt.Sprintf("%d sectors read", res.Sectors)
	if n := res.Bad.Total(); n > 0 {
		msg += fmt.Sprintf(", %d unreadable (zero-filled)", n)
	}
	if res.StoppedAt > 0 {
		msg += fmt.Sprintf("; stopped at the unwritten tail at sector %d", res.StoppedAt)
	}
	e.endStage(id, letter, stageIdx, "done", res.CmdLines, res.Stderr, msg)
	_, _ = e.update(id, func(d *store.Disc) error {
		sd := d.Side(letter)
		sd.Imaged, sd.ImageDone, sd.Bad, sd.StoppedAt = true, 1, res.Bad, res.StoppedAt
		sd.Sectors = res.Sectors
		if res.StoppedAt > 0 {
			sd.Sectors = res.StoppedAt
		}
		return nil
	})

	// Processing starts now, from the image; the drive is free for the
	// next step whatever happens there.
	e.goProcess(id, letter)
	e.afterImaged(ctx, dc, id, letter)
}

// afterImaged decides whether to ask for side B or release the drive.
func (e *Engine) afterImaged(ctx context.Context, dc *driveCtl, id, letter string) {
	if letter == "A" {
		// The sides answer is needed now; keep the tray closed until it comes.
		for {
			d, err := e.St.Disc(id)
			if err != nil || d.State.Terminal() {
				return
			}
			if d.Answered {
				break
			}
			if d.State != store.AwaitingAnswer {
				_, _ = e.update(id, func(d *store.Disc) error { d.State = store.AwaitingAnswer; return nil })
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
		}
		d, _ := e.St.Disc(id)
		if d.SidesWanted == 2 && d.Side("B") == nil {
			_, _ = e.update(id, func(d *store.Disc) error {
				d.State, d.Message = store.AwaitingFlip, "Flip the disc and insert side B"
				return nil
			})
			_ = e.eject(dc)
			return
		}
	}
	_ = e.eject(dc)
	e.setHolder(dc, "")
	_, _ = e.update(id, func(d *store.Disc) error {
		if d.State.HoldsDrive() {
			d.State, d.Message = store.Processing, ""
		}
		return nil
	})
	e.maybeFinalize(id)
}

func (e *Engine) beginStage(id, letter, name, msg string) int {
	idx := 0
	_, _ = e.update(id, func(d *store.Disc) error {
		s := d.Side(letter)
		s.Stages = append(s.Stages, store.Stage{Name: name, Status: "running", Started: time.Now(), Message: msg})
		idx = len(s.Stages) - 1
		return nil
	})
	return idx
}

func (e *Engine) endStage(id, letter string, idx int, status string, cmds []string, stderr, msg string) {
	_, _ = e.update(id, func(d *store.Disc) error {
		s := d.Side(letter)
		if idx >= len(s.Stages) {
			return nil
		}
		st := &s.Stages[idx]
		st.Status, st.Ended, st.Cmd, st.Message = status, time.Now(), cmds, msg
		if status == "done" {
			st.Progress = 1
		}
		if len(stderr) > 4000 {
			stderr = stderr[len(stderr)-4000:]
		}
		st.Stderr = stderr
		return nil
	})
}
