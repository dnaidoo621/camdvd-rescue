// Package tools wraps the external programs the pipeline relies on. Every
// call goes through Run: argument slices (never a shell), its own process
// group so a cancel kills children too, and a bounded tail of stderr kept for
// the job's evidence.
package tools

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Set resolves tool names to paths. Bundled tools (FFmpeg, dvd-vr) are looked
// up in BinDir first; everything else comes from PATH.
type Set struct {
	BinDir string
}

// Path returns the executable to run for name.
func (s Set) Path(name string) string {
	if s.BinDir != "" {
		p := filepath.Join(s.BinDir, name)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return p
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	return name
}

// Cmd describes one invocation.
type Cmd struct {
	Name string
	Args []string
	Dir  string
	// Stdout receives standard output; nil collects it into Result.Stdout.
	Stdout io.Writer
	// OnLine is called for every line (split on \n or \r) of stdout, or of
	// stderr when LinesFromStderr is set. Used for progress.
	OnLine          func(string)
	LinesFromStderr bool
}

// Result is what a finished command left behind.
type Result struct {
	CmdLine string
	Stdout  string
	Stderr  string // last TailBytes of stderr
	Exit    int
}

// TailBytes bounds the stderr kept per command.
const TailBytes = 8 << 10

// Error is returned when a command exits non-zero.
type Error struct {
	Result Result
	Err    error
}

func (e *Error) Error() string {
	tail := strings.TrimSpace(e.Result.Stderr)
	if i := strings.LastIndexByte(tail, '\n'); i >= 0 && len(tail)-i < 300 {
		tail = tail[i+1:]
	} else if len(tail) > 300 {
		tail = tail[len(tail)-300:]
	}
	return fmt.Sprintf("%s: %v: %s", filepath.Base(strings.Fields(e.Result.CmdLine + " ?")[0]), e.Err, tail)
}

func (e *Error) Unwrap() error { return e.Err }

// Run executes c and waits. Cancelling ctx kills the whole process group.
func (s Set) Run(ctx context.Context, c Cmd) (Result, error) {
	path := s.Path(c.Name)
	cmd := exec.Command(path, c.Args...)
	cmd.Dir = c.Dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	res := Result{CmdLine: shellQuote(append([]string{path}, c.Args...))}

	var stdout bytes.Buffer
	stderr := &tailBuffer{max: TailBytes}
	var lineW *lineWriter
	if c.OnLine != nil {
		lineW = &lineWriter{fn: c.OnLine}
	}
	switch {
	case c.Stdout != nil && lineW != nil && !c.LinesFromStderr:
		cmd.Stdout = io.MultiWriter(c.Stdout, lineW)
	case c.Stdout != nil:
		cmd.Stdout = c.Stdout
	case lineW != nil && !c.LinesFromStderr:
		cmd.Stdout = io.MultiWriter(&stdout, lineW)
	default:
		cmd.Stdout = &stdout
	}
	if lineW != nil && c.LinesFromStderr {
		cmd.Stderr = io.MultiWriter(stderr, lineW)
	} else {
		cmd.Stderr = stderr
	}

	if err := cmd.Start(); err != nil {
		res.Exit = -1
		return res, &Error{res, err}
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			// SIGTERM the group, then SIGKILL if it lingers.
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
		case <-done:
		}
	}()
	err := cmd.Wait()
	close(done)
	if lineW != nil {
		lineW.flush()
	}
	res.Stdout = stdout.String()
	res.Stderr = stderr.String()
	if cmd.ProcessState != nil {
		res.Exit = cmd.ProcessState.ExitCode()
	}
	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	if err != nil {
		return res, &Error{res, err}
	}
	return res, nil
}

// Output runs a command and returns its stdout, for quick queries.
func (s Set) Output(ctx context.Context, name string, args ...string) (string, error) {
	r, err := s.Run(ctx, Cmd{Name: name, Args: args})
	return r.Stdout, err
}

type tailBuffer struct {
	mu  sync.Mutex
	max int
	b   []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > t.max {
		t.b = append(t.b[:0], t.b[len(t.b)-t.max:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.b)
}

// lineWriter splits on \n and \r, since progress tools redraw with \r.
type lineWriter struct {
	mu  sync.Mutex
	fn  func(string)
	buf []byte
}

func (l *lineWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf = append(l.buf, p...)
	for {
		i := bytes.IndexAny(l.buf, "\r\n")
		if i < 0 {
			break
		}
		if i > 0 {
			l.fn(string(l.buf[:i]))
		}
		l.buf = l.buf[i+1:]
	}
	return len(p), nil
}

func (l *lineWriter) flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.buf) > 0 {
		l.fn(string(l.buf))
		l.buf = nil
	}
}

func shellQuote(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		if a != "" && !strings.ContainsAny(a, " \t\n'\"\\$`!*?[](){}<>|&;#~") {
			q[i] = a
			continue
		}
		q[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(q, " ")
}

// IsNotFound reports whether err means the executable wasn't found.
func IsNotFound(err error) bool {
	return errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist)
}

// ScanLines is a helper for parsing tool output line by line.
func ScanLines(s string, fn func(string)) {
	sc := bufio.NewScanner(strings.NewReader(s))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		fn(sc.Text())
	}
}
