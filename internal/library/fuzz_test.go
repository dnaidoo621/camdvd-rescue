package library

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// Descriptions come from the UI and become folder names.
func FuzzSanitize(f *testing.F) {
	for _, s := range []string{"2004-12 Durban holiday", "../../etc", "a/b\\c", "\x00\x07", " . "} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		out := Sanitize(s)
		if strings.ContainsAny(out, "/\\\x00") || out == "." || out == ".." || strings.HasPrefix(out, ".") {
			t.Fatalf("Sanitize(%q) = %q is unsafe", s, out)
		}
		if utf8.RuneCountInString(out) > MaxFolderRunes {
			t.Fatalf("Sanitize(%q) too long", s)
		}
	})
}
