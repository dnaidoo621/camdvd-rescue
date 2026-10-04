package jobs

import (
	"context"
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
