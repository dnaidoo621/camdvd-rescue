package library

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Item is one thing to rename: a file inside a disc folder, or a folder.
type Item struct {
	Kind string `json:"kind"` // "file" or "folder"
	ID   string `json:"id"`   // clip id or disc id
	Dir  string `json:"dir"`  // absolute parent directory
	Name string `json:"name"` // current base name, extension included
	// Token values for this item.
	Desc     string    `json:"desc"`
	Side     string    `json:"side"`
	Date     time.Time `json:"date"`
	Duration float64   `json:"duration"`
}

// Pattern is what the user typed in the rename form.
type Pattern struct {
	Template string `json:"template"` // e.g. "{desc} - {side}{n:00}"; empty keeps the name
	Start    int    `json:"start"`
	Step     int    `json:"step"`
	Find     string `json:"find"`
	Replace  string `json:"replace"`
	Regex    bool   `json:"regex"`
	Case     string `json:"case"` // "", lower, upper, title
}

// Op is one planned rename.
type Op struct {
	Item
	NewName  string `json:"new_name"`
	Conflict string `json:"conflict,omitempty"`
}

// Changed reports whether the op renames anything.
func (o Op) Changed() bool { return o.NewName != o.Name }

// Plan computes new names and flags conflicts. Apply must be refused unless
// no op has a Conflict.
func Plan(items []Item, p Pattern) ([]Op, error) {
	if p.Step == 0 {
		p.Step = 1
	}
	if p.Start == 0 && p.Template != "" {
		p.Start = 1
	}
	var re *regexp.Regexp
	if p.Find != "" {
		expr := p.Find
		if !p.Regex {
			expr = regexp.QuoteMeta(expr)
		}
		var err error
		if re, err = regexp.Compile(expr); err != nil {
			return nil, fmt.Errorf("find pattern: %w", err)
		}
	}
	ops := make([]Op, len(items))
	for i, it := range items {
		ext := ""
		if it.Kind == "file" {
			ext = filepath.Ext(it.Name)
		}
		orig := strings.TrimSuffix(it.Name, ext)
		stem := orig
		if p.Template != "" {
			var err error
			stem, err = expand(p.Template, it, orig, p.Start+i*p.Step)
			if err != nil {
				return nil, err
			}
		}
		if re != nil {
			stem = re.ReplaceAllString(stem, p.Replace)
		}
		stem = changeCase(stem, p.Case)
		ops[i] = Op{Item: it, NewName: stem + ext}
	}
	flagConflicts(ops)
	return ops, nil
}

var reToken = regexp.MustCompile(`\{(\w+)(?::([^}]*))?\}`)

