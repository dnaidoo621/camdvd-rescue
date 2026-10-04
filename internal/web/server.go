// Package web serves the UI (html/template + htmx, everything embedded) and
// the JSON API, with Server-Sent Events for live progress.
package web

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dnaidoo621/camdvd-rescue/internal/doctor"
	"github.com/dnaidoo621/camdvd-rescue/internal/jobs"
	"github.com/dnaidoo621/camdvd-rescue/internal/library"
	"github.com/dnaidoo621/camdvd-rescue/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// Version is shown in the footer; set by main.
var Version = "dev"

// Server is the HTTP side of the app.
type Server struct {
	E   *jobs.Engine
	Log *slog.Logger
	// Doctor re-runs the self-check.
	Doctor func(ctx context.Context) doctor.Report

	pages     map[string]*template.Template
	frags     *template.Template
	secret    []byte
	mu        sync.Mutex
	report    doctor.Report
	loginFail map[string][]time.Time
}

// New builds the server. stateDir holds the session key.
func New(e *jobs.Engine, log *slog.Logger, stateDir string, doc func(context.Context) doctor.Report) (*Server, error) {
	pages, frags, err := loadTemplates()
	if err != nil {
		return nil, err
	}
	s := &Server{E: e, Log: log, Doctor: doc, pages: pages, frags: frags, loginFail: map[string][]time.Time{}}
	s.secret, err = sessionKey(filepath.Join(stateDir, "session.key"))
	if err != nil {
		return nil, err
	}
	return s, nil
}

// SetReport stores the latest doctor report.
func (s *Server) SetReport(r doctor.Report) {
	s.mu.Lock()
	s.report = r
	s.mu.Unlock()
}

func (s *Server) lastReport() doctor.Report {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.report
}

func sessionKey(path string) ([]byte, error) {
	if b, err := os.ReadFile(path); err == nil && len(b) >= 32 {
		return b, nil
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	return b, os.WriteFile(path, b, 0o600)
}

// Handler returns the routed handler.
func (s *Server) Handler() http.Handler {
	m := http.NewServeMux()
	static, _ := fs.Sub(staticFS, "static")
	m.Handle("GET /static/", http.StripPrefix("/static/", cacheFor(http.FileServerFS(static), 24*time.Hour)))

	// Pages.
	m.HandleFunc("GET /{$}", s.pageDashboard)
	m.HandleFunc("GET /jobs/{id}", s.pageJob)
	m.HandleFunc("GET /history", s.pageHistory)
	m.HandleFunc("GET /library", s.pageLibrary)
	m.HandleFunc("GET /library/{id}", s.pageFolder)
	m.HandleFunc("GET /rename", s.pageRename)
	m.HandleFunc("GET /settings", s.pageSettings)
	m.HandleFunc("POST /settings", s.saveSettings)
	m.HandleFunc("GET /doctor", s.pageDoctor)
	m.HandleFunc("POST /doctor", s.rerunDoctor)
	m.HandleFunc("GET /login", s.pageLogin)
	m.HandleFunc("POST /login", s.doLogin)
	m.HandleFunc("POST /logout", s.doLogout)

	// htmx fragments.
	m.HandleFunc("GET /ui/drives", s.fragDrives)
	m.HandleFunc("GET /ui/active", s.fragActive)
	m.HandleFunc("GET /ui/jobs/{id}/card", s.fragCard)
	m.HandleFunc("POST /ui/jobs/{id}/answers", s.uiAnswers)
	m.HandleFunc("POST /ui/jobs/{id}/{action}", s.uiJobAction)
	m.HandleFunc("POST /ui/drives/{id}/eject", s.uiEject)
	m.HandleFunc("POST /ui/drives/{id}/insert", s.uiInsert)
	m.HandleFunc("POST /ui/library/{id}", s.uiEditFolder)
	m.HandleFunc("POST /ui/library/{id}/imported", s.uiImported)
	m.HandleFunc("POST /ui/library/{id}/clean", s.uiClean)
	m.HandleFunc("POST /ui/library/{id}/add-side", s.uiAddSide)
	m.HandleFunc("POST /ui/rescan", s.uiRescan)
	m.HandleFunc("POST /ui/rename/preview", s.uiRenamePreview)
	m.HandleFunc("POST /ui/rename/apply", s.uiRenameApply)
	m.HandleFunc("POST /ui/rename/undo/{id}", s.uiRenameUndo)

	// JSON API (see the spec's API table).
	m.HandleFunc("GET /api/drives", s.apiDrives)
	m.HandleFunc("POST /api/drives/{id}/eject", s.apiEject)
	m.HandleFunc("POST /api/drives/{id}/insert", s.apiInsert)
	m.HandleFunc("GET /api/jobs", s.apiJobs)
	m.HandleFunc("GET /api/jobs/{id}", s.apiJob)
	m.HandleFunc("POST /api/jobs/{id}/answers", s.apiAnswers)
	m.HandleFunc("POST /api/jobs/{id}/{action}", s.apiJobAction)
	m.HandleFunc("DELETE /api/jobs/{id}", s.apiDeleteJob)
	m.HandleFunc("GET /api/library", s.apiLibrary)
	m.HandleFunc("PATCH /api/library/folders/{id}", s.apiPatchFolder)
	m.HandleFunc("POST /api/library/rescan", s.apiRescan)
	m.HandleFunc("POST /api/rename/preview", s.apiRenamePreview)
	m.HandleFunc("POST /api/rename/apply", s.apiRenameApply)
	m.HandleFunc("POST /api/rename/undo/{batchId}", s.apiRenameUndo)
	m.HandleFunc("GET /api/rename/batches", s.apiBatches)
	m.HandleFunc("GET /api/files/{id}/stream", s.apiStream)
	m.HandleFunc("GET /api/files/{id}/thumb", s.apiThumb)
	m.HandleFunc("GET /api/doctor", s.apiDoctor)
	m.HandleFunc("GET /api/events", s.apiEvents)
	m.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]string{"status": "ok", "version": Version})
	})

	return s.logRequests(s.auth(s.sameOrigin(m)))
}

