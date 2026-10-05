package jobs

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/dnaidoo621/camdvd-rescue/internal/config"
	"github.com/dnaidoo621/camdvd-rescue/internal/drive"
	"github.com/dnaidoo621/camdvd-rescue/internal/events"
	"github.com/dnaidoo621/camdvd-rescue/internal/library"
	"github.com/dnaidoo621/camdvd-rescue/internal/store"
	"github.com/dnaidoo621/camdvd-rescue/internal/tools"
)

type harness struct {
	t   *testing.T
	e   *Engine
	im  *drive.ImageDrive
	lib string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	gen, _ := filepath.Abs(filepath.Join("..", "..", "testdata", "gen"))
	if _, err := os.Stat(filepath.Join(gen, "finalized.img")); err != nil {
		t.Skip("fixtures not generated; run scripts/make-test-images.sh")
	}
	ts := tools.Set{BinDir: os.Getenv("CAMDVD_BIN")}
	for _, n := range []string{"ffmpeg", "ffprobe", "exiftool", "blkid"} {
		if _, err := exec.LookPath(ts.Path(n)); err != nil {
			t.Skip(n + " not installed")
		}
	}
	PollInterval = 100 * time.Millisecond
	ProbeSettle = 50 * time.Millisecond
	dir := t.TempDir()
	cfg := config.Defaults()
	cfg.Library = filepath.Join(dir, "library")
	cfg.StateDir = dir
	cfg.MinFreeGB = 0.01
	os.MkdirAll(cfg.Library, 0o755)
	st, err := store.Open(filepath.Join(dir, "camdvd.db"))
	if err != nil {
		t.Fatal(err)
	}
	im := drive.NewImageDrive("demo", gen, ts)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if testing.Verbose() {
		log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	e := New(cfg, st, ts, events.NewHub(), log, []drive.Drive{im})
	set := config.DefaultSettings()
	set.Preset = "small"
	set.Make, set.Model = "Hitachi", "VDR-M50"
	e.SaveSettings(set)
	ctx, cancel := context.WithCancel(context.Background())
	if err := e.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); e.Wait(); st.Close() })
	return &harness{t, e, im, cfg.Library}
}

// waitFor polls until cond holds, failing after a minute.
func (h *harness) waitFor(what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	discs, _ := h.e.St.Discs()
	for _, d := range discs {
		h.t.Logf("disc %s state=%s msg=%q sides=%d", d.ID, d.State, d.Message, len(d.Sides))
		for _, s := range d.Sides {
			h.t.Logf("  side %s imaged=%v processed=%v step=%s err=%s class=%s", s.Letter, s.Imaged, s.Processed, s.Step, s.Error, s.Decision.Class)
		}
	}
	h.t.Fatalf("timed out waiting for %s; drives %+v", what, h.e.Drives())
}

func (h *harness) only() *store.Disc {
	discs, _ := h.e.St.Discs()
	if len(discs) == 0 {
		return nil
	}
	return discs[0]
}

func (h *harness) state(id string) store.State {
	d, err := h.e.St.Disc(id)
	if err != nil {
		return ""
	}
	return d.State
}

