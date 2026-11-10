#!/bin/bash
# Builds the Porter SD card image for the Raspberry Pi 5.
#
#   sudo image/build.sh [output-dir]
#
# Needs root (file ownership in the root file system, chroot) and:
#   mmdebstrap, dosfstools, mtools, e2fsprogs, fdisk (sfdisk), xz-utils,
#   gpg, curl, a Go toolchain, and - on anything but an arm64 host -
#   qemu-user-static with binfmt_misc registered, plus arch-test.
#
# The image is assembled from partition files; nothing is loop-mounted,
# so this also runs inside unprivileged-ish containers and CI runners.
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(dirname "$HERE")
OUT=${1:-$REPO/build}
SUITE=trixie
DISK_ID=504f5254          # must match fstab, cmdline.txt and ensure-data
BOOT_MB=128
DATA_MB=256

VERSION=${PORTER_VERSION:-$(git -C "$REPO" describe --tags --always --dirty 2>/dev/null || echo dev)}
BUILD_TIME=${SOURCE_DATE_EPOCH:-$(date +%s)}

[ "$(id -u)" = 0 ] || { echo "build.sh must run as root" >&2; exit 1; }
for tool in mmdebstrap mkfs.vfat mcopy mkfs.ext4 sfdisk xz gpg curl go; do
    command -v "$tool" >/dev/null || { echo "missing tool: $tool" >&2; exit 1; }
done

if [ "$(dpkg --print-architecture)" != arm64 ]; then
    command -v arch-test >/dev/null || { echo "missing tool: arch-test (needed to build for arm64 on this host)" >&2; exit 1; }
    arch-test arm64 >/dev/null 2>&1 || { echo "this host cannot run arm64 binaries: install qemu-user-static and binfmt-support" >&2; exit 1; }
fi

WORK=$(mktemp -d "${TMPDIR:-/var/tmp}/porter-build.XXXXXX")
trap 'rm -rf "$WORK"' EXIT
ROOT=$WORK/root
mkdir -p "$OUT"

echo "==> porterd $VERSION (arm64)"
( cd "$REPO/daemon" && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath \
    -ldflags "-s -w -X main.version=$VERSION -X main.buildTime=$BUILD_TIME" \
    -o "$WORK/bin/" ./cmd/porterd ./cmd/porterctl )

echo "==> package signing keys"
gpg --batch --yes --dearmor -o "$WORK/rpi.gpg" < "$HERE/keys/raspberrypi-archive.asc"
DEBKEYS=/usr/share/keyrings/debian-archive-keyring.gpg
if ! { [ -r "$DEBKEYS" ] && curl -fsS "https://deb.debian.org/debian/dists/$SUITE/InRelease" \
        | gpgv --keyring "$DEBKEYS" >/dev/null 2>&1; }; then
    # The host's Debian keyring is missing or too old to know this release
    # (typical on Ubuntu). Fetch the current keyring package from Debian.
    echo "    host keyring does not cover $SUITE; fetching debian-archive-keyring"
    deb=$(curl -fsS "https://deb.debian.org/debian/dists/$SUITE/main/binary-all/Packages.xz" | xz -dc \
        | awk '/^Package: debian-archive-keyring$/{p=1} p&&/^Filename:/&&!f{f=$2} END{print f}')
    curl -fsS -o "$WORK/keyring.deb" "https://deb.debian.org/debian/$deb"
    dpkg-deb -x "$WORK/keyring.deb" "$WORK/keyring"
    DEBKEYS=$WORK/keyring/usr/share/keyrings/debian-archive-keyring.gpg
fi
cat "$DEBKEYS" "$WORK/rpi.gpg" > "$WORK/keys.gpg"