// ListenAndServe runs until ctx is cancelled.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	srv := &http.Server{Addr: addr, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	err := srv.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func cacheFor(h http.Handler, d time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", int(d.Seconds())))
		h.ServeHTTP(w, r)
	})
}

func (s *Server) logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			s.Log.Info("request", "method", r.Method, "path", r.URL.Path, "remote", r.RemoteAddr)
		}
		h.ServeHTTP(w, r)
	})
}

// sameOrigin rejects cross-site state changes: a browser always sends
// Origin on POST/PATCH/DELETE, and it must name this host.
func (s *Server) sameOrigin(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if o := r.Header.Get("Origin"); o != "" {
				u, err := url.Parse(o)
				if err != nil || u.Host != r.Host {
					http.Error(w, "cross-origin request refused", http.StatusForbidden)
					return
				}
			}
		}
		h.ServeHTTP(w, r)
	})
}

// --- Sessions ---------------------------------------------------------------

const cookieName = "camdvd_session"

func (s *Server) passwordOn() bool { return s.E.Cfg.Password != "" }

// sign makes a session token. The password is part of the HMAC key, so
// changing CAMDVD_PASSWORD invalidates every existing session.
func (s *Server) sign(exp int64) string {
	key := append(append([]byte{}, s.secret...), s.E.Cfg.Password...)
	m := hmac.New(sha256.New, key)
	fmt.Fprintf(m, "camdvd-session|%d", exp)
	return strconv.FormatInt(exp, 10) + "." + hex.EncodeToString(m.Sum(nil))
}

func (s *Server) validSession(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return false
	}
	expStr, _, ok := strings.Cut(c.Value, ".")
	if !ok {
		return false
	}
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return false
	}
	return hmac.Equal([]byte(c.Value), []byte(s.sign(exp)))
}

func (s *Server) auth(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.passwordOn() || s.validSession(r) || r.URL.Path == "/login" || strings.HasPrefix(r.URL.Path, "/static/") || r.URL.Path == "/api/health" {
			h.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.Error(w, "login required", http.StatusUnauthorized)
			return
		}
		if r.Header.Get("HX-Request") != "" {
			w.Header().Set("HX-Redirect", "/login")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
	})
}

func (s *Server) pageLogin(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "login", map[string]any{"Next": r.URL.Query().Get("next")})
}