func (h *harness) files(folder string) []string {
	ents, _ := os.ReadDir(filepath.Join(h.lib, folder))
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

func TestDoubleSidedUnfinalizedDisc(t *testing.T) {
	h := newHarness(t)
	h.im.Insert("unfinalized-a.img")
	h.waitFor("job", func() bool { return h.only() != nil })
	id := h.only().ID
	h.waitFor("side A imaged", func() bool { return h.state(id) == store.AwaitingAnswer })
	if h.im.Current() == "" {
		t.Fatal("tray ejected before the sides answer")
	}
	if _, err := h.e.Answers(id, 2, "2004-12 Durban holiday"); err != nil {
		t.Fatal(err)
	}
	h.waitFor("flip", func() bool { return h.state(id) == store.AwaitingFlip && h.im.Current() == "" })

	// The same side again is refused.
	h.im.Insert("unfinalized-a.img")
	h.waitFor("same-side notice", func() bool { return strings.Contains(h.e.Drives()[0].Notice, "side A again") })
	h.waitFor("re-eject", func() bool { return h.im.Current() == "" })

	h.im.Insert("unfinalized-b.img")
	h.waitFor("done", func() bool { return h.state(id) == store.Done })
	d, _ := h.e.St.Disc(id)
	if d.Folder != "2004-12 Durban holiday" {
		t.Errorf("folder %q", d.Folder)
	}
	want := []string{"2004-12 Durban holiday - A01.mp4", "2004-12 Durban holiday - A02.mp4", "2004-12 Durban holiday - A03.mp4",
		"2004-12 Durban holiday - B01.mp4", "2004-12 Durban holiday - B02.mp4"}
	if got := h.files(d.Folder); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("files %v", got)
	}
	for _, s := range d.Sides {
		if s.Decision.Class != "unfinalized" {
			t.Errorf("side %s class %s", s.Letter, s.Decision.Class)
		}
		if _, err := os.Stat(h.e.imagePath(id, s.Letter)); err != nil {
			t.Errorf("unfinalized image for side %s should be kept", s.Letter)
		}
	}
	if _, err := os.Stat(filepath.Join(h.e.workDir(id), "disc.json")); err != nil {
		t.Error("manifest missing")
	}
	clips, _ := h.e.St.Clips(id)
	for _, c := range clips {
		if c.State != "placed" || c.SHA256 == "" || c.Thumb == "" {
			t.Errorf("clip %+v", c)
		}
	}
	// No dates on an unfinalized disc: setting a disc date retags in place.
	h.e.update(id, func(d *store.Disc) error { d.DiscDate = "2004-12-24T10:00"; return nil })
	if err := h.e.Retag(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	out, _ := h.e.Tools.Output(context.Background(), "exiftool", "-s3", "-Keys:CreationDate", filepath.Join(h.lib, d.Folder, want[4]))
	if !strings.Contains(out, "2004:12:24 10:04:00+02:00") {
		t.Errorf("B02 creation date %q (want disc date + 4 min)", out)
	}

	// Inserting the same disc again offers "import again?".
	h.im.Insert("unfinalized-a.img")
	h.waitFor("duplicate", func() bool {
		discs, _ := h.e.St.Discs(store.Duplicate)
		return len(discs) == 1
	})
	dups, _ := h.e.St.Discs(store.Duplicate)
	if dups[0].DuplicateOf != id {
		t.Errorf("duplicate of %s", dups[0].DuplicateOf)
	}
	if err := h.e.Cancel(dups[0].ID); err != nil {
		t.Fatal(err)
	}
	h.waitFor("eject after cancel", func() bool { return h.im.Current() == "" })
}

func TestFinalizedSingleSided(t *testing.T) {
	h := newHarness(t)
	set := h.e.Settings()
	set.Batch, set.BatchSides = true, 1
	h.e.SaveSettings(set)
	h.im.Insert("finalized.img")
	h.waitFor("job", func() bool { return h.only() != nil })
	id := h.only().ID
	h.waitFor("done", func() bool { return h.state(id) == store.Done })
	d, _ := h.e.St.Disc(id)
	if d.Side("A").Decision.Class != "finalized" {
		t.Errorf("class %s: %s", d.Side("A").Decision.Class, d.Side("A").Decision.Reason)
	}
	if !strings.HasPrefix(d.Folder, "Imported ") {
		t.Errorf("folder %q", d.Folder)
	}
	files := h.files(d.Folder)
	if len(files) != 4 || !strings.HasSuffix(files[0], " - 01.mp4") {
		t.Errorf("files %v", files)
	}
	if _, err := os.Stat(h.e.imagePath(id, "A")); err == nil {
		t.Error("finalized image should be deleted")
	}
	if _, err := os.Stat(filepath.Join(h.e.sourceDir(id, "A"), "VIDEO_TS", "VIDEO_TS.IFO")); err != nil {
		t.Error("sources should be kept")
	}
}

func TestDamagedDiscIsFlagged(t *testing.T) {
	h := newHarness(t)
	set := h.e.Settings()
	set.Batch, set.BatchSides, set.BatchDesc = true, 1, "Scratched"
	h.e.SaveSettings(set)
	h.im.Insert("damaged.img")
	h.waitFor("job", func() bool { return h.only() != nil })
	id := h.only().ID
	h.waitFor("done", func() bool { return h.state(id) == store.Done })
	d, _ := h.e.St.Disc(id)
	a := d.Side("A")
	if a.Bad.Total() != 40 || a.Readable() > 0.999 {
		t.Errorf("bad %+v readable %.4f", a.Bad, a.Readable())
	}
	found := false
	for _, w := range d.Warnings {
		if strings.Contains(w, "40 unreadable sectors") {
			found = true
		}
	}
	if !found {
		t.Errorf("warnings %v", d.Warnings)
	}
}

func TestBlankDiscEjectsWithoutJob(t *testing.T) {
	h := newHarness(t)
	h.im.Insert("blank.img")
	h.waitFor("eject", func() bool { return h.im.Current() == "" })
	if h.only() != nil {
		t.Error("blank disc created a job")
	}
	if n := h.e.Drives()[0].Notice; !strings.Contains(n, "blank") {
		t.Errorf("notice %q", n)
	}
}

func TestCancelAndResumeImaging(t *testing.T) {
	h := newHarness(t)
	h.im.Insert("unfinalized-a.img")
	h.waitFor("job", func() bool { return h.only() != nil })
	id := h.only().ID
	h.waitFor("imaging", func() bool { return h.state(id) == store.ImagingA || h.state(id) == store.AwaitingAnswer })
	if err := h.e.Cancel(id); err != nil {
		t.Fatal(err)
	}
	h.waitFor("ejected", func() bool { return h.im.Current() == "" && !h.e.Running(id) })
	if err := h.e.Resume(id); err != nil {
		t.Fatal(err)
	}
	d, _ := h.e.St.Disc(id)
	if !d.Side("A").Imaged && d.State != store.Paused {
		t.Errorf("state %s", d.State)
	}
	if !d.Side("A").Imaged {
		h.im.Insert("unfinalized-a.img")
	}
	h.e.Answers(id, 1, "")
	h.waitFor("done", func() bool { return h.state(id) == store.Done })
}

func TestLibraryRenameUndoAndRescan(t *testing.T) {
	h := newHarness(t)
	set := h.e.Settings()
	set.Batch, set.BatchSides, set.BatchDesc = true, 1, "Durban"
	h.e.SaveSettings(set)
	h.im.Insert("unfinalized-a.img")
	h.waitFor("job", func() bool { return h.only() != nil })
	id := h.only().ID
	h.waitFor("done", func() bool { return h.state(id) == store.Done })

	pv, err := h.e.PreviewRename(Selection{FolderFiles: []string{id}, Folders: []string{id}}, library.Pattern{Template: "Beach {n:00}"})
	if err != nil {
		t.Fatal(err)
	}
	if !pv.Clean || pv.Changes != 4 {
		t.Fatalf("preview %+v", pv)
	}
	b, err := h.e.ApplyRename(pv.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Files got "Beach 01..03", the folder "Beach 04".
	d, _ := h.e.St.Disc(id)
	if d.Folder != "Beach 04" {
		t.Errorf("folder %q", d.Folder)
	}
	if got := h.files("Beach 04"); strings.Join(got, "|") != "Beach 01.mp4|Beach 02.mp4|Beach 03.mp4" {
		t.Errorf("files %v", got)
	}
	clips, _ := h.e.St.Clips(id)
	if clips[0].Output != filepath.Join("Beach 04", "Beach 01.mp4") {
		t.Errorf("clip output %q", clips[0].Output)
	}
	if err := h.e.UndoRename(b.ID); err != nil {
		t.Fatal(err)
	}
	d, _ = h.e.St.Disc(id)
	if d.Folder != "Durban" || len(h.files("Durban")) != 3 {
		t.Errorf("undo: folder %q files %v", d.Folder, h.files("Durban"))
	}

	// Rename outside the app, as over SMB, then rescan.
	os.Rename(filepath.Join(h.lib, "Durban"), filepath.Join(h.lib, "Durban 2004"))
	os.Rename(filepath.Join(h.lib, "Durban 2004", "Durban - 02.mp4"), filepath.Join(h.lib, "Durban 2004", "best bit.mp4"))
	os.WriteFile(filepath.Join(h.lib, "Durban 2004", "notes.txt"), []byte("x"), 0o644)
	rep, err := h.e.Rescan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Relinked) != 3 || len(rep.Missing) != 0 || len(rep.Unknown) != 1 {
		t.Fatalf("rescan %+v", rep)
	}
	d, _ = h.e.St.Disc(id)
	c2, _ := h.e.St.Clip(clips[1].ID)
	if d.Folder != "Durban 2004" || c2.Output != filepath.Join("Durban 2004", "best bit.mp4") {
		t.Errorf("after rescan folder %q clip %q", d.Folder, c2.Output)
	}
}

func TestDataDiscCopiesFiles(t *testing.T) {
	h := newHarness(t)
	set := h.e.Settings()
	set.Batch, set.BatchSides, set.BatchDesc = true, 1, "Tax papers"
	h.e.SaveSettings(set)
	h.im.Insert("data.img")
	h.waitFor("job", func() bool { return h.only() != nil })
	id := h.only().ID
	h.waitFor("done", func() bool { return h.state(id) == store.Done })
	d, _ := h.e.St.Disc(id)
	if d.Side("A").Decision.Class != "data" {
		t.Errorf("class %s", d.Side("A").Decision.Class)
	}
	if b, err := os.ReadFile(filepath.Join(h.lib, "Tax papers", "Documents", "tax.txt")); err != nil || !strings.Contains(string(b), "tax 2004") {
		t.Errorf("copied file: %q %v", b, err)
	}
}

func TestAddAnotherSide(t *testing.T) {
	h := newHarness(t)
	set := h.e.Settings()
	set.Batch, set.BatchSides, set.BatchDesc = true, 1, "Picnic"
	h.e.SaveSettings(set)
	h.im.Insert("unfinalized-a.img")
	h.waitFor("job", func() bool { return h.only() != nil })
	id := h.only().ID
	h.waitFor("done", func() bool { return h.state(id) == store.Done })
	h.waitFor("ejected", func() bool { return h.im.Current() == "" })
	if err := h.e.AddSide(id); err != nil {
		t.Fatal(err)
	}
	if h.state(id) != store.AwaitingFlip {
		t.Fatalf("state %s", h.state(id))
	}
	h.im.Insert("unfinalized-b.img")
	h.waitFor("done again", func() bool {
		d, _ := h.e.St.Disc(id)
		return d.State == store.Done && len(d.Sides) == 2
	})
	files := h.files("Picnic")
	if len(files) != 5 || files[3] != "Picnic - B01.mp4" {
		t.Errorf("files %v", files)
	}
}

// slowImage makes a large unfinalized image so imaging takes long enough
// to pull the disc out halfway.
func slowImage(t *testing.T) string {
	dir := t.TempDir()
	src, _ := filepath.Abs(filepath.Join("..", "..", "testdata", "gen", "unfinalized-a.img"))
	b, _ := os.ReadFile(src)
	p := filepath.Join(dir, "big.img")
	f, _ := os.Create(p)
	for range 40 { // ~1.1 GB of repeated recordings
		f.Write(b)
	}
	fi, _ := f.Stat()
	f.Close()
	os.WriteFile(p+".media.json", []byte(fmt.Sprintf(`{"type":"DVD-R Sequential","disc_status":"appendable","session_state":"incomplete","next_writable":%d}`, fi.Size()/2048)), 0o644)
	return p
}

func TestDiscRemovedMidReadResumes(t *testing.T) {
	h := newHarness(t)
	img := slowImage(t)
	h.im.InsertPath(img)
	h.waitFor("job", func() bool { return h.only() != nil })
	id := h.only().ID
	h.waitFor("some progress", func() bool {
		d, _ := h.e.St.Disc(id)
		return d != nil && d.Side("A").ImageDone > 0.05
	})
	h.im.Eject(context.Background())
	h.waitFor("paused", func() bool { return h.state(id) == store.Paused })
	d, _ := h.e.St.Disc(id)
	if !strings.Contains(d.Message, "removed") {
		t.Errorf("message %q", d.Message)
	}
	h.im.InsertPath(img)
	h.waitFor("resumed imaging", func() bool { s := h.state(id); return s == store.ImagingA || s == store.AwaitingAnswer })
	h.e.Cancel(id) // converting 120 copies of each clip would take a while
	stages := 0
	d, _ = h.e.St.Disc(id)
	for _, s := range d.Side("A").Stages {
		if s.Name == "image" {
			stages++
		}
	}
	if stages != 2 {
		t.Errorf("image stages %d, want 2 (interrupted + resumed)", stages)
	}
}

// The track layout of a real unfinalized camcorder DVD-R: only the written
// tracks are read, so the gap before the video doesn't end imaging early,
// and nothing unwritten counts as damage.
func TestCamcorderTrackLayout(t *testing.T) {
	h := newHarness(t)
	set := h.e.Settings()
	set.Batch, set.BatchSides, set.BatchDesc = true, 1, "Mauritius 2"
	h.e.SaveSettings(set)
	h.im.Insert("unfinalized-tracks.img")
	h.waitFor("job", func() bool { return h.only() != nil })
	id := h.only().ID
	h.waitFor("done", func() bool { return h.state(id) == store.Done })
	d, _ := h.e.St.Disc(id)
	a := d.Side("A")
	if a.StoppedAt != 0 || a.Bad.Total() != 0 {
		t.Errorf("stopped at %d, bad %+v", a.StoppedAt, a.Bad)
	}
	if got := h.files("Mauritius 2"); len(got) != 3 {
		t.Errorf("files %v", got)
	}
	// The middle recording spans a clock reset: one clip of both parts.
	clips, _ := h.e.St.Clips(id)
	if len(clips) == 3 {
		if d := clips[1].Info.Duration; d < 7.5 || d > 9.5 { // clock span includes each segment's lead-in
			t.Errorf("long recording lasts %.1f s, want ~8", d)
		}
	}
}

func TestReprocessFromSavedImage(t *testing.T) {
	h := newHarness(t)
	set := h.e.Settings()
	set.Batch, set.BatchSides, set.BatchDesc = true, 1, "Again"
	h.e.SaveSettings(set)
	h.im.Insert("unfinalized-a.img")
	h.waitFor("job", func() bool { return h.only() != nil })
	id := h.only().ID
	h.waitFor("processing", func() bool { return h.state(id) == store.Processing })
	if err := h.e.Reprocess(id); err == nil {
		t.Fatal("reprocess allowed while running")
	}
	h.e.Cancel(id)
	h.waitFor("stopped", func() bool { return !h.e.Running(id) })
	// An earlier run's failed listing must not stick to the reprocessed side.
	h.e.update(id, func(d *store.Disc) error { d.Side("A").Probe.ListFailed = true; return nil })
	if err := h.e.Reprocess(id); err != nil {
		t.Fatal(err)
	}
	h.waitFor("done", func() bool { return h.state(id) == store.Done })
	if got := h.files("Again"); len(got) != 3 {
		t.Errorf("files %v", got)
	}
	if d, _ := h.e.St.Disc(id); d.Side("A").Probe.ListFailed || d.Side("A").Decision.Class != "unfinalized" {
		t.Errorf("stale verdict kept: %+v", d.Side("A").Decision)
	}
}

func TestOverview(t *testing.T) {
	h := newHarness(t)
	set := h.e.Settings()
	set.Batch, set.BatchSides, set.BatchDesc = true, 1, "Overview"
	h.e.SaveSettings(set)
	h.im.Insert("unfinalized-a.img")
	h.waitFor("job", func() bool { return h.only() != nil })
	id := h.only().ID
	h.waitFor("clips queued", func() bool {
		o := h.e.Overview()
		return len(o.Discs) == 1 && o.ClipsLeft > 0 && o.MinutesLeft > 0
	})
	if o := h.e.Overview(); o.ETA.IsZero() || o.Settings != "small quality, balanced speed" {
		t.Errorf("overview %+v", o)
	}
	h.waitFor("done", func() bool { return h.state(id) == store.Done })
	o := h.e.Overview()
	if len(o.Discs) != 0 || o.ClipsLeft != 0 || o.DoneToday != 1 || !o.Measured || o.Speed <= 0 {
		t.Errorf("after done: %+v", o)
	}

	// A disc left "processing" with clips to go but no work running is stuck.
	h.e.update(id, func(d *store.Disc) error { d.State = store.Processing; return nil })
	clips, _ := h.e.St.Clips(id)
	clips[0].State = "pending"
	h.e.St.SaveClip(clips[0])
	if o := h.e.Overview(); len(o.Stuck) != 1 || !strings.Contains(o.Stuck[0], "Press Resume") {
		t.Errorf("stuck %v", o.Stuck)
	}
}
