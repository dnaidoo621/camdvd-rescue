#!/usr/bin/env bash
# Builds the release bundle camdvd-<version>-linux-x64.tar.gz in dist/:
# the static app, the pinned FFmpeg/ffprobe, dvd-vr built at a pinned commit,
# the installer and its packaging files, plus a SHA-256 checksum.
#
#   scripts/build-release.sh 1.0.0
#
# Needs Go, curl, xz, git and a C compiler (for dvd-vr). Runs on Linux x64.
set -euo pipefail
VERSION=${1:?usage: build-release.sh VERSION}
VERSION=${VERSION#v}
ROOT=$(cd "$(dirname "$0")/.." && pwd)
# shellcheck source=versions.env
. "$ROOT/scripts/versions.env"
NAME=camdvd-$VERSION-linux-x64
WORK=$ROOT/dist/work
OUT=$ROOT/dist/$NAME
rm -rf "$OUT" && mkdir -p "$OUT/bin" "$WORK"

echo "==> camdvd $VERSION"
(cd "$ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
  -ldflags "-s -w -X main.version=v$VERSION" -o "$OUT/camdvd" ./cmd/camdvd)

echo "==> FFmpeg ($FFMPEG_ASSET)"
FF=$WORK/$FFMPEG_ASSET
if [ ! -f "$FF" ]; then
  curl -fsSL -o "$FF.tmp" "https://github.com/BtbN/FFmpeg-Builds/releases/download/$FFMPEG_TAG/$FFMPEG_ASSET"
  mv "$FF.tmp" "$FF"
fi
echo "$FFMPEG_SHA256  $FF" | sha256sum -c -
rm -rf "$WORK/ffmpeg" && mkdir "$WORK/ffmpeg"
tar -xJf "$FF" -C "$WORK/ffmpeg" --strip-components=1
cp "$WORK/ffmpeg/bin/ffmpeg" "$WORK/ffmpeg/bin/ffprobe" "$OUT/bin/"
cp "$WORK/ffmpeg/LICENSE.txt" "$OUT/bin/FFMPEG-LICENSE.txt" 2>/dev/null || true

echo "==> dvd-vr ($DVDVR_COMMIT)"
if [ ! -d "$WORK/dvd-vr" ]; then
  git clone -q "$DVDVR_REPO" "$WORK/dvd-vr"
fi
(cd "$WORK/dvd-vr" && git checkout -q "$DVDVR_COMMIT" && make -s clean >/dev/null 2>&1 || true; make -s)
cp "$WORK/dvd-vr/dvd-vr" "$OUT/bin/"
cp "$WORK/dvd-vr/COPYING" "$OUT/bin/DVD-VR-COPYING.txt"

echo "==> installer and docs"
cp "$ROOT/install.sh" "$OUT/"
cp -r "$ROOT/packaging" "$OUT/"
cp "$ROOT/LICENSE" "$ROOT/README.md" "$ROOT/THIRD_PARTY.md" "$OUT/"
echo "v$VERSION" >"$OUT/VERSION"

demuxers=$("$OUT/bin/ffmpeg" -hide_banner -demuxers)
grep -qw dvdvideo <<<"$demuxers" || { echo "bundled FFmpeg lacks dvdvideo" >&2; exit 1; }
"$OUT/camdvd" version

(cd "$ROOT/dist" && tar -czf "$NAME.tar.gz" --owner=0 --group=0 "$NAME" && sha256sum "$NAME.tar.gz" >"$NAME.tar.gz.sha256")
echo "==> dist/$NAME.tar.gz"
cat "$ROOT/dist/$NAME.tar.gz.sha256"
