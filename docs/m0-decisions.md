# M0 spike: decision table

What the hardware spike settles, what's been settled already, and how to run
the rest. Each "Needs a real disc" row has a command; record what it prints.

## Settled during the build (2026-10-04)

| Question | Answer | Evidence |
| --- | --- | --- |
| Which FFmpeg 7.x build to pin? | None: BtbN no longer ships 7.x. Pinned **FFmpeg 8.1.3** (BtbN `autobuild-2026-10-03-18-14`, static GPL). It has the `dvdvideo` demuxer, libdvdnav/libdvdread, libx264, `bwdif`, VAAPI. | `scripts/versions.env`; `camdvd doctor` checks the features on every start |
| Does `-preindex` work with chapter ranges? | No: with `-chapter_start/-chapter_end` it reports no duration. The demuxer is used without it, which gives exact chapter durations. | `internal/pipeline` tests |
| Does 7-Zip extract a DVD's UDF without mounting? | Yes, `7zz x` (7-Zip 21.07 from Ubuntu 22.04's `7zip` package) on genisoimage UDF/ISO images. | `TestFinalizedEndToEnd` |
| Does `dvd-vr` print per-recording dates? | Yes: `num : N` then `date : YYYY-MM-DD HH:MM:SS` (camera local time) per program; parsed by `ParseDVDVRDates`. | dvd-vr source, unit test |
| Does Photos-relevant tagging work on the bundled stack? | ExifTool 12.40 writes `Keys:CreationDate` with offset and UTC QuickTime dates into the MP4 without re-encoding. | `TestFinalizedEndToEnd`, `TestDoubleSidedUnfinalizedDisc` |
| Test drive on the HTPC | HL-DT-ST DVDRAM GUC0N (SATA, tray, lists DVD-RAM read); `/dev/sr0` ↔ `/dev/sg1`, both group `cdrom`. Not the Sony DRX-S90U in the spec. | `camdvd doctor` |

## Needs a real disc

Run on the server with the disc in the drive (stop the service first so it
doesn't grab the disc: `sudo systemctl stop camdvd`).

| # | Question | How to answer it | Result |
| --- | --- | --- | --- |
| 1 | Does the drive return an open session's sectors through `/dev/sr0`, or only via `sg_dd`? | Unfinalized DVD-R: `dvd+rw-mediainfo /dev/sr0` (note Next Writable Address N), then `dd if=/dev/sr0 of=/tmp/a bs=2048 count=1000 skip=$((N-1000))` vs `sg_dd if=/dev/sg1 of=/tmp/b bs=2048 count=1000 skip=$((N-1000))` | |
| 2 | Does the drive read 8 cm DVD-RAM, and does 7-Zip extract its UDF? | DVD-RAM side: `ddrescue -b 2048 -n /dev/sr0 ram.img ram.map && 7zz l ram.img` (expect `DVD_RTAV/VR_MOVIE.VRO`) | |
| 3 | Does the camcorder's DVD-Video put each recording in its own chapter? | Finalized disc: `camdvd rip -sides 1 -out /tmp/t` then compare the clip count with the number of recordings on the camera; if one clip per disc, set Settings › one file per Title (or report the IFO layout) | |
| 4 | Where does the VDR-M50 keep recording dates on DVD-R? | Record a short clip on a blank DVD-R with the clock set to a known time (e.g. 2026-10-04 13:57). Image it unfinalized, then `grep -c` the image for the time in BCD (`13 57`), ASCII, and look at the VOB subtitle streams (`ffprobe -show_streams`) and GOP timecodes (`ffprobe -show_frames -select_streams v -read_intervals %+1 \| grep timecode`). Finalize a copy and check the menu. | |
| 5 | Photos reads `Keys:CreationDate` from .mp4 on the current macOS? | Import one finished MP4 into Photos on the Mac; Info should show the disc's date, time, zone and camera. | |
| 6 | One MP4 per recording only, or also a combined file? | Product choice. Off by default; Settings › "Also write one combined MP4 per disc". | |

## Not implemented yet, pending answers above

- Date sources 2–4 (Sony date subtitles, OCR of the finalize menu, the
  camera's own scene table on unfinalized discs). The engine resolves dates
  from DVD-VR first, then a disc-level date you set in the library, spaced
  one minute apart per recording. Results of #4 decide which source to add.
- AVCHD-on-DVD, tapes, Blu-ray, more than one drive: non-goals for v1.
