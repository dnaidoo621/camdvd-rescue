# Third-party software

CamDVD Rescue itself is MIT-licensed (see LICENSE). The release bundle also
ships two separate GPL programs in `bin/`, run as external processes. Their
exact sources:

| Component | Version | Licence | Source |
| --- | --- | --- | --- |
| FFmpeg, ffprobe | n8.1.3-14-g330caae0c1 (BtbN static GPL build, `autobuild-2026-10-03-18-14`) | GPL-3.0-or-later (with libx264, libdvdnav, libdvdread) | https://github.com/FFmpeg/FFmpeg/tree/330caae0c1 · build recipe https://github.com/BtbN/FFmpeg-Builds |
| dvd-vr | 0.9.8b, commit f4cba008bbbd3bd237c1cdd99d1b8356521d377e | GPL-2.0-or-later | https://github.com/pixelb/dvd-vr/tree/f4cba008bbbd3bd237c1cdd99d1b8356521d377e |

Linked into the `camdvd` binary (permissive licences):

| Module | Licence |
| --- | --- |
| modernc.org/sqlite and its modernc.org dependencies | BSD-3-Clause |
| github.com/BurntSushi/toml | MIT |
| golang.org/x/sys | BSD-3-Clause |
| htmx 2.0.11, htmx-ext-sse 2.2.4 (embedded web assets) | 0BSD |

Installed from the distro by `install.sh`: GNU ddrescue, sg3_utils,
dvd+rw-tools, 7-Zip, ExifTool, util-linux, eject, lsscsi.
