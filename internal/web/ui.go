package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"math"
	"net/http"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dnaidoo621/camdvd-rescue/internal/config"
	"github.com/dnaidoo621/camdvd-rescue/internal/discinfo"
	"github.com/dnaidoo621/camdvd-rescue/internal/jobs"
	"github.com/dnaidoo621/camdvd-rescue/internal/library"
	"github.com/dnaidoo621/camdvd-rescue/internal/store"
)

var funcs = template.FuncMap{
	"dur": func(sec float64) string {
		if sec <= 0 {
			return "–"
		}
		d := time.Duration(sec * float64(time.Second)).Round(time.Second)
		h, m, s := int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60
		if h > 0 {
			return fmt.Sprintf("%d:%02d:%02d", h, m, s)
		}
		return fmt.Sprintf("%d:%02d", m, s)
	},
	"pct":  func(f float64) string { return fmt.Sprintf("%.0f%%", math.Max(0, math.Min(1, f))*100) },
	"pct1": func(f float64) string { return fmt.Sprintf("%.1f%%", f*100) },
	"mins": func(m float64) string {
		switch {
		case m <= 0:
			return "–"
		case m < 1:
			return "under a minute"
		case m < 90:
			return fmt.Sprintf("%.0f min", m)
		}
		return fmt.Sprintf("%.1f h", m/60)
	},
	"until": func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		d := time.Until(t).Round(time.Minute)
		at := t.Format("15:04")
		if t.YearDay() != time.Now().YearDay() {
			at = t.Format("Mon 15:04")
		}
		if d < time.Minute {
			return "under a minute"
		}
		if d < time.Hour {
			return fmt.Sprintf("about %d min (around %s)", int(d.Minutes()), at)
		}
		return fmt.Sprintf("about %dh %02dm (around %s)", int(d.Hours()), int(d.Minutes())%60, at)
	},
	"fracOf": func(a, b int) float64 {
		if b == 0 {
			return 0
		}
		return float64(a) / float64(b)
	},
	"compact": func(clips []*store.Clip) bool { return len(clips) > 8 },
	"busyClips": func(clips []*store.Clip) []*store.Clip {
		var out []*store.Clip
		for _, c := range clips {
			if c.State == "converting" || c.State == "failed" {
				out = append(out, c)
			}
		}
		return out
	},
	"countDone": func(clips []*store.Clip) int {
		n := 0
		for _, c := range clips {
			if c.Done() {
				n++
			}
		}
		return n
	},
	"size": func(n int64) string {
		switch {
		case n >= 1e9:
			return fmt.Sprintf("%.2f GB", float64(n)/1e9)
		case n >= 1e6:
			return fmt.Sprintf("%.0f MB", float64(n)/1e6)
		case n > 0:
			return fmt.Sprintf("%.0f kB", float64(n)/1e3)
		}
		return "–"
	},
	"ago": func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		d := time.Since(t)
		switch {
		case d < time.Minute:
			return "just now"
		case d < time.Hour:
			return fmt.Sprintf("%d min ago", int(d.Minutes()))
		case d < 24*time.Hour:
			return fmt.Sprintf("%d h ago", int(d.Hours()))
		}
		return t.Format("2 Jan 2006")
	},
	"when": func(t time.Time) string {
		if t.IsZero() {
			return "–"
		}
		return t.Format("2 Jan 2006 15:04")
	},
	"clock": func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.Format("15:04:05")
	},
	"base":  filepath.Base,
	"lower": strings.ToLower,
	"join":  strings.Join,
	"add":   func(a, b int) int { return a + b },
	"stateTone": func(s store.State) string {
		switch s {
		case store.Done:
			return "ok"
		case store.Failed:
			return "bad"
		case store.Cancelled:
			return "muted"
		case store.AwaitingAnswer, store.AwaitingFlip, store.Duplicate, store.Paused:
			return "attn"
		}
		return "busy"
	},
	"classLabel": func(c discinfo.Class) string {
		return map[discinfo.Class]string{
			discinfo.Finalized: "Finalized DVD-Video", discinfo.FinalizedVR: "Finalized DVD-VR", discinfo.RAM: "DVD-RAM",
			discinfo.Unfinalized: "Unfinalized", discinfo.Damaged: "Damaged", discinfo.DataDisc: "Data disc",
			discinfo.Blank: "Blank", discinfo.Unsupported: "Unsupported",
		}[c]
	},
	"clipTone": func(st string) string {
		switch st {
		case "placed", "converted":
			return "ok"
		case "failed":
			return "bad"
		case "converting":
			return "busy"
		}
		return "muted"
	},
	"canResume": func(s store.State) bool { return s == store.Cancelled || s == store.Failed || s == store.Paused },
	"canReprocess": func(d *store.Disc) bool {
		if d.State != store.Cancelled && d.State != store.Failed || len(d.Sides) == 0 {
			return false
		}
		for _, s := range d.Sides {
			if !s.Imaged {
				return false
			}
		}
		return true
	},
	"canCancel": func(s store.State) bool { return !s.Terminal() },
	"canForceRaw": func(d *store.Disc) bool {
		if d.State == store.Done || d.State == store.Cancelled {
			return false
		}
		for _, s := range d.Sides {
			if s.Imaged {
				return true
			}
		}
		return false
	},
	"json": func(v any) string { b, _ := json.MarshalIndent(v, "", "  "); return string(b) },
	"title": func(d *store.Disc) string {
		if d.Description != "" {
			return d.Description
		}
		if d.Folder != "" {
			return d.Folder
		}
		return "Disc " + strings.TrimPrefix(d.ID, "d-")
	},
	"sidesClips": func(clips []*store.Clip, side string) []*store.Clip {
		var out []*store.Clip
		for _, c := range clips {
			if c.Side == side {
				out = append(out, c)
			}
		}
		return out
	},
	"seq": func(n int) []int {
		s := make([]int, n)
		for i := range s {
			s[i] = i + 1
		}
		return s
	},
}

