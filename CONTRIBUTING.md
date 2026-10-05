# Contributing

Thanks for helping. Bug reports from real discs are as valuable as code: if a
disc rips wrongly, open a **Disc didn't rip right** issue with the job's
evidence.

## How changes land

`main` is protected. Every change, including the maintainer's, goes through a
pull request that:

- passes CI (`test`: vet, gofmt, race-enabled tests against synthetic disc
  images) and has no new high-severity CodeQL alerts,
- has every review conversation resolved,
- is squash-merged, keeping history linear.

Release tags (`v*`) are created only by maintainers and can't be moved or
deleted once pushed.

## Development setup

You need Linux x64 for the drive code and the full test suite. Go version: see
`go.mod`.

```sh
sudo apt install dvdauthor genisoimage udftools 7zip libimage-exiftool-perl gddrescue sg3-utils dvd+rw-tools lsscsi
# An FFmpeg 7+ with dvdvideo, libx264 and bwdif; the pinned one:
. scripts/versions.env
curl -fsSLO "https://github.com/BtbN/FFmpeg-Builds/releases/download/$FFMPEG_TAG/$FFMPEG_ASSET"
mkdir ff && tar -xJf "$FFMPEG_ASSET" -C ff --strip-components=1
FFMPEG=$PWD/ff/bin/ffmpeg scripts/make-test-images.sh
CAMDVD_BIN=$PWD/ff/bin go test ./...
go run ./cmd/camdvd serve -config dev.toml -demo testdata/gen
```

A minimal `dev.toml`:

```toml
listen = "127.0.0.1:8780"
library = "/tmp/camdvd/library"
state_dir = "/tmp/camdvd/state"
tools_dir = "/path/to/ff/bin"
```

The demo drive lets you "insert" the synthetic images from the dashboard.
On a Mac, `CAMDVD_HOST=user@linuxbox scripts/remote.sh go test ./...` runs
commands on a Linux box.

## Guidelines

- Match the surrounding code. Read [docs/architecture.md](docs/architecture.md)
  first.
- External tools are called through `internal/tools` with argument slices,
  never shell strings. File writes go through `library.Within` so they stay
  in the library.
- New behaviour comes with a test; pipeline changes ideally with a fixture in
  `scripts/make-test-images.sh`.
- Update `CHANGELOG.md` and the docs when behaviour changes.
- Run `gofmt`, `go vet ./...` and the tests before pushing.

By contributing you agree that your work is licensed under the MIT licence.
