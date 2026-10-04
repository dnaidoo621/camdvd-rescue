// Package library owns the output folders: naming, the disc manifests, the
// bulk-rename planner with its transactional apply and undo, and rescans that
// re-link files renamed outside the app.
package library

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// MaxFolderRunes caps generated folder and file names.
const MaxFolderRunes = 120

// Sanitize makes s safe as one path element on Linux and over SMB: no
// slashes, control characters or characters Windows/macOS clients reject,
// no leading/trailing dots or spaces, at most MaxFolderRunes characters.
func Sanitize(s string) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		switch {
		case r == '/' || r == '\\' || r == ':' || r == '*' || r == '?' || r == '"' || r == '<' || r == '>' || r == '|':
			r = '-'
		case unicode.IsSpace(r):
			r = ' '
		case unicode.IsControl(r) || r == utf8.RuneError:
			continue
		}
		if r == ' ' {
			if space {
				continue
			}
			space = true
		} else {
			space = false
		}
		b.WriteRune(r)
	}
	out := strings.Trim(b.String(), " .")
	if utf8.RuneCountInString(out) > MaxFolderRunes {
		out = strings.TrimRight(string([]rune(out)[:MaxFolderRunes]), " .")
	}
	return out
}

// FolderName picks a disc folder name: the description, else the earliest
// recording date, else the import time.
func FolderName(desc string, earliest time.Time, imported time.Time) string {
	if s := Sanitize(desc); s != "" {
		return s
	}
	if !earliest.IsZero() {
		return earliest.Format("2006-01-02")
	}
	return "Imported " + imported.Format("2006-01-02 1504")
}

// FileStem is the base for a disc's files: "{desc} - A01", or "- 01" when
// single-sided. desc falls back to the folder name.
func FileStem(desc, folder, side string, n int, singleSided bool) string {
	d := Sanitize(desc)
	if d == "" {
		d = folder
	}
	if singleSided {
		side = ""
	}
	return fmt.Sprintf("%s - %s%02d", d, side, n)
}

// Unique returns name, or "name (2)", "name (3)"... so that no entry called
// that exists in dir. taken lists names reserved but not yet on disk.
func Unique(dir, name, ext string, taken map[string]bool) string {
	cand := name + ext
	for i := 2; ; i++ {
		_, err := os.Lstat(filepath.Join(dir, cand))
		if os.IsNotExist(err) && !taken[strings.ToLower(cand)] {
			return cand
		}
		cand = fmt.Sprintf("%s (%d)%s", name, i, ext)
	}
}

// Within resolves rel under root and refuses anything that escapes it,
// including through symlinks in existing parents.
func Within(root, rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("absolute path %q not allowed", rel)
	}
	p := filepath.Join(root, rel)
	if err := checkInside(root, p); err != nil {
		return "", err
	}
	return p, nil
}

func checkInside(root, p string) error {
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	// Resolve the deepest existing ancestor.
	dir := p
	rest := ""
	for {
		if real, err := filepath.EvalSymlinks(dir); err == nil {
			p = filepath.Join(real, rest)
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return fmt.Errorf("%s has no existing ancestor", p)
		}
		rest = filepath.Join(filepath.Base(dir), rest)
		dir = parent
	}
	rel, err := filepath.Rel(rootReal, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%s is outside the library", p)
	}
	return nil
}
