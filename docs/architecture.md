# Architecture

One Go binary, run by systemd as the unprivileged `camdvd` user, serves the
UI, runs the job engine and calls proven command-line tools. The browser only
renders. The only original media code is the carver.

```
                       ┌──────────────────────── camdvd serve ────────────────────────┐
 Browser ──HTTP/SSE──▶ │ internal/web      html/template + htmx, JSON API, /api/events │
                       │      │                                                        │
                       │ internal/jobs     drive watchers · state machine · convert    │
                       │      │            queue · finalize · library operations       │
                       │      ├── internal/store     SQLite (modernc, pure Go)         │
                       │      ├── internal/drive     PhysicalDrive / ImageDrive        │
                       │      ├── internal/pipeline  list, extract, split, carve,      │
                       │      │                      verify, convert, tag              │
                       │      ├── internal/library   naming, rename planner, undo      │
                       │      └── internal/tools     exec with process groups          │
                       └──────┬───────────────────────────────┬────────────────────────┘
                              │                               │
                 /dev/srN, /dev/sgN (cdrom group)     ffmpeg · ffprobe · dvd-vr (bundled)
                                                      ddrescue · sg_dd · dvd+rw-mediainfo
                                                      7zz · exiftool · blkid · lsscsi
```

## Packages

| Package | Responsibility |
| --- | --- |
| `cmd/camdvd` | `serve`, `rip`, `carve`, `doctor`, `update`, `rollback`, `uninstall` |
| `internal/carve` | MPEG-2 pack/SCR parser and the clip-splitting rules for raw recovery |
| `internal/discinfo` | Parses `dvd+rw-mediainfo`; the classification table |
| `internal/dvdifo` | Reads the title table of `VIDEO_TS.IFO` |
| `internal/drive` | `Drive` interface; ioctls (status, eject), ddrescue, chunked `sg_dd`; ddrescue-format mapfiles; `ImageDrive` for tests and demo |
| `internal/tools` | Runs tools with argument slices (never a shell), `Setpgid` and context cancellation; dependency checks |
| `internal/pipeline` | Stages over files: list/extract image, split DVD-Video and DVD-VR, carve, verify, convert, thumbnail, tag; date rules |
| `internal/store` | Discs, clips, rename previews and batches, settings. Rows keep their record as JSON next to the queried columns |
| `internal/jobs` | The engine: watchers, imaging, processing, finalize, retag, clean-up, library operations, recovery after restart |
| `internal/library` | Name sanitizing, folder/file naming, the rename planner, two-phase apply, undo, path confinement |
| `internal/events` | SSE hub; coalesces bursts per subscriber, never drops a distinct event |
| `internal/doctor` | The self-check that gates disc processing |
| `internal/web` | Pages, htmx fragments, JSON API, login, streaming |

## Job state machine

```
Detected → Probing → Imaging A ─┬─▶ (Awaiting answer) ─┬─▶ Awaiting flip → Imaging B ─┐
                                │                      └──────────────────────────────┤
                                └──────── side processing starts as each side is read ┤
                                                                                      ▼
                                           Processing (classify → extract | carve → verify → convert)
                                                                                      ▼
                                                              Finalizing → Done
   Any state → Failed | Cancelled | Paused (disc removed, restart, library full) → Resume
   Duplicate: already imported, waiting for "Import again?"
```

- A job **holds the drive** from Detected to the end of imaging. After that
  everything runs from the image, so the next disc can go in.
- **Answers** are fields on the job, not a state. Only side A's end waits for
  the sides answer, keeping the tray closed.
- Every transition is persisted; on start, `recover()` resumes processing,
  re-attaches drives, and waits for interrupted discs to come back.
- Finalize is guarded by the Processing → Finalizing transition, so two sides
  finishing together place files once.

## Imaging

| Disc | Method |
| --- | --- |
| Finalized, DVD-RAM | `ddrescue -b 2048 -n` then `-d -r3` on `/dev/srN`, with a mapfile |
| Unfinalized (open session, no file system) | `sg_dd` on `/dev/sgN` from LBA 0 to the next-writable address, in 512-sector chunks retried as 32 and then single sectors; unreadable sectors are zero-filled and recorded in a ddrescue-format mapfile; 4096 consecutive failures mark the unwritten tail |

A side's **fingerprint** (media type, written size, hash of 1 MB of written
data) detects the same side twice, a returning disc, and discs imported
before. On an unfinalized side the sample is the last 1 MB before
next-writable, since the start is an unwritten area both sides share.

## Carving

Every 2048-byte sector starting with an MPEG-2 pack header (`00 00 01 BA`,
valid marker bits) is video. Consecutive packs form a run. A clip ends at a
non-video sector or when the stream clock (SCR) goes backwards or jumps more
than 10 s; runs under 100 sectors are dropped. Sectors the mapfile marks
unreadable don't split a clip if the clock continues across them; the clip
is flagged instead.

## Files on disk

```
/opt/camdvd/<version>/        camdvd, bin/ffmpeg, bin/ffprobe, bin/dvd-vr, install.sh, packaging/
/opt/camdvd/current -> <version>
/etc/camdvd/config.toml       listen, library, drives…      /etc/camdvd/env   CAMDVD_PASSWORD
/var/lib/camdvd/camdvd.db     SQLite (WAL)                  /var/lib/camdvd/backup/  pre-update backups
<library>/<Disc folder>/*.mp4
<library>/.camdvd/<disc-id>/  disc.json, image/, source/, out/, thumbs/
```

## Security

- Unprivileged service user with only the `cdrom` group; systemd
  `ProtectSystem=strict`, `NoNewPrivileges`, `PrivateTmp`, `DevicePolicy=closed`
  with only `sr`, `sg` and `drm` devices allowed, and write access only to the
  state folder and the library.
- External tools get argument arrays, never shell strings.
- Every file operation is confined to the library: names are sanitized and
  paths resolved (including symlinks) before each write.
- State-changing requests with a foreign `Origin` are refused. With a
  password set, sessions are HMAC-signed `SameSite=Strict` cookies and login
  attempts are rate-limited.
