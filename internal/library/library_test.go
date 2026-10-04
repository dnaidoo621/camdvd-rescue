package library

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSanitize(t *testing.T) {
	cases := map[string]string{
		"2004-12 Durban holiday":       "2004-12 Durban holiday",
		"  a/b\\c:d  ":                 "a-b-c-d",
		"tab\there\nnew":               "tab here new",
		"..hidden.":                    "hidden",
		"bell\x07":                     "bell",
		strings.Repeat("é", 200) + "x": strings.Repeat("é", 120),
	}
	for in, want := range cases {
		if got := Sanitize(in); got != want {
			t.Errorf("Sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFolderAndFileNames(t *testing.T) {
	imp := time.Date(2026, 10, 4, 14, 32, 0, 0, time.UTC)
	rec := time.Date(2004, 12, 24, 14, 30, 0, 0, time.UTC)
	if got := FolderName("2004-12 Durban holiday", rec, imp); got != "2004-12 Durban holiday" {
		t.Error(got)
	}
	if got := FolderName("", rec, imp); got != "2004-12-24" {
		t.Error(got)
	}
	if got := FolderName("  ", time.Time{}, imp); got != "Imported 2026-10-04 1432" {
		t.Error(got)
	}
	if got := FileStem("Durban holiday", "x", "B", 1, false); got != "Durban holiday - B01" {
		t.Error(got)
	}
	if got := FileStem("", "2004-12-24", "A", 3, true); got != "2004-12-24 - 03" {
		t.Error(got)
	}
}

func TestUnique(t *testing.T) {
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "Holiday"), 0o755)
	if got := Unique(dir, "Holiday", "", nil); got != "Holiday (2)" {
		t.Error(got)
	}
	if got := Unique(dir, "Holiday", "", map[string]bool{"holiday (2)": true}); got != "Holiday (3)" {
		t.Error(got)
	}
}

func TestWithinRefusesEscapes(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	os.Symlink(outside, filepath.Join(root, "link"))
	for _, rel := range []string{"../x", "a/../../x", "link/x", "/etc/passwd"} {
		if _, err := Within(root, rel); err == nil {
			t.Errorf("%q accepted", rel)
		}
	}
	if _, err := Within(root, "Disc/new/file.mp4"); err != nil {
		t.Error(err)
	}
}

func items(dir string, names ...string) []Item {
	var its []Item
	for i, n := range names {
		os.WriteFile(filepath.Join(dir, n), []byte(n), 0o644)
		its = append(its, Item{Kind: "file", ID: n, Dir: dir, Name: n, Desc: "Durban", Side: "A",
			Date: time.Date(2004, 12, 24, 14, 30+i, 0, 0, time.UTC), Duration: 192})
	}
	return its
}

func TestPlanTokens(t *testing.T) {
	dir := t.TempDir()
	its := items(dir, "a.mp4", "b.mp4")
	ops, err := Plan(its, Pattern{Template: "{date:yyyy-MM-dd} {time} {desc} {side}{n:000} {duration} ({orig})", Start: 5, Step: 5})
	if err != nil {
		t.Fatal(err)
	}
	if ops[0].NewName != "2004-12-24 14-30 Durban A005 03m12s (a).mp4" || ops[1].NewName != "2004-12-24 14-31 Durban A010 03m12s (b).mp4" {
		t.Fatalf("got %q, %q", ops[0].NewName, ops[1].NewName)
	}
	if _, err := Plan(its, Pattern{Template: "{bogus}"}); err == nil {
		t.Error("unknown token accepted")
	}
}

func TestPlanFindReplaceAndCase(t *testing.T) {
	dir := t.TempDir()
	its := items(dir, "Durban Holiday - A01.mp4", "Durban Holiday - A02.mp4")
	ops, _ := Plan(its, Pattern{Find: `Holiday - A(\d+)`, Replace: "trip $1", Regex: true, Case: "lower"})
	if ops[0].NewName != "durban trip 01.mp4" {
		t.Fatalf("got %q", ops[0].NewName)
	}
	ops, _ = Plan(its, Pattern{Find: "(", Replace: "x"})
	if ops[0].NewName != "Durban Holiday - A01.mp4" {
		t.Errorf("literal find changed name: %q", ops[0].NewName)
	}
}

func TestPlanConflicts(t *testing.T) {
	dir := t.TempDir()
	its := items(dir, "a.mp4", "b.mp4", "keep.mp4")
	conflict := func(p Pattern, its []Item) []string {
		ops, err := Plan(its, p)
		if err != nil {
			t.Fatal(err)
		}
		var c []string
		for _, o := range ops {
			c = append(c, o.Conflict)
		}
		return c
	}
	if c := conflict(Pattern{Template: "same"}, its[:2]); c[0] != "duplicate target name" || c[1] != "duplicate target name" {
		t.Errorf("duplicates: %v", c)
	}
	if c := conflict(Pattern{Template: "keep"}, its[:1]); c[0] != "a file with this name already exists" {
		t.Errorf("existing: %v", c)
	}
	if c := conflict(Pattern{Template: "bad:name"}, its[:1]); !strings.HasPrefix(c[0], "illegal") {
		t.Errorf("illegal: %v", c)
	}
	if c := conflict(Pattern{Template: strings.Repeat("x", 300)}, its[:1]); !strings.Contains(c[0], "bytes") {
		t.Errorf("long: %v", c)
	}
	if c := conflict(Pattern{Template: "Keep", Case: "lower"}, its[:1]); c[0] == "" {
		t.Error("case-insensitive clash with keep.mp4 not flagged")
	}
	// A swap is not a conflict: both names are vacated.
	swap := []Item{its[0], its[1]}
	ops, _ := Plan(swap, Pattern{Find: "^a$|^b$", Regex: true})
	_ = ops
	ops = []Op{{Item: its[0], NewName: "b.mp4"}, {Item: its[1], NewName: "a.mp4"}}
	flagConflicts(ops)
	if ops[0].Conflict != "" || ops[1].Conflict != "" {
		t.Errorf("swap flagged: %+v", ops)
	}
}

func TestApplySwapAndUndo(t *testing.T) {
	root := t.TempDir()
	disc := filepath.Join(root, "Disc")
	os.Mkdir(disc, 0o755)
	its := items(disc, "a.mp4", "b.mp4")
	ops := []Op{{Item: its[0], NewName: "b.mp4"}, {Item: its[1], NewName: "a.mp4"}}
	flagConflicts(ops)
	folder := Op{Item: Item{Kind: "folder", ID: "d1", Dir: root, Name: "Disc"}, NewName: "Renamed"}
	ops = append([]Op{folder}, ops...)
	moves, err := Apply(root, "b1", ops)
	if err != nil {
		t.Fatal(err)
	}
	read := func(p string) string { b, _ := os.ReadFile(p); return string(b) }
	if read(filepath.Join(root, "Renamed", "b.mp4")) != "a.mp4" || read(filepath.Join(root, "Renamed", "a.mp4")) != "b.mp4" {
		t.Fatal("swap didn't happen inside the renamed folder")
	}
	if err := Execute(root, "u1", Inverse(moves)); err != nil {
		t.Fatal(err)
	}
	if read(filepath.Join(disc, "a.mp4")) != "a.mp4" || read(filepath.Join(disc, "b.mp4")) != "b.mp4" {
		t.Fatal("undo didn't restore names")
	}
	left, _ := filepath.Glob(filepath.Join(root, "*", ".camdvd-rename-*"))
	if len(left) > 0 {
		t.Errorf("temp files left: %v", left)
	}
}

func TestApplyRollsBackOnFailure(t *testing.T) {
	root := t.TempDir()
	its := items(root, "a.mp4", "b.mp4")
	ops := []Op{{Item: its[0], NewName: "x.mp4"}, {Item: its[1], NewName: "y.mp4"}}
	os.Remove(filepath.Join(root, "b.mp4")) // vanished after preview
	if _, err := Apply(root, "b2", ops); err == nil {
		t.Fatal("expected failure")
	}
	if !exists(filepath.Join(root, "a.mp4")) || exists(filepath.Join(root, "x.mp4")) {
		t.Fatal("partial rename not rolled back")
	}
	ops[0].Conflict = "duplicate target name"
	if _, err := Apply(root, "b3", ops); err != ErrConflicts {
		t.Fatalf("want ErrConflicts, got %v", err)
	}
}
