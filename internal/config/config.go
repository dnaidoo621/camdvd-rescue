// Package config loads /etc/camdvd/config.toml (written by the installer)
// and defines the runtime settings that the UI edits.
package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"
)

// DefaultPath is where the installer writes the config.
const DefaultPath = "/etc/camdvd/config.toml"

// Drive is one configured optical drive.
type Drive struct {
	ID    string `toml:"id"`
	Block string `toml:"block"`
	SG    string `toml:"sg"`
}

// Config is the install-time configuration.
type Config struct {
	Listen      string  `toml:"listen"`       // 127.0.0.1:8780 or 0.0.0.0:8780
	Library     string  `toml:"library"`      // output folder
	StateDir    string  `toml:"state_dir"`    // database, logs
	ToolsDir    string  `toml:"tools_dir"`    // bundled ffmpeg, dvd-vr
	SharedGroup string  `toml:"shared_group"` // group for output files, e.g. for SMB
	Drives      []Drive `toml:"drives"`
	// DemoImages adds an image drive serving the .img files in this folder.
	DemoImages string `toml:"demo_images"`
	// ConvertWorkers is how many encodes run at once (default 1).
	ConvertWorkers int `toml:"convert_workers"`
	// MinFreeGB is the space required before each stage (default 5).
	MinFreeGB float64 `toml:"min_free_gb"`

	// Password comes from CAMDVD_PASSWORD in /etc/camdvd/env, never the file.
	Password string `toml:"-"`
}

// Defaults are used for anything the file leaves out.
func Defaults() Config {
	return Config{
		Listen:         "127.0.0.1:8780",
		Library:        "/srv/camdvd/library",
		StateDir:       "/var/lib/camdvd",
		ToolsDir:       "/opt/camdvd/current/bin",
		ConvertWorkers: 1,
		MinFreeGB:      5,
	}
}

// Load reads the config file (a missing file means defaults) and the
// environment.
func Load(path string) (Config, error) {
	c := Defaults()
	if _, err := os.Stat(path); err == nil {
		if _, err := toml.DecodeFile(path, &c); err != nil {
			return c, fmt.Errorf("%s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return c, err
	}
	if v := os.Getenv("CAMDVD_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("CAMDVD_LIBRARY"); v != "" {
		c.Library = v
	}
	if v := os.Getenv("CAMDVD_STATE_DIR"); v != "" {
		c.StateDir = v
	}
	if v := os.Getenv("CAMDVD_TOOLS_DIR"); v != "" {
		c.ToolsDir = v
	}
	c.Password = os.Getenv("CAMDVD_PASSWORD")
	if c.ConvertWorkers < 1 {
		c.ConvertWorkers = 1
	}
	if c.MinFreeGB <= 0 {
		c.MinFreeGB = 5
	}
	return c, c.Validate()
}

// Validate checks the values that would otherwise fail later and obscurely.
func (c Config) Validate() error {
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		return fmt.Errorf("listen %q: %w", c.Listen, err)
	}
	for _, p := range []string{c.Library, c.StateDir} {
		if !filepath.IsAbs(p) {
			return fmt.Errorf("path %q must be absolute", p)
		}
	}
	return nil
}

// Write saves the config as TOML.
func (c Config) Write(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	fmt.Fprintln(f, "# CamDVD Rescue configuration. Restart camdvd after editing:")
	fmt.Fprintln(f, "#   sudo systemctl restart camdvd")
	fmt.Fprintln(f, "# The login password lives in /etc/camdvd/env as CAMDVD_PASSWORD.")
	return toml.NewEncoder(f).Encode(c)
}

// Settings are changed from the UI and stored in the database.
type Settings struct {
	Preset     string   `json:"preset"`   // archive, standard, small, copy
	Split      string   `json:"split"`    // chapter or title, for DVD-Video
	HWAccel    string   `json:"hwaccel"`  // "", vaapi, nvenc
	TimeZone   string   `json:"timezone"` // IANA zone
	Make       string   `json:"make"`     // default camera
	Model      string   `json:"model"`
	ResetDates []string `json:"reset_dates"` // camera clock defaults treated as unknown
	Combined   bool     `json:"combined"`    // also write one MP4 per disc with chapters
	AutoClean  int      `json:"auto_clean"`  // days after verification to delete image and sources; 0 = never
	LastSides  int      `json:"last_sides"`  // default for the sides question
	Batch      bool     `json:"batch"`       // answer every disc with the batch preset
	BatchSides int      `json:"batch_sides"`
	BatchDesc  string   `json:"batch_desc"`
}

// DefaultSettings match the spec.
func DefaultSettings() Settings {
	return Settings{
		Preset:     "standard",
		Split:      "chapter",
		TimeZone:   "Africa/Johannesburg",
		ResetDates: []string{"2004-01-01"},
		LastSides:  1,
		BatchSides: 1,
	}
}

// Location returns the settings' time zone, falling back to UTC.
func (s Settings) Location() *time.Location {
	if l, err := time.LoadLocation(s.TimeZone); err == nil {
		return l
	}
	return time.UTC
}
