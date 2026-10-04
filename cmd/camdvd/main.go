// Command camdvd turns camcorder DVDs into MP4s.
//
//	camdvd serve                  run the web app (what the systemd unit runs)
//	camdvd rip [flags]            the same pipeline headless, for one disc
//	camdvd carve IMAGE            recover recordings from a raw image
//	camdvd doctor                 check dependencies and devices
//	camdvd update | rollback      switch versions (root)
//	camdvd uninstall              remove the app; never touches the library
//	camdvd version
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/dnaidoo621/camdvd-rescue/internal/config"
	"github.com/dnaidoo621/camdvd-rescue/internal/doctor"
	"github.com/dnaidoo621/camdvd-rescue/internal/drive"
	"github.com/dnaidoo621/camdvd-rescue/internal/events"
	"github.com/dnaidoo621/camdvd-rescue/internal/jobs"
	"github.com/dnaidoo621/camdvd-rescue/internal/pipeline"
	"github.com/dnaidoo621/camdvd-rescue/internal/store"
	"github.com/dnaidoo621/camdvd-rescue/internal/tools"
	"github.com/dnaidoo621/camdvd-rescue/internal/web"
)

// version is set at build time with -ldflags "-X main.version=…".
var version = "dev"

func main() {
	web.Version = version
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "serve":
		err = serve(args)
	case "rip":
		err = rip(args)
	case "carve":
		err = carveCmd(args)
	case "doctor":
		err = doctorCmd(args)
	case "update":
		err = update(args)
	case "rollback":
		err = rollback(args)
	case "uninstall":
		err = uninstall(args)
	case "version", "--version", "-v":
		fmt.Println("camdvd", version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "camdvd: unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "camdvd:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `CamDVD Rescue `+version+` — camcorder DVDs to MP4

Usage:
  camdvd serve [-config FILE]          run the web app
  camdvd rip [flags]                   rip one disc without the web UI
  camdvd carve IMAGE -out DIR          recover recordings from a raw image
  camdvd doctor [-config FILE]         check tools, drives and storage
  sudo camdvd update [-version vX.Y.Z] install a newer release
  sudo camdvd rollback                 return to the previous release
  sudo camdvd uninstall [-purge]       remove the app (never the library)
  camdvd version
`)
}

func logger() *slog.Logger {
	// journald adds its own timestamps.
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
		if a.Key == slog.TimeKey && os.Getenv("JOURNAL_STREAM") != "" {
			return slog.Attr{}
		}
		return a
	}}))
}

// drivesFrom builds drives from config, discovering them when none are set.
func drivesFrom(ctx context.Context, cfg config.Config, ts tools.Set, log *slog.Logger) []drive.Drive {
	var ds []drive.Drive
	conf := cfg.Drives
	if len(conf) == 0 {
		found, err := drive.Discover(ctx, ts)
		if err != nil {
			log.Warn("drive discovery failed", "err", err)
		}
		for _, f := range found {
			conf = append(conf, config.Drive{ID: f.ID, Block: f.Block, SG: f.SG})
		}
	}
	for _, d := range conf {
		id := d.ID
		if id == "" {
			id = filepath.Base(d.Block)
		}
		ds = append(ds, drive.NewPhysicalDrive(id, d.Block, d.SG, ts))
	}
	if cfg.DemoImages != "" {
		ds = append(ds, drive.NewImageDrive("demo", cfg.DemoImages, ts))
	}
	return ds
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultPath, "config file")
	demo := fs.String("demo", "", "add an image drive serving the .img files in this folder")
	_ = fs.Parse(args)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if *demo != "" {
		cfg.DemoImages = *demo
	}
	log := logger()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := os.MkdirAll(cfg.StateDir, 0o750); err != nil {
		return err
	}
	st, err := store.Open(filepath.Join(cfg.StateDir, "camdvd.db"))
	if err != nil {
		return err
	}
	defer st.Close()

	ts := tools.Set{BinDir: cfg.ToolsDir}
	drives := drivesFrom(ctx, cfg, ts, log)
	hub := events.NewHub()
	eng := jobs.New(cfg, st, ts, hub, log, drives)

	runDoctor := func(ctx context.Context) doctor.Report {
		rep := doctor.Run(ctx, cfg, ts, drives)
		eng.SetBlocked(rep.Failures())
		for _, c := range rep.Checks {
			if !c.OK && !c.Warn {
				log.Error("self-check failed", "check", c.Name, "detail", c.Detail, "fix", c.Fix)
			}
		}
		return rep
	}
	rep := runDoctor(ctx)

	srv, err := web.New(eng, log, cfg.StateDir, runDoctor)
	if err != nil {
		return err
	}
	srv.SetReport(rep)
	if err := eng.Start(ctx); err != nil {
		return err
	}
	log.Info("CamDVD Rescue started", "version", version, "listen", cfg.Listen, "library", cfg.Library,
		"drives", len(drives), "selfcheck", rep.OK, "login", cfg.Password != "")
	err = srv.ListenAndServe(ctx, cfg.Listen)
	stop()
	eng.Wait()
	return err
}

