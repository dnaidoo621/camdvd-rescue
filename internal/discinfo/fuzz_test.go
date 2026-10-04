package discinfo

import "testing"

func FuzzParseMediaInfo(f *testing.F) {
	f.Add(unfinalized)
	f.Add(finalized)
	f.Add(ram)
	f.Fuzz(func(t *testing.T, s string) {
		m := ParseMediaInfo(s)
		_ = Classify(Probe{Media: m, Listed: true})
		if m.UsedSectors() < 0 {
			t.Fatalf("negative used sectors from %q", s)
		}
	})
}