func loadTemplates() (map[string]*template.Template, *template.Template, error) {
	base, err := template.New("").Funcs(funcs).ParseFS(templateFS, "templates/layout.html", "templates/frags.html")
	if err != nil {
		return nil, nil, err
	}
	files, err := fs.Glob(templateFS, "templates/page_*.html")
	if err != nil {
		return nil, nil, err
	}
	pages := map[string]*template.Template{}
	for _, f := range files {
		t, err := base.Clone()
		if err != nil {
			return nil, nil, err
		}
		if _, err := t.ParseFS(templateFS, f); err != nil {
			return nil, nil, err
		}
		name := strings.TrimSuffix(strings.TrimPrefix(path.Base(f), "page_"), ".html")
		pages[name] = t
	}
	return pages, base, nil
}

// page is the data every page gets.
type page struct {
	Title      string
	Nav        string
	Report     any
	ReportOK   bool
	Blocked    string
	PasswordOn bool
	Version    string
	Settings   config.Settings
	Data       any
	Error      string
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, data any) {
	t, ok := s.pages[name]
	if !ok {
		http.Error(w, "no template "+name, http.StatusInternalServerError)
		return
	}
	rep := s.lastReport()
	p := page{Title: name, Nav: name, Report: rep, ReportOK: rep.OK || rep.At.IsZero(), Blocked: s.E.Blocked(),
		PasswordOn: s.passwordOn(), Version: Version, Settings: s.E.Settings(), Data: data}
	if m, ok := data.(map[string]any); ok {
		if e, ok := m["Error"].(string); ok {
			p.Error = e
		}
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "layout", p); err != nil {
		s.Log.Error("render", "page", name, "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = buf.WriteTo(w)
}

func (s *Server) frag(w http.ResponseWriter, name string, data any) {
	var buf bytes.Buffer
	if err := s.frags.ExecuteTemplate(&buf, name, data); err != nil {
		s.Log.Error("render fragment", "name", name, "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = buf.WriteTo(w)
}

// fragError shows an error inline in an htmx target.
func (s *Server) fragError(w http.ResponseWriter, err error) {
	w.Header().Set("HX-Reswap", "innerHTML")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `<p class="flash bad">%s</p>`, template.HTMLEscapeString(err.Error()))
}

// toast sends a message the layout shows briefly.
func toast(w http.ResponseWriter, msg string) {
	b, _ := json.Marshal(map[string]string{"toast": msg})
	w.Header().Set("HX-Trigger", string(b))
}

// --- Dashboard ---------------------------------------------------------------

type cardData struct {
	*JobView
	LastSides int
	Detail    bool
	Demo      bool
}

func (s *Server) card(id string, detail bool) (*cardData, error) {
	j, err := s.job(id)
	if err != nil {
		return nil, err
	}
	return &cardData{JobView: j, LastSides: s.E.Settings().LastSides, Detail: detail}, nil
}

func (s *Server) activeCards() ([]*cardData, error) {
	discs, err := s.E.St.Discs()
	if err != nil {
		return nil, err
	}
	var out []*cardData
	for _, d := range discs {
		recent := d.State.Terminal() && time.Since(d.UpdatedAt) < 12*time.Hour && !(d.State == store.Cancelled && time.Since(d.UpdatedAt) > time.Hour)
		if !d.State.Terminal() || recent {
			c, err := s.card(d.ID, false)
			if err != nil {
				return nil, err
			}
			out = append(out, c)
		}
	}
	return out, nil
}

func (s *Server) pageDashboard(w http.ResponseWriter, r *http.Request) {
	cards, err := s.activeCards()
	if err != nil {
		httpError(w, err)
		return
	}
	s.render(w, r, "dashboard", map[string]any{"Drives": s.E.Drives(), "Cards": cards, "Overview": s.E.Overview()})
}

func (s *Server) fragDrives(w http.ResponseWriter, r *http.Request) {
	s.frag(w, "drives", map[string]any{"Drives": s.E.Drives(), "Blocked": s.E.Blocked()})
}

func (s *Server) fragOverview(w http.ResponseWriter, r *http.Request) {
	s.frag(w, "overview", s.E.Overview())
}

func (s *Server) fragActive(w http.ResponseWriter, r *http.Request) {
	cards, err := s.activeCards()
	if err != nil {
		httpError(w, err)
		return
	}
	s.frag(w, "active", cards)
}

func (s *Server) fragCard(w http.ResponseWriter, r *http.Request) {
	c, err := s.card(r.PathValue("id"), r.URL.Query().Get("detail") != "")
	if err != nil {
		httpError(w, err)
		return
	}
	s.frag(w, "card", c)
}

func (s *Server) uiAnswers(w http.ResponseWriter, r *http.Request) {
	sides, _ := strconv.Atoi(r.FormValue("sides"))
	desc := r.FormValue("description")
	if r.FormValue("skip") != "" {
		desc = ""
	}
	if _, err := s.E.Answers(r.PathValue("id"), sides, desc); err != nil {
		s.fragError(w, err)
		return
	}
	c, err := s.card(r.PathValue("id"), false)
	if err != nil {
		httpError(w, err)
		return
	}
	s.frag(w, "answered", c)
}

func (s *Server) uiJobAction(w http.ResponseWriter, r *http.Request) {
	id, action := r.PathValue("id"), r.PathValue("action")
	if err := s.jobAction(id, action); err != nil {
		if err == errUnknownAction {
			http.NotFound(w, r)
			return
		}
		toast(w, err.Error())
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if action == "delete" {
		w.Header().Set("HX-Redirect", "/history")
		return
	}
	c, err := s.card(id, r.FormValue("detail") != "")
	if err != nil {
		httpError(w, err)
		return
	}
	s.frag(w, "card", c)
}

func (s *Server) uiEject(w http.ResponseWriter, r *http.Request) {
	if err := s.E.Eject(r.PathValue("id")); err != nil {
		toast(w, err.Error())
	}
	s.fragDrives(w, r)
}

func (s *Server) uiInsert(w http.ResponseWriter, r *http.Request) {
	if err := s.E.Insert(r.PathValue("id"), r.FormValue("image")); err != nil {
		toast(w, err.Error())
	}
	s.fragDrives(w, r)
}

func (s *Server) pageJob(w http.ResponseWriter, r *http.Request) {
	c, err := s.card(r.PathValue("id"), true)
	if err != nil {
		httpError(w, err)
		return
	}
	s.render(w, r, "job", c)
}

func (s *Server) pageHistory(w http.ResponseWriter, r *http.Request) {
	discs, err := s.E.St.Discs()
	if err != nil {
		httpError(w, err)
		return
	}
	s.render(w, r, "history", discs)
}

// --- Library -----------------------------------------------------------------

func (s *Server) pageLibrary(w http.ResponseWriter, r *http.Request) {
	lib, err := s.E.Library()
	if err != nil {
		httpError(w, err)
		return
	}
	pending := 0
	for _, f := range lib {
		if !f.Disc.Imported {
			pending++
		}
	}
	s.render(w, r, "library", map[string]any{"Folders": lib, "Pending": pending, "Filter": r.URL.Query().Get("show")})
}

var commonZones = []string{"Africa/Johannesburg", "Europe/London", "Europe/Amsterdam", "America/New_York", "America/Los_Angeles", "Australia/Sydney", "Asia/Dubai", "UTC"}

func (s *Server) pageFolder(w http.ResponseWriter, r *http.Request) {
	f, err := s.E.Folder(r.PathValue("id"))
	if err != nil {
		httpError(w, err)
		return
	}
	s.render(w, r, "folder", map[string]any{"F": f, "Zones": commonZones, "Saved": r.URL.Query().Get("saved") != ""})
}

func (s *Server) uiEditFolder(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	str := func(k string) *string { v := r.FormValue(k); return &v }
	ed := jobs.DiscEdit{Description: str("description"), Folder: str("folder"), Make: str("make"), Model: str("model"),
		TZ: str("tz"), DiscDate: str("disc_date"), Location: str("location")}
	if err := s.E.EditDisc(r.Context(), id, ed); err != nil {
		s.fragError(w, err)
		return
	}
	w.Header().Set("HX-Redirect", "/library/"+id+"?saved=1")
}

func (s *Server) uiImported(w http.ResponseWriter, r *http.Request) {
	v := r.FormValue("imported") == "on"
	if err := s.E.EditDisc(r.Context(), r.PathValue("id"), jobs.DiscEdit{Imported: &v}); err != nil {
		s.fragError(w, err)
		return
	}
	if v {
		fmt.Fprint(w, `<span class="pill ok">In Photos</span>`)
	} else {
		fmt.Fprint(w, `<span class="pill attn">Not yet</span>`)
	}
}

func (s *Server) uiClean(w http.ResponseWriter, r *http.Request) {
	if err := s.E.CleanDisc(r.Context(), r.PathValue("id")); err != nil {
		s.fragError(w, err)
		return
	}
	fmt.Fprint(w, `<p class="flash ok">Image and sources deleted. The MP4s are untouched.</p>`)
}

func (s *Server) uiAddSide(w http.ResponseWriter, r *http.Request) {
	if err := s.E.AddSide(r.PathValue("id")); err != nil {
		s.fragError(w, err)
		return
	}
	w.Header().Set("HX-Redirect", "/")
}

func (s *Server) uiRescan(w http.ResponseWriter, r *http.Request) {
	rep, err := s.E.Rescan(r.Context())
	if err != nil {
		s.fragError(w, err)
		return
	}
	s.frag(w, "rescan", rep)
}

// --- Rename ------------------------------------------------------------------

func (s *Server) pageRename(w http.ResponseWriter, r *http.Request) {
	lib, err := s.E.Library()
	if err != nil {
		httpError(w, err)
		return
	}
	batches, _ := s.E.St.Batches(20)
	s.render(w, r, "rename", map[string]any{"Folders": lib, "Batches": batches, "Preselect": r.URL.Query().Get("disc")})
}

func (s *Server) uiRenamePreview(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.fragError(w, err)
		return
	}
	var sel jobs.Selection
	for _, v := range r.Form["clip"] {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			sel.Clips = append(sel.Clips, id)
		}
	}
	sel.Folders = r.Form["folder"]
	sel.AllFiles = r.FormValue("all_files") != ""
	start, _ := strconv.Atoi(r.FormValue("start"))
	step, _ := strconv.Atoi(r.FormValue("step"))
	p := library.Pattern{Template: r.FormValue("template"), Start: start, Step: step, Find: r.FormValue("find"),
		Replace: r.FormValue("replace"), Regex: r.FormValue("regex") != "", Case: r.FormValue("case")}
	if len(sel.Clips) == 0 && len(sel.Folders) == 0 && !sel.AllFiles {
		fmt.Fprint(w, `<p class="hint">Tick files or folders on the left to see a preview.</p>`)
		return
	}
	pv, err := s.E.PreviewRename(sel, p)
	if err != nil {
		s.fragError(w, err)
		return
	}
	rel := func(dir string) string {
		r, _ := filepath.Rel(s.E.Cfg.Library, dir)
		if r == "." {
			return ""
		}
		return r
	}
	type row struct {
		library.Op
		Where string
	}
	var rows []row
	for _, o := range pv.Ops {
		rows = append(rows, row{o, rel(o.Dir)})
	}
	slices.SortStableFunc(rows, func(a, b row) int {
		if (a.Conflict != "") != (b.Conflict != "") {
			if a.Conflict != "" {
				return -1
			}
			return 1
		}
		return 0
	})
	s.frag(w, "rename-preview", map[string]any{"P": pv, "Rows": rows})
}

func (s *Server) uiRenameApply(w http.ResponseWriter, r *http.Request) {
	b, err := s.E.ApplyRename(r.FormValue("preview_id"))
	if err != nil {
		s.fragError(w, err)
		return
	}
	w.Header().Set("HX-Trigger", `{"renamed": true}`)
	fmt.Fprintf(w, `<p class="flash ok">Done: %s. <a href="#batches">Undo is below.</a></p>`, template.HTMLEscapeString(b.Summary))
}

func (s *Server) uiRenameUndo(w http.ResponseWriter, r *http.Request) {
	if err := s.E.UndoRename(r.PathValue("id")); err != nil {
		toast(w, err.Error())
	} else {
		w.Header().Set("HX-Trigger", `{"renamed": true, "toast": "Rename undone"}`)
	}
	batches, _ := s.E.St.Batches(20)
	s.frag(w, "batches", batches)
}

// --- Settings and doctor -----------------------------------------------------

func (s *Server) pageSettings(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "settings", map[string]any{"S": s.E.Settings(), "Zones": commonZones, "Saved": r.URL.Query().Get("saved") != "",
		"Cfg": s.E.Cfg})
}

func (s *Server) saveSettings(w http.ResponseWriter, r *http.Request) {
	set := s.E.Settings()
	set.Preset = pick(r.FormValue("preset"), set.Preset, "archive", "standard", "small", "copy")
	set.Speed = pick(r.FormValue("speed"), set.Speed, "best", "balanced", "fast")
	set.Split = pick(r.FormValue("split"), set.Split, "chapter", "title")
	set.HWAccel = pick(r.FormValue("hwaccel"), set.HWAccel, "", "vaapi", "nvenc")
	if tz := strings.TrimSpace(r.FormValue("timezone")); tz != "" {
		if _, err := time.LoadLocation(tz); err != nil {
			s.render(w, r, "settings", map[string]any{"S": set, "Zones": commonZones, "Cfg": s.E.Cfg, "Error": "Unknown time zone " + tz})
			return
		}
		set.TimeZone = tz
	}
	set.Make, set.Model = strings.TrimSpace(r.FormValue("make")), strings.TrimSpace(r.FormValue("model"))
	set.ResetDates = nil
	for _, d := range strings.FieldsFunc(r.FormValue("reset_dates"), func(r rune) bool { return r == ',' || r == ' ' || r == '\n' }) {
		if _, err := time.Parse("2006-01-02", d); err == nil {
			set.ResetDates = append(set.ResetDates, d)
		}
	}
	set.Combined = r.FormValue("combined") != ""
	set.AutoClean, _ = strconv.Atoi(r.FormValue("auto_clean"))
	set.Batch = r.FormValue("batch") != ""
	set.BatchSides, _ = strconv.Atoi(r.FormValue("batch_sides"))
	if set.BatchSides != 2 {
		set.BatchSides = 1
	}
	set.BatchDesc = strings.TrimSpace(r.FormValue("batch_desc"))
	if err := s.E.SaveSettings(set); err != nil {
		httpError(w, err)
		return
	}
	s.E.Hub.Publish("drives", "")
	http.Redirect(w, r, "/settings?saved=1", http.StatusSeeOther)
}

func pick(v, def string, allowed ...string) string {
	if slices.Contains(allowed, v) {
		return v
	}
	return def
}

func (s *Server) pageDoctor(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "doctor", s.lastReport())
}

func (s *Server) rerunDoctor(w http.ResponseWriter, r *http.Request) {
	s.SetReport(s.Doctor(r.Context()))
	http.Redirect(w, r, "/doctor", http.StatusSeeOther)
}
