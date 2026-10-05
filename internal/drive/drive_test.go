package drive

import (
	"bytes"
	"context"
	"errors"
	"github.com/dnaidoo621/camdvd-rescue/internal/tools"
	"os"
	"path/filepath"
	"testing"
)

func TestMapSetMergesAndSplits(t *testing.T) {
	m := &Map{}
	m.Set(0, 100, NonTried)
	m.Set(10, 20, Finished)
	m.Set(30, 10, Finished)
	m.Set(50, 5, BadSector)
	want := []Block{{0, 10, '?'}, {10, 30, '+'}, {40, 10, '?'}, {50, 5, '-'}, {55, 45, '?'}}
	if len(m.Blocks) != len(want) {
		t.Fatalf("got %+v", m.Blocks)
	}
	for i := range want {
		if m.Blocks[i] != want[i] {
			t.Fatalf("block %d: got %+v want %+v", i, m.Blocks[i], want[i])
		}
	}
	if m.Status(52) != BadSector || m.Status(99) != NonTried || m.Status(15) != Finished {
		t.Error("Status lookup wrong")
	}
	p := filepath.Join(t.TempDir(), "x.map")
	if err := m.Write(p); err != nil {
		t.Fatal(err)
	}
	m2, err := ReadMap(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(m2.Blocks) != len(want) || m2.Blocks[3] != want[3] {
		t.Fatalf("round trip %+v", m2.Blocks)
	}
}

func TestReadsRealDdrescueMap(t *testing.T) {
	p := filepath.Join(t.TempDir(), "d.map")
	os.WriteFile(p, []byte(`# Mapfile. Created by GNU ddrescue version 1.26
# Command line: ddrescue -b 2048 -n /dev/sr0 a.img a.map
# Start time:   2026-10-04 12:00:00
# current_pos  current_status  current_pass
0x00010000     +               1
#      pos        size  status
0x00000000  0x00010000  +
0x00010000  0x00001000  -
0x00011000  0x0000F000  +
`), 0o644)
	m, err := ReadMap(p)
	if err != nil {
		t.Fatal(err)
	}
	bad := m.BadSectors(0x20000)
	if len(bad) != 1 || bad[0] != (Range{32, 34}) {
		t.Fatalf("bad %+v", bad)
	}
}

func makeImage(t *testing.T, sectors int) (string, []byte) {
	t.Helper()
	b := make([]byte, sectors*SectorSize)
	for i := range sectors {
		copy(b[i*SectorSize:], []byte{0, 0, 1, 0xBA, byte(i), byte(i >> 8)})
	}
	p := filepath.Join(t.TempDir(), "disc.img")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p, b
}

func reader(b []byte, bad Ranges) func(context.Context, int64, int64) ([]byte, error) {
	return func(_ context.Context, lba, n int64) ([]byte, error) {
		for s := lba; s < lba+n; s++ {
			if bad.Contains(s) {
				return nil, errors.New("medium error")
			}
		}
		return append([]byte(nil), b[lba*SectorSize:(lba+n)*SectorSize]...), nil
	}
}

func TestCopyLoopZeroFillsBadSectors(t *testing.T) {
	_, src := makeImage(t, 2000)
	dir := t.TempDir()
	out, mp := filepath.Join(dir, "a.img"), filepath.Join(dir, "a.map")
	bad := Ranges{{700, 703}, {1500, 1501}}
	var last Progress
	res, err := copyLoop(context.Background(), reader(src, bad), 2000, nil, out, mp, 0, func(p Progress) { last = p })
	if err != nil {
		t.Fatal(err)
	}
	if res.Bad.Total() != 4 || len(res.Bad) != 2 || res.Bad[0] != (Range{700, 703}) {
		t.Fatalf("bad ranges %+v", res.Bad)
	}
	got, _ := os.ReadFile(out)
	if len(got) != len(src) {
		t.Fatalf("size %d", len(got))
	}
	if !bytes.Equal(got[701*SectorSize:702*SectorSize], make([]byte, SectorSize)) {
		t.Error("bad sector not zero-filled")
	}
	if !bytes.Equal(got[:700*SectorSize], src[:700*SectorSize]) || !bytes.Equal(got[703*SectorSize:1500*SectorSize], src[703*SectorSize:1500*SectorSize]) {
		t.Error("good data differs")
	}
	if last.Done != last.Total || last.Bad != 4*SectorSize {
		t.Errorf("progress %+v", last)
	}
}

func TestCopyLoopStopsAtUnwrittenTail(t *testing.T) {
	_, src := makeImage(t, 10000)
	dir := t.TempDir()
	res, err := copyLoop(context.Background(), reader(src, Ranges{{100, 101}, {5000, 10000}}), 10000, nil,
		filepath.Join(dir, "a.img"), filepath.Join(dir, "a.map"), 4096, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.StoppedAt != 5000 {
		t.Fatalf("stopped at %d", res.StoppedAt)
	}
	if res.Bad.Total() != 1 {
		t.Fatalf("bad %+v (the tail must not count as bad)", res.Bad)
	}
}

func TestCopyLoopResumes(t *testing.T) {
	_, src := makeImage(t, 3000)
	dir := t.TempDir()
	out, mp := filepath.Join(dir, "a.img"), filepath.Join(dir, "a.map")
	ctx, cancel := context.WithCancel(context.Background())
	reads := 0
	read := func(c context.Context, lba, n int64) ([]byte, error) {
		reads++
		if lba >= 1024 {
			cancel()
		}
		return reader(src, nil)(c, lba, n)
	}
	if _, err := copyLoop(ctx, read, 3000, nil, out, mp, 0, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("want cancel, got %v", err)
	}
	var firstLBA int64 = -1
	read2 := func(c context.Context, lba, n int64) ([]byte, error) {
		if firstLBA < 0 {
			firstLBA = lba
		}
		return reader(src, nil)(c, lba, n)
	}
	if _, err := copyLoop(context.Background(), read2, 3000, nil, out, mp, 0, nil); err != nil {
		t.Fatal(err)
	}
	if firstLBA < 1024 {
		t.Errorf("resume started at %d, should skip finished sectors", firstLBA)
	}
	got, _ := os.ReadFile(out)
	if !bytes.Equal(got, src) {
		t.Error("resumed image differs from source")
	}
}

func TestParseLsscsi(t *testing.T) {
	out := `[0:0:0:0]    disk    ATA      Kingmax SSD 120G 5.0   /dev/sda   /dev/sg0
[1:0:0:0]    cd/dvd  HL-DT-ST DVDRAM GUC0N     AS01  /dev/sr0   /dev/sg1
[6:0:0:0]    cd/dvd  Sony     DRX-S90U         1.00  /dev/sr1   /dev/sg2
`
	ds := ParseLsscsi(out)
	if len(ds) != 2 || ds[0].Block != "/dev/sr0" || ds[0].SG != "/dev/sg1" || ds[1].SG != "/dev/sg2" {
		t.Fatalf("got %+v", ds)
	}
	if ds[0].Model != "HL-DT-ST DVDRAM GUC0N" {
		t.Errorf("model %q", ds[0].Model)
	}
}

// Unwritten gaps between camcorder tracks must be skipped, not read until
// the "unwritten tail" heuristic gives up before the video.
func TestCopyLoopSkipsGapsBetweenExtents(t *testing.T) {
	_, src := makeImage(t, 12000)
	dir := t.TempDir()
	unwritten := Ranges{{0, 528}, {600, 6000}, {6064, 6080}, {11000, 12000}}
	var reads int
	read := func(c context.Context, lba, n int64) ([]byte, error) {
		reads++
		return reader(src, unwritten)(c, lba, n)
	}
	exts := []Range{{528, 600}, {6000, 6064}, {6080, 11000}}
	res, err := copyLoop(context.Background(), read, 12000, exts, filepath.Join(dir, "a.img"), filepath.Join(dir, "a.map"), 4096, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.StoppedAt != 0 || res.Bad.Total() != 0 {
		t.Fatalf("stopped at %d, bad %+v", res.StoppedAt, res.Bad)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "a.img"))
	if !bytes.Equal(got[6080*SectorSize:11000*SectorSize], src[6080*SectorSize:11000*SectorSize]) {
		t.Error("video track not copied")
	}
	if reads > 40 {
		t.Errorf("%d reads; gaps should not be read at all", reads)
	}
}

func TestBlocksPerTransfer(t *testing.T) {
	dir := t.TempDir()
	if got := BlocksPerTransfer(dir); got != 32 {
		t.Errorf("unknown limit: %d", got)
	}
	for kb, want := range map[string]int64{"120": 60, "512": 64, "1280\n": 64, "1": 32, "junk": 32} {
		os.WriteFile(filepath.Join(dir, "max_hw_sectors_kb"), []byte(kb), 0o644)
		if got := BlocksPerTransfer(dir); got != want {
			t.Errorf("max_hw_sectors_kb=%q: %d, want %d", kb, got, want)
		}
	}
}

// A recording cut off mid-write: the last ~1600 sectors of the video are
// unreadable, each failed read costing the drive seconds. The passes must
// find the edges with a handful of failed reads, not try every sector.
func TestCopyLoopDamagedTailIsCheap(t *testing.T) {
	_, src := makeImage(t, 20000)
	dir := t.TempDir()
	bad := Ranges{{18300, 19900}}
	var failed int
	read := func(c context.Context, lba, n int64) ([]byte, error) {
		b, err := reader(src, bad)(c, lba, n)
		if err != nil {
			failed++
		}
		return b, err
	}
	res, err := copyLoop(context.Background(), read, 20000, []Range{{0, 19900}}, filepath.Join(dir, "a.img"), filepath.Join(dir, "a.map"), 4096, nil)
	if err != nil {
		t.Fatal(err)
	}
	if failed > 14 { // 4 copy + 2 trim + 1 trailing edge + 7 sweep probes
		t.Errorf("%d failed reads; the dead area should be skipped, not scraped", failed)
	}
	if res.Bad.Total() != 1600 || res.Bad[0] != (Range{18300, 19900}) {
		t.Errorf("bad %+v", res.Bad)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "a.img"))
	if !bytes.Equal(got[:18300*SectorSize], src[:18300*SectorSize]) {
		t.Error("readable data before the damage was lost (edges not trimmed)")
	}
}

func TestCopyLoopTrimsAroundAHoleInsideAChunk(t *testing.T) {
	_, src := makeImage(t, 4096)
	dir := t.TempDir()
	res, err := copyLoop(context.Background(), reader(src, Ranges{{1000, 1100}}), 4096, nil,
		filepath.Join(dir, "a.img"), filepath.Join(dir, "a.map"), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Bad.Total() != 100 {
		t.Errorf("bad %+v", res.Bad)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "a.img"))
	// The hole spans chunks 512-1024 and 1024-1536; trimming recovers the
	// readable parts of both chunks around it.
	for _, r := range []Range{{512, 1000}, {1100, 1536}} {
		if !bytes.Equal(got[r.Start*SectorSize:r.End*SectorSize], src[r.Start*SectorSize:r.End*SectorSize]) {
			t.Errorf("sectors %d-%d not recovered", r.Start, r.End)
		}
	}
}

func TestFingerprintSurvivesUnreadableTail(t *testing.T) {
	p, _ := makeImage(t, 6000)
	dir := filepath.Dir(p)
	media := `{"type":"DVD-R Sequential","disc_status":"appendable","session_state":"incomplete","next_writable":6000,
	  "tracks":[{"state":"incomplete incremental","start":100,"size":5900,"next_writable":6000,"last_recorded":5999}]}`
	os.WriteFile(p+".media.json", []byte(media), 0o644)
	d := NewImageDrive("t", dir, tools.Set{})
	if err := d.Insert("disc.img"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	probe, _, _ := d.Probe(ctx)

	os.WriteFile(p+".bad.json", []byte(`[{"start":5000,"end":6000}]`), 0o644)
	fp, err := Fingerprint(ctx, d, probe)
	if err != nil || fp == "" || fp[0] == 'm' {
		t.Fatalf("stepping back should find readable data: %q %v", fp, err)
	}
	os.WriteFile(p+".bad.json", []byte(`[{"start":0,"end":6000}]`), 0o644)
	fp2, err := Fingerprint(ctx, d, probe)
	if err != nil || fp2 == "" || fp2[0] != 'm' {
		t.Fatalf("unreadable disc should get a track-table fingerprint: %q %v", fp2, err)
	}
}
