package tools

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Tool is one external dependency and how to check it.
type Tool struct {
	Name    string   // executable name
	Alt     []string // accepted alternatives, e.g. 7z for 7zz
	Package string   // apt package, or "bundled"
	Purpose string
	Version []string // args that print a version
	// Check inspects the tool beyond "it runs"; it returns a problem or "".
	Check func(ctx context.Context, s Set, path string) string
}

// Required lists every tool the pipeline calls.
var Required = []Tool{
	{Name: "ffmpeg", Package: "bundled", Purpose: "split, convert", Version: []string{"-hide_banner", "-version"}, Check: checkFFmpeg},
	{Name: "ffprobe", Package: "bundled", Purpose: "verify clips", Version: []string{"-hide_banner", "-version"}},
	{Name: "dvd-vr", Package: "bundled", Purpose: "split DVD-VR recordings", Version: []string{"--version"}},
	{Name: "ddrescue", Package: "gddrescue", Purpose: "image finalized discs", Version: []string{"--version"}},
	{Name: "sg_dd", Package: "sg3-utils", Purpose: "raw reads of unfinalized discs", Version: []string{"-V"}},
	{Name: "dvd+rw-mediainfo", Package: "dvd+rw-tools", Purpose: "media type and finalization state"},
	{Name: "7zz", Alt: []string{"7z"}, Package: "7zip", Purpose: "extract UDF/ISO9660 from images", Version: nil},
	{Name: "exiftool", Package: "libimage-exiftool-perl", Purpose: "Apple Photos date and camera tags", Version: []string{"-ver"}},
	{Name: "blkid", Package: "util-linux", Purpose: "detect file systems", Version: []string{"-V"}},
	{Name: "lsscsi", Package: "lsscsi", Purpose: "pair /dev/srN with /dev/sgN", Version: []string{"-V"}},
}

// Status is the doctor's verdict on one tool.
type Status struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Version string `json:"version"`
	OK      bool   `json:"ok"`
	Problem string `json:"problem,omitempty"`
	Fix     string `json:"fix,omitempty"`
}

// Resolve returns the name to run for t (the first alternative found).
func (s Set) Resolve(t Tool) (string, bool) {
	for _, n := range append([]string{t.Name}, t.Alt...) {
		p := s.Path(n)
		if _, err := exec.LookPath(p); err == nil {
			return n, true
		}
	}
	return t.Name, false
}

// SevenZip is the 7-Zip executable to use.
func (s Set) SevenZip() string {
	for _, t := range Required {
		if t.Name == "7zz" {
			n, _ := s.Resolve(t)
			return n
		}
	}
	return "7z"
}

var reVersion = regexp.MustCompile(`\d+\.\d+(\.\d+)?[\w.+-]*`)

// CheckAll runs every tool once and reports.
func (s Set) CheckAll(ctx context.Context) []Status {
	out := make([]Status, 0, len(Required))
	for _, t := range Required {
		st := Status{Name: t.Name}
		name, ok := s.Resolve(t)
		st.Path = s.Path(name)
		if !ok {
			st.Problem = "not installed"
			st.Fix = fixFor(t)
			out = append(out, st)
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		r, err := s.Run(cctx, Cmd{Name: name, Args: t.Version})
		cancel()
		text := r.Stdout + r.Stderr
		if t.Version != nil && err != nil && strings.TrimSpace(text) == "" {
			st.Problem = "doesn't run: " + err.Error()
			st.Fix = fixFor(t)
			out = append(out, st)
			continue
		}
		st.Version = reVersion.FindString(text)
		st.OK = true
		if t.Check != nil {
			if p := t.Check(ctx, s, name); p != "" {
				st.OK, st.Problem, st.Fix = false, p, fixFor(t)
			}
		}
		out = append(out, st)
	}
	return out
}

func fixFor(t Tool) string {
	if t.Package == "bundled" {
		return "Reinstall CamDVD Rescue; " + t.Name + " ships in /opt/camdvd/current/bin"
	}
	return "sudo apt install " + t.Package
}

func checkFFmpeg(ctx context.Context, s Set, name string) string {
	var missing []string
	for _, q := range []struct{ flag, want string }{
		{"-demuxers", "dvdvideo"},
		{"-encoders", "libx264"},
		{"-filters", "bwdif"},
	} {
		out, _ := s.Output(ctx, name, "-hide_banner", q.flag)
		if !regexp.MustCompile(`\b` + q.want + `\b`).MatchString(out) {
			missing = append(missing, q.want)
		}
	}
	if len(missing) > 0 {
		return fmt.Sprintf("FFmpeg lacks %s (need FFmpeg 7+ built with libdvdnav, libdvdread and libx264)", strings.Join(missing, ", "))
	}
	return ""
}
