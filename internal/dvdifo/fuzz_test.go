package dvdifo

import "testing"

func FuzzParse(f *testing.F) {
	b := make([]byte, 3*2048)
	copy(b, "DVDVIDEO-VMG")
	b[0xC7] = 1
	b[2049] = 1
	f.Add(b)
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _ = Parse(b) // must not panic on a corrupt IFO
	})
}
