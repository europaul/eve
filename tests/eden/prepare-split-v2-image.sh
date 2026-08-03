#!/bin/bash
#
# Build a second split-rootfs EVE image (v2) to update TO from an existing
# split image (v1), for the split->split test scenarios.
#
# v2 differs from v1 in both its version AND its Extension content. The
# Extension gets a /etc/eve-ext-release marker carrying v2's version, so the
# two Extensions are genuinely different images with different dm-verity root
# hashes.
#
# That difference is the point. With byte-identical Extensions, ext-imga.img
# and ext-imgb.img are indistinguishable, so a device that mounted the wrong
# slot's Extension -- or never wrote the new slot's at all -- would still look
# healthy. Differing Extensions make the A/B pairing observable: a test can
# read /persist/exts/etc/eve-ext-release on the running device and check it
# against the version EVE reports.
#
# Note this makes v2 marginally non-stock: it carries one file a released
# Extension would not. The alternative -- bumping a package to force real
# content drift -- costs a full rebuild and gives the test nothing it can
# assert on directly.
#
# Prerequisites:
#   - An existing good split build (or this will run `make eve-split`)
#   - veritysetup on PATH (to regenerate the Extension's dm-verity metadata)
#   - Docker Hub login (unless SKIP_PUSH=1)
#
# Usage:
#   ./tests/eden/prepare-split-v2-image.sh
#
# Options:
#   GOOD_VERSION=<ver>   Reuse an existing split build as v1 (much faster)
#   V2_VERSION=<ver>     Version for v2 (default: <v1>-v2)
#   REGISTRY_USER=<user> Docker Hub username (default: auto-detect)
#   EVE_REGISTRY=<path>  Full registry path (default: <REGISTRY_USER>/eve)
#   HV_TAG=<hv>          Extra tag alias to publish, e.g. kvm
#   SKIP_PUSH=1          Don't push, just build
#   FORCE=1              Overwrite an existing v2 dist directory
#   KERNEL_TAG=<tag>     Custom kernel tag (passed to make)

set -e

PREFIX="[SPLIT-V2]"

EVE_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
EVE_HV="${EVE_HV:-uni}"
EVE_ARCH="${EVE_ARCH:-amd64}"

cd "$EVE_ROOT"

# shellcheck source=tests/eden/lib-split-image.sh
. "$EVE_ROOT/tests/eden/lib-split-image.sh"

if ! command -v veritysetup >/dev/null 2>&1; then
    echo "$PREFIX Error: veritysetup not found. It is required to regenerate the"
    echo "         Extension's dm-verity metadata after changing its contents."
    exit 1
fi

split_resolve_registry

KERNEL_OPT=""
if [ -n "$KERNEL_TAG" ]; then
    KERNEL_OPT="KERNEL_TAG=$KERNEL_TAG"
fi

# ── Step 1: Obtain v1 ───────────────────────────────────────────────
V1=$(split_obtain_good_build)
V2="${V2_VERSION:-${V1}-v2}"
V2_DIR="dist/$EVE_ARCH/$V2"
INSTALLER="$V2_DIR/installer"

echo "$PREFIX v1: $V1"
echo "$PREFIX v2: $V2"

# ── Step 2: Copy v1 and stamp v2's version ──────────────────────────
split_copy_build "$V1" "$V2"
split_stamp_version "$V2_DIR" "$V2"

# ── Step 3: Give the Extension a version marker ─────────────────────
# The stock ext yml ends with an empty `files: []`. Point it at eve_version,
# which step 2 just rewrote, so the Extension's contents now depend on the
# version. makerootfs.sh resolves `source:` relative to its -d directory.
EXT_YML="$V2_DIR/rootfs-${EVE_HV}-ext.yml"
if [ ! -f "$EXT_YML" ]; then
    EXT_YML="images/out/rootfs-${EVE_HV}-ext.yml"
    cp "$EXT_YML" "$V2_DIR/rootfs-${EVE_HV}-ext.yml"
    EXT_YML="$V2_DIR/rootfs-${EVE_HV}-ext.yml"
fi

if ! grep -q '^files: \[\]' "$EXT_YML"; then
    echo "$PREFIX Error: expected 'files: []' in $EXT_YML; the template changed."
    echo "         Add the /etc/eve-ext-release entry by hand and re-run."
    exit 1
fi

echo "$PREFIX Adding /etc/eve-ext-release marker to the Extension..."
sed -i 's|^files: \[\]|files:\n  - path: /etc/eve-ext-release\n    source: eve_version|' "$EXT_YML"

# ── Step 4: Rebuild the Extension and its dm-verity metadata ────────
# The Extension is always erofs. Changing its contents changes the root hash,
# which the Core embeds -- hence the Core rebuild in step 5.
echo "================================================================"
echo "$PREFIX Rebuilding Extension image..."
echo "================================================================"

rm -f "$V2_DIR/rootfs-ext.tar" "$INSTALLER/rootfs-ext.img" \
      "$INSTALLER/rootfs-ext.img.roothash"

./tools/makerootfs.sh tar -y "$EXT_YML" \
    -t "$V2_DIR/rootfs-ext.tar" \
    -d "$INSTALLER" -a "$EVE_ARCH"
./tools/makerootfs.sh imagefromtar \
    -t "$V2_DIR/rootfs-ext.tar" \
    -i "$INSTALLER/rootfs-ext.img" \
    -f erofs -a "$EVE_ARCH"

echo "$PREFIX Generating dm-verity metadata..."
./tools/make-ext-verity.sh "$INSTALLER/rootfs-ext.img" \
    "$INSTALLER/rootfs-ext.img.roothash"
cp "$INSTALLER/rootfs-ext.img.roothash" "$INSTALLER/ext-verity-roothash"

V1_ROOTHASH=$(head -1 "dist/$EVE_ARCH/$V1/installer/ext-verity-roothash")
V2_ROOTHASH=$(head -1 "$INSTALLER/ext-verity-roothash")
echo "$PREFIX v1 roothash: $V1_ROOTHASH"
echo "$PREFIX v2 roothash: $V2_ROOTHASH"
if [ "$V1_ROOTHASH" = "$V2_ROOTHASH" ]; then
    echo "$PREFIX Error: v2's Extension is identical to v1's, so the A/B pairing"
    echo "         would be untestable. The marker file was not applied."
    exit 1
fi

# ── Step 5: Rebuild Core (embeds the new roothash) and package ──────
split_rebuild_core "$V2_DIR"
split_package_oci "$V2_DIR" "$V2"
split_verify_version "$V2"
split_tag_and_push "$V2"

echo ""
echo "================================================================"
echo "$PREFIX Done."
echo "  v1: $V1 (untouched)"
echo "  v2: $V2"
echo "  The Extensions differ, so the mounted one is identifiable:"
echo "    cat /persist/exts/etc/eve-ext-release   →  ${V2}-${EVE_HV}-${EVE_ARCH}"
echo ""
echo "  evetest:"
echo "    EVETEST_INITIAL_EVE_VERSION=$V1 EVETEST_EVE_VERSION=$V2 \\"
echo "      make evetest NAME=TestSplitUpdateSplitToSplit"
echo ""
echo "  eden:"
echo "    EVE_VERSION=$V1 EVE_VERSION_2=$V2 EVE_REGISTRY=$EVE_REGISTRY \\"
echo "      ./eden test ./tests/update_eve_image -v debug \\"
echo "      -r TestEdenScripts/update_split_to_split"
echo "================================================================"
