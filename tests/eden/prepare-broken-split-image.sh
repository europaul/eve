#!/bin/bash
#
# Build a broken split-rootfs EVE image for rollback testing.
#
# Produces a split image whose Core embeds a corrupted ext-verity-roothash.
# The Extension image itself is left byte-for-byte valid, so nothing fails
# until dm-verity compares it against the wrong expected hash -- one artifact
# therefore models both a tampered and a corrupted Extension.
#
# Result: Core boots fine, extsloader finds the Extension but the mount fails →
# nodeagent testing window expires → automatic rollback.
#
# The broken image is built under its OWN version (default: <good>-broken) in
# its OWN dist directory. The good build's artifacts and Docker tags are never
# touched, so the good and broken images can coexist -- which the evetest
# scenarios require, since they update from one to the other.
#
# Prerequisites:
#   - Docker Hub login (unless SKIP_PUSH=1)
#   - Packages already built (make UNIVERSAL=1 pkgs)
#
# Usage:
#   ./tests/eden/prepare-broken-split-image.sh
#
# Options:
#   GOOD_VERSION=<ver>     Reuse an existing good build in dist/<arch>/<ver>
#                          instead of running `make eve-split` (much faster;
#                          the version is the dist directory name)
#   BROKEN_VERSION=<ver>   Version for the broken image (default: <good>-broken)
#   REGISTRY_USER=<user>   Docker Hub username (default: auto-detect)
#   EVE_REGISTRY=<path>    Full registry path (default: <REGISTRY_USER>/eve)
#   HV_TAG=<hv>            Extra tag alias to publish, e.g. kvm. evetest builds
#                          image names as <repo>:<version>-<hv>-<arch> and has
#                          no "uni" hypervisor, so it needs this alias.
#   SKIP_PUSH=1            Don't push, just build
#   FORCE=1                Overwrite an existing broken dist directory
#   KERNEL_TAG=<tag>       Custom kernel tag (passed to make)

set -e

PREFIX="[BROKEN-SPLIT]"

EVE_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
EVE_HV="${EVE_HV:-uni}"
EVE_ARCH="${EVE_ARCH:-amd64}"

cd "$EVE_ROOT"

# shellcheck source=tests/eden/lib-split-image.sh
. "$EVE_ROOT/tests/eden/lib-split-image.sh"

split_resolve_registry

KERNEL_OPT=""
if [ -n "$KERNEL_TAG" ]; then
    KERNEL_OPT="KERNEL_TAG=$KERNEL_TAG"
fi

# --- Step 1: Obtain a good split build ---
VER=$(split_obtain_good_build)
BROKEN_VER="${BROKEN_VERSION:-${VER}-broken}"
BROKEN_DIR="dist/$EVE_ARCH/$BROKEN_VER"
INSTALLER="$BROKEN_DIR/installer"

echo "$PREFIX Good version:   $VER"
echo "$PREFIX Broken version: $BROKEN_VER"

# --- Step 2: Copy the good build and stamp the broken version ---
split_copy_build "$VER" "$BROKEN_VER"
split_stamp_version "$BROKEN_DIR" "$BROKEN_VER"

# --- Step 3: Corrupt the roothash ---
# Only the expected hash is wrong; rootfs-ext.img stays byte-for-byte valid,
# so nothing fails until dm-verity compares the two. One artifact therefore
# models both a tampered and a corrupted Extension.
echo "$PREFIX Original roothash: $(head -1 "$INSTALLER/ext-verity-roothash")"

HASH=$(head -1 "$INSTALLER/ext-verity-roothash")
OFFSET=$(tail -1 "$INSTALLER/ext-verity-roothash")
FIRST_CHAR="${HASH:0:1}"
if [ "$FIRST_CHAR" = "0" ]; then
    NEW_FIRST="f"
else
    NEW_FIRST="0"
fi
printf '%s\n%s\n' "${NEW_FIRST}${HASH:1}" "$OFFSET" > "$INSTALLER/ext-verity-roothash"

echo "$PREFIX Corrupted roothash: $(head -1 "$INSTALLER/ext-verity-roothash")"

if [ "$(head -1 "dist/$EVE_ARCH/$VER/installer/ext-verity-roothash")" = \
     "$(head -1 "$INSTALLER/ext-verity-roothash")" ]; then
    echo "$PREFIX Error: roothash was not corrupted."
    exit 1
fi

# --- Step 4: Rebuild Core with the corrupted roothash, and package ---
split_rebuild_core "$BROKEN_DIR"
split_package_oci "$BROKEN_DIR" "$BROKEN_VER"
split_verify_version "$BROKEN_VER"
split_tag_and_push "$BROKEN_VER"

echo ""
echo "================================================================"
echo "$PREFIX Done."
echo "  Broken image version: $BROKEN_VER"
echo "  Good image ($VER) and its dist directory are untouched."
echo "  The Core rootfs has a corrupted ext-verity-roothash;"
echo "  the Extension image is valid but dm-verity will fail on mount."
echo ""
echo "  evetest:"
echo "    EVETEST_BROKEN_EVE_VERSION=$BROKEN_VER \\"
echo "      make evetest NAME=TestSplitBrokenExtensionRollback"
echo ""
echo "  eden:"
echo "    EVE_VERSION_BROKEN=$BROKEN_VER EVE_REGISTRY=$EVE_REGISTRY \\"
echo "      ./eden test ./tests/update_eve_image -v debug \\"
echo "      -r TestEdenScripts/update_split_broken_rollback"
echo "================================================================"
