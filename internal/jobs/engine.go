// Package jobs runs the disc state machine: a watcher per drive, imaging
// while the drive is held, then per-side processing and a shared conversion
// queue. Every transition is persisted, so a restart resumes where it stopped.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/dnaidoo621/camdvd-rescue/internal/config"
	"github.com/dnaidoo621/camdvd-rescue/internal/drive"
	"github.com/dnaidoo621/camdvd-rescue/internal/events"
	"github.com/dnaidoo621/camdvd-rescue/internal/store"
	"github.com/dnaidoo621/camdvd-rescue/internal/tools"
)

// PollInterval is how often drives are polled.
var PollInterval = 2 * time.Second

// Engine owns the drives and all disc work.
type Engine struct {
	Cfg   config.Config
	St    *store.Store
	Tools tools.Set
	Hub   *events.Hub
	Log   *slog.Logger

	drives  []*driveCtl
	convSem chan struct{}

	mu      sync.Mutex
	ctx     context.Context
	cancels map[string]map[int]context.CancelFunc // disc id -> running work
	nextTok int
	blocked string // doctor failure that blocks disc processing
	gid     int    // shared group id, -1 if none
	wg      sync.WaitGroup
}

type driveCtl struct {
	d      drive.Drive
	mu     sync.Mutex
	status drive.TrayStatus
	holder string // disc id that owns the drive
	busy   bool   // probing or imaging right now
	notice string
}

// DriveState is a drive as the UI sees it.
type DriveState struct {
	drive.Info
	Status  string   `json:"status"`
	Holder  string   `json:"holder,omitempty"`
	Busy    bool     `json:"busy"`
	Notice  string   `json:"notice,omitempty"`
	Images  []string `json:"images,omitempty"` // demo drives
	Current string   `json:"current,omitempty"`
}

// New builds an engine over the given drives.
func New(cfg config.Config, st *store.Store, ts tools.Set, hub *events.Hub, log *slog.Logger, drives []drive.Drive) *Engine {
	e := &Engine{Cfg: cfg, St: st, Tools: ts, Hub: hub, Log: log,
		convSem: make(chan struct{}, cfg.ConvertWorkers), cancels: map[string]map[int]context.CancelFunc{}, gid: -1}
	for _, d := range drives {
		e.drives = append(e.drives, &driveCtl{d: d, status: -1})
	}
	if cfg.SharedGroup != "" {
		if g, err := user.LookupGroup(cfg.SharedGroup); err == nil {
			e.gid, _ = strconv.Atoi(g.Gid)
		} else {
			log.Warn("shared group not found", "group", cfg.SharedGroup, "err", err)
		}
	}
	return e
}

// SetBlocked records a doctor failure; while set, new discs aren't started.
func (e *Engine) SetBlocked(reason string) {
	e.mu.Lock()
	e.blocked = reason
	e.mu.Unlock()
	e.Hub.Publish("drives", "")
}

// Blocked returns the doctor failure, or "".
func (e *Engine) Blocked() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.blocked
}

// Start recovers unfinished jobs and starts the drive watchers.
func (e *Engine) Start(ctx context.Context) error {
	e.ctx = ctx
	if err := os.MkdirAll(e.workRoot(), 0o755); err != nil {
		return err
	}
	if err := e.recover(); err != nil {
		return err
	}
	for _, dc := range e.drives {
		e.wg.Add(1)
		go func() { defer e.wg.Done(); e.watch(ctx, dc) }()
	}
	e.wg.Add(1)
	go func() { defer e.wg.Done(); e.autoClean(ctx) }()
	return nil
}

// Wait blocks until watchers and work have stopped after ctx is cancelled.
func (e *Engine) Wait() { e.wg.Wait() }

// Settings returns the current runtime settings.
func (e *Engine) Settings() config.Settings {
	s := config.DefaultSettings()
	_, _ = e.St.Setting("settings", &s)
	return s
}

// SaveSettings stores runtime settings.
func (e *Engine) SaveSettings(s config.Settings) error {
	return e.St.SetSetting("settings", s)
}

func (e *Engine) workRoot() string         { return filepath.Join(e.Cfg.Library, ".camdvd") }
func (e *Engine) workDir(id string) string { return filepath.Join(e.workRoot(), id) }
func (e *Engine) imagePath(id, side string) string {
	return filepath.Join(e.workDir(id), "image", "side-"+side+".img")
}
func (e *Engine) mapPath(id, side string) string {
	return filepath.Join(e.workDir(id), "image", "side-"+side+".map")
}
func (e *Engine) sourceDir(id, side string) string {
	return filepath.Join(e.workDir(id), "source", "side-"+side)
}

// track registers cancellable work for a disc; call the returned func when
// the work ends.
func (e *Engine) track(id string) (context.Context, func()) {
	ctx, cancel := context.WithCancel(e.ctx)
	e.mu.Lock()
	if e.cancels[id] == nil {
		e.cancels[id] = map[int]context.CancelFunc{}
	}
	tok := e.nextTok
	e.nextTok++
	e.cancels[id][tok] = cancel
	e.mu.Unlock()
	e.wg.Add(1)
	return ctx, func() {
		cancel()
		e.mu.Lock()
		delete(e.cancels[id], tok)
		if len(e.cancels[id]) == 0 {
			delete(e.cancels, id)
		}
		e.mu.Unlock()
		e.wg.Done()
	}
}

func (e *Engine) cancelWork(id string) {
	e.mu.Lock()
	for _, c := range e.cancels[id] {
		c()
	}
	e.mu.Unlock()
}

