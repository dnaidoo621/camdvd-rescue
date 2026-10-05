#!/usr/bin/env bash
# Builds synthetic disc images for tests and demo mode in testdata/gen/.
#
#   finalized.img       DVD-Video: a 4:3 and a 16:9 title, 2 chapters each
#   unfinalized-a.img   raw packs, no file system, 3 recordings (side A)
#   unfinalized-b.img   raw packs, 2 recordings (side B of the same disc)
#   unfinalized-tracks.img  a camcorder track table with unwritten gaps
#   damaged.img         unfinalized-a with unreadable sectors mid-recording
#   data.img            ISO9660 with documents, no video
#   blank.img           blank DVD-R
#
# Each image has a .media.json sidecar standing in for dvd+rw-mediainfo, and
# damaged.img a .bad.json listing the sectors the image drive fails to read.
# Needs ffmpeg (any version with -target pal-dvd), dvdauthor and genisoimage.
set -euo pipefail
FFMPEG=${FFMPEG:-ffmpeg}
OUT=${1:-"$(cd "$(dirname "$0")/.." && pwd)/testdata/gen"}
SECS=${SECS:-4}
rm -rf "$OUT" && mkdir -p "$OUT/clips"
cd "$OUT"

clip() { # name seconds aspect hue
  "$FFMPEG" -hide_banner -loglevel error -y \
    -f lavfi -i "testsrc2=size=720x576:rate=25,hue=h=$4,drawtext=text='$1':fontsize=64:fontcolor=white:x=40:y=40" \
    -f lavfi -i "sine=frequency=$((300 + $4)):sample_rate=48000" \
    -t "$2" -target pal-dvd -aspect "$3" -flags +ilme+ildct -top 1 -b:v 4000k \
    "clips/$1.mpg"
}
clip rec1 "$SECS" 4:3 0
clip rec2 "$SECS" 4:3 90
clip rec3 "$SECS" 16:9 180
clip rec4 "$SECS" 16:9 270
clip recB1 "$SECS" 4:3 45
clip recB2 "$SECS" 4:3 135
# One recording in two clock segments, like a Hitachi camcorder writes: the
# second part's clock restarts but its GOP timecode carries on.
"$FFMPEG" -hide_banner -loglevel error -y -f lavfi -i "testsrc2=size=720x576:rate=25,drawtext=text='long':fontsize=64:fontcolor=white:x=40:y=40" \
  -f lavfi -i "sine=frequency=500:sample_rate=48000" -t "$SECS" -target pal-dvd -flags +ilme+ildct -top 1 -b:v 4000k clips/long1.mpg
"$FFMPEG" -hide_banner -loglevel error -y -f lavfi -i "testsrc2=size=720x576:rate=25,drawtext=text='long':fontsize=64:fontcolor=white:x=40:y=40" \
  -f lavfi -i "sine=frequency=500:sample_rate=48000" -t "$SECS" -target pal-dvd -flags +ilme+ildct -top 1 -b:v 4000k \
  -timecode "00:00:$(printf %02d "$SECS"):00" clips/long2.mpg

sidecar() { # file type status session next
  printf '{"type":"%s","disc_status":"%s","session_state":"%s","next_writable":%s}\n' "$2" "$3" "$4" "$5" >"$1.media.json"
}

# Finalized DVD-Video.
cat >dvd.xml <<'XML'
<dvdauthor>
  <vmgm />
  <titleset>
    <titles>
      <video format="pal" aspect="4:3" />
      <pgc>
        <vob file="clips/rec1.mpg" chapters="0" />
        <vob file="clips/rec2.mpg" chapters="0" />
      </pgc>
    </titles>
  </titleset>
  <titleset>
    <titles>
      <video format="pal" aspect="16:9" />
      <pgc>
        <vob file="clips/rec3.mpg" chapters="0" />
        <vob file="clips/rec4.mpg" chapters="0" />
      </pgc>
    </titles>
  </titleset>
</dvdauthor>
XML
VIDEO_FORMAT=PAL dvdauthor -o finalized -x dvd.xml 2>/dev/null
genisoimage -quiet -dvd-video -V CAMDVD_TEST -o finalized.img finalized
sidecar finalized.img "DVD-R Sequential" complete complete 0

# Unfinalized sides: a reserved, unwritten file-system area, the recordings
# back to back (each restarts its clock), then the unwritten tail.
raw() { # out clips...
  local out=$1; shift
  head -c $((2048 * 8432)) /dev/zero >"$out"
  for c in "$@"; do cat "clips/$c.mpg" >>"$out"; done
  local used=$(( $(stat -c %s "$out") / 2048 ))
  head -c $((2048 * 2048)) /dev/zero >>"$out"
  sidecar "$out" "DVD-R Sequential" appendable incomplete "$used"
}
raw unfinalized-a.img rec1 rec2 rec3
raw unfinalized-b.img recB1 recB2

# A camcorder's real track layout (scaled down): a reserved, unwritten
# file-system track, a small management track, an unwritten gap longer than
# the 4096-sector "unwritten tail" limit, another small track, then the video.
# The unwritten areas are listed as unreadable, as a drive reports them.
{
  head -c $((2048 * 6080)) /dev/urandom >tracks.tmp
  dd if=/dev/zero of=tracks.tmp bs=2048 count=528 conv=notrunc status=none
  dd if=/dev/zero of=tracks.tmp bs=2048 seek=600 count=5400 conv=notrunc status=none
  dd if=/dev/zero of=tracks.tmp bs=2048 seek=6064 count=16 conv=notrunc status=none
  cat clips/rec1.mpg clips/long1.mpg clips/long2.mpg clips/rec3.mpg >>tracks.tmp
  last=$(( $(stat -c %s tracks.tmp) / 2048 ))
  head -c $((2048 * 16)) /dev/zero >>tracks.tmp
  mv tracks.tmp unfinalized-tracks.img
  cat >unfinalized-tracks.img.media.json <<JSON
{"type":"DVD-R Sequential","disc_status":"appendable","session_state":"incomplete","next_writable":$((last + 16)),
 "tracks":[
  {"state":"reserved incremental","start":0,"size":512},
  {"state":"partial incremental","start":528,"size":5400,"next_writable":616,"last_recorded":599},
  {"state":"complete incremental","start":6000,"size":64,"last_recorded":6063},
  {"state":"incomplete incremental","start":6080,"size":700000,"next_writable":$((last + 16)),"last_recorded":$((last - 1))}]}
JSON
  echo "[{\"start\":0,\"end\":528},{\"start\":600,\"end\":6000},{\"start\":6064,\"end\":6080},{\"start\":$last,\"end\":$((last + 16))}]" >unfinalized-tracks.img.bad.json
}

# Damaged: 40 unreadable sectors inside the first recording.
cp unfinalized-a.img damaged.img
cp unfinalized-a.img.media.json damaged.img.media.json
dd if=/dev/zero of=damaged.img bs=2048 seek=8500 count=40 conv=notrunc status=none
echo '[{"start":8500,"end":8540}]' >damaged.img.bad.json

# Data disc.
mkdir -p data/Documents && echo "tax 2004" >data/Documents/tax.txt
genisoimage -quiet -J -r -V DATA -o data.img data
sidecar data.img "DVD-R Sequential" complete complete 0

# Blank.
: >blank.img
sidecar blank.img "DVD-R Sequential" blank empty 0

rm -rf data dvd.xml
ls -la "$OUT"
