# CamDVD Rescue — Camcorder DVD Archiver: Spec & Implementation Plan

Oct 4, 2026 · @Darren

## Overview

CamDVD Rescue turns a stack of 8cm camcorder DVDs into folders of MP4s, with one disc insert and two optional answers per disc. It runs on a Linux x64 machine with a USB DVD drive and is driven from any browser on the network or on the same machine.

**Users.** The first user archives family footage on a home server and drives it from a Mac. Anyone else with a Linux PC and a drive runs the same app on localhost.

**Goals**

- Recover every recording from finalized DVD-R, unfinalized DVD-R and DVD-RAM (both sides), without the original camcorder.
- Ask only what the machine can't know: single or double-sided, plus an optional description.
- Produce MP4 (H.264 + AAC) that lands in iCloud Photos on the right date with the right camera, and plays on phones, TVs, Macs and browsers.
- Keep the untouched source (disc image or source files), so nothing is lost if conversion settings change later.
- Bulk-rename outputs safely, with preview and undo.
- Install with one command that brings every dependency, plus a startup self-check that refuses to run with anything missing.

**Non-goals for v1**

- Writing or finalizing discs. Finalizing changes the disc; this tool only reads.
- Editing, trimming or DVD menus.
- Windows or macOS hosts. A Mac is a client only, because the raw-read path depends on Linux's sr and sg drivers and their ioctls.
- Commercial movies and series, protected or not: this is for home recordings only. Also tapes, Blu-ray and AVCHD-on-DVD.
- Fedora and other distros, and more than one drive. The job model is per drive, so a second drive can come later.

## Deployment model

One process serves the UI and drives the hardware, and the browser is only a client. The server case and the single-PC case are the same app with a different bind address.

| Scenario | Where it runs | Reached at | Bind | Password |
| --- | --- | --- | --- | --- |
| Home server | Linux x64 server, drive on USB | `http://<server-ip>:8780` from a Mac | 0.0.0.0 | Off; CAMDVD\_PASSWORD turns it on |
| Single PC | Linux x64 desktop or laptop | `http://localhost:8780` | 127.0.0.1 | Off |

- **Host:** any mainstream Linux x64 distro with the `sr` and `sg` kernel drivers and systemd. The app installs natively as a systemd service; the installer supports apt (Debian, Ubuntu) only in v1.
- **Drive:** USB, tray-loading. Slot-loading drives can jam on 8cm discs.
- **Disk space:** about 5 GB free per disc while processing (1.4 GB image, sources, MP4s).
- **Drive access:** the service user is in the `cdrom` group, which owns `/dev/srN` (block reads, eject) and the matching `/dev/sgN` (raw SCSI reads for unfinalized discs) under the distro's default udev rules. Nothing runs as root after install.
- **Library:** a folder on local disk or a NAS mount, `/srv/camdvd/library` by default, chosen at install. Output is owned by the service user with a configurable shared group, so it's usable from the host and over SMB. App state lives outside it: config in `/etc/camdvd/`, database and logs in `/var/lib/camdvd/`.
- **Port:** 8780 by default, configurable. Choosing network access opens it in ufw when it's active.

## User workflow

The person answers two questions per disc; detection, extraction, conversion and naming are automatic.

&#91;embedded content: disc workflow · 2 questions, 2 decisions, 1 flip loop\]

Imaging starts the moment a disc is detected, so the questions never hold up the drive.

1. **Insert a disc.** The drive watcher detects it, probes the media and starts imaging.
2. **Answer the card** that appears in every connected browser: single or double-sided (defaults to the last answer), and an optional description such as "2004-12 Durban holiday". Submit or skip.
3. **Double-sided:** as soon as side A is imaged, the tray ejects with "Flip the disc and insert side B". Side B is imaged into the same folder while side A is still being processed.
4. **Classification** appears with its reason: blank, finalized, unfinalized or DVD-RAM.
5. **Extraction:** finalized discs have their files pulled from the image and split per recording; unfinalized discs go through raw recovery.
6. **Conversion:** each recording becomes an MP4, with per-clip progress, thumbnails and a preview player.
7. **Done:** the card shows the folder name, clip count, total duration and any warnings. The tray already ejected when the last side was imaged, so the next disc can go in while this one converts.
8. **Later:** the library view handles renaming folders and descriptions, bulk renaming files, previewing, and cleaning up sources.

