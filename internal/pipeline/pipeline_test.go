package pipeline

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dnaidoo621/camdvd-rescue/internal/tools"
)

func TestParseDVDVRDates(t *testing.T) {
	out := `format: DVD-VR V1.1

Number of programs: 3

num  : 1
title:
date : 2004-12-24 14:30:00
size : 123

num  : 2
date : not set

num  : 3
date : 2004-12-25 09:05:10
`
	d := ParseDVDVRDates(out)
	if len(d) != 2 || d[1] != time.Date(2004, 12, 24, 14, 30, 0, 0, time.UTC) || d[3].Hour() != 9 {
		t.Fatalf("got %v", d)
	}
}

func TestDateRules(t *testing.T) {
	jhb, _ := time.LoadLocation("Africa/Johannesburg")
	r := DateRules{Zone: jhb, ResetDates: []string{"2004-01-01"}, Now: time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)}
	recs := []time.Time{
		time.Date(2004, 12, 24, 14, 30, 0, 0, time.UTC),
		time.Date(2004, 1, 1, 0, 3, 0, 0, time.UTC), // reset clock
		{},
	}
	got := ResolveDates(r, recs, time.Time{})
	if got[0].Source != "disc" || got[0].When.Format(time.RFC3339) != "2004-12-24T14:30:00+02:00" {
		t.Errorf("clip 1: %+v", got[0])
	}
	if got[1].Source != "none" || !strings.Contains(got[1].Flag, "reset default") {
		t.Errorf("clip 2: %+v", got[1])
	}
	disc := time.Date(2004, 12, 26, 10, 0, 0, 0, time.UTC)
	got = ResolveDates(r, recs, disc)
	if got[1].Source != "disc-date" || got[2].When.Sub(got[1].When) != time.Minute {
		t.Errorf("disc date spacing: %+v", got)
	}
	if ok, _ := r.Sane(time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)); ok {
		t.Error("future date accepted")
	}
	if ok, _ := r.Sane(time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC)); ok {
		t.Error("1990 accepted")
	}
}

func TestConvertArgs(t *testing.T) {
	s := Source{Kind: "dvdvideo", Path: "/x/VIDEO_TS", Title: 1, Chapter: 2}
	in := Info{FieldOrder: "tt", Duration: 10}
	a := ConvertArgs(s, in, EncodeOptions{Preset: PresetStandard}, "/o.mp4")
	j := strings.Join(a, " ")
	for _, want := range []string{"-f dvdvideo", "-chapter_start 2 -chapter_end 2", "bwdif=mode=send_field", "-crf 18", "-preset slow",
		"-profile:v high", "-level:v 4.1", "-c:a aac -b:a 192k", "+faststart", "/o.mp4"} {
		if !strings.Contains(j, want) {
			t.Errorf("missing %q in %s", want, j)
		}
	}
	a = ConvertArgs(Source{Kind: "file", Path: "/a.mpg"}, Info{FieldOrder: "progressive"}, EncodeOptions{Preset: PresetCopy}, "/o.mkv")
	j = strings.Join(a, " ")
	if strings.Contains(j, "bwdif") || !strings.Contains(j, "-c copy -f matroska") {
		t.Errorf("copy preset: %s", j)
	}
	a = ConvertArgs(Source{Kind: "file", Path: "/a.mpg"}, in, EncodeOptions{Preset: PresetSmall, HWAccel: "vaapi"}, "/o.mp4")
	if !slices.Contains(a, "h264_vaapi") || !slices.Contains(a, "-vaapi_device") {
		t.Errorf("vaapi: %v", a)
	}
}

