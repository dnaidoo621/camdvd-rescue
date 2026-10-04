package jobs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/dnaidoo621/camdvd-rescue/internal/drive"
	"github.com/dnaidoo621/camdvd-rescue/internal/store"
)

// ErrBusy is returned when an action conflicts with running work.
var ErrBusy = errors.New("busy")

// recover puts unfinished jobs back on track after a restart or crash.
func (e *Engine) recover() error {
	discs, err := e.St.Discs()
	if err != nil {
		return err
	}
	for _, d := range discs {
		if d.State.Terminal() {
			continue
		}
		dc := e.driveFor(d.DriveID)
		switch d.State {
		case store.Detected, store.Probing, store.ImagingA, store.ImagingB:
			// Imaging was interrupted; it resumes from the mapfile when the
			// watcher sees the same disc.
			if dc != nil {
				dc.holder = d.ID
			}
			_, _ = e.St.UpdateDisc(d.ID, func(d *store.Disc) error {
				d.State, d.Message = store.Paused, "CamDVD restarted while imaging; it continues when the disc is detected."
				return nil
			})
		case store.AwaitingAnswer, store.AwaitingFlip, store.Duplicate:
			if dc != nil {
				dc.holder = d.ID
			}
			if d.State == store.AwaitingAnswer {
				// The disc is (probably) still in the tray; wait for the answer.
				id := d.ID
				ctx, done := e.track(id)
				go func() {
					defer done()
					e.afterImaged(ctx, dc, id, "A")
				}()
			}
		case store.Paused:
			if dc != nil && needsDrive(d) {
				dc.holder = d.ID
			}
			if !needsDrive(d) {
				e.resumeProcessing(d)
			}
		case store.Processing, store.Finalizing:
			_, _ = e.St.UpdateDisc(d.ID, func(d *store.Disc) error { d.State = store.Processing; return nil })
			e.resumeProcessing(d)
		}
	}
	return nil
}

// needsDrive reports whether a disc still has a side to read.
func needsDrive(d *store.Disc) bool {
	if d.State == store.AwaitingFlip {
		return true
	}
	for _, s := range d.Sides {
		if !s.Imaged {
			return true
		}
	}
	return false
}

func (e *Engine) resumeProcessing(d *store.Disc) {
	started := false
	for _, s := range d.Sides {
		if s.Imaged && !s.Processed {
			e.goProcess(d.ID, s.Letter)
			started = true
		}
	}
	if !started {
		go e.maybeFinalize(d.ID)
	}
}

// Answers records the two questions for a disc.
func (e *Engine) Answers(id string, sides int, desc string) (*store.Disc, error) {
	if sides != 1 && sides != 2 {
		return nil, fmt.Errorf("sides must be 1 or 2")
	}
	d, err := e.update(id, func(d *store.Disc) error {
		if d.Answered && d.SidesWanted == 2 && sides == 1 && d.Side("B") != nil {
			return fmt.Errorf("side B is already imaged")
		}
		d.Answered, d.SidesWanted, d.Description = true, sides, strings.TrimSpace(desc)
		return nil
	})
	if err != nil {
		return nil, err
	}
	set := e.Settings()
	set.LastSides = sides
	_ = e.SaveSettings(set)
	// Side A may have finished processing while we waited for the answer.
	if d.State == store.Processing {
		go e.maybeFinalize(id)
	}
	return d, nil
}

// Cancel stops a job's work, ejecting the disc if it holds the drive.
func (e *Engine) Cancel(id string) error {
	d, err := e.update(id, func(d *store.Disc) error {
		if d.State.Terminal() {
			return fmt.Errorf("job already %s", d.State)
		}
		d.State, d.Message = store.Cancelled, "Cancelled. Resume continues where it stopped."
		return nil
	})
	if err != nil {
		return err
	}
	e.cancelWork(id)
	if dc := e.driveFor(d.DriveID); dc != nil && e.holder(dc) == id {
		e.setHolder(dc, "")
		_ = e.eject(dc)
	}
	return nil
}

// Resume restarts a cancelled, failed or paused job from where it stopped.
func (e *Engine) Resume(id string) error {
	if e.Running(id) {
		return fmt.Errorf("%w: job is still running", ErrBusy)
	}
	d, err := e.St.Disc(id)
	if err != nil {
		return err
	}
	switch d.State {
	case store.Cancelled, store.Failed, store.Paused:
	default:
		return fmt.Errorf("a %s job can't be resumed", d.State)
	}
	if needsDrive(d) {
		dc := e.driveFor(d.DriveID)
		if dc == nil {
			return fmt.Errorf("drive %s is not configured", d.DriveID)
		}
		if h := e.holder(dc); h != "" && h != id {
			return fmt.Errorf("%w: drive is in use by job %s", ErrBusy, h)
		}
		e.setHolder(dc, id)
		state, msg := store.Paused, "Insert the disc to continue imaging."
		if d.Side("A").Imaged && d.SidesWanted == 2 && d.Side("B") == nil {
			state, msg = store.AwaitingFlip, "Flip the disc and insert side B"
		}
		_, _ = e.update(id, func(d *store.Disc) error { d.State, d.Message = state, msg; return nil })
		// If the disc is already in the tray, start now.
		if st, _ := dc.d.Status(e.ctx); st == drive.StatusDiscOK && dc.tryBusy() {
			e.wg.Add(1)
			go func() { defer e.wg.Done(); defer dc.idle(); e.onInsert(dc) }()
		}
		// Sides that are already imaged can process meanwhile.
		for _, s := range d.Sides {
			if s.Imaged && !s.Processed {
				e.goProcess(id, s.Letter)
			}
		}
		return nil
	}
	d, err = e.update(id, func(d *store.Disc) error {
		d.State, d.Message = store.Processing, ""
		for _, s := range d.Sides {
			s.Error = ""
		}
		return nil
	})
	if err != nil {
		return err
	}
	// Failed clips get another try.
	clips, _ := e.St.Clips(id)
	for _, c := range clips {
		if c.State == "failed" || c.State == "converting" || c.State == "queued" {
			c.State, c.Error = "pending", ""
			_ = e.St.SaveClip(c)
			_, _ = e.update(id, func(d *store.Disc) error { d.Side(c.Side).Processed = false; return nil })
		}
	}
	d, _ = e.St.Disc(id)
	e.resumeProcessing(d)
	return nil
}