## Disc detection and finalization logic

The app decides finalized vs unfinalized from the drive's own report on the disc, then confirms by looking for a file system. The user is never asked.

1. **Media present.** A watcher polls each drive every 2 s with the `CDROM_DRIVE_STATUS` ioctl. `CDS_DISC_OK` triggers a probe.
2. **Media type and state.** `dvd+rw-mediainfo /dev/srN` reports the media (DVD-R Sequential, DVD-RAM, DVD-RW…), disc status (blank, appendable, complete), state of the last session, and the track's start and next-writable address. The address bounds raw reads later.
3. **File system.** `blkid -p` on the drive shows whether UDF or ISO9660 is present. After imaging, a listing of the image looks for `VIDEO_TS/` or `DVD_RTAV/`.

| Media | Disc status | File system | Classification | Path |
| --- | --- | --- | --- | --- |
| Any | Blank | None | Blank | Tell the user, eject |
| DVD-R / DVD-RW | Complete | UDF/ISO + `VIDEO_TS` | Finalized DVD-Video | Pull files |
| DVD-RW | Complete | UDF + `DVD_RTAV` | Finalized DVD-VR | Pull files |
| DVD-RAM | Not applicable | UDF + `DVD_RTAV` | DVD-RAM | Pull files |
| DVD-R / DVD-RW | Appendable, session incomplete | None | Unfinalized | Raw recovery |
| Any | Any | Present, but file copy fails | Damaged | Raw recovery |
| Not a DVD | Any | Any | Unsupported | Explain, eject |

- DVD-RAM never needs finalizing. It takes the pull-files path whenever its UDF reads.
- The classification and its evidence (tool output) are stored on the job and shown in plain words, e.g. "Unfinalized DVD-R: no file system, about 4 min of data". A **Force raw recovery** button overrides a wrong call.
- Double-sidedness can't be detected, because each side reports as its own 1.4 GB disc. That's why it's asked.

## Extraction pipelines

Every side is imaged first and processed from the image, so the drive reads each side once and a failed conversion never needs the disc again.

**Stage 1: image (all discs)**

- **Finalized and DVD-RAM:** `ddrescue -b 2048` from `/dev/srN` to `image/side-A.img`, with a mapfile. A fast first pass, then up to 3 retries on bad areas. Resumable after a crash or cancel.
- **Unfinalized:** the kernel may misreport the capacity of an open session, so read LBA 0 up to the next-writable address with `sg_dd` on `/dev/sgN`, same mapfile semantics. Stop after 4096 consecutive unreadable sectors (8 MB), which marks the unwritten tail.
- Unreadable sectors are zero-filled and counted. The UI shows a readability bar per side.

**Stage 2a: finalized, pull the files**

- Extract the file system from the image with `7z x` (reads ISO9660 and UDF, no mount, no root) into `source/side-A/`.
- **DVD-Video (`VIDEO_TS`):** one clip per recording via FFmpeg's `dvdvideo` demuxer, which reads chapters and programs from the IFO files. Fallback: concatenate the VOBs of each title.
- **DVD-VR (`DVD_RTAV`, DVD-RAM/RW):** split `VR_MOVIE.VRO` into recordings with `dvd-vr`, which reads `VR_MANGR.IFO` and keeps each recording's date and time. Fallback: carve as in 2b.

**Stage 2b: unfinalized, raw recovery**

- Scan the image's 2048-byte sectors for MPEG-2 pack headers (`00 00 01 BA`) and group consecutive packs into runs.
- Start a new clip at any non-video sector, or when the stream clock (SCR) jumps backwards or forward by more than 10 s. Drop runs under 100 sectors.
- Output `source/side-A/carved_001.mpg` and so on. This is the logic already tested in `dvdcam_rescue.py`, ported.

**Stage 3: verify**

- `ffprobe` every source clip: duration above zero, a video stream present, first and last seconds decodable.
- Failures are kept and flagged, never silently dropped.

## Conversion to MP4

Each recording becomes one MP4 with H.264 video and AAC audio, the most widely playable combination.