func (s *Server) doLogin(w http.ResponseWriter, r *http.Request) {
	host := r.RemoteAddr
	if i := strings.LastIndexByte(host, ':'); i > 0 {
		host = host[:i]
	}
	s.mu.Lock()
	var recent []time.Time
	for _, t := range s.loginFail[host] {
		if time.Since(t) < 10*time.Minute {
			recent = append(recent, t)
		}
	}
	s.loginFail[host] = recent
	s.mu.Unlock()
	if len(recent) >= 10 {
		s.render(w, r, "login", map[string]any{"Error": "Too many attempts. Try again in a few minutes."})
		return
	}
	pw := r.FormValue("password")
	if !s.passwordOn() || subtle.ConstantTimeCompare([]byte(pw), []byte(s.E.Cfg.Password)) != 1 {
		s.mu.Lock()
		s.loginFail[host] = append(s.loginFail[host], time.Now())
		s.mu.Unlock()
		w.WriteHeader(http.StatusUnauthorized)
		s.render(w, r, "login", map[string]any{"Error": "Wrong password.", "Next": r.FormValue("next")})
		return
	}
	exp := time.Now().Add(30 * 24 * time.Hour).Unix()
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: s.sign(exp), Path: "/", HttpOnly: true,
		Secure: overTLS(r), SameSite: http.SameSiteStrictMode, Expires: time.Unix(exp, 0)})
	http.Redirect(w, r, localRedirect(r.FormValue("next")), http.StatusSeeOther)
}

// localRedirect returns next if it's a path on this site, else "/". It
// refuses scheme-relative ("//host") and backslash ("/\host") forms, which
// browsers treat as other hosts.
func localRedirect(next string) string {
	if next == "" || next[0] != '/' || strings.ContainsAny(next, "\\\r\n") {
		return "/"
	}
	u, err := url.Parse(next)
	if err != nil || u.IsAbs() || u.Host != "" || strings.HasPrefix(u.Path, "//") {
		return "/"
	}
	return u.RequestURI()
}

// overTLS reports whether the browser reached us over HTTPS, directly or
// through a reverse proxy. The cookie is Secure only then: on a plain-HTTP
// LAN, the default, a Secure cookie would never be sent back.
func overTLS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func (s *Server) doLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true,
		Secure: overTLS(r), SameSite: http.SameSiteStrictMode})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// --- SSE ---------------------------------------------------------------------

func (s *Server) apiEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	ch, stop := s.E.Hub.Subscribe()
	defer stop()
	fmt.Fprint(w, "retry: 3000\n\n")
	fl.Flush()
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		case ev, ok := <-ch:
			if !ok {
				return
			}
			data := ev.Data
			if data == "" {
				data = "{}"
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Name, strings.ReplaceAll(data, "\n", "\ndata: "))
			fl.Flush()
		}
	}
}

// --- Files -------------------------------------------------------------------

func (s *Server) clipFile(w http.ResponseWriter, r *http.Request) (*store.Clip, string, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return nil, "", false
	}
	c, err := s.E.St.Clip(id)
	if err != nil {
		http.Error(w, "no such file", http.StatusNotFound)
		return nil, "", false
	}
	var p string
	switch {
	case c.Output != "":
		p, err = library.Within(s.E.Cfg.Library, c.Output)
	case c.Work != "":
		p = c.Work
	default:
		http.Error(w, "not converted yet", http.StatusNotFound)
		return nil, "", false
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return nil, "", false
	}
	return c, p, true
}

// apiStream serves a clip with range requests, for the preview player.
func (s *Server) apiStream(w http.ResponseWriter, r *http.Request) {
	_, p, ok := s.clipFile(w, r)
	if !ok {
		return
	}
	f, err := os.Open(p)
	if err != nil {
		http.Error(w, "file missing; try Rescan in the library", http.StatusNotFound)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if strings.EqualFold(filepath.Ext(p), ".mkv") {
		w.Header().Set("Content-Type", "video/x-matroska")
	} else {
		w.Header().Set("Content-Type", "video/mp4")
	}
	if r.URL.Query().Get("download") != "" {
		w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(filepath.Base(p)))
	}
	http.ServeContent(w, r, filepath.Base(p), fi.ModTime(), f)
}

func (s *Server) apiThumb(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	c, err := s.E.St.Clip(id)
	if err != nil || c.Thumb == "" {
		http.NotFound(w, r)
		return
	}
	// Thumbs live in the disc's working folder, never user-supplied paths.
	if !strings.HasPrefix(c.Thumb, filepath.Join(s.E.Cfg.Library, ".camdvd")+string(filepath.Separator)) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "max-age=3600")
	http.ServeFile(w, r, c.Thumb)
}

// --- Helpers -----------------------------------------------------------------

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func httpError(w http.ResponseWriter, err error) {
	code := http.StatusBadRequest
	switch {
	case errors.Is(err, store.ErrNotFound):
		code = http.StatusNotFound
	case errors.Is(err, jobs.ErrBusy), errors.Is(err, library.ErrConflicts):
		code = http.StatusConflict
	}
	http.Error(w, err.Error(), code)
}

func readJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}
