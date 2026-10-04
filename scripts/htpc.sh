#!/usr/bin/env bash
# Sync the working tree to the test HTPC and run a command there.
#   scripts/htpc.sh go test ./...
set -euo pipefail
HOST=${CAMDVD_HOST:-darren@192.168.10.158}
DIR=${CAMDVD_DIR:-camdvd-rescue}
cd "$(dirname "$0")/.."
rsync -az --delete --exclude .git --exclude dist/ --exclude '*.img' --exclude testdata/gen/ --exclude /camdvd ./ "$HOST:$DIR/"
ssh "$HOST" "cd $DIR && export PATH=\$HOME/.local/go/bin:\$HOME/go/bin:\$PATH && $*"