// Running reports whether any work is active for the disc.
func (e *Engine) Running(id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.cancels[id]) > 0
}

// AnyRunning reports whether any disc has active work (blocks updates).
func (e *Engine) AnyRunning() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.cancels) > 0
}

func (e *Engine) driveFor(id string) *driveCtl {
	for _, dc := range e.drives {
		if dc.d.Info().ID == id {
			return dc
		}
	}
	return nil
}

// Drives reports every drive.
func (e *Engine) Drives() []DriveState {
	var out []DriveState
	for _, dc := range e.drives {
		dc.mu.Lock()
		ds := DriveState{Info: dc.d.Info(), Status: dc.status.String(), Holder: dc.holder, Busy: dc.busy, Notice: dc.notice}
		dc.mu.Unlock()
		if dc.status < 0 {
			ds.Status = "starting"
		}
		if im, ok := dc.d.(*drive.ImageDrive); ok {
			ds.Images = im.Images()
			ds.Current = filepath.Base(im.Current())
			if ds.Current == "." {
				ds.Current = ""
			}
		}
		out = append(out, ds)
	}
	return out
}

// Drive returns the underlying drive for an id.
func (e *Engine) Drive(id string) drive.Drive {
	if dc := e.driveFor(id); dc != nil {
		return dc.d
	}
	return nil
}

func (e *Engine) notice(dc *driveCtl, msg string) {
	dc.mu.Lock()
	dc.notice = msg
	dc.mu.Unlock()
	if msg != "" {
		e.Log.Info("drive notice", "drive", dc.d.Info().ID, "msg", msg)
		e.Hub.Publish("notice", msg)
	}
	e.Hub.Publish("drives", "")
}

func (e *Engine) setHolder(dc *driveCtl, id string) {
	dc.mu.Lock()
	dc.holder = id
	dc.mu.Unlock()
	e.Hub.Publish("drives", "")
}

func (e *Engine) holder(dc *driveCtl) string {
	dc.mu.Lock()
	defer dc.mu.Unlock()
	return dc.holder
}

// tryBusy marks a drive busy; false if it already is.
func (dc *driveCtl) tryBusy() bool {
	dc.mu.Lock()
	defer dc.mu.Unlock()
	if dc.busy {
		return false
	}
	dc.busy = true
	return true
}

func (dc *driveCtl) idle() {
	dc.mu.Lock()
	dc.busy = false
	dc.mu.Unlock()
}

// watch polls one drive and reacts to a disc arriving.
func (e *Engine) watch(ctx context.Context, dc *driveCtl) {
	t := time.NewTicker(PollInterval)
	defer t.Stop()
	for {
		st, err := dc.d.Status(ctx)
		if err != nil {
			st = drive.StatusNoInfo
		}
		dc.mu.Lock()
		prev := dc.status
		dc.status = st
		dc.mu.Unlock()
		if prev != st {
			e.Hub.Publish("drives", "")
		}
		if st == drive.StatusDiscOK && prev != drive.StatusDiscOK {
			if dc.tryBusy() {
				e.wg.Add(1)
				go func() {
					defer e.wg.Done()
					defer dc.idle()
					e.onInsert(dc)
					e.Hub.Publish("drives", "")
				}()
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// update saves a disc change and tells the browsers.
// The job list is re-rendered only when a state changes, so progress ticks
// don't redraw (and wipe) an answers form being typed into.
func (e *Engine) update(id string, fn func(*store.Disc) error) (*store.Disc, error) {
	var before store.State
	d, err := e.St.UpdateDisc(id, func(d *store.Disc) error { before = d.State; return fn(d) })
	if err == nil {
		e.Hub.Publish("job-"+id, "")
		if d.State != before {
			e.Hub.Publish("jobs", "")
		}
	}
	return d, err
}

func (e *Engine) fail(id, msg string) {
	e.Log.Error("job failed", "disc", id, "msg", msg)
	_, _ = e.update(id, func(d *store.Disc) error {
		if d.State == store.Cancelled {
			return nil
		}
		d.State, d.Message = store.Failed, msg
		return nil
	})
}

// freeBytes returns free space in the library.
func (e *Engine) freeBytes() int64 {
	var s unix.Statfs_t
	if err := unix.Statfs(e.Cfg.Library, &s); err != nil {
		return -1
	}
	return int64(s.Bavail) * int64(s.Bsize)
}

// needSpace reports a problem if fewer than need bytes plus the configured
// reserve are free.
func (e *Engine) needSpace(need int64) string {
	free := e.freeBytes()
	reserve := int64(e.Cfg.MinFreeGB * 1e9 / 5) // keep a fifth of the 5 GB guideline spare
	if free >= 0 && free < need+reserve {
		return fmt.Sprintf("Library is full: %.1f GB free, %.1f GB needed. Free some space and press Resume.",
			float64(free)/1e9, float64(need+reserve)/1e9)
	}
	return ""
}

// own gives a path to the shared group, group-writable, so the library is
// usable over SMB.
func (e *Engine) own(p string) {
	if e.gid < 0 {
		return
	}
	_ = os.Lchown(p, -1, e.gid)
	if fi, err := os.Stat(p); err == nil {
		mode := fi.Mode().Perm() | 0o060
		if fi.IsDir() {
			mode |= 0o010 | os.ModeSetgid
		}
		_ = os.Chmod(p, mode)
	}
}

var errStop = errors.New("stop")
