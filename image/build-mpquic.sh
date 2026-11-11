#!/bin/sh
# Builds the mpquic pinned in mpquic.pin for Linux on the given
# architecture.
#
#   image/build-mpquic.sh arm64 /path/to/mpquic
set -eu
HERE=$(cd "$(dirname "$0")" && pwd)
[ $# -eq 2 ] || { echo "usage: $0 <go architecture> <output file>" >&2; exit 2; }
arch=$1
out=$(cd "$(dirname "$2")" && pwd)/$(basename "$2")
. "$HERE/mpquic.pin"

src=$(mktemp -d "${TMPDIR:-/tmp}/mpquic-src.XXXXXX")
trap 'rm -rf "$src"' EXIT
git -C "$src" init -q
git -C "$src" fetch -q --depth 1 https://github.com/ryanzhou/mpquic "$MPQUIC_COMMIT"
git -C "$src" -c advice.detachedHead=false checkout -q FETCH_HEAD
got=$(git -C "$src" rev-parse HEAD)
[ "$got" = "$MPQUIC_COMMIT" ] || { echo "fetched mpquic $got, not the $MPQUIC_COMMIT of image/mpquic.pin" >&2; exit 1; }
short=$(printf '%.7s' "$MPQUIC_COMMIT")
( cd "$src" && CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath \
    -ldflags "-s -w -X main.version=$short" -o "$out" ./cmd/mpquic )
echo "$short"
