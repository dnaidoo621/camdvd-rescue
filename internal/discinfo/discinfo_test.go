package discinfo

import (
	"strings"
	"testing"
)

const unfinalized = `INQUIRY:                [HL-DT-ST][DVDRAM GUC0N    ][AS01]
GET [CURRENT] CONFIGURATION:
 Mounted Media:         11h, DVD-R Sequential
 Media ID:              CMC MAG. AM3
 Current Write Speed:   4.0x1385=5540KB/s
READ DVD STRUCTURE[#0h]:
 Media Book Type:       25h, DVD-R book [revision 5]
 Last border-out at:    0*2KB=0
READ DISC INFORMATION:
 Disc status:           appendable
 Number of Sessions:    1
 State of Last Session: incomplete
 "Next" Track:          1
 Number of Tracks:      2
READ TRACK INFORMATION[#1]:
 Track State:           partial incremental
 Track Start Address:   0*2KB
 Next Writable Address: 0*2KB
 Free Blocks:           0*2KB
 Track Size:            8432*2KB
READ TRACK INFORMATION[#2]:
 Track State:           incomplete incremental
 Track Start Address:   8432*2KB
 Next Writable Address: 125360*2KB
 Free Blocks:           588640*2KB
 Track Size:            714000*2KB
READ CAPACITY:          0*2048=0
`

const finalized = ` Mounted Media:         11h, DVD-R Sequential
 Disc status:           complete
 Number of Sessions:    1
 State of Last Session: complete
 Track Start Address:   0*2KB
 Free Blocks:           0*2KB
READ CAPACITY:          712224*2048=1458634752
`

const ram = ` Mounted Media:         12h, DVD-RAM
 Disc status:           other
READ CAPACITY:          697696*2048=1428881408
`

func TestParseUnfinalized(t *testing.T) {
	m := ParseMediaInfo(unfinalized)
	if m.Type != "DVD-R Sequential" || m.DiscStatus != "appendable" || m.SessionState != "incomplete" {
		t.Fatalf("parsed %+v", m)
	}
	if m.TrackStart != 8432 || m.NextWritable != 125360 || m.Capacity != 0 {
		t.Errorf("addresses %+v", m)
	}
	if !m.Open() || m.UsedSectors() != 125360 {
		t.Errorf("open=%v used=%d", m.Open(), m.UsedSectors())
	}
}

func TestNoMedia(t *testing.T) {
	m := ParseMediaInfo(":-( no media mounted, exiting...\n")
	if !m.NoMedia {
		t.Fatal("expected NoMedia")
	}
}

func TestClassificationTable(t *testing.T) {
	fin := ParseMediaInfo(finalized)
	open := ParseMediaInfo(unfinalized)
	dram := ParseMediaInfo(ram)
	cd := Media{Type: "CD-R", DiscStatus: "complete", Capacity: 1000}
	blank := Media{Type: "DVD-R Sequential", DiscStatus: "blank"}
	rw := Media{Type: "DVD-RW Restricted Overwrite", DiscStatus: "complete", Capacity: 700000}

	cases := []struct {
		name string
		p    Probe
		want Class
		path Path
	}{
		{"blank", Probe{Media: blank}, Blank, PathNone},
		{"finalized video", Probe{Media: fin, FSType: "udf", Listed: true, HasVideoTS: true}, Finalized, PathFiles},
		{"finalized VR on RW", Probe{Media: rw, FSType: "udf", Listed: true, HasRTAV: true}, FinalizedVR, PathFiles},
		{"dvd-ram", Probe{Media: dram, FSType: "udf", Listed: true, HasRTAV: true}, RAM, PathFiles},
		{"unfinalized", Probe{Media: open}, Unfinalized, PathRaw},
		{"fs unreadable", Probe{Media: fin, FSType: "udf", Listed: true, ListFailed: true}, Damaged, PathRaw},
		{"complete without fs", Probe{Media: fin, Listed: true}, Damaged, PathRaw},
		{"data disc", Probe{Media: fin, FSType: "iso9660", Listed: true}, DataDisc, PathCopy},
		{"not a dvd", Probe{Media: cd}, Unsupported, PathReject},
	}
	for _, c := range cases {
		d := Classify(c.p)
		if d.Class != c.want || d.Class.Path() != c.path {
			t.Errorf("%s: got %s (%s), want %s (%s)", c.name, d.Class, d.Class.Path(), c.want, c.path)
		}
		if d.Reason == "" {
			t.Errorf("%s: empty reason", c.name)
		}
	}
	if d := Classify(Probe{Media: open}); !strings.Contains(d.Reason, "Unfinalized DVD-R: no file system, about 4 min") {
		t.Errorf("reason %q", d.Reason)
	}
}

func TestPreClassifyMethod(t *testing.T) {
	if _, m, ok := PreClassify(Probe{Media: ParseMediaInfo(unfinalized)}); !ok || m != ImageRaw {
		t.Errorf("unfinalized: %v %v", m, ok)
	}
	if _, m, ok := PreClassify(Probe{Media: ParseMediaInfo(finalized), FSType: "udf"}); !ok || m != ImageBlock {
		t.Errorf("finalized: %v %v", m, ok)
	}
	if _, m, ok := PreClassify(Probe{Media: ParseMediaInfo(ram), FSType: "udf"}); !ok || m != ImageBlock {
		t.Errorf("ram: %v %v", m, ok)
	}
}