| Setting | Default | Why |
| --- | --- | --- |
| Video | H.264 (libx264), High profile, level 4.1 | Plays on every phone, TV and browser |
| Quality | CRF 18, preset slow | Visually lossless for 720×576 MPEG-2 sources |
| Deinterlace | `bwdif`, field rate (50p PAL, 59.94p NTSC) | Camcorder footage is interlaced; field rate keeps motion smooth |
| Aspect | Source display aspect (4:3 or 16:9 flag) kept | No squashed or stretched picture |
| Audio | AAC 192 kb/s stereo, from AC-3, MP2 or LPCM | Universal |
| Container | MP4 with `+faststart` | Streams in the browser preview and over SMB |
| Metadata | `creation_time` plus Apple QuickTime keys (below) | Lands on the right day in iCloud Photos |

- **Presets:** Archive (CRF 16), Standard (CRF 18, default), Small (CRF 22), Copy only (MPEG-2 remuxed to MKV, no quality loss).
- **Optional:** one combined MP4 per disc, with a chapter per recording.
- **Optional:** VAAPI or NVENC hardware encoding when /dev/dri is present and the service user is in the render group. Off by default.
- Encodes run in a queue, one at a time by default, so imaging the next disc isn't starved of CPU or I/O.

**Apple Photos and iCloud metadata**

The MP4s go into the family's iCloud Photos library, so each carries every date and camera tag Photos reads. ExifTool writes them after encoding, so a later date fix rewrites tags without re-encoding.

