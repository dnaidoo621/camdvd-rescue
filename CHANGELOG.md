# Changelog

## v0.1.4 — 2026-10-05

- **DVD-RAM:** Panasonic camcorder DVD-RAM discs (UDF 2.00) couldn't be
  read by 7-Zip, so they fell back to raw carving and lost their dates. A
  built-in, read-only UDF reader now lists and extracts them when 7-Zip
  can't, and `dvd-vr` splits the recordings with each one's date and time.
  Found on a real disc: 45 dated recordings instead of 47 undated fragments.
- **Process again:** a cancelled or failed job can be classified and
  extracted again from its saved image, without the disc.

## v0.1.3 — 2026-10-05

- **Fix:** unfinalized discs from cameras that restart the stream clock
  inside a recording (Hitachi, every ~21.6 s) were split into dozens of short
  clips. The carver now keeps a recording together while its video timecode
  carries on, and splits where the timecode restarts. Durations of such
  clips come from the disc, not ffprobe's first-segment estimate.
- **Fix:** typing a description while a disc was being read was lost on
  every progress update.
- **Fix:** after a restart, a job waiting for its answer or a flip didn't
  continue converting the side already read.

## v0.1.2 — 2026-10-05

- **Fix:** raw reads on USB drives failed outright: `sg_dd` asked for 64
  sectors (128 KB) per transfer, but USB bridges such as the Sony DRX-S90U's
  allow 120 KB. The transfer size now follows the kernel's limit for each
  drive (`max_hw_sectors_kb`).

## v0.1.1 — 2026-10-05

- **Fix:** unfinalized camcorder DVD-Rs with several tracks (a reserved
  file-system track, small management tracks, unwritten gaps, then the
  video) failed to start ("read fingerprint sample") and would have stopped
  imaging at the first large unwritten gap, before the video. Raw imaging now
  reads only the written ranges from the drive's track table, and the
  fingerprint samples the end of the last written track. Found on a real
  Hitachi disc in a Sony DRX-S90U.
- **Security:** open redirect after login, demo drive accepting arbitrary
  paths, cookie flags (from code scanning).
- Fuzz tests for the disc-data parsers; release provenance attestations.

## v0.1.0 — 2026-10-04

First release. Tested end to end against synthetic disc images and on an
HL-DT-ST GUC0N drive (self-check, install, update, rollback, uninstall); not
yet against real camcorder discs — see [docs/m0-decisions.md](docs/m0-decisions.md).

- Detects discs and classifies them: finalized DVD-Video, DVD-VR (DVD-RW,
  DVD-RAM), unfinalized DVD-R, damaged, data, blank, unsupported.
- Images every side first (ddrescue, or chunked sg_dd for open sessions),
  resumable from a mapfile; unreadable sectors zero-filled and reported.
- Splits DVD-Video by chapter or title, DVD-VR with dvd-vr (keeping dates),
  and carves unfinalized discs by MPEG-2 pack and clock continuity.
- Converts to H.264/AAC MP4 (bwdif to field rate, aspect kept, faststart);
  presets Archive, Standard, Small and Copy only; optional VAAPI/NVENC and a
  combined per-disc MP4 with chapters.
- Writes Apple Photos tags (Keys:CreationDate with offset, UTC QuickTime
  dates, camera, title, keywords, place); date fixes retag without re-encoding.
- Web UI: live drive and job cards, the two-question card, double-sided flip
  flow with same-side detection, library with preview player, bulk rename with
  preview, conflicts and undo, settings, self-check; optional password login.
- Edge cases: disc removed mid-read, restart recovery, cancel/resume, force
  raw recovery, already-imported detection, add another side, library full,
  rescan after renames over SMB.
- One-command installer, systemd sandboxing, udev rule against automount,
  `camdvd update`, `rollback`, `uninstall`, `doctor`, `rip`, `carve`.
