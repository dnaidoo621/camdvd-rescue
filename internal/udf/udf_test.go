package udf

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRealImage runs against a real disc image when CAMDVD_UDF_IMAGE names
// one (e.g. a Panasonic DVD-RAM camcorder disc), listing and extracting it.
func TestRealImage(t *testing.T) {
	img := os.Getenv("CAMDVD_UDF_IMAGE")
	if img == "" {
		t.Skip("set CAMDVD_UDF_IMAGE to a UDF disc image")
	}
	f, err := os.Open(img)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fs, err := Open(f)
	if err != nil {
		t.Fatal(err)
	}
	var files int
	err = fs.Walk(func(e *Entry) error {
		t.Logf("%v %10d %s", e.ModTime.Format(time.DateTime), e.Size, e.Path)
		if !e.Dir {
			files++
			n, err := io.Copy(io.Discard, fs.Open(e))
			if err != nil || n != e.Size {
				return fmt.Errorf("%s: read %d of %d: %v", e.Path, n, e.Size, err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files == 0 {
		t.Fatal("no files")
	}
}

// TestFixtureImage checks the mkudffs-made fixture from
// scripts/make-test-images.sh against the files that were copied into it.
func TestFixtureImage(t *testing.T) {
	gen := filepath.Join("..", "..", "testdata", "gen")
	img := filepath.Join(gen, "dvdram.img")
	if _, err := os.Stat(img); err != nil {
		t.Skip("fixture not generated (needs mkudffs and sudo)")
	}
	f, err := os.Open(img)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fs, err := Open(f)
	if err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if err := fs.Extract(dst); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"VR_MANGR.IFO", "VR_MOVIE.VRO"} {
		want := sum(t, filepath.Join(gen, "dvdram-src", "DVD_RTAV", name))
		got := sum(t, filepath.Join(dst, "DVD_RTAV", name))
		if got != want {
			t.Errorf("%s differs after extraction", name)
		}
	}
}

func sum(t *testing.T, p string) string {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	io.Copy(h, f)
	return fmt.Sprintf("%x", h.Sum(nil))
}

func TestRejectsNonUDF(t *testing.T) {
	if _, err := Open(strings.NewReader(strings.Repeat("\x00", 300*sectorSize))); err == nil {
		t.Fatal("zeros accepted as UDF")
	}
}

func TestDstringAndTimestamp(t *testing.T) {
	if got := dstring([]byte("\x08VR_MOVIE.VRO")); got != "VR_MOVIE.VRO" {
		t.Errorf("cs0 8: %q", got)
	}
	if got := dstring([]byte{16, 0, 'A', 0x00, 0xE9}); got != "Aé" {
		t.Errorf("cs0 16: %q", got)
	}
	// 2006-05-20 11:09:21 at +02:00 (type 1, tz 120 minutes).
	b := []byte{120, 0x10, 0xD6, 0x07, 5, 20, 11, 9, 21, 0, 0, 0}
	ts := timestamp(b)
	if ts.Format(time.RFC3339) != "2006-05-20T11:09:21+02:00" {
		t.Errorf("timestamp %v", ts)
	}
}
