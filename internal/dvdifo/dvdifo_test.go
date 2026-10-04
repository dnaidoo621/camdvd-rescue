package dvdifo

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func TestParseSynthetic(t *testing.T) {
	b := make([]byte, 3*2048)
	copy(b, "DVDVIDEO-VMG")
	binary.BigEndian.PutUint32(b[0xC4:], 1) // TT_SRPT in sector 1
	tt := b[2048:]
	binary.BigEndian.PutUint16(tt, 2)
	e := tt[8:]
	e[1] = 1
	binary.BigEndian.PutUint16(e[2:], 3)
	e[6], e[7] = 1, 1
	e = tt[20:]
	e[1] = 1
	binary.BigEndian.PutUint16(e[2:], 1)
	e[6], e[7] = 1, 2

	got, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	want := []Title{{1, 3, 1, 1, 1}, {2, 1, 1, 1, 2}}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %+v", got)
	}
}

func TestRejectsOtherFiles(t *testing.T) {
	if _, err := Parse(make([]byte, 4096)); err == nil {
		t.Fatal("expected error")
	}
}

// TestDVDAuthorFixture runs against the IFO that scripts/make-test-images.sh
// writes with dvdauthor, when it has been generated.
func TestDVDAuthorFixture(t *testing.T) {
	p := filepath.Join("..", "..", "testdata", "gen", "finalized", "VIDEO_TS", "VIDEO_TS.IFO")
	if _, err := os.Stat(p); err != nil {
		t.Skip("fixture not generated; run scripts/make-test-images.sh")
	}
	got, err := ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	// The fixture authors two title sets of two recordings each.
	if len(got) != 2 || got[0].Chapters != 2 || got[1].Chapters != 2 || got[1].VTS != 2 {
		t.Fatalf("got %+v", got)
	}
}