func TestTagArgs(t *testing.T) {
	jhb, _ := time.LoadLocation("Africa/Johannesburg")
	a := TagArgs(Tags{When: time.Date(2004, 12, 24, 14, 30, 0, 0, jhb), Make: "Hitachi", Model: "VDR-M50", Comment: "d-1 A 1"}, "/f.mp4")
	j := strings.Join(a, "\n")
	for _, want := range []string{"-Keys:CreationDate=2004:12:24 14:30:00+02:00", "-QuickTime:CreateDate=2004:12:24 12:30:00",
		"-QuickTime:MediaCreateDate=2004:12:24 12:30:00", "-FileModifyDate=2004:12:24 14:30:00+02:00", "-Keys:Model=VDR-M50", "-Keys:Comment=d-1 A 1"} {
		if !strings.Contains(j, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(j, "Location") {
		t.Error("empty location written")
	}
}

// fixtures returns testdata/gen, skipping when it hasn't been generated or
// the media tools are missing.
func fixtures(t *testing.T) (string, tools.Set) {
	t.Helper()
	dir, _ := filepath.Abs(filepath.Join("..", "..", "testdata", "gen"))
	if _, err := os.Stat(filepath.Join(dir, "finalized.img")); err != nil {
		t.Skip("fixtures not generated; run scripts/make-test-images.sh")
	}
	ts := tools.Set{BinDir: os.Getenv("CAMDVD_BIN")}
	for _, n := range []string{"ffmpeg", "ffprobe", "exiftool"} {
		if _, err := exec.LookPath(ts.Path(n)); err != nil {
			t.Skip(n + " not installed")
		}
	}
	return dir, ts
}

func TestFinalizedEndToEnd(t *testing.T) {
	dir, ts := fixtures(t)
	ctx := context.Background()
	l, err := ListImage(ctx, ts, filepath.Join(dir, "finalized.img"))
	if err != nil || !l.HasVideoTS || l.HasRTAV {
		t.Fatalf("listing %+v: %v", l, err)
	}
	src := t.TempDir()
	if _, err := ExtractImage(ctx, ts, filepath.Join(dir, "finalized.img"), src); err != nil {
		t.Fatal(err)
	}
	srcs, err := DVDVideoSources(FindVideoTS(src), "chapter")
	if err != nil || len(srcs) != 4 {
		t.Fatalf("%d sources: %v", len(srcs), err)
	}
	for i, s := range srcs {
		in, err := Probe(ctx, ts, s)
		if err != nil {
			t.Fatalf("probe %s: %v", s.Label, err)
		}
		if !in.HasVideo || in.Duration < 3 || in.Duration > 5.5 {
			t.Errorf("%s: %+v", s.Label, in)
		}
		if p, w := Verify(ctx, ts, s, in); p != "" || len(w) > 0 {
			t.Errorf("%s: problem %q warnings %q", s.Label, p, w)
		}
		if i == 2 { // rec3 is 16:9 interlaced
			if !in.Interlaced() || in.DAR != "16:9" {
				t.Errorf("rec3 info %+v", in)
			}
			out := filepath.Join(t.TempDir(), "c.mp4")
			var last float64
			if _, err := Convert(ctx, ts, s, in, EncodeOptions{Preset: PresetSmall}, out, func(f float64) { last = f }); err != nil {
				t.Fatal(err)
			}
			got, err := Probe(ctx, ts, Source{Kind: "file", Path: out})
			if err != nil || got.VideoCodec != "h264" || got.AudioCodec != "aac" || got.DAR != "16:9" || got.FrameRate != "50/1" {
				t.Fatalf("output %+v: %v", got, err)
			}
			if last < 0.9 {
				t.Errorf("progress ended at %.2f", last)
			}
			jhb, _ := time.LoadLocation("Africa/Johannesburg")
			if _, err := Tag(ctx, ts, Tags{When: time.Date(2004, 12, 24, 14, 30, 0, 0, jhb), Make: "Hitachi", Model: "VDR-M50", Comment: "d-test A 3"}, out); err != nil {
				t.Fatal(err)
			}
			o, _ := ts.Output(ctx, "exiftool", "-s", "-Keys:CreationDate", "-QuickTime:CreateDate", "-Keys:Model", out)
			if !strings.Contains(o, "2004:12:24 14:30:00+02:00") || !strings.Contains(o, "2004:12:24 12:30:00") || !strings.Contains(o, "VDR-M50") {
				t.Errorf("tags:\n%s", o)
			}
			if c := ReadComment(ctx, ts, out); c != "d-test A 3" {
				t.Errorf("comment %q", c)
			}
		}
	}
}

func TestCarveUnfinalizedFixture(t *testing.T) {
	dir, ts := fixtures(t)
	ctx := context.Background()
	out := t.TempDir()
	srcs, clips, err := CarveSources(filepath.Join(dir, "unfinalized-a.img"), filepath.Join(out, "none.map"), out)
	if err != nil {
		t.Fatal(err)
	}
	if len(srcs) != 3 {
		t.Fatalf("carved %d clips, want 3: %+v", len(clips), clips)
	}
	for _, s := range srcs {
		in, err := Probe(ctx, ts, s)
		if err != nil || !in.HasVideo || in.Duration < 3 {
			t.Errorf("%s: %+v %v", s.Label, in, err)
		}
	}
}
