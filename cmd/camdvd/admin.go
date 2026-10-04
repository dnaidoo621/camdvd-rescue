package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/dnaidoo621/camdvd-rescue/internal/config"
	"github.com/dnaidoo621/camdvd-rescue/internal/store"
)

const (
	optDir    = "/opt/camdvd"
	backupDir = "/var/lib/camdvd/backup"
)

func repo() string {
	if r := os.Getenv("CAMDVD_REPO"); r != "" {
		return r
	}
	return "dnaidoo621/camdvd-rescue"
}

func needRoot() error {
	if os.Geteuid() != 0 {
		return errors.New("run this with sudo")
	}
	return nil
}

func systemctl(args ...string) error {
	out, err := exec.Command("systemctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// activeJobs lists jobs that aren't finished, from the live database.
func activeJobs(cfg config.Config) ([]string, error) {
	st, err := store.Open(filepath.Join(cfg.StateDir, "camdvd.db"))
	if err != nil {
		return nil, err
	}
	defer st.Close()
	discs, err := st.Discs()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, d := range discs {
		if !d.State.Terminal() {
			out = append(out, fmt.Sprintf("%s (%s)", d.ID, d.State.Label()))
		}
	}
	return out, nil
}

type lastUpdate struct {
	From string    `json:"from"`
	To   string    `json:"to"`
	DB   string    `json:"db"`
	At   time.Time `json:"at"`
}

func update(args []string) error {
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	want := fs.String("version", "", "release tag to install (default: latest)")
	force := fs.Bool("force", false, "update even with unfinished jobs")
	_ = fs.Parse(args)
	if err := needRoot(); err != nil {
		return err
	}
	cfg, err := config.Load(config.DefaultPath)
	if err != nil {
		return err
	}
	if active, err := activeJobs(cfg); err != nil {
		return err
	} else if len(active) > 0 && !*force {
		return fmt.Errorf("jobs in progress: %s; finish or cancel them first (or use -force)", strings.Join(active, ", "))
	}

	tag := *want
	if tag == "" {
		tag, err = latestTag()
		if err != nil {
			return err
		}
	}
	cur, _ := os.Readlink(filepath.Join(optDir, "current"))
	if filepath.Base(cur) == tag {
		fmt.Println("Already on", tag)
		return nil
	}
	fmt.Println("Updating", filepath.Base(cur), "→", tag)

	// Back up the database before the new version migrates it.
	if err := os.MkdirAll(backupDir, 0o750); err != nil {
		return err
	}
	st, err := store.Open(filepath.Join(cfg.StateDir, "camdvd.db"))
	if err != nil {
		return err
	}
	bak := filepath.Join(backupDir, fmt.Sprintf("camdvd-%s-%s.db", filepath.Base(cur), time.Now().Format("20060102-150405")))
	err = st.Backup(bak)
	st.Close()
	if err != nil {
		return fmt.Errorf("backup database: %w", err)
	}

	dest := filepath.Join(optDir, tag)
	if err := fetchRelease(tag, dest); err != nil {
		return err
	}
	if err := switchTo(tag); err != nil {
		return err
	}
	b, _ := json.Marshal(lastUpdate{From: filepath.Base(cur), To: tag, DB: bak, At: time.Now()})
	_ = os.WriteFile(filepath.Join(backupDir, "last-update.json"), b, 0o640)
	if err := systemctl("restart", "camdvd"); err != nil {
		return err
	}
	fmt.Println("Updated to", tag, "— database backup at", bak)
	fmt.Println("If anything is wrong: sudo camdvd rollback")
	return nil
}

func rollback(args []string) error {
	if err := needRoot(); err != nil {
		return err
	}
	b, err := os.ReadFile(filepath.Join(backupDir, "last-update.json"))
	if err != nil {
		return errors.New("no update to roll back")
	}
	var lu lastUpdate
	if err := json.Unmarshal(b, &lu); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(optDir, lu.From)); err != nil {
		return fmt.Errorf("previous version %s is no longer in %s", lu.From, optDir)
	}
	cfg, err := config.Load(config.DefaultPath)
	if err != nil {
		return err
	}
	if err := systemctl("stop", "camdvd"); err != nil {
		return err
	}
	db := filepath.Join(cfg.StateDir, "camdvd.db")
	for _, suffix := range []string{"-wal", "-shm"} {
		_ = os.Remove(db + suffix)
	}
	if err := copyFile(lu.DB, db); err != nil {
		return fmt.Errorf("restore database: %w", err)
	}
	_ = exec.Command("chown", "camdvd:camdvd", db).Run()
	if err := switchTo(lu.From); err != nil {
		return err
	}
	if err := systemctl("start", "camdvd"); err != nil {
		return err
	}
	_ = os.Remove(filepath.Join(backupDir, "last-update.json"))
	fmt.Println("Rolled back to", lu.From, "and restored the database from", lu.DB)
	return nil
}

func uninstall(args []string) error {
	if err := needRoot(); err != nil {
		return err
	}
	script := filepath.Join(optDir, "current", "install.sh")
	if _, err := os.Stat(script); err != nil {
		return fmt.Errorf("%s not found; download install.sh and run it with --uninstall", script)
	}
	cmd := exec.Command("bash", append([]string{script, "--uninstall"}, args...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

func switchTo(tag string) error {
	tmp := filepath.Join(optDir, ".current.tmp")
	_ = os.Remove(tmp)
	if err := os.Symlink(tag, tmp); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(optDir, "current"))
}

func latestTag() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://api.github.com/repos/"+repo()+"/releases/latest", nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("GitHub releases: %s", resp.Status)
	}
	var r struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return "", err
	}
	return r.TagName, nil
}

func download(url string, w io.Writer) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	_, err = io.Copy(w, resp.Body)
	return err
}

// fetchRelease downloads, verifies and unpacks a release into dest.
func fetchRelease(tag, dest string) error {
	name := fmt.Sprintf("camdvd-%s-linux-x64.tar.gz", strings.TrimPrefix(tag, "v"))
	base := fmt.Sprintf("https://github.com/%s/releases/download/%s/", repo(), tag)
	tmp, err := os.CreateTemp("", "camdvd-*.tar.gz")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	fmt.Println("Downloading", name)
	h := sha256.New()
	if err := download(base+name, io.MultiWriter(tmp, h)); err != nil {
		return err
	}
	var sum strings.Builder
	if err := download(base+name+".sha256", &sum); err != nil {
		return err
	}
	want := strings.Fields(sum.String())
	if len(want) == 0 || want[0] != hex.EncodeToString(h.Sum(nil)) {
		return errors.New("checksum mismatch; download refused")
	}
	if _, err := tmp.Seek(0, 0); err != nil {
		return err
	}
	stage := dest + ".partial"
	_ = os.RemoveAll(stage)
	if err := untar(tmp, stage); err != nil {
		return err
	}
	_ = os.RemoveAll(dest)
	return os.Rename(stage, dest)
}

// untar unpacks a gzip tarball whose entries share one top-level folder.
func untar(r io.Reader, dest string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		parts := strings.SplitN(filepath.Clean(hdr.Name), string(filepath.Separator), 2)
		if len(parts) < 2 || strings.Contains(parts[1], "..") {
			continue
		}
		p := filepath.Join(dest, parts[1])
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, os.FileMode(hdr.Mode)&0o755)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		}
	}
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