echo "==> root file system"
PACKAGES=$(grep -vE '^\s*(#|$)' "$HERE/packages" | paste -sd,)
mmdebstrap --architectures=arm64 --variant=minbase --mode=root \
    --keyring="$WORK/keys.gpg" \
    --include="$PACKAGES" \
    --aptopt='Apt::Install-Recommends "false"' \
    --dpkgopt='force-confold' \
    --dpkgopt='path-exclude=/usr/share/man/*' \
    --dpkgopt='path-exclude=/usr/share/doc/*' \
    --dpkgopt='path-include=/usr/share/doc/*/copyright' \
    --dpkgopt='path-exclude=/usr/share/locale/*' \
    --essential-hook='mkdir -p "$1/etc/initramfs-tools" && echo update_initramfs=no > "$1/etc/initramfs-tools/update-initramfs.conf"' \
    "$SUITE" "$ROOT" \
    "deb https://deb.debian.org/debian $SUITE main contrib non-free non-free-firmware" \
    "deb https://deb.debian.org/debian $SUITE-updates main contrib non-free non-free-firmware" \
    "deb https://deb.debian.org/debian-security $SUITE-security main contrib non-free non-free-firmware" \
    "deb https://archive.raspberrypi.com/debian $SUITE main"

echo "==> customising"
cp -a "$HERE/rootfs/." "$ROOT/"
chown -R 0:0 "$ROOT/etc/systemd" "$ROOT/usr/lib/porter" "$ROOT/etc/bluetooth" "$ROOT/etc/ssh/sshd_config.d" \
    "$ROOT/etc/sysctl.d" "$ROOT/etc/tmpfiles.d" "$ROOT/etc/fstab" "$ROOT/etc/hostname" "$ROOT/etc/hosts"
install -m 755 "$WORK/bin/porterd" "$ROOT/usr/sbin/porterd"
install -m 755 "$WORK/bin/porterctl" "$ROOT/usr/sbin/porterctl"
mkdir -p "$ROOT/data" "$ROOT/boot/firmware"
printf 'PORTER_VERSION=%s\nPORTER_BUILD_TIME=%s\n' "$VERSION" "$BUILD_TIME" > "$ROOT/etc/porter-release"

# Country database for country rules and the flow list. It is baked in
# here because the router itself never downloads anything. DB-IP publishes
# a new file each month; early in a month the previous one may be all
# there is.
if [ "${PORTER_NO_GEOIP:-0}" != 1 ]; then
    echo "==> country database"
    GEO_DATE=
    for month in "$(date -u +%Y-%m)" "$(date -u -d "$(date -u +%Y-%m-15) -1 month" +%Y-%m)"; do
        if curl -fsS -o "$WORK/geoip.csv.gz" "https://download.db-ip.com/free/dbip-country-lite-$month.csv.gz"; then
            GEO_DATE=$month
            break
        fi
    done
    if [ -z "$GEO_DATE" ] || ! gzip -dc "$WORK/geoip.csv.gz" | head -n 1 | grep -Eq '^[0-9a-fA-F.:]+,[0-9a-fA-F.:]+,[A-Z]{2}$'; then
        echo "could not fetch the country database from db-ip.com (set PORTER_NO_GEOIP=1 to build without country rules)" >&2
        exit 1
    fi
    install -D -m 644 "$WORK/geoip.csv.gz" "$ROOT/usr/share/porter/geoip.csv.gz"
    echo "$GEO_DATE" > "$ROOT/usr/share/porter/geoip.csv.gz.date"
    cat > "$ROOT/usr/share/porter/geoip.LICENSE" <<LICENSE
IP to Country Lite database ($GEO_DATE) by DB-IP, https://db-ip.com
Licensed under Creative Commons Attribution 4.0 International (CC BY 4.0).
LICENSE
    echo "    DB-IP country lite $GEO_DATE"
fi

# The router's own lookups go through its own forwarder.
ln -sfn /run/porter/resolv.conf "$ROOT/etc/resolv.conf"
# No machine identity or host keys baked into an image that many cards
# will be flashed from: the machine id is generated in RAM at boot, SSH
# host keys on the data partition when SSH is first enabled.
: > "$ROOT/etc/machine-id"
rm -f "$ROOT"/etc/ssh/ssh_host_* "$ROOT/var/lib/dbus/machine-id"
# No password logins exist.
sed -i 's|^root:[^:]*:|root:*:|' "$ROOT/etc/shadow"
# bluetoothd wants its configuration directory at this mode and cannot
# fix it up on a read-only root.
chmod 555 "$ROOT/etc/bluetooth"
# The kernel boots straight from the card without an initramfs.
rm -f "$ROOT"/boot/initrd.img-* "$ROOT"/boot/firmware/initramfs*

