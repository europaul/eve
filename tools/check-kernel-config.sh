#!/bin/sh
#
# check-kernel-config.sh -- Assert that a kernel image enables the config
# symbols a build target depends on.
#
# The split rootfs mounts its Extension as a compressed erofs image through
# dm-verity. A kernel without those options produces an image that installs
# and boots fine, but whose Extension can never be mounted -- the failure only
# shows up on device as a mount error from extsloader, long after the build.
#
# The config is read from kernel-dev.tar (usr/src/linux-headers-*/.config),
# which eve-kernel images ship. CONFIG_IKCONFIG is deliberately not used: the
# IKCFG_ST marker lives inside the compressed bzImage payload, so extracting it
# would mean reimplementing scripts/extract-ikconfig for every compression
# format.
#
# A symbol counts as present when set to y or m. Loadable is fine for both
# erofs and dm-verity: mount(8) and veritysetup(8) autoload them.
#
# Usage:
#   ./tools/check-kernel-config.sh <kernel-tag> <SYMBOL>...
#
# Environment:
#   LINUXKIT           path to the linuxkit binary (default: build-tools/bin/linuxkit)
#   ZARCH              target architecture (default: amd64)
#   SKIP_KERNEL_CHECK  set to non-empty to skip the check entirely
#
set -e

KERNEL_TAG="$1"
[ -n "$KERNEL_TAG" ] || { echo "usage: $0 <kernel-tag> <SYMBOL>..." >&2; exit 1; }
shift
[ $# -gt 0 ] || { echo "usage: $0 <kernel-tag> <SYMBOL>..." >&2; exit 1; }

if [ -n "$SKIP_KERNEL_CHECK" ]; then
    echo "check-kernel-config: SKIP_KERNEL_CHECK set, skipping"
    exit 0
fi

LINUXKIT="${LINUXKIT:-build-tools/bin/linuxkit}"
ZARCH="${ZARCH:-amd64}"

if [ ! -x "$LINUXKIT" ] && ! command -v "$LINUXKIT" >/dev/null 2>&1; then
    echo "check-kernel-config: WARNING: $LINUXKIT not found, skipping check" >&2
    exit 0
fi

config=$("$LINUXKIT" cache export --platform "linux/$ZARCH" --format filesystem \
             --outfile - "$KERNEL_TAG" 2>/dev/null \
         | tar -xOf - kernel-dev.tar 2>/dev/null \
         | tar -xO --wildcards '*/.config' 2>/dev/null) || true

# An unreadable config must not break builds for kernel flavors that ship no
# kernel-dev.tar; warn and let the build proceed.
if ! echo "$config" | grep -q '^CONFIG_'; then
    echo "check-kernel-config: WARNING: could not read .config from $KERNEL_TAG," \
         "skipping check" >&2
    exit 0
fi

missing=
for sym in "$@"; do
    echo "$config" | grep -qE "^${sym}=(y|m)$" || missing="$missing $sym"
done

if [ -n "$missing" ]; then
    echo "ERROR: kernel $KERNEL_TAG is missing required config:$missing" >&2
    echo "" >&2
    echo "The split rootfs Extension is a compressed erofs image mounted via" >&2
    echo "dm-verity; without these the Extension cannot be mounted on device." >&2
    echo "Build against a kernel that enables them, e.g.:" >&2
    echo "  make KERNEL_TAG=<tag-with-erofs-and-dm-verity> ..." >&2
    echo "or set SKIP_KERNEL_CHECK=1 to build anyway." >&2
    exit 1
fi

echo "check-kernel-config: $KERNEL_TAG has$(for s in "$@"; do printf ' %s' "$s"; done)"
