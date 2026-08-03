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

# Auto-detect Docker Hub username
if [ -z "$REGISTRY_USER" ]; then
    REGISTRY_USER=$(docker info 2>/dev/null | grep "Username:" | awk '{print $2}')
fi
if [ -z "$REGISTRY_USER" ] && [ -z "$SKIP_PUSH" ] && [ -z "$EVE_REGISTRY" ]; then
    echo "$PREFIX Error: Cannot detect Docker Hub username."
    exit 1
fi
EVE_REGISTRY="${EVE_REGISTRY:-${REGISTRY_USER}/eve}"

KERNEL_OPT=""
if [ -n "$KERNEL_TAG" ]; then
    KERNEL_OPT="KERNEL_TAG=$KERNEL_TAG"
fi

# ── Step 1: Obtain a good split image (ext + core + OCI) ────────────
if [ -n "$GOOD_VERSION" ]; then
    VER="$GOOD_VERSION"
    if [ ! -d "dist/$EVE_ARCH/$VER" ]; then
        echo "$PREFIX Error: GOOD_VERSION=$VER has no dist/$EVE_ARCH/$VER directory."
        exit 1
    fi
    echo "$PREFIX Reusing existing good build: $VER"
else
    echo "================================================================"
    echo "$PREFIX Building good split image first..."
    echo "================================================================"
    # shellcheck disable=SC2086
    make UNIVERSAL=1 $KERNEL_OPT eve-split
    VER=$(basename "$(readlink -f "dist/$EVE_ARCH/current")")
fi

GOOD_DIR="dist/$EVE_ARCH/$VER"
BROKEN_VER="${BROKEN_VERSION:-${VER}-broken}"
BROKEN_DIR="dist/$EVE_ARCH/$BROKEN_VER"
INSTALLER="$BROKEN_DIR/installer"

echo "$PREFIX Good version:   $VER"
echo "$PREFIX Broken version: $BROKEN_VER"

if [ "$BROKEN_VER" = "$VER" ]; then
    echo "$PREFIX Error: BROKEN_VERSION must differ from the good version,"
    echo "         otherwise the broken image would replace the good one."
    exit 1
fi
if [ ! -f "$GOOD_DIR/installer/ext-verity-roothash" ]; then
    echo "$PREFIX Error: ext-verity-roothash not found in $GOOD_DIR/installer."
    echo "         Is $VER really a split (HV=uni) build?"
    exit 1
fi
if [ -e "$BROKEN_DIR" ]; then
    if [ -n "$FORCE" ]; then
        echo "$PREFIX Removing existing $BROKEN_DIR (FORCE=1)"
        rm -rf "$BROKEN_DIR"
    else
        echo "$PREFIX Error: $BROKEN_DIR already exists. Use FORCE=1 to overwrite."
        exit 1
    fi
fi

# ── Step 2: Copy the good build ─────────────────────────────────────
# Everything below rewrites files in place, so this must be a real copy --
# hardlinks (cp -al) would corrupt the good image.
echo "================================================================"
echo "$PREFIX Copying $GOOD_DIR to $BROKEN_DIR ..."
echo "================================================================"
cp -a "$GOOD_DIR" "$BROKEN_DIR"

# ── Step 3: Stamp the broken version ────────────────────────────────
# This single file is the only source of the version EVE reports: pkg/eve
# serves it as /bits/eve_version for `docker run <image> version`, and the
# core rootfs yml installs it as /etc/eve-release. The version does not
# appear in the yml itself, so no yml regeneration is needed.
echo "${BROKEN_VER}-${EVE_HV}-${EVE_ARCH}" > "$INSTALLER/eve_version"
echo "$PREFIX Stamped version: $(cat "$INSTALLER/eve_version")"

# ── Step 4: Corrupt the roothash ────────────────────────────────────
echo "$PREFIX Original roothash: $(head -1 "$INSTALLER/ext-verity-roothash")"

# Flip the first hex char of the hash; keep the second line (offset) intact.
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

# ── Step 5: Rebuild ONLY Core tar + img with the corrupted roothash ──
# `make eve-split` must NOT be used here: it regenerates the Extension and
# with it a fresh ext-verity-roothash, silently undoing the corruption.
echo "================================================================"
echo "$PREFIX Rebuilding Core with corrupted roothash..."
echo "================================================================"

# Prefer the yml snapshotted alongside the good build so the package set
# matches it exactly; fall back to the freshly generated one.
CORE_YML="$BROKEN_DIR/rootfs-${EVE_HV}-core.yml"
if [ ! -f "$CORE_YML" ]; then
    CORE_YML="images/out/rootfs-${EVE_HV}-core.yml"