func expand(tmpl string, it Item, orig string, n int) (string, error) {
	var bad error
	out := reToken.ReplaceAllStringFunc(tmpl, func(m string) string {
		sm := reToken.FindStringSubmatch(m)
		name, arg := sm[1], sm[2]
		switch name {
		case "desc":
			return it.Desc
		case "side":
			return it.Side
		case "orig":
			return orig
		case "n":
			if arg != "" && strings.Trim(arg, "0") == "" {
				return fmt.Sprintf("%0*d", len(arg), n)
			}
			return strconv.Itoa(n)
		case "date":
			if it.Date.IsZero() {
				return "undated"
			}
			if arg == "" {
				arg = "yyyy-MM-dd"
			}
			return it.Date.Format(goLayout(arg))
		case "time":
			if it.Date.IsZero() {
				return "untimed"
			}
			return it.Date.Format("15-04")
		case "duration":
			d := time.Duration(it.Duration * float64(time.Second)).Round(time.Second)
			return fmt.Sprintf("%02dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
		}
		bad = fmt.Errorf("unknown token {%s}", name)
		return m
	})
	return out, bad
}

// goLayout converts yyyy-MM-dd style patterns to Go's reference layout.
func goLayout(p string) string {
	r := strings.NewReplacer("yyyy", "2006", "yy", "06", "MMM", "Jan", "MM", "01", "dd", "02", "HH", "15", "mm", "04", "ss", "05")
	return r.Replace(p)
}

func changeCase(s, c string) string {
	switch c {
	case "lower":
		return strings.ToLower(s)
	case "upper":
		return strings.ToUpper(s)
	case "title":
		prev := ' '
		return strings.Map(func(r rune) rune {
			defer func() { prev = r }()
			if unicode.IsSpace(prev) || prev == '-' || prev == '_' {
				return unicode.ToUpper(r)
			}
			return unicode.ToLower(r)
		}, s)
	}
	return s
}

// MaxNameBytes is the Linux limit for one path element.
const MaxNameBytes = 255

func flagConflicts(ops []Op) {
	// Names leaving their path free up the target for others; SMB and macOS
	// clients are case-insensitive, so compare folded.
	key := func(dir, name string) string { return dir + "\x00" + strings.ToLower(name) }
	leaving := map[string]bool{}
	target := map[string]int{}
	for _, o := range ops {
		if o.Changed() {
			leaving[key(o.Dir, o.Name)] = true
		}
		target[key(o.Dir, o.NewName)]++
	}
	for i := range ops {
		o := &ops[i]
		n := o.NewName
		stem := strings.TrimSuffix(n, filepath.Ext(n))
		switch {
		case strings.TrimSpace(stem) == "" || n == "." || n == "..":
			o.Conflict = "empty name"
		case strings.ContainsAny(n, "/\\:*?\"<>|") || strings.IndexFunc(n, unicode.IsControl) >= 0 || !utf8.ValidString(n):
			o.Conflict = `illegal character (/ \ : * ? " < > | or control)`
		case strings.HasPrefix(n, ".") || strings.HasSuffix(n, ".") || strings.HasSuffix(n, " ") || strings.HasPrefix(n, " "):
			o.Conflict = "starts or ends with a dot or space"
		case len(n) > MaxNameBytes:
			o.Conflict = fmt.Sprintf("name is %d bytes, over %d", len(n), MaxNameBytes)
		case target[key(o.Dir, n)] > 1:
			o.Conflict = "duplicate target name"
		case o.Changed() && !strings.EqualFold(n, o.Name) && !leaving[key(o.Dir, n)] && exists(filepath.Join(o.Dir, n)):
			o.Conflict = "a file with this name already exists"
		}
	}
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// Move is one applied rename, recorded for undo.
type Move struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
	From string `json:"from"`
	To   string `json:"to"`
}

// ErrConflicts is returned when a plan with conflicts is applied.
var ErrConflicts = errors.New("the preview has conflicts; fix them before applying")

// Apply renames in two phases through temporary names in the same
// directory, so swaps (A→B, B→A) work, and rolls everything back if any step
// fails. Files are renamed before folders, so file paths stay valid. root
// confines every path to the library.
func Apply(root, batchID string, ops []Op) ([]Move, error) {
	var moves []Move
	for _, kind := range []string{"file", "folder"} {
		for _, o := range ops {
			if o.Conflict != "" {
				return nil, ErrConflicts
			}
			if o.Kind == kind && o.Changed() {
				moves = append(moves, Move{o.Kind, o.ID, filepath.Join(o.Dir, o.Name), filepath.Join(o.Dir, o.NewName)})
			}
		}
	}
	// Folder moves after file moves: a folder's files are recorded at their
	// old folder path, which is still valid when they move.
	return moves, Execute(root, batchID, moves)
}

// Execute performs moves transactionally. Used by Apply and by Undo.
func Execute(root, batchID string, moves []Move) error {
	for _, m := range moves {
		for _, p := range []string{m.From, m.To} {
			if err := checkInside(root, p); err != nil {
				return err
			}
		}
		if filepath.Dir(m.From) != filepath.Dir(m.To) {
			return fmt.Errorf("rename %s must stay in its folder", m.From)
		}
	}
	// Moves are grouped by phase: everything of one kind goes to temp names,
	// then to targets, before the next kind starts, because folder moves
	// change the parent of file paths.
	var done []Move // completed physical renames, for rollback
	rollback := func() {
		for i := len(done) - 1; i >= 0; i-- {
			_ = os.Rename(done[i].To, done[i].From)
		}
	}
	for start := 0; start < len(moves); {
		end := start
		for end < len(moves) && moves[end].Kind == moves[start].Kind {
			end++
		}
		group := moves[start:end]
		tmps := make([]string, len(group))
		for i, m := range group {
			if _, err := os.Lstat(m.From); err != nil {
				rollback()
				return fmt.Errorf("%s: %w", m.From, err)
			}
			tmps[i] = filepath.Join(filepath.Dir(m.From), fmt.Sprintf(".camdvd-rename-%s-%d", batchID, start+i))
			if err := os.Rename(m.From, tmps[i]); err != nil {
				rollback()
				return err
			}
			done = append(done, Move{From: m.From, To: tmps[i]})
		}
		for i, m := range group {
			if exists(m.To) && !strings.EqualFold(m.To, m.From) {
				rollback()
				return fmt.Errorf("%s appeared while renaming; nothing was changed", m.To)
			}
			if err := os.Rename(tmps[i], m.To); err != nil {
				rollback()
				return err
			}
			done = append(done, Move{From: tmps[i], To: m.To})
		}
		start = end
	}
	return nil
}

// Inverse returns the moves that undo moves, in the right order.
func Inverse(moves []Move) []Move {
	inv := make([]Move, len(moves))
	for i, m := range moves {
		inv[len(moves)-1-i] = Move{m.Kind, m.ID, m.To, m.From}
	}
	return inv
}
