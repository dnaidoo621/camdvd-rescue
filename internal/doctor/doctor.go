// Package doctor is the self-check run on every start and at /api/doctor.
// Any failure blocks disc processing, and each line says how to fix it.
package doctor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/dnaidoo621/camdvd-rescue/internal/config"
	"github.com/dnaidoo621/camdvd-rescue/internal/drive"
	"github.com/dnaidoo621/camdvd-rescue/internal/tools"
)

// Check is one line of the report.
type Check struct {
	Group  string `json:"group"` // tools, drives, storage
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Warn   bool   `json:"warn,omitempty"` // shown, but doesn't block
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
}

// Report is the whole self-check.
type Report struct {
	At     time.Time `json:"at"`
	OK     bool      `json:"ok"`
	Checks []Check   `json:"checks"`
}

// Failures summarizes the blocking problems in one line.
func (r Report) Failures() string {
	var f []string
	for _, c := range r.Checks {
		if !c.OK && !c.Warn {
			f = append(f, c.Name+": "+c.Detail)
		}
	}
	return strings.Join(f, "; ")
}

// Run performs every check.
func Run(ctx context.Context, cfg config.Config, ts tools.Set, drives []drive.Drive) Report {
	r := Report{At: time.Now(), OK: true}
	add := func(c Check) {
		if !c.OK && !c.Warn {
			r.OK = false
		}
		r.Checks = append(r.Checks, c)
	}

	for _, st := range ts.CheckAll(ctx) {
		detail := st.Version
		if st.Path != "" && st.OK {
			detail = strings.TrimSpace(st.Version + " (" + st.Path + ")")
		}
		if !st.OK {
			detail = st.Problem
		}
		add(Check{Group: "tools", Name: st.Name, OK: st.OK, Detail: detail, Fix: st.Fix})
	}

	if len(drives) == 0 {
		add(Check{Group: "drives", Name: "drives", Detail: "no optical drive configured",
			Fix: "Plug in a USB DVD drive and re-run the installer, or add it to " + config.DefaultPath})
	}
	for _, d := range drives {
		for _, c := range checkDrive(ctx, ts, d) {
			add(c)
		}
	}

	if c, ok := checkVAAPI(ctx, ts); ok {
		add(c)
	}
	add(checkWritable("library", cfg.Library, cfg.MinFreeGB))
	add(checkWritable("state", cfg.StateDir, 0))
	return r
}

var reLoader = regexp.MustCompile(`Loading mechanism:\s*(\w+)`)