// rip runs one disc through the engine without the web UI.
func rip(args []string) error {
	fs := flag.NewFlagSet("rip", flag.ExitOnError)
	dev := fs.String("device", "/dev/sr0", "block device")
	sg := fs.String("sg", "", "SCSI generic device for raw reads (default: discovered)")
	image := fs.String("image", "", "rip a disc image instead of a drive")
	out := fs.String("out", ".", "library folder to write into")
	sides := fs.Int("sides", 1, "1 or 2")
	desc := fs.String("desc", "", "description (names the folder and files)")
	preset := fs.String("preset", "standard", "archive, standard, small or copy")
	bin := fs.String("tools", "/opt/camdvd/current/bin", "folder with bundled ffmpeg and dvd-vr")
	_ = fs.Parse(args)

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	lib, err := filepath.Abs(*out)
	if err != nil {
		return err
	}
	cfg := config.Defaults()
	cfg.Library, cfg.StateDir, cfg.ToolsDir = lib, filepath.Join(lib, ".camdvd"), *bin
	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		return err
	}
	ts := tools.Set{BinDir: cfg.ToolsDir}
	st, err := store.Open(filepath.Join(cfg.StateDir, "rip.db"))
	if err != nil {
		return err
	}
	defer st.Close()

	var d drive.Drive
	if *image != "" {
		abs, _ := filepath.Abs(*image)
		d = drive.NewImageDrive("image", filepath.Dir(abs), ts)
	} else {
		sgDev := *sg
		if sgDev == "" {
			if found, err := drive.Discover(ctx, ts); err == nil {
				for _, f := range found {
					if f.Block == *dev {
						sgDev = f.SG
					}
				}
			}
		}
		d = drive.NewPhysicalDrive(filepath.Base(*dev), *dev, sgDev, ts)
	}
	hub := events.NewHub()
	eng := jobs.New(cfg, st, ts, hub, log, []drive.Drive{d})
	set := config.DefaultSettings()
	set.Preset = *preset
	set.Batch, set.BatchSides, set.BatchDesc = true, *sides, *desc
	if err := eng.SaveSettings(set); err != nil {
		return err
	}
	if rep := doctor.Run(ctx, cfg, ts, []drive.Drive{d}); !rep.OK {
		return fmt.Errorf("self-check failed: %s", rep.Failures())
	}
	if err := eng.Start(ctx); err != nil {
		return err
	}
	if im, ok := d.(*drive.ImageDrive); ok {
		abs, _ := filepath.Abs(*image)
		if err := im.InsertPath(abs); err != nil {
			return err
		}
	} else {
		fmt.Println("Insert a disc (or leave it in the drive)…")
	}

	ch, unsub := hub.Subscribe()
	defer unsub()
	var id, lastLine string
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			eng.Wait()
			return ctx.Err()
		case ev := <-ch:
			if ev.Name == "notice" {
				fmt.Println("·", ev.Data)
			}
		case <-tick.C:
		}
		if id == "" {
			if ds, _ := st.Discs(); len(ds) > 0 {
				id = ds[0].ID
			}
			continue
		}
		disc, err := st.Disc(id)
		if err != nil {
			continue
		}
		line := describe(disc)
		if line != lastLine {
			fmt.Println(line)
			lastLine = line
		}
		switch disc.State {
		case store.Done:
			clips, _ := st.Clips(id)
			fmt.Printf("\nDone: %d clips in %s\n", len(clips), filepath.Join(lib, disc.Folder))
			for _, w := range disc.Warnings {
				fmt.Println("  warning:", w)
			}
			stop()
			eng.Wait()
			return nil
		case store.Failed, store.Cancelled:
			stop()
			eng.Wait()
			return errors.New(disc.Message)
		}
	}
}