fi
echo "$PREFIX Using core yml: $CORE_YML"

rm -f "$BROKEN_DIR/rootfs-core.tar" "$INSTALLER/rootfs-core.img"

echo "$PREFIX Building core tar..."
./tools/makerootfs.sh tar -y "$CORE_YML" \
    -t "$BROKEN_DIR/rootfs-core.tar" \
    -d "$INSTALLER" -a "$EVE_ARCH"

echo "$PREFIX Building core img..."
./tools/makerootfs.sh imagefromtar \
    -t "$BROKEN_DIR/rootfs-core.tar" \
    -i "$INSTALLER/rootfs-core.img" \
    -f squash -a "$EVE_ARCH"

# ── Step 6: Package OCI ─────────────────────────────────────────────
echo "$PREFIX Packaging OCI..."
cp -f "$INSTALLER/rootfs-core.img" "$INSTALLER/rootfs.img"

# The copied build already carries a Dockerfile with the Extension label
# substituted; regenerate only if it is missing or unlabelled.
if ! grep -q 'org.lfedge.eci.artifact.disk-0' "$BROKEN_DIR/Dockerfile" 2>/dev/null; then
    echo "$PREFIX Regenerating Dockerfile..."
    cp images/out/*.yml "$BROKEN_DIR/"
    cp -f pkg/eve/runme.sh "$BROKEN_DIR/runme.sh"
    cp -f pkg/eve/build.yml "$BROKEN_DIR/build.yml"
    DOCKER_ARCH_TAG=$EVE_ARCH KERNEL_TAG="${KERNEL_TAG}" PLATFORM=generic \
        ./tools/parse-pkgs.sh pkg/eve/Dockerfile.in > "$BROKEN_DIR/Dockerfile"
    sed -i 's|#SPLIT_ROOTFS_LABEL#|LABEL org.lfedge.eci.artifact.disk-0="/bits/rootfs-ext.img"|' \
        "$BROKEN_DIR/Dockerfile"
fi

LINUXKIT="build-tools/bin/linuxkit"
$LINUXKIT pkg build --platforms "linux/$EVE_ARCH" \
    --hash-path "$EVE_ROOT" \
    --hash "${BROKEN_VER}-${EVE_HV}" \
    --docker --force \
    "$BROKEN_DIR"

rm -f "$INSTALLER/rootfs.img"

TAG="${BROKEN_VER}-${EVE_HV}-${EVE_ARCH}"

# ── Step 7: Verify the good image was left alone ────────────────────
echo "================================================================"
echo "$PREFIX Verifying..."
echo "================================================================"
BROKEN_REPORTS=$(docker run --rm "lfedge/eve:$TAG" version 2>/dev/null | tr -d '\r\n')
echo "$PREFIX Broken image reports: $BROKEN_REPORTS"
if [ "$BROKEN_REPORTS" != "${BROKEN_VER}-${EVE_HV}-${EVE_ARCH}" ]; then
    echo "$PREFIX Error: broken image reports an unexpected version."
    exit 1
fi
GOOD_ROOTHASH=$(head -1 "$GOOD_DIR/installer/ext-verity-roothash")
BROKEN_ROOTHASH=$(head -1 "$INSTALLER/ext-verity-roothash")
echo "$PREFIX Good roothash:   $GOOD_ROOTHASH"
echo "$PREFIX Broken roothash: $BROKEN_ROOTHASH"
if [ "$GOOD_ROOTHASH" = "$BROKEN_ROOTHASH" ]; then
    echo "$PREFIX Error: roothash was not corrupted."
    exit 1
fi

# ── Step 8: Tag and push ────────────────────────────────────────────
# evetest resolves images as <repo>:<version>-<hv>-<arch> and knows no "uni"
# hypervisor, so publish an alias under the hypervisor it will ask for.
TAGS="$TAG"
if [ -n "$HV_TAG" ] && [ "$HV_TAG" != "$EVE_HV" ]; then
    ALIAS="${BROKEN_VER}-${HV_TAG}-${EVE_ARCH}"
    docker tag "lfedge/eve:$TAG" "lfedge/eve:$ALIAS"
    TAGS="$TAGS $ALIAS"
    echo "$PREFIX Tagged alias: lfedge/eve:$ALIAS"
fi

echo "$PREFIX Built broken image: lfedge/eve:$TAG"

if [ -z "$SKIP_PUSH" ]; then
    for t in $TAGS; do
        echo "$PREFIX Pushing to $EVE_REGISTRY:$t"
        docker tag "lfedge/eve:$t" "$EVE_REGISTRY:$t"
        docker push "$EVE_REGISTRY:$t"
    done
fi

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
