# Changelog

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