| Tag | Value | Why |
| --- | --- | --- |
| `Keys:CreationDate` | Local recording time with offset, e.g. `2004-12-24T14:30:00+02:00` | Photos prefers it over the UTC date ([source](https://discussions.apple.com/thread/254805855)) |
| `QuickTime:CreateDate`, track and media dates | The same moment in UTC | Fallback for Photos and other apps |
| File modified time | The same moment | Last resort on import |
| `Keys:Make`, `Keys:Model` | e.g. Panasonic, VDR-M50, from settings, editable per disc | Camera shown in Photos' info panel |
| `Keys:Title`, `Keys:Description`, `Keys:Keywords` | Description, disc and side | Searchable where supported; Photos' support for video captions and keywords is weak ([source](https://discussions.apple.com/thread/251584390)) |
| `Keys:LocationISO6709` | Optional place, set per disc in the library | Shows on the Photos map |
| `Keys:Comment` | Disc id, side and clip number | Re-links files to their disc after renames |

- **Where dates come from,** first match wins. 1: DVD-VR's `VR_MANGR.IFO`, which holds a date and time per recording that `dvd-vr` reads ([source](http://www.pixelbeat.org/systems/hitachi_DZ-BX35E/)). 2: a date subtitle stream in the VOBs, the method Sony camcorders use ([source](https://forum.doom9.org/showthread.php?t=159688)). 3: OCR of the dates in the menu the camera writes when finalizing, the only place a Canon DC50 keeps them ([camcorder-dvd-extractor](https://github.com/DrRob/camcorder-dvd-extractor)). 4: the camera's own scene table on an unfinalized disc, if M0 finds it. 5: a disc-level date. Clips with only a disc-level date are spaced one minute apart in recording order, so they sort correctly in Photos.
- **Clock sanity:** a camcorder whose backup battery died records its reset date (this VDR-M50 showed 1 January 2004). Dates on that default, before 1995 or in the future are treated as unknown and flagged; setting a disc date in the library rewrites the tags in place.
- **Time zone:** one setting, Africa/Johannesburg by default, overridable per disc.
- **Getting them into iCloud:** iCloud Photos has no supported upload API on Linux, so a Mac imports each disc folder from the SMB share (Photos, File, Import). The library lists folders not yet ticked off as imported.

## Output layout, naming and bulk rename

Each disc gets one folder, including both sides of a double-sided disc. The description, if given, names the folder and its files.

```
<library>/
  2004-12 Durban holiday/           description, else recording date, else import date
    Durban holiday - A01.mp4        side letter + recording number
    Durban holiday - A02.mp4
    Durban holiday - B01.mp4
  .camdvd/
    d-7f3a9c/                       one per disc
      disc.json                     manifest: media, classification, tool output, clips, dates, hashes
      image/side-A.img, side-A.map  kept only for unfinalized or damaged discs
      source/side-A/...             VIDEO_TS / DVD_RTAV files, or carved .mpg
      logs/
```

- **Folder name:** the description; with none, the earliest recording date ("2004-12-24"); with no usable date, the import date ("Imported 2026-10-04 1432"). Names are sanitized (no `/` or control characters, at most 120 characters) and a clash gets " (2)". The description stays editable, and renaming the folder updates the manifest.
- **File names:** `{desc} - {side}{nn}`. Single-sided discs drop the side letter (`- 01`). With no description, {desc} falls back to the folder name. When the recording date is known, it's available as a token.
- **Working files** live in `<library>/.camdvd/<disc-id>/`, outside the disc folders, so importing a disc folder into Photos brings in only the MP4s. Each MP4 also carries its disc id in metadata, so a folder renamed outside the app is found again.
- **Retention:** the image is kept only for unfinalized or damaged discs; sources are always kept. Auto-clean is opt-in: when enabled, it deletes images and sources a set number of days after the disc's MP4s are verified.

**Bulk rename**

- Select files in one folder, many folders or the whole library, or select folders themselves.
- Pattern tokens: `{desc}`, `{n}` or `{n:000}` (sequence, with start and step), `{side}`, `{date:yyyy-MM-dd}`, `{time}`, `{orig}`, `{duration}`. Plus find/replace (optional regex) and case change.
- A live preview table shows old name → new name and highlights conflicts: duplicate targets, existing files, illegal characters, names over 255 bytes. Apply stays disabled until the preview is clean.
- Apply is transactional: a two-phase rename through temporary names, so swaps like A→B with B→A work. Each apply is recorded as a batch with one-click undo.
- The manifest and database follow every rename, so folders and UI never disagree.

## Architecture and tech stack

A single Go binary, run by systemd as an unprivileged user, serves the UI, runs the job engine and calls the bundled command-line tools; the browser only renders. Go fits this kind of system tool best: one static binary with no runtime to install, direct syscalls for the drive, and cheap, cancellable process control.

&#91;embedded content: architecture · one systemd service, four workers, two host resources\]

Workers are thin wrappers around proven tools; the only original media code is the carver.

| Layer | Choice | Reason |
| --- | --- | --- |
| Language | Go, current stable release, built with `CGO_ENABLED=0` | One static binary; nothing to install on the host |
| Web server | `net/http` with the standard router | A dozen routes need no framework |
| UI | Server-rendered `html/template` + htmx, assets embedded with `embed` | No JavaScript build step; the binary serves everything |
| Real-time | Server-Sent Events at `/api/events` | Progress, drive and queue events to every open browser |
| Persistence | SQLite via `modernc.org/sqlite` (pure Go), file in `/var/lib/camdvd/` | No cgo, no server; kept off the library because SQLite locking is unreliable on NAS mounts |
| Process control | `os/exec` with argument slices, `Setpgid` process groups, `context` cancellation | Safe, cancellable tool calls |
| Device I/O | `golang.org/x/sys/unix` ioctls for drive status and eject | No extra tools for polling |
| Media tools | FFmpeg, ddrescue, sg3\_utils, dvd+rw-tools, 7-Zip, dvd-vr, ExifTool | Mature and scriptable |
| Packaging | Release tarball on GitHub + `install.sh` + systemd unit | One-command install |

**Code layout:** `cmd/camdvd` (one binary: `camdvd serve`, plus the same pipeline headless as `camdvd rip`, `camdvd carve` and `camdvd doctor`), `internal/drive` (`Drive` interface with `PhysicalDrive` and `ImageDrive`), `internal/tools` (one adapter per external tool, each with a version check), `internal/pipeline` (probe, image, classify, extract, carve, convert, tag), `internal/jobs` (state machine, queue), `internal/library` (manifests, rename planner, undo), `internal/web` (handlers, templates, SSE).

## API and job model

A disc job is a state machine persisted in SQLite, so a restart or crash resumes where it stopped.

**States:** Detected → Probing → Imaging side A → (AwaitingFlip → Imaging side B) → Classifying → Extracting or Carving → Converting → Done, Failed or Cancelled. The drive is released when the last side is imaged; everything after runs from the images.

- **Probing** runs `dvd+rw-mediainfo` and `blkid` before any read, because the imaging method depends on the result: ddrescue on `/dev/srN`, or `sg_dd` up to the next-writable address for an open session.
- **Answers** are a flag on the job, not a state, so they never block imaging. The sides answer is needed when side A finishes; if it's still missing, the tray stays closed and the card is highlighted. The description is needed only at naming and stays editable afterwards through rename.
- **AwaitingFlip** ejects the tray and waits for media. The new side's capacity, used size and a hash of its first 1 MB of written data must differ from side A's (an unfinalized side has no file-system area yet, so a fixed 64 KB window at LBA 0 can match on both sides); if they match, the UI says "This looks like side A again, flip it over".
- Each stage records start and end times, progress (bytes or seconds), the tool command line and the tail of its stderr.
- **Cancel** kills the stage's process group. **Resume** restarts the stage; ddrescue continues from its mapfile.
- A drive belongs to a job only while imaging, so the next disc can go in while earlier ones convert. v1 drives one drive, but jobs are per drive so more can be added later. Conversions share one global queue.
- **Batch mode:** preset answers (e.g. "all single-sided, no description") for working through a stack without touching the UI.

| Method | Path | Purpose |
| --- | --- | --- |
| GET | `/api/drives` | Drives, tray state, current job |
| POST | `/api/drives/{id}/eject` | Eject |
| GET | `/api/jobs?state=` | Job list and history |
| GET | `/api/jobs/{id}` | Job detail, stages, evidence |
| POST | `/api/jobs/{id}/answers` | `{ sides: 1 or 2, description? }` |
| POST | `/api/jobs/{id}/force-raw` | Override classification |
| POST | `/api/jobs/{id}/cancel`, `/resume` | Job control |
| GET | `/api/library` | Folders and files |
| PATCH | `/api/library/folders/{id}` | Rename folder, edit description |
| POST | `/api/rename/preview` | Selection + pattern → planned renames and conflicts |
| POST | `/api/rename/apply` | Apply a clean preview by its id |
| POST | `/api/rename/undo/{batchId}` | Undo a batch |
| GET | `/api/files/{id}/stream` | Range-request streaming for the preview player |
| GET | `/api/doctor` | Dependency and device self-check |
| GET | `/api/events` | Live job, drive and queue events (Server-Sent Events) |

## Dependency bundling and installation

The installer puts the app binary and a pinned FFmpeg in `/opt/camdvd`, installs the remaining tools from the distro, and the app checks itself on every start, so a missing tool surfaces at install time, never mid-disc.

**Release bundle** (`camdvd-<version>-linux-x64.tar.gz` on GitHub Releases, built in CI, with a SHA-256 checksum)

| Component | Source | Used for |
| --- | --- | --- |
| `camdvd` binary | Static Go build, so the host needs no runtime | The app |
| FFmpeg 7.x + ffprobe (libdvdnav, libdvdread, libx264) | Pinned build in `/opt/camdvd/bin`. Distro FFmpeg is too old for the `dvdvideo` demuxer (added in 7.0): Debian 12 ships 5.1, Ubuntu 24.04 ships 6.1 | Split, convert, probe |
| dvd-vr | Built from source at a pinned commit in CI | Split DVD-RAM VRO into recordings |

**Licence:** the app is MIT. FFmpeg and dvd-vr ship as separate GPL binaries in `bin/`, with `THIRD_PARTY.md` linking the exact source for each version.

**Distro packages**

| Tool | apt package | Used for |
| --- | --- | --- |
| GNU ddrescue | `gddrescue` | Imaging finalized discs |
| sg3\_utils | `sg3-utils` | Raw reads of unfinalized discs |
| dvd+rw-tools | `dvd+rw-tools` | Media type and finalization state |
| 7-Zip | `7zip` (`p7zip-full` on Ubuntu 22.04) | Extract ISO9660/UDF from images |
| ExifTool | `libimage-exiftool-perl` | Apple Photos date and camera tags |
| util-linux, eject, lsscsi | `util-linux`, `eject`, `lsscsi` | `blkid`, eject, sr-to-sg mapping |

**Install, one command:** `curl -fsSL https://…/install.sh | sudo bash`

1. Checks for Linux x64, systemd, and apt (Debian 12+ or Ubuntu 22.04+).
2. Installs the distro packages, then downloads and verifies the release bundle into `/opt/camdvd/<version>/` and points `/opt/camdvd/current` at it.
3. Creates the `camdvd` system user with the supplementary group `cdrom` (plus `render` if hardware encoding is chosen).
4. Lists optical drives with `lsscsi -g` and pairs each `/dev/srN` with its `/dev/sgN`.
5. Asks for the library folder, the port, and whether other machines will connect (sets the bind address and opens the port in ufw if active). Writes `/etc/camdvd/config.toml` and `/etc/camdvd/env`, where `CAMDVD_PASSWORD` can be set.
6. Adds a udev rule setting `UDISKS_IGNORE` on the configured drives, so a desktop doesn't automount discs or open a player.
7. Installs and starts `camdvd.service`, runs the doctor and prints the URL.

**systemd unit:** `User=camdvd`, `SupplementaryGroups=cdrom`, `Restart=on-failure`, `NoNewPrivileges=yes`, `ProtectSystem=strict`, `PrivateTmp=yes`, and `ReadWritePaths=` for the library and `/var/lib/camdvd`. `ProtectHome=yes` unless the library is under `/home`. Logs go to journald (`journalctl -u camdvd`).

**Doctor** (on every start, and at `/api/doctor`)

- Every binary present and runnable, with versions; FFmpeg has the `dvdvideo` demuxer, `libx264` and `bwdif`.
- Each configured drive opens as the service user, its `sg` device matches, and the drive's DVD-RAM read support and loading mechanism (tray vs slot) are reported.
- The library is writable by the service user with at least 5 GB free.
- Any failure shows a red banner and blocks disc processing. Each line says how to fix it.

**Updates and removal:** `camdvd update` backs up the database, unpacks the new bundle beside the current one, switches the symlink and restarts; `camdvd rollback` switches back and restores the database. Migrations run at start; a job in progress blocks the update. `camdvd uninstall` removes the app, unit, user and udev rule, and never touches the library.

**Docker (later, optional):** the same bundle in an image, run with `--device` for `/dev/srN` and `/dev/sgN`, `--group-add cdrom` and the library as a volume. The watcher already polls instead of relying on udev, so it works inside a container. Docker on a macOS host still can't reach a USB drive.

## Security, errors and edge cases

CamDVD Rescue is a LAN tool, not an internet service. Login is off by default; setting `CAMDVD_PASSWORD` turns it on.

**Security**

- Bind address and port come from config: 127.0.0.1 on a single PC, 0.0.0.0 on a server. There's no login by default; setting `CAMDVD_PASSWORD` in `/etc/camdvd/env` adds a password login with a cookie session. TLS and remote access are out of scope (use Tailscale or a reverse proxy).
- The service runs as the non-root `camdvd` user with only the `cdrom` group and the systemd sandboxing above. The installer is the only step that needs root.
- All file operations are confined to the configured library folder. Names are sanitized and paths canonicalized before every write, so a rename can't escape the library.
- External tools are started with argument arrays, never shell strings.

**Edge cases**

| Case | Behaviour |
| --- | --- |
| Blank disc | "Disc is blank", eject, no job |
| Scratched or partly unreadable | ddrescue retries; clips with zero-filled gaps are kept and flagged; FFmpeg runs with `+discardcorrupt` |
| Same side inserted twice | Hash check, ask to flip |
| Disc removed mid-read | Job pauses; resumes when the same disc (matched by hash) returns |
| Said single-sided, was double | "Add another side" button on a finished job |
| Disc already imported | Hash match: "Imported on \<date> as \<folder>. Import again?" |
| Library full | Space check before each stage; pause with a message |
| Data DVD, not camcorder | Offer "copy files only", no conversion |
| Slot-loading drive | Doctor warns before an 8cm disc goes in |
| Desktop automounts the disc | Installer's udev rule prevents it; if a mount is still found, the app unmounts before reading or ejecting |
| Files renamed outside the app (e.g. over SMB) | Library rescan matches files to the manifest by hash and updates names; unknown files are listed, never deleted |

## Testing strategy

Most of the pipeline is tested without a drive, by feeding disc images through a file-backed drive adapter. Real discs cover what can't be synthesized.

- **Unit tests:** pack and SCR parser, carver splitting rules, classification table, rename planner (collisions, swaps, illegal names, undo), state-machine transitions.
- **Synthetic images in CI:** FFmpeg `-target pal-dvd` and `ntsc-dvd` clips, authored with `dvdauthor` and `genisoimage -dvd-video` for finalized DVD-Video. The same streams written raw with no file system stand in for unfinalized discs. Zero-filled ranges simulate bad sectors.
- **Drive adapter:** `Drive` with `PhysicalDrive` (ioctl, ddrescue, sg\_dd) and `ImageDrive` (a file with simulated insert, eject and flip). The whole UI flow runs in CI and in a demo mode.
- **Golden corpus (real discs, kept private):** finalized DVD-R, unfinalized DVD-R (Panasonic and Acoss media), DVD-RAM both sides, one scratched disc, one blank disc. Their images become regression fixtures for the carver and `dvd-vr` steps.
- **Release checklist:** install, update, rollback and uninstall on clean Debian 12, Ubuntu 22.04 and Ubuntu 24.04; doctor green; one disc of each class end-to-end from a Mac browser; flip flow; cancel and resume; rename and undo.
- **Playback check:** MP4s import into Photos on a Mac with the right date, camera and place, sync through iCloud, and play on an iPhone and Apple TV.

## Implementation plan and milestones

Build in seven milestones, each ending at a gate that must pass before the next starts; the hardware spike goes first because its results decide how raw reads are done.

&#91;embedded content: milestones M0–M6 · each with its exit gate\]

The pipeline is usable from the command line after M1, so your own discs can be rescued well before the UI is finished.

- [ ] **M0 Spike:** run `dvd+rw-mediainfo`, `ddrescue`, `sg_dd`, `7z`, `ffmpeg -f dvdvideo` and `dvd-vr` by hand on each disc type, record a test clip with the clock set to a known time and search the raw image for it, import a tagged test MP4 into Photos on a Mac, and record results in a decision table.
- [ ] **M1 Core pipeline:** Go module with `internal/drive`, `internal/tools`, `internal/pipeline` and the `camdvd` CLI; port the carver from `dvdcam_rescue.py` with its tests.
- [ ] **M2 Web app:** drive watcher, persisted job engine, htmx pages for drives, the answers card and job progress.
- [ ] **M3 Double-sided and library:** flip flow, same-side hash check, folder naming, library browser, range-request preview player, Apple Photos tags and disc date fixes.
- [ ] **M4 Bulk rename:** rename planner, preview API and UI, transactional apply, undo.
- [ ] **M5 Packaging:** CI-built release bundle (self-contained app, pinned FFmpeg, dvd-vr), `install.sh`, systemd unit, udev rule, doctor, docs.
- [ ] **M6 Hardening:** damaged-disc handling, cancel/resume, optional password login, already-imported detection; v1 tag.

## Open questions

The spec's tool behaviour is based on how these tools normally work, not yet tested on this drive and these discs; the first six items are what the M0 spike settles.

- [ ] Does the Sony DRX-S90U return the written sectors of an open session through `/dev/sr0`, or only through `sg_dd` on `/dev/sgN`?
- [ ] Does the drive read 8cm DVD-RAM, and does 7-Zip extract its UDF?
- [ ] Does the camcorder's DVD-Video put each recording in its own chapter or program, so the `dvdvideo` demuxer splits cleanly?
- [ ] Which FFmpeg 7.x build to pin, given distro packages predate `dvdvideo`: an existing static build that includes libdvdnav and libdvdread, or one built in CI?
- [ ] Where do the VDR-M50's DVD-R discs keep recording dates? The camera shows them in Disc Navigation, but nothing public documents the format for Hitachi-built cameras. M0 test: record a short clip on a blank DVD-R with the clock set to a known time, then look for that timestamp in the raw unfinalized image, the VOB subtitle streams, the MPEG GOP timecode and, after finalizing a copy, the menu.
- [ ] Smoke test only: Photos on macOS 13 to 15 takes the date and time zone from `Keys:CreationDate` in .mp4, and the older .mp4 date bugs were fixed in macOS 13 ([Apple user tip](https://discussions.apple.com/docs/DOC-250002750)). Confirm once on the current macOS with a tagged test file; .m4v with the same streams is the fallback.
- [ ] One MP4 per recording only, or also a combined file per disc by default?