func checkDrive(ctx context.Context, ts tools.Set, d drive.Drive) []Check {
	info := d.Info()
	name := "drive " + info.ID
	if info.Demo {
		return []Check{{Group: "drives", Name: name, OK: true, Detail: "image drive serving " + info.Block}}
	}
	var out []Check
	fd, err := unix.Open(info.Block, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	switch {
	case err == unix.ENOENT:
		return append(out, Check{Group: "drives", Name: name, Detail: info.Block + " doesn't exist",
			Fix: "Check the drive is connected; lsscsi -g lists it"})
	case err != nil:
		return append(out, Check{Group: "drives", Name: name, Detail: fmt.Sprintf("can't open %s: %v", info.Block, err),
			Fix: "Add the service user to the cdrom group: sudo usermod -aG cdrom camdvd"})
	}
	unix.Close(fd)
	if _, err := d.Status(ctx); err != nil {
		out = append(out, Check{Group: "drives", Name: name, Detail: "drive status ioctl failed: " + err.Error()})
		return out
	}
	out = append(out, Check{Group: "drives", Name: name, OK: true, Detail: strings.TrimSpace(info.Model + " at " + info.Block)})

	if info.SG == "" {
		out = append(out, Check{Group: "drives", Name: name + " raw reads", Detail: "no SCSI generic device paired",
			Fix: "Set sg = \"/dev/sgN\" for this drive in " + config.DefaultPath + " (see lsscsi -g)"})
		return out
	}
	if err := unix.Access(info.SG, unix.R_OK|unix.W_OK); err != nil {
		out = append(out, Check{Group: "drives", Name: name + " raw reads", Detail: fmt.Sprintf("%s not accessible: %v", info.SG, err),
			Fix: "The cdrom group must own " + info.SG + "; check the distro's udev rules"})
		return out
	}
	// The sg device must be the same drive.
	if ds, err := drive.Discover(ctx, ts); err == nil {
		for _, x := range ds {
			if x.Block == info.Block && x.SG != info.SG {
				out = append(out, Check{Group: "drives", Name: name + " raw reads", Detail: fmt.Sprintf("%s pairs with %s, not %s", info.Block, x.SG, info.SG),
					Fix: "Fix sg in " + config.DefaultPath})
				return out
			}
		}
	}
	feat, _ := ts.Output(ctx, "sg_get_config", info.SG)
	ram := strings.Contains(feat, "profile: DVD-RAM")
	loader := "unknown"
	if m := reLoader.FindStringSubmatch(feat); m != nil {
		loader = strings.ToLower(m[1])
	}
	out = append(out, Check{Group: "drives", Name: name + " raw reads", OK: true, Detail: info.SG + " paired"})
	out = append(out, Check{Group: "drives", Name: name + " DVD-RAM", OK: ram, Warn: !ram,
		Detail: map[bool]string{true: "reads DVD-RAM", false: "doesn't list DVD-RAM support; DVD-RAM discs need another drive"}[ram]})
	slot := loader == "slot"
	out = append(out, Check{Group: "drives", Name: name + " loader", OK: !slot, Warn: slot,
		Detail: "loading mechanism: " + loader + map[bool]string{true: " — 8cm discs can jam in slot-loading drives", false: ""}[slot],
		Fix:    map[bool]string{true: "Use a tray-loading drive for 8cm discs", false: ""}[slot]})
	return out
}

// checkVAAPI tries a one-frame hardware encode when a GPU is present. It
// only warns: conversions fall back to x264 when VAAPI fails.
func checkVAAPI(ctx context.Context, ts tools.Set) (Check, bool) {
	const dev = "/dev/dri/renderD128"
	if _, err := os.Stat(dev); err != nil {
		return Check{}, false
	}
	c := Check{Group: "tools", Name: "VAAPI hardware encoding", OK: true}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	r, err := ts.Run(cctx, tools.Cmd{Name: "ffmpeg", Args: []string{"-hide_banner", "-nostdin", "-v", "error",
		"-vaapi_device", dev, "-f", "lavfi", "-i", "color=c=black:s=320x240:d=0.2", "-vf", "format=nv12,hwupload",
		"-c:v", "h264_vaapi", "-f", "null", "-"}})
	if err != nil {
		c.OK, c.Warn = false, true
		msg := strings.TrimSpace(r.Stderr)
		if i := strings.LastIndexByte(msg, '\n'); i >= 0 {
			msg = msg[i+1:]
		}
		if len(msg) > 160 {
			msg = msg[:160]
		}
		c.Detail = "not usable here (" + msg + "); x264 is used"
		c.Fix = "Optional. Needs the render group (re-run the installer with --hwaccel) and a libva new enough for the bundled FFmpeg"
		return c, true
	}
	c.Detail = "works (" + dev + ")"
	return c, true
}

func checkWritable(name, dir string, minGB float64) Check {
	c := Check{Group: "storage", Name: name}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		c.Detail, c.Fix = err.Error(), "Create "+dir+" and give the camdvd user write access"
		return c
	}
	probe := filepath.Join(dir, ".camdvd-write-test")
	if err := os.WriteFile(probe, []byte("ok"), 0o644); err != nil {
		c.Detail, c.Fix = "not writable: "+err.Error(), "sudo chown camdvd "+dir
		return c
	}
	os.Remove(probe)
	var s unix.Statfs_t
	if err := unix.Statfs(dir, &s); err == nil {
		free := float64(s.Bavail) * float64(s.Bsize) / 1e9
		c.Detail = fmt.Sprintf("%s, %.1f GB free", dir, free)
		if minGB > 0 && free < minGB {
			c.Detail += fmt.Sprintf(" (need %.0f GB per disc)", minGB)
			c.Fix = "Free space or choose a bigger library folder"
			return c
		}
	}
	c.OK = true
	return c
}
