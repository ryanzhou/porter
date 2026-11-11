#!/bin/sh
# Runs the kernel-level and end-to-end tests inside the userland the router
# image is made of: Debian with exactly the packages in image/packages, and
# nothing else. Run on a development machine, the same tests use whatever
# that machine has installed, which hides two kinds of mistake: relying on
# a file or program the image does not contain, and relying on the
# behaviour of a different version of nft, dnsmasq, dhcpcd, hostapd or
# wpa_supplicant than the one the router runs.
#
#   sudo daemon/e2e/in-image-userland.sh [go-test-flags...]
#
# The root file system is built for this machine's architecture (the tests
# need the real kernel, so they cannot run under emulation) and without
# the Raspberry Pi kernel and firmware, which only matter on the hardware.
# Needs root, mmdebstrap, curl and a Go toolchain.
set -eu

HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/../.." && pwd)
SUITE=trixie

[ "$(id -u)" = 0 ] || { echo "must run as root" >&2; exit 1; }
for tool in mmdebstrap curl go xz dpkg-deb; do
    command -v "$tool" >/dev/null || { echo "missing tool: $tool" >&2; exit 1; }
done

WORK=$(mktemp -d "${TMPDIR:-/var/tmp}/porter-userland.XXXXXX")
ROOT=$WORK/root
cleanup() {
    for m in lib/modules run/netns run tmp dev sys proc ""; do
        umount -R -l "$ROOT/$m" 2>/dev/null || true
    done
    rm -rf --one-file-system "$WORK"
}
trap cleanup EXIT

echo "==> test binaries"
mkdir -p "$WORK/bin"
( cd "$REPO/daemon"
  export CGO_ENABLED=0
  go build -trimpath -o "$WORK/bin/porterd" ./cmd/porterd
  go test -c -o "$WORK/bin/fw.test" ./internal/fw/
  go test -c -tags e2e -o "$WORK/bin/e2e.test" ./e2e/ )

# The mpquic the image ships, for this machine's architecture.
"$REPO/image/build-mpquic.sh" "$(cd "$REPO/daemon" && go env GOARCH)" "$WORK/bin/mpquic" >/dev/null

echo "==> root file system from image/packages"
# Ubuntu's copy of the Debian keyring is often too old for the current
# release, so take the keyring from Debian itself.
# release, so take the keyring from Debian itself: the package pinned for
# the image, checked against its hash.
. "$REPO/image/debian-keyring.pin"
curl -fsS -o "$WORK/keyring.deb" "https://deb.debian.org/debian/$DEBIAN_KEYRING_DEB"
echo "$DEBIAN_KEYRING_SHA256  $WORK/keyring.deb" | sha256sum -c --quiet - \
    || { echo "the Debian keyring package does not have the hash in image/debian-keyring.pin" >&2; exit 1; }
dpkg-deb -x "$WORK/keyring.deb" "$WORK/keyring"
PACKAGES=$(grep -vE '^\s*(#|$)' "$REPO/image/packages" \
    | grep -vE '^(raspberrypi-archive-keyring|linux-image-|raspi-firmware|firmware-|bluez-firmware)' | paste -sd,)
mmdebstrap --variant=minbase --mode=root \
    --keyring="$WORK/keyring/usr/share/keyrings/debian-archive-keyring.gpg" \
    --include="$PACKAGES" \
    --aptopt='Apt::Install-Recommends "false"' \
    --dpkgopt='force-confold' \
    --dpkgopt='path-exclude=/usr/share/man/*' \
    --dpkgopt='path-exclude=/usr/share/doc/*' \
    --dpkgopt='path-exclude=/usr/share/locale/*' \
    "$SUITE" "$ROOT" \
    "deb https://deb.debian.org/debian $SUITE main" \
    "deb https://deb.debian.org/debian $SUITE-updates main" \
    "deb https://deb.debian.org/debian-security $SUITE-security main" >"$WORK/mmdebstrap.log" 2>&1 \
    || { tail -40 "$WORK/mmdebstrap.log" >&2; exit 1; }

mkdir -p "$ROOT/opt/porter-test"
cp "$WORK/bin/"* "$ROOT/opt/porter-test/"
# "ip netns" insists that / is a mount point, so make the root one.
mount --bind "$ROOT" "$ROOT"
mount -t proc proc "$ROOT/proc"
mount --rbind /sys "$ROOT/sys"
mount --rbind /dev "$ROOT/dev"
mount -t tmpfs tmpfs "$ROOT/run"
mount -t tmpfs tmpfs "$ROOT/tmp"
mkdir -p "$ROOT/run/netns"
# The Wi-Fi test loads the simulated-radio module, which belongs to the
# running kernel, not to the image.
if [ -d /lib/modules ]; then
    mkdir -p "$ROOT/lib/modules"
    mount --bind -o ro /lib/modules "$ROOT/lib/modules"
fi

# Where the kernel has no WireGuard, the tests can use a userspace one.
WG=
if [ -n "${PORTER_WG_USERSPACE:-}" ]; then
    cp "$PORTER_WG_USERSPACE" "$ROOT/opt/porter-test/wireguard"
    WG=/opt/porter-test/wireguard
fi

MPQ=
[ -x "$ROOT/opt/porter-test/mpquic" ] && MPQ=/opt/porter-test/mpquic

inside() {
    chroot "$ROOT" /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin HOME=/root \
        PORTER_E2E_BIN=/opt/porter-test/porterd PORTER_WG_USERSPACE="$WG" PORTER_MPQUIC="$MPQ" "$@"
}

echo "==> firewall and policy routing"
inside sh -c 'cd /tmp && /opt/porter-test/fw.test -test.count=1 -test.v' | tee "$WORK/fw.log"
echo "==> end to end"
inside sh -c 'cd /tmp && /opt/porter-test/e2e.test -test.count=1 -test.v -test.timeout 10m "$@"' sh "$@" | tee "$WORK/e2e.log"

# A test that skipped itself here did so because the image lacks something
# it needs, which is exactly what this run exists to find.
if grep -E '^(--- SKIP: (TestRender|TestRouter)\b|    --- SKIP: TestRouter/MPQUIC\b)' "$WORK/fw.log" "$WORK/e2e.log"; then
    echo "a test that must run inside the image's userland was skipped" >&2
    exit 1
fi
if grep -E '^(--- FAIL|FAIL)' "$WORK/fw.log" "$WORK/e2e.log" >/dev/null; then
    exit 1
fi
echo "==> passed inside the image's userland"