# porterd runs hostapd, wpa_supplicant, dnsmasq and dhcpcd itself and
# switches SSH and time sync on demand; nothing may start them behind its
# back, and nothing may try to update the system.
SYSTEMCTL="systemctl --root=$ROOT --quiet"
$SYSTEMCTL mask hostapd.service wpa_supplicant.service sshd-keygen.service \
    apt-daily.timer apt-daily-upgrade.timer dpkg-db-backup.timer e2scrub_all.timer \
    e2scrub_reap.service systemd-pstore.service systemd-random-seed.service \
    systemd-update-utmp.service systemd-machine-id-commit.service 2>/dev/null || true
$SYSTEMCTL disable ssh.service ssh.socket systemd-timesyncd.service 2>/dev/null || true
$SYSTEMCTL enable porterd.service bluetooth.service

# Trim what was only needed to install packages.
rm -rf "$ROOT"/var/lib/apt/lists/* "$ROOT"/var/cache/apt/* "$ROOT"/var/log/*.log "$ROOT"/var/log/apt

echo "==> boot partition"
BOOT=$WORK/boot
mkdir -p "$BOOT"
[ -f "$ROOT/boot/firmware/kernel_2712.img" ] || { echo "kernel_2712.img missing from the root file system" >&2; exit 1; }
cp "$ROOT/boot/firmware/kernel_2712.img" "$BOOT/"
cp "$ROOT"/boot/firmware/bcm2712*.dtb "$BOOT/"
cp -r "$ROOT/boot/firmware/overlays" "$BOOT/"
cp "$ROOT/boot/firmware/LICENCE.broadcom" "$BOOT/" 2>/dev/null || true
cp "$HERE/boot/config.txt" "$HERE/boot/cmdline.txt" "$HERE/boot/README.txt" "$BOOT/"
# The files live on the FAT partition; the directory is its mount point.
rm -rf "$ROOT/boot/firmware" && mkdir "$ROOT/boot/firmware"

truncate -s "${BOOT_MB}M" "$WORK/boot.vfat"
mkfs.vfat -F 32 -n BOOTFS -i "${DISK_ID}" "$WORK/boot.vfat" >/dev/null
mcopy -s -i "$WORK/boot.vfat" "$BOOT"/* ::/

echo "==> root and data partitions"
ROOT_KB=$(du -sk --apparent-size "$ROOT" | cut -f1)
ROOT_MB=$(( ROOT_KB * 125 / 100 / 1024 + 96 ))
truncate -s "${ROOT_MB}M" "$WORK/root.ext4"
mkfs.ext4 -q -F -L rootfs -d "$ROOT" "$WORK/root.ext4"
mkdir -p "$WORK/data"
truncate -s "${DATA_MB}M" "$WORK/data.ext4"
mkfs.ext4 -q -F -L porter-data -d "$WORK/data" "$WORK/data.ext4"

echo "==> disk image"
IMG=$OUT/porter-$VERSION.img
ALIGN_MB=4
BOOT_START=$ALIGN_MB
ROOT_START=$(( BOOT_START + BOOT_MB ))
DATA_START=$(( ROOT_START + ROOT_MB ))
TOTAL_MB=$(( DATA_START + DATA_MB ))
rm -f "$IMG" "$IMG.xz"
truncate -s "${TOTAL_MB}M" "$IMG"
sfdisk --quiet "$IMG" <<PARTS
label: dos
label-id: 0x$DISK_ID
unit: sectors
start=$(( BOOT_START * 2048 )), size=$(( BOOT_MB * 2048 )), type=c, bootable
start=$(( ROOT_START * 2048 )), size=$(( ROOT_MB * 2048 )), type=83
start=$(( DATA_START * 2048 )), size=$(( DATA_MB * 2048 )), type=83
PARTS
dd if="$WORK/boot.vfat" of="$IMG" bs=1M seek=$BOOT_START conv=notrunc,sparse status=none
dd if="$WORK/root.ext4" of="$IMG" bs=1M seek=$ROOT_START conv=notrunc,sparse status=none
dd if="$WORK/data.ext4" of="$IMG" bs=1M seek=$DATA_START conv=notrunc,sparse status=none

echo "==> compressing"
xz -T0 -6 -f "$IMG"
( cd "$OUT" && sha256sum "$(basename "$IMG").xz" > "$(basename "$IMG").xz.sha256" )
ls -lh "$IMG.xz"
echo "==> done: $IMG.xz (${TOTAL_MB} MB uncompressed)"
