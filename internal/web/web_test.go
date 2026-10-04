package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dnaidoo621/camdvd-rescue/internal/config"
	"github.com/dnaidoo621/camdvd-rescue/internal/doctor"
	"github.com/dnaidoo621/camdvd-rescue/internal/drive"
	"github.com/dnaidoo621/camdvd-rescue/internal/events"
	"github.com/dnaidoo621/camdvd-rescue/internal/jobs"
	"github.com/dnaidoo621/camdvd-rescue/internal/store"
	"github.com/dnaidoo621/camdvd-rescue/internal/tools"
)

func newServer(t *testing.T, password string) (*httptest.Server, *jobs.Engine) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Defaults()
	cfg.Library, cfg.StateDir, cfg.Password = filepath.Join(dir, "lib"), dir, password
	st, err := store.Open(filepath.Join(dir, "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ts := tools.Set{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	im := drive.NewImageDrive("demo", dir, ts)
	e := jobs.New(cfg, st, ts, events.NewHub(), log, []drive.Drive{im})
	ctx, cancel := context.WithCancel(context.Background())
	if err := e.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); e.Wait() })
	s, err := New(e, log, dir, func(context.Context) doctor.Report { return doctor.Report{OK: true} })
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv, e
}

func get(t *testing.T, c *http.Client, u string) (int, string) {
	t.Helper()
	resp, err := c.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestPagesRender(t *testing.T) {
	srv, _ := newServer(t, "")
	for _, p := range []string{"/", "/library", "/rename", "/history", "/settings", "/doctor", "/ui/drives", "/ui/active", "/api/drives", "/api/jobs", "/api/library", "/static/app.css"} {
		code, body := get(t, srv.Client(), srv.URL+p)
		if code != 200 {
			t.Errorf("%s: %d %s", p, code, body)
		}
	}
	_, body := get(t, srv.Client(), srv.URL+"/")
	if !strings.Contains(body, "Insert a camcorder disc") || !strings.Contains(body, `sse-connect="/api/events"`) {
		t.Error("dashboard missing empty state or SSE hookup")
	}
	if code, _ := get(t, srv.Client(), srv.URL+"/api/jobs/d-nope"); code != 404 {
		t.Errorf("unknown job: %d", code)
	}
}

func TestPasswordLogin(t *testing.T) {
	srv, _ := newServer(t, "s3cret")
	c := srv.Client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if code, _ := get(t, c, srv.URL+"/api/jobs"); code != 401 {
		t.Fatalf("api without login: %d", code)
	}
	if code, _ := get(t, c, srv.URL+"/library"); code != 303 {
		t.Fatalf("page without login: %d", code)
	}
	resp, _ := c.PostForm(srv.URL+"/login", url.Values{"password": {"wrong"}})
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("wrong password: %d", resp.StatusCode)
	}
	resp, _ = c.PostForm(srv.URL+"/login", url.Values{"password": {"s3cret"}, "next": {"/library"}})
	resp.Body.Close()
	if resp.StatusCode != 303 || resp.Header.Get("Location") != "/library" {
		t.Fatalf("login: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	var cookie *http.Cookie
	for _, ck := range resp.Cookies() {
		if ck.Name == cookieName {
			cookie = ck
		}
	}
	req, _ := http.NewRequest("GET", srv.URL+"/api/jobs", nil)
	req.AddCookie(cookie)
	r2, _ := c.Do(req)
	r2.Body.Close()
	if r2.StatusCode != 200 {
		t.Fatalf("with cookie: %d", r2.StatusCode)
	}
	// An open redirect via next is refused.
	resp, _ = c.PostForm(srv.URL+"/login", url.Values{"password": {"s3cret"}, "next": {"//evil.example"}})
	resp.Body.Close()
	if resp.Header.Get("Location") != "/" {
		t.Errorf("next not sanitized: %s", resp.Header.Get("Location"))
	}
}

func TestCrossOriginPostRefused(t *testing.T) {
	srv, _ := newServer(t, "")
	req, _ := http.NewRequest("POST", srv.URL+"/api/drives/demo/eject", nil)
	req.Header.Set("Origin", "http://evil.example")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("cross-origin POST: %d", resp.StatusCode)
	}
	req, _ = http.NewRequest("POST", srv.URL+"/api/drives/demo/eject", nil)
	req.Header.Set("Origin", srv.URL)
	resp, _ = srv.Client().Do(req)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("same-origin POST: %d", resp.StatusCode)
	}
}

func TestAPIErrors(t *testing.T) {
	srv, _ := newServer(t, "")
	resp, _ := srv.Client().Post(srv.URL+"/api/rename/preview", "application/json", strings.NewReader(`{"selection":{},"pattern":{}}`))
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("empty selection: %d", resp.StatusCode)
	}
	resp, _ = srv.Client().Post(srv.URL+"/api/jobs/d-x/answers", "application/json", strings.NewReader(`{"sides":3}`))
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("bad sides: %d", resp.StatusCode)
	}
}

func TestLocalRedirect(t *testing.T) {
	cases := map[string]string{
		"/library":             "/library",
		"/jobs/d-1?detail=1":   "/jobs/d-1?detail=1",
		"":                     "/",
		"https://evil.example": "/",
		"//evil.example":       "/",
		"/\\evil.example":      "/",
		"/%2F%2Fevil.example":  "/",
		"library":              "/",
		"/a\r\nSet-Cookie: x":  "/",
	}
	for in, want := range cases {
		if got := localRedirect(in); got != want {
			t.Errorf("localRedirect(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDemoInsertRefusesPaths(t *testing.T) {
	srv, _ := newServer(t, "")
	for _, img := range []string{"/etc/passwd", "../../etc/passwd", "nope.img"} {
		resp, err := srv.Client().Post(srv.URL+"/api/drives/demo/insert", "application/json", strings.NewReader(`{"image":"`+img+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode == 200 {
			t.Errorf("insert %q accepted", img)
		}
	}
}