func describe(d *store.Disc) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[%s] %s", d.ID, d.State.Label())
	for _, s := range d.Sides {
		fmt.Fprintf(&b, " | side %s", s.Letter)
		if !s.Imaged {
			fmt.Fprintf(&b, " imaging %.0f%%", s.ImageDone*100)
		} else if s.Step != "" {
			fmt.Fprintf(&b, " %s", s.Step)
		}
		if s.Decision.Class != "" {
			fmt.Fprintf(&b, " (%s)", s.Decision.Class)
		}
	}
	if d.Message != "" {
		fmt.Fprintf(&b, " — %s", d.Message)
	}
	return b.String()
}

func carveCmd(args []string) error {
	fs := flag.NewFlagSet("carve", flag.ExitOnError)
	out := fs.String("out", "carved", "output folder")
	mp := fs.String("map", "", "ddrescue mapfile marking unreadable sectors")
	_ = fs.Parse(reorder(args))
	if fs.NArg() != 1 {
		return errors.New("usage: camdvd carve IMAGE [-out DIR] [-map MAPFILE]")
	}
	srcs, clips, err := pipeline.CarveSources(fs.Arg(0), *mp, *out)
	if err != nil {
		return err
	}
	for i, s := range srcs {
		c := clips[i]
		fmt.Printf("%s  sectors %d  ~%.1f s", filepath.Base(s.Path), c.Sectors, c.Duration())
		if c.BadSectors > 0 {
			fmt.Printf("  %d zero-filled", c.BadSectors)
		}
		fmt.Println()
	}
	fmt.Printf("%d recordings written to %s\n", len(srcs), *out)
	return nil
}

// reorder lets flags follow the positional argument.
func reorder(args []string) []string {
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		if strings.HasPrefix(args[i], "-") {
			flags = append(flags, args[i])
			if !strings.Contains(args[i], "=") && i+1 < len(args) {
				flags = append(flags, args[i+1])
				i++
			}
		} else {
			pos = append(pos, args[i])
		}
	}
	return append(flags, pos...)
}

func doctorCmd(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultPath, "config file")
	_ = fs.Parse(args)
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	ctx := context.Background()
	ts := tools.Set{BinDir: cfg.ToolsDir}
	rep := doctor.Run(ctx, cfg, ts, drivesFrom(ctx, cfg, ts, logger()))
	for _, c := range rep.Checks {
		mark := "ok  "
		if !c.OK {
			mark = "FAIL"
			if c.Warn {
				mark = "warn"
			}
		}
		fmt.Printf("%s  %-8s %-22s %s\n", mark, c.Group, c.Name, c.Detail)
		if !c.OK && c.Fix != "" {
			fmt.Printf("      fix: %s\n", c.Fix)
		}
	}
	if !rep.OK {
		return errors.New("self-check failed")
	}
	fmt.Println("All checks passed.")
	return nil
}