// ForceRaw overrides the classification: every imaged side is carved again.
func (e *Engine) ForceRaw(id string) error {
	d, err := e.St.Disc(id)
	if err != nil {
		return err
	}
	if d.State == store.Done {
		return fmt.Errorf("the disc is already finished")
	}
	e.cancelWork(id)
	if d.State.HoldsDrive() {
		// Imaging goes on; only the processing restarts.
		d, err = e.update(id, func(d *store.Disc) error {
			d.ForceRaw = true
			for _, s := range d.Sides {
				s.Extracted, s.Processed = false, false
			}
			return nil
		})
		if err != nil {
			return err
		}
		for _, s := range d.Sides {
			_ = e.St.DeleteClips(id, s.Letter)
			if s.Imaged {
				e.goProcess(id, s.Letter)
			}
		}
		return nil
	}
	d, err = e.update(id, func(d *store.Disc) error {
		d.ForceRaw = true
		d.State, d.Message = store.Processing, ""
		for _, s := range d.Sides {
			s.Extracted, s.Processed, s.Error = false, false, ""
		}
		return nil
	})
	if err != nil {
		return err
	}
	_ = os.RemoveAll(filepath.Join(e.workDir(id), "out"))
	for _, s := range d.Sides {
		_ = e.St.DeleteClips(id, s.Letter)
	}
	e.resumeProcessing(d)
	return nil
}

// ConfirmDuplicate answers "Import again?" with yes.
func (e *Engine) ConfirmDuplicate(id string) error {
	d, err := e.St.Disc(id)
	if err != nil {
		return err
	}
	if d.State != store.Duplicate {
		return fmt.Errorf("job is not waiting for a duplicate decision")
	}
	dc := e.driveFor(d.DriveID)
	if dc == nil || !dc.tryBusy() {
		return fmt.Errorf("%w: drive is busy", ErrBusy)
	}
	_, _ = e.update(id, func(d *store.Disc) error { d.State, d.Message = store.ImagingA, ""; return nil })
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		defer dc.idle()
		e.imageSide(dc, id, "A")
	}()
	return nil
}

// AddSide reopens a finished single-sided disc to image its side B.
func (e *Engine) AddSide(id string) error {
	d, err := e.St.Disc(id)
	if err != nil {
		return err
	}
	if d.State != store.Done || d.Side("B") != nil {
		return fmt.Errorf("only a finished single-sided disc can get another side")
	}
	dc := e.driveFor(d.DriveID)
	if dc == nil && len(e.drives) > 0 {
		dc = e.drives[0]
	}
	if dc == nil {
		return fmt.Errorf("no drive")
	}
	if h := e.holder(dc); h != "" {
		return fmt.Errorf("%w: drive is in use by job %s", ErrBusy, h)
	}
	e.setHolder(dc, id)
	_, err = e.update(id, func(d *store.Disc) error {
		d.DriveID = dc.d.Info().ID
		d.SidesWanted = 2
		d.State, d.Message = store.AwaitingFlip, "Flip the disc and insert side B"
		return nil
	})
	if err != nil {
		return err
	}
	_ = e.eject(dc)
	return nil
}

// Eject ejects a drive unless a job is reading it.
func (e *Engine) Eject(driveID string) error {
	dc := e.driveFor(driveID)
	if dc == nil {
		return fmt.Errorf("no drive %q", driveID)
	}
	if h := e.holder(dc); h != "" {
		if d, err := e.St.Disc(h); err == nil && (d.State == store.ImagingA || d.State == store.ImagingB || d.State == store.Probing) {
			return fmt.Errorf("%w: job %s is reading the disc; cancel it first", ErrBusy, h)
		}
	}
	return e.eject(dc)
}

// Insert loads an image into a demo drive.
func (e *Engine) Insert(driveID, image string) error {
	dc := e.driveFor(driveID)
	if dc == nil {
		return fmt.Errorf("no drive %q", driveID)
	}
	im, ok := dc.d.(*drive.ImageDrive)
	if !ok {
		return fmt.Errorf("drive %s is a real drive; insert a disc by hand", driveID)
	}
	if im.Current() != "" {
		return fmt.Errorf("%w: eject the current image first", ErrBusy)
	}
	return im.Insert(image)
}

// DeleteJob forgets a finished, failed or cancelled job and its working
// files. Its MP4s in the library stay.
func (e *Engine) DeleteJob(id string) error {
	d, err := e.St.Disc(id)
	if err != nil {
		return err
	}
	if !d.State.Terminal() || e.Running(id) {
		return fmt.Errorf("%w: cancel the job first", ErrBusy)
	}
	if err := os.RemoveAll(e.workDir(id)); err != nil {
		return err
	}
	if err := e.St.DeleteDisc(id); err != nil {
		return err
	}
	e.Hub.Publish("jobs", "")
	return nil
}
