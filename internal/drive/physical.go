package drive

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/dnaidoo621/camdvd-rescue/internal/discinfo"
	"github.com/dnaidoo621/camdvd-rescue/internal/tools"
)

// Linux cdrom ioctls (linux/cdrom.h).
const (
	cdromEject       = 0x5309
	cdromCloseTray   = 0x5319
	cdromDriveStatus = 0x5326
	cdromLockDoor    = 0x5329
	cdslCurrent      = 0x7FFFFFFF
)

// PhysicalDrive is a real drive at /dev/srN with its SCSI generic /dev/sgN.
type PhysicalDrive struct {
	id, block, sg string
	tools         tools.Set
	mu            sync.Mutex // one ioctl/tool at a time per drive
}

// NewPhysicalDrive wraps a drive; sg may be "" if raw reads aren't needed.
func NewPhysicalDrive(id, block, sg string, ts tools.Set) *PhysicalDrive {
	return &PhysicalDrive{id: id, block: block, sg: sg, tools: ts}
}

func (d *PhysicalDrive) Info() Info {
	name := filepath.Base(d.block)
	read := func(f string) string {
		b, _ := os.ReadFile(filepath.Join("/sys/class/block", name, "device", f))
		return strings.TrimSpace(string(b))
	}
	model := strings.TrimSpace(read("vendor") + " " + read("model"))
	return Info{ID: d.id, Block: d.block, SG: d.sg, Model: model}
}

func (d *PhysicalDrive) open() (int, error) {
	return unix.Open(d.block, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
}

// Status polls CDROM_DRIVE_STATUS; it never spins the disc up.
func (d *PhysicalDrive) Status(context.Context) (TrayStatus, error) {
	fd, err := d.open()
	if err != nil {
		return StatusNoInfo, err
	}
	defer unix.Close(fd)
	r, err := unix.IoctlRetInt(fd, cdromDriveStatus)
	if err != nil {
		// Some drives want the slot argument.
		r2, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), cdromDriveStatus, cdslCurrent)
		if errno != 0 {
			return StatusNoInfo, errno
		}
		r = int(r2)
	}
	if r < 0 || r > int(StatusDiscOK) {
		return StatusNoInfo, nil
	}
	return TrayStatus(r), nil
}

// Eject unmounts the disc if a desktop mounted it, unlocks the door and
// ejects.
func (d *PhysicalDrive) Eject(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.unmount(ctx); err != nil {
		return err
	}
	fd, err := d.open()
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	_, _, _ = unix.Syscall(unix.SYS_IOCTL, uintptr(fd), cdromLockDoor, 0)
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), cdromEject, 0); errno != 0 {
		return fmt.Errorf("eject %s: %w", d.block, errno)
	}
	return nil
}

// Mounted reports where the disc is mounted, if anywhere.
func (d *PhysicalDrive) Mounted() string {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return ""
	}
	defer f.Close()
	real, _ := filepath.EvalSymlinks(d.block)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fs := strings.Fields(sc.Text())
		// ... mountpoint is field 4; the source follows the " - " separator.
		for i, x := range fs {
			if x == "-" && i+2 < len(fs) && (fs[i+2] == d.block || fs[i+2] == real) {
				return strings.ReplaceAll(fs[4], `\040`, " ")
			}
		}
	}
	return ""
}

// unmount releases a desktop automount through udisks, which the service
// user may do without root on a local seat.
func (d *PhysicalDrive) unmount(ctx context.Context) error {
	if d.Mounted() == "" {
		return nil
	}
	_, err := d.tools.Output(ctx, "udisksctl", "unmount", "--no-user-interaction", "-b", d.block)
	if err != nil && d.Mounted() != "" {
		return fmt.Errorf("%s is mounted at %s and couldn't be unmounted (install the udev rule from the installer): %w", d.block, d.Mounted(), err)
	}
	return nil
}

func (d *PhysicalDrive) Probe(ctx context.Context) (discinfo.Probe, string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.unmount(ctx); err != nil {
		return discinfo.Probe{}, "", err
	}
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	r, err := d.tools.Run(cctx, tools.Cmd{Name: "dvd+rw-mediainfo", Args: []string{d.block}})
	text := r.Stdout + r.Stderr
	evidence := "$ " + r.CmdLine + "\n" + text
	m := discinfo.ParseMediaInfo(text)
	if err != nil && !m.NoMedia && m.Type == "" {
		return discinfo.Probe{}, evidence, fmt.Errorf("dvd+rw-mediainfo: %w", err)
	}
	p := discinfo.Probe{Media: m}
	if m.NoMedia || m.DiscStatus == "blank" {
		return p, evidence, nil
	}
	// blkid reads the start of the disc; on an open session that area is
	// unwritten and may take the drive a while to give up on.
	bctx, bcancel := context.WithTimeout(ctx, 45*time.Second)
	defer bcancel()
	br, _ := d.tools.Run(bctx, tools.Cmd{Name: "blkid", Args: []string{"-p", "-o", "value", "-s", "TYPE", d.block}})
	p.FSType = strings.TrimSpace(br.Stdout)
	evidence += "$ " + br.CmdLine + "\n" + br.Stdout + br.Stderr
	return p, evidence, nil
}

