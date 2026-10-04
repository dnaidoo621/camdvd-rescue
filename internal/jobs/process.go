package jobs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dnaidoo621/camdvd-rescue/internal/config"
	"github.com/dnaidoo621/camdvd-rescue/internal/discinfo"
	"github.com/dnaidoo621/camdvd-rescue/internal/pipeline"
	"github.com/dnaidoo621/camdvd-rescue/internal/store"
	"github.com/dnaidoo621/camdvd-rescue/internal/tools"
)

// goProcess starts processing a side in the background.
func (e *Engine) goProcess(id, letter string) {
	ctx, done := e.track(id)
	go func() {
		defer done()
		if err := e.processSide(ctx, id, letter); err != nil && ctx.Err() == nil {
			e.update(id, func(d *store.Disc) error {
				d.Side(letter).Error = err.Error()
				return nil
			})
			e.fail(id, fmt.Sprintf("Side %s: %v", letter, err))
			return
		}
		if ctx.Err() == nil {
			e.maybeFinalize(id)
		}
	}()
}

func (e *Engine) setStep(id, letter, step string) {
	e.update(id, func(d *store.Disc) error { d.Side(letter).Step = step; return nil })
}

// processSide classifies an imaged side, extracts or carves its recordings,
// verifies them and converts each one. It's idempotent: finished steps and
// converted clips are skipped on resume.
func (e *Engine) processSide(ctx context.Context, id, letter string) error {
	d, err := e.St.Disc(id)
	if err != nil {
		return err
	}
	s := d.Side(letter)
	if s == nil || !s.Imaged {
		return nil
	}
	if !s.Extracted {
		if err := e.extractSide(ctx, d, letter); err != nil {
			return err
		}
	}
	d, _ = e.St.Disc(id)
	if d.Side(letter).Decision.Class.Path() == discinfo.PathCopy {
		e.update(id, func(d *store.Disc) error { sd := d.Side(letter); sd.Processed, sd.Step = true, "done"; return nil })
		return nil
	}

	clips, err := e.St.Clips(id)
	if err != nil {
		return err
	}
	set := e.Settings()
	e.setStep(id, letter, "converting")
	for _, c := range clips {
		if c.Side != letter || c.Done() {
			continue
		}
		if err := e.convertClip(ctx, c, set); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			c.State, c.Error = "failed", err.Error()
			_ = e.St.SaveClip(c)
			e.Hub.Publish("job-"+id, "")
		}
	}
	e.update(id, func(d *store.Disc) error { sd := d.Side(letter); sd.Processed, sd.Step = true, "done"; return nil })
	return nil
}

// extractSide runs classification and extraction or carving, then records
// one clip per recording.
func (e *Engine) extractSide(ctx context.Context, d *store.Disc, letter string) error {
	id := d.ID
	s := d.Side(letter)
	img, mp := e.imagePath(id, letter), e.mapPath(id, letter)

	e.setStep(id, letter, "classifying")
	idx := e.beginStage(id, letter, "classify", "Looking for a file system and video folders")
	probe := s.Probe
	listing, lerr := pipeline.ListImage(ctx, e.Tools, img)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	probe.Listed = true
	probe.HasVideoTS, probe.HasRTAV = listing.HasVideoTS, listing.HasRTAV
	if lerr != nil && probe.FSType != "" {
		probe.ListFailed = true
	}
	if lerr == nil && probe.FSType == "" {
		// blkid missed it (or ran on an open session); 7-Zip found files.
		probe.FSType = "udf"
	}
	dec := discinfo.Classify(probe)
	if d.ForceRaw && dec.Class.Path() != discinfo.PathNone && dec.Class.Path() != discinfo.PathReject {
		dec = discinfo.Decision{Class: discinfo.Damaged, Reason: "Raw recovery forced (was: " + dec.Reason + ")"}
	}
	evidence := "$ 7zz l " + filepath.Base(img) + "\n" + listing.Output
	e.endStage(id, letter, idx, "done", nil, "", dec.Reason)
	e.update(id, func(d *store.Disc) error {
		sd := d.Side(letter)
		sd.Probe, sd.Decision = probe, dec
		sd.Evidence = strings.TrimSpace(sd.Evidence + "\n" + evidence)
		return nil
	})

	src := e.sourceDir(id, letter)
	var sources []pipeline.Source
	switch dec.Class.Path() {
	case discinfo.PathFiles, discinfo.PathCopy:
		e.setStep(id, letter, "extracting")
		idx := e.beginStage(id, letter, "extract", "Pulling files from the image")
		_ = os.RemoveAll(src)
		r, err := pipeline.ExtractImage(ctx, e.Tools, img, src)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// The file system is there but won't copy: damaged, recover raw.
			e.endStage(id, letter, idx, "failed", []string{r.CmdLine}, r.Stderr, "File copy failed; falling back to raw recovery")
			e.update(id, func(d *store.Disc) error {
				d.Side(letter).Decision = discinfo.Decision{Class: discinfo.Damaged, Reason: "File system present, but file copy failed; recovering raw"}
				return nil
			})
			sources, err = e.carve(ctx, id, letter, img, mp, src)
			if err != nil {
				return err
			}
			break
		}
		e.endStage(id, letter, idx, "done", []string{r.CmdLine}, r.Stderr, "Files extracted")
		if dec.Class.Path() == discinfo.PathCopy {
			e.update(id, func(d *store.Disc) error { d.Side(letter).Extracted = true; return nil })
			return nil
		}
		sources, err = e.split(ctx, id, letter, src, dec.Class)
		if err != nil {
			e.update(id, func(d *store.Disc) error {
				d.Warnings = append(d.Warnings, fmt.Sprintf("Side %s: splitting failed (%v); recovering raw instead", letter, err))
				return nil
			})
			sources, err = e.carve(ctx, id, letter, img, mp, src)
			if err != nil {
				return err
			}
		}
	case discinfo.PathRaw:
		var err error
		sources, err = e.carve(ctx, id, letter, img, mp, src)
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("%s", dec.Reason)
	}
	if len(sources) == 0 {
		return fmt.Errorf("no recordings found on side %s", letter)
	}

	if err := e.St.DeleteClips(id, letter); err != nil {
		return err
	}
	for i, src := range sources {
		c := &store.Clip{DiscID: id, Side: letter, Num: i + 1, Source: src, State: "pending"}
		if err := e.St.AddClip(c); err != nil {
			return err
		}
	}
	e.update(id, func(d *store.Disc) error { d.Side(letter).Extracted = true; return nil })
	return nil
}

