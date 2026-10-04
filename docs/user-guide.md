# User guide

## Before you start

- A Linux x64 machine (Debian 12+ or Ubuntu 22.04+, including derivatives
  such as Pop!_OS and Mint) with systemd.
- A **tray-loading** DVD drive. Slot-loading drives can jam on 8 cm discs; the
  self-check warns if it detects one.
- About 5 GB free per disc while it's being processed.

## Install

```sh
curl -fsSL https://github.com/dnaidoo621/camdvd-rescue/releases/latest/download/install.sh | sudo bash
```

The installer asks four things:

| Question | Default | Notes |
| --- | --- | --- |
| Library folder | `/srv/camdvd/library` | Local disk or a NAS mount. Share it over SMB to import into Photos from a Mac. |
| Web port | `8780` | |
| Will other machines connect? | yes | `yes` listens on all interfaces (and opens the port in ufw if it's active); `no` listens on `127.0.0.1` only. |
| Shared group | none | Group that owns the library, e.g. the group your Samba share uses. |

Non-interactive: `sudo bash install.sh --library /srv/camdvd/library --port 8780 --network --yes`.
Run `bash install.sh --help` for every option.

When it finishes it prints the address, e.g. `http://192.168.1.20:8780`.

### Turning on a login

The app has no login by default (it's a LAN tool). To require a password:

```sh
sudoedit /etc/camdvd/env          # set CAMDVD_PASSWORD=something
sudo systemctl restart camdvd
```

For access from outside your network, use Tailscale or a reverse proxy with
TLS; the app doesn't do TLS itself.

## Ripping a disc

1. **Insert the disc.** The drive is polled every 2 seconds. Imaging starts as
   soon as the disc is detected.
2. **Answer the card** that appears in every open browser:
   - *Single or double-sided?* Each side of a double-sided disc reports as its
     own disc, so this can't be detected. It defaults to your last answer.
   - *Description* (optional), e.g. `2004-12 Durban holiday`. It names the
     folder and the files. *Skip description* names them by date instead.
   You can answer while the disc is still being read. If side A finishes
   first, the tray stays closed and the card is highlighted until you answer.
3. **Double-sided:** when side A is read the tray opens with *Flip the disc and
   insert side B*. Side B is read into the same folder while side A converts.
   If you put side A back in, it says so and ejects.
4. **Classification** appears on the card with its reason:

   | Shown as | Meaning | What happens |
   | --- | --- | --- |
   | Finalized DVD-Video | `VIDEO_TS` folder | Each chapter (setting: or title) becomes a clip |
   | Finalized DVD-VR / DVD-RAM | `DVD_RTAV` folder | `dvd-vr` splits recordings and keeps their dates |
   | Unfinalized | No file system | Raw recovery: the image is scanned for video packs |
   | Damaged | File system present but unreadable | Raw recovery |
   | Data disc | Files but no video | Files are copied as they are |
   | Blank / Unsupported | — | Ejected with a message, no job |

   If a call is wrong, **Force raw recovery** re-processes the disc from its image.
5. **Converting:** each clip shows progress and, when done, a thumbnail. The
   tray already opened after the last side was read, so the next disc can go
   in while this one converts.
6. **Done:** the card shows the folder, the clip count and any warnings.

### Warnings you may see

- *No recording date; set a disc date in the library.* Unfinalized and
  DVD-Video discs don't store per-recording dates. See [Dates](#dates).
- *Date … is the camera's reset default.* The camcorder's clock battery was
  flat. The date is ignored.
- *N unreadable sectors were zero-filled inside this clip.* Scratches. The
  clip is kept; there may be a glitch where the gap was.
- *Kept but not converted: …* The recording couldn't be decoded. Its source is
  kept in the disc's working folder.

### Cancel, resume and problems

- **Cancel** stops the work and ejects. **Resume** continues where it stopped;
  imaging continues from its mapfile, finished clips aren't redone.
- Pulling the disc out mid-read pauses the job. Put the same disc back to
  continue.
- If the service restarts, unfinished jobs carry on by themselves.
- A disc that was imported before is recognised: *Imported on … Import again?*
- **Add another side** on a finished single-sided disc reads its side B into
  the same folder.

## The library

**Library** lists one row per disc. Open a folder to preview clips (click a
thumbnail), download them, and edit the disc:

| Field | Effect |
| --- | --- |
| Description, folder name | Renames the folder (undoable from Rename) |
| Disc date | Date for clips without their own; `yyyy-mm-dd` or `yyyy-mm-ddThh:mm` |
| Time zone | Zone the camera's clock was in (default Africa/Johannesburg) |
| Camera make and model | Shown in Photos' info panel |
| Place | ISO 6709, e.g. `+29.8587+031.0218/`; shows on the Photos map |

Saving rewrites the tags inside the MP4s. Nothing is re-encoded.

**Clean up sources** deletes a disc's image and source files once its MP4s
verify. Settings › Clean-up can do this automatically after N days.

**Rescan** finds folders and files renamed outside the app (e.g. over SMB)
by size and hash, and lists files it doesn't know. It never deletes anything.

### Dates

Each MP4's date comes from, in order:

1. The disc's own time for that recording (DVD-RAM and DVD-RW in VR mode).
2. The disc date you set in the library, with clips one minute apart in
   recording order so they sort correctly in Photos.
3. Nothing: the clip is flagged, and Photos uses the import date.

Dates on the camera's reset default, before 1995, or in the future are treated
as unknown.

### Getting the clips into iCloud Photos

iCloud has no upload API on Linux, so import from a Mac:

1. Share the library over SMB and connect from the Mac (Finder › Go › Connect to Server).
2. In Photos: File › Import, choose a disc folder, Import All.
3. Back in CamDVD's library, tick *In Photos* for that folder. *Not yet in
   Photos* lists the ones left.

Working files live in `<library>/.camdvd/`, outside the disc folders, so a
folder import brings in only the MP4s.

## Bulk rename

**Rename** (or *Bulk rename* on a folder):

1. Tick files, whole folders' files, the folders themselves, or every file.
2. Type a pattern. Tokens:

   | Token | Value |
   | --- | --- |
   | `{desc}` | The disc's description (or folder name) |
   | `{side}` | `A` or `B` |
   | `{rec}` `{rec:00}` | Recording number on its side |
   | `{n}` `{n:000}` | Sequence across the selection, from *Start at* by *Step* |
   | `{date:yyyy-MM-dd}` | Recording date (`yyyy`, `yy`, `MM`, `MMM`, `dd`, `HH`, `mm`, `ss`) |
   | `{time}` | Recording time, `HH-mm` |
   | `{orig}` | The current name without extension |
   | `{duration}` | e.g. `03m12s` |

   Then optionally find/replace (plain or regular expression, `$1` for groups)
   and change case.
3. The preview shows old → new and flags duplicate names, existing files,
   illegal characters and names over 255 bytes. *Apply* stays disabled until
   it's clean.
4. Every apply is listed under *Recent renames* with **Undo**.

## Settings

| Setting | Default | |
| --- | --- | --- |
| Quality preset | Standard (CRF 18) | Archive CRF 16, Small CRF 22, Copy only (MPEG-2 in MKV, no re-encode, no Photos tags) |
| Hardware encoding | Off | VAAPI (Intel/AMD) or NVENC. Install with `--hwaccel` for VAAPI's render group |
| DVD-Video: one file per | Chapter | Switch to Title if your camcorder doesn't mark one chapter per recording |
| Combined MP4 | Off | Also write one MP4 per disc with a chapter per recording |
| Time zone, camera, reset dates | Africa/Johannesburg, —, 2004-01-01 | Defaults for new discs |
| Batch mode | Off | Answer every disc the same way, for working through a stack |
| Clean-up | 0 (never) | Days after verification to delete images and sources |

Install-time settings (port, library, drives) live in `/etc/camdvd/config.toml`.

## Command line

```sh
camdvd doctor                         # self-check: tools, drives, storage
camdvd rip -sides 2 -desc "Holiday" -out /srv/camdvd/library   # one disc, no UI
camdvd carve side-A.img -out carved/  # raw recovery from an image
sudo camdvd update                    # latest release; backs up the database
sudo camdvd rollback                  # previous release and database
sudo camdvd uninstall [-purge]        # never touches the library
journalctl -u camdvd -f               # logs
```

## Troubleshooting

| Symptom | Fix |
| --- | --- |
| Red banner *Self-check failed* | Open **Doctor**; each red line says how to fix it. |
| *Disc detected, but CamDVD can't process discs…* | Same: fix the Doctor lines, then **Run again**. |
| A file manager or player opens when a disc goes in | The udev rule isn't active: re-run the installer, or check `/etc/udev/rules.d/70-camdvd.rules`. |
| *… is mounted and couldn't be unmounted* | As above; or unmount it in the file manager. |
| Folder shows *missing* | It was renamed or moved outside the app: press **Rescan**. |
| Wrong classification | **Force raw recovery** on the card. |
| Clips cut at the wrong places on a finalized disc | Settings › one file per Title, then Force raw recovery or re-rip. |