func (d *PhysicalDrive) ReadSectors(ctx context.Context, lba, n int64, raw bool) ([]byte, error) {
	if raw {
		return d.sgRead(ctx, lba, n)
	}
	f, err := os.Open(d.block)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readAtFile(f, lba, n)
}

// sgRead reads sectors with sg_dd through SCSI generic, bypassing the sr
// driver's idea of the capacity of an open session.
func (d *PhysicalDrive) sgRead(ctx context.Context, lba, n int64) ([]byte, error) {
	if d.sg == "" {
		return nil, errors.New("no SCSI generic device configured for raw reads")
	}
	var out bytes.Buffer
	_, err := d.tools.Run(ctx, tools.Cmd{Name: "sg_dd", Args: []string{
		"if=" + d.sg, "of=-", "bs=2048", "bpt=" + strconv.FormatInt(min(n, 64), 10),
		"skip=" + strconv.FormatInt(lba, 10), "count=" + strconv.FormatInt(n, 10),
	}, Stdout: &out})
	if err != nil {
		if st, _ := d.Status(ctx); st != StatusDiscOK {
			return nil, ErrMediaRemoved
		}
		return out.Bytes(), err
	}
	return out.Bytes(), nil
}

func (d *PhysicalDrive) Image(ctx context.Context, req ImageRequest, progress func(Progress)) (ImageResult, error) {
	if err := d.unmount(ctx); err != nil {
		return ImageResult{}, err
	}
	if req.Method == discinfo.ImageRaw {
		if req.Sectors <= 0 {
			return ImageResult{}, errors.New("raw imaging needs the next-writable address")
		}
		res, err := copyLoop(ctx, d.sgRead, req.Sectors, req.Out, req.Map, 4096, progress)
		res.CmdLines = []string{fmt.Sprintf("sg_dd if=%s of=- bs=2048 (chunked, LBA 0..%d, stop after 4096 unreadable)", d.sg, req.Sectors)}
		return res, err
	}
	return d.ddrescue(ctx, req, progress)
}

// ddrescue images through the block device: a fast first pass without
// scraping, then up to three retries of the bad areas.
func (d *PhysicalDrive) ddrescue(ctx context.Context, req ImageRequest, progress func(Progress)) (ImageResult, error) {
	var res ImageResult
	total := req.Sectors * SectorSize
	poll := func(stop <-chan struct{}) {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if m, err := ReadMap(req.Map); err == nil && progress != nil {
					done := m.Count(Finished) + m.Count(BadSector)
					progress(Progress{Done: done, Total: total, Bad: m.Count(BadSector) + m.Count(NonScraped) + m.Count(NonTrimmed)})
				}
			}
		}
	}
	passes := [][]string{
		{"-b", "2048", "-n", "-f"},
		{"-b", "2048", "-d", "-r3", "-f"},
	}
	for _, args := range passes {
		args = append(args, d.block, req.Out, req.Map)
		if req.Sectors > 0 {
			args = append([]string{"-s", strconv.FormatInt(total, 10)}, args...)
		}
		stop := make(chan struct{})
		go poll(stop)
		r, err := d.tools.Run(ctx, tools.Cmd{Name: "ddrescue", Args: args})
		close(stop)
		res.CmdLines = append(res.CmdLines, r.CmdLine)
		res.Stderr = r.Stderr
		if err != nil {
			if st, _ := d.Status(ctx); st != StatusDiscOK && ctx.Err() == nil {
				return res, ErrMediaRemoved
			}
			return res, err
		}
	}
	m, err := ReadMap(req.Map)
	if err != nil {
		return res, err
	}
	if total == 0 {
		for _, b := range m.Blocks {
			total = max(total, b.Pos+b.Size)
		}
	}
	res.Sectors = total / SectorSize
	res.Bad = m.BadSectors(total)
	if progress != nil {
		progress(Progress{Done: total, Total: total, Bad: res.Bad.Total() * SectorSize})
	}
	return res, nil
}

// CloseTray closes a motorized tray, for "insert side B" on drives that can.
func (d *PhysicalDrive) CloseTray() error {
	fd, err := d.open()
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), cdromCloseTray, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

// Discover pairs every /dev/srN with its /dev/sgN using lsscsi -g output.
func Discover(ctx context.Context, ts tools.Set) ([]Info, error) {
	out, err := ts.Output(ctx, "lsscsi", "-g")
	if err != nil {
		return nil, err
	}
	return ParseLsscsi(out), nil
}

// ParseLsscsi extracts cd/dvd rows from `lsscsi -g`.
func ParseLsscsi(out string) []Info {
	var ds []Info
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "cd/dvd") {
			continue
		}
		fs := strings.Fields(line)
		if len(fs) < 4 {
			continue
		}
		block, sg := fs[len(fs)-2], fs[len(fs)-1]
		if !strings.HasPrefix(block, "/dev/sr") {
			continue
		}
		model := strings.Join(fs[2:len(fs)-3], " ")
		ds = append(ds, Info{ID: filepath.Base(block), Block: block, SG: sg, Model: model})
	}
	return ds
}

var (
	_ Drive = (*PhysicalDrive)(nil)
	_ Drive = (*ImageDrive)(nil)
)