func (e *Engine) split(ctx context.Context, id, letter, src string, class discinfo.Class) ([]pipeline.Source, error) {
	e.setStep(id, letter, "splitting")
	idx := e.beginStage(id, letter, "split", "Finding each recording")
	var sources []pipeline.Source
	var r tools.Result
	var err error
	switch class {
	case discinfo.Finalized:
		vts := pipeline.FindVideoTS(src)
		if vts == "" {
			err = fmt.Errorf("VIDEO_TS missing after extraction")
			break
		}
		sources, err = pipeline.DVDVideoSources(vts, e.Settings().Split)
	default: // DVD-VR on DVD-RAM or DVD-RW
		rtav := pipeline.FindRTAV(src)
		if rtav == "" {
			err = fmt.Errorf("DVD_RTAV missing after extraction")
			break
		}
		sources, r, err = pipeline.DVDVRSources(ctx, e.Tools, rtav, filepath.Join(src, "recordings"))
	}
	status, msg := "done", fmt.Sprintf("%d recordings", len(sources))
	if err != nil {
		status, msg = "failed", err.Error()
	}
	var cmds []string
	if r.CmdLine != "" {
		cmds = []string{r.CmdLine}
	}
	e.endStage(id, letter, idx, status, cmds, r.Stderr, msg)
	return sources, err
}

func (e *Engine) carve(ctx context.Context, id, letter, img, mp, src string) ([]pipeline.Source, error) {
	e.setStep(id, letter, "carving")
	idx := e.beginStage(id, letter, "carve", "Scanning the image for MPEG-2 packs")
	_ = os.MkdirAll(src, 0o755)
	sources, clips, err := pipeline.CarveSources(img, mp, src)
	if err != nil {
		e.endStage(id, letter, idx, "failed", nil, "", err.Error())
		return nil, err
	}
	var bad int64
	for _, c := range clips {
		bad += c.BadSectors
	}
	msg := fmt.Sprintf("%d recordings carved", len(sources))
	if bad > 0 {
		msg += fmt.Sprintf(", %d zero-filled sectors inside them", bad)
	}
	e.endStage(id, letter, idx, "done", []string{"carve " + filepath.Base(img)}, "", msg)
	return sources, nil
}

// convertClip verifies one source and converts it into the work folder.
func (e *Engine) convertClip(ctx context.Context, c *store.Clip, set config.Settings) error {
	id := c.DiscID
	info, err := pipeline.Probe(ctx, e.Tools, c.Source)
	if err != nil {
		return fmt.Errorf("unreadable: %w", err)
	}
	c.Info = info
	problem, warnings := pipeline.Verify(ctx, e.Tools, c.Source, info)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	c.Warnings = warnings
	if problem != "" {
		c.State, c.Error = "failed", "Kept but not converted: "+problem
		return e.St.SaveClip(c)
	}

	// Wait for a conversion slot; imaging the next disc isn't starved.
	c.State = "queued"
	_ = e.St.SaveClip(c)
	e.Hub.Publish("job-"+id, "")
	select {
	case e.convSem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-e.convSem }()

	need := int64(info.Duration * 2e6) // ~16 Mb/s ceiling for CRF 16 at 50p
	for {
		msg := e.needSpace(need)
		if msg == "" {
			break
		}
		e.update(id, func(d *store.Disc) error { d.Message = msg; return nil })
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(30 * time.Second):
		}
	}

	out := filepath.Join(e.workDir(id), "out", fmt.Sprintf("%s%02d%s", c.Side, c.Num, pipeline.Ext(set.Preset)))
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	c.State, c.Progress, c.Error = "converting", 0, ""
	_ = e.St.SaveClip(c)
	e.Hub.Publish("job-"+id, "")
	opts := pipeline.EncodeOptions{Preset: set.Preset, HWAccel: set.HWAccel}
	r, err := pipeline.Convert(ctx, e.Tools, c.Source, info, opts, out, func(f float64) {
		if e.Hub.Throttled(fmt.Sprintf("clip-%d", c.ID), "", 750*time.Millisecond) {
			c.Progress = f
			_ = e.St.SaveClip(c)
			e.Hub.Publish("job-"+id, "")
		}
	})
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		e.Log.Error("convert failed", "disc", id, "clip", c.ID, "cmd", r.CmdLine, "stderr", r.Stderr)
		return fmt.Errorf("conversion failed: %w", err)
	}
	thumb := filepath.Join(e.workDir(id), "thumbs", fmt.Sprintf("%d.jpg", c.ID))
	if err := pipeline.Thumbnail(ctx, e.Tools, out, info.Duration, thumb); err == nil {
		c.Thumb = thumb
	}
	if fi, err := os.Stat(out); err == nil {
		c.Size = fi.Size()
	}
	c.State, c.Progress, c.Work = "converted", 1, out
	if err := e.St.SaveClip(c); err != nil {
		return err
	}
	e.Hub.Publish("job-"+id, "")
	return nil
}
