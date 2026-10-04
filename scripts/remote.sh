#!/usr/bin/env bash
# Sync the working tree to a Linux test box and run a command there, for
# developing from a Mac. Go must be in ~/.local/go/bin on the remote.
#   CAMDVD_HOST=user@testbox scripts/remote.sh go test ./...
set -euo pipefail
HOST=${CAMDVD_HOST:?set CAMDVD_HOST=user@host}
DIR=${CAMDVD_DIR:-camdvd-rescue}
cd "$(dirname "$0")/.."
rsync -az --delete --exclude .git --exclude dist/ --exclude '*.img' --exclude testdata/gen/ --exclude /camdvd ./ "$HOST:$DIR/"
ssh "$HOST" "cd $DIR && export PATH=\$HOME/.local/go/bin:\$HOME/go/bin:\$PATH && $*"
