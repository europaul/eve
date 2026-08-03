# shellcheck shell=bash
#
# Shared helpers for building split-rootfs EVE image variants for testing.
#
# The split-rootfs test scenarios need several images that differ from a stock
# build in one specific way -- a corrupted roothash, a second version to update
# to -- while remaining otherwise identical to it. Every such variant is built
# the same way: copy a good build, change the one thing, rebuild only what
# depends on it, and repackage under a version of its own so the variant and
# the original can coexist.
#
# Sourced by prepare-broken-split-image.sh and prepare-split-v2-image.sh.
# Callers set: EVE_ROOT, EVE_HV, EVE_ARCH, PREFIX.

# split_resolve_registry -- work out where to publish, unless SKIP_PUSH.
split_resolve_registry() {
    if [ -z "$REGISTRY_USER" ]; then
        REGISTRY_USER=$(docker info 2>/dev/null | grep "Username:" | awk '{print $2}')
    fi
    if [ -z "$REGISTRY_USER" ] && [ -z "$SKIP_PUSH" ] && [ -z "$EVE_REGISTRY" ]; then
        echo "$PREFIX Error: Cannot detect Docker Hub username."
        exit 1
    fi
    EVE_REGISTRY="${EVE_REGISTRY:-${REGISTRY_USER}/eve}"
}

# split_obtain_good_build -- echo the version of a good split build to work
# from, building one unless GOOD_VERSION names an existing build.
split_obtain_good_build() {
    local ver
    if [ -n "$GOOD_VERSION" ]; then
        ver="$GOOD_VERSION"
        if [ ! -d "dist/$EVE_ARCH/$ver" ]; then
            echo "$PREFIX Error: GOOD_VERSION=$ver has no dist/$EVE_ARCH/$ver directory." >&2
            exit 1
        fi
        echo "$PREFIX Reusing existing good build: $ver" >&2
    else
        echo "$PREFIX Building good split image first..." >&2
        # shellcheck disable=SC2086
        make UNIVERSAL=1 $KERNEL_OPT eve-split >&2
        ver=$(basename "$(readlink -f "dist/$EVE_ARCH/current")")
    fi
    echo "$ver"
}

# split_copy_build <good_ver> <variant_ver> -- copy a good build into the
# variant's own dist directory.
#
# Everything downstream rewrites files in place, so this must be a real copy;
# hardlinks would corrupt the build it was copied from. Refuses to reuse the
# good version, or to clobber an existing variant directory without FORCE.
split_copy_build() {
    local good_ver="$1" variant_ver="$2"
    local good_dir="dist/$EVE_ARCH/$good_ver"
    local variant_dir="dist/$EVE_ARCH/$variant_ver"

    if [ "$variant_ver" = "$good_ver" ]; then
        echo "$PREFIX Error: the variant version must differ from the good version," >&2
        echo "         otherwise the variant would replace the good image." >&2
        exit 1
    fi
    if [ ! -f "$good_dir/installer/ext-verity-roothash" ]; then
        echo "$PREFIX Error: no ext-verity-roothash in $good_dir/installer." >&2
        echo "         Is $good_ver really a split (HV=uni) build?" >&2
        exit 1
    fi
    if [ -e "$variant_dir" ]; then
        if [ -n "$FORCE" ]; then
            echo "$PREFIX Removing existing $variant_dir (FORCE=1)" >&2
            rm -rf "$variant_dir"
        else
            echo "$PREFIX Error: $variant_dir already exists. Use FORCE=1 to overwrite." >&2
            exit 1
        fi
    fi

    echo "$PREFIX Copying $good_dir to $variant_dir ..." >&2
    cp -a "$good_dir" "$variant_dir"
}

# split_stamp_version <variant_dir> <variant_ver> -- give the variant its own
# version.
#
# installer/eve_version is the only source of the version EVE reports: pkg/eve
# serves it as /bits/eve_version for `docker run <image> version`, and the core
# rootfs yml installs it as /etc/eve-release. It does NOT appear in the core
# yml, so regenerating that yml is unnecessary.
split_stamp_version() {
    local variant_dir="$1" variant_ver="$2"
    echo "${variant_ver}-${EVE_HV}-${EVE_ARCH}" > "$variant_dir/installer/eve_version"
    echo "$PREFIX Stamped version: $(cat "$variant_dir/installer/eve_version")" >&2
}

# split_rebuild_core <variant_dir> -- rebuild the Core tar and image.
#
# Required after anything that changes what the Core embeds, above all
# ext-verity-roothash. `make eve-split` must NOT be used: it regenerates the
# Extension and with it a fresh roothash, silently undoing whatever the caller
# just changed.
split_rebuild_core() {
    local variant_dir="$1"
    local installer="$variant_dir/installer"

    # Prefer the yml snapshotted alongside the build so the package set matches
    # it exactly; fall back to the freshly generated one.
    local core_yml="$variant_dir/rootfs-${EVE_HV}-core.yml"
    if [ ! -f "$core_yml" ]; then
        core_yml="images/out/rootfs-${EVE_HV}-core.yml"
    fi
    echo "$PREFIX Rebuilding Core using $core_yml ..." >&2

    rm -f "$variant_dir/rootfs-core.tar" "$installer/rootfs-core.img"
    ./tools/makerootfs.sh tar -y "$core_yml" \
        -t "$variant_dir/rootfs-core.tar" \
        -d "$installer" -a "$EVE_ARCH" >&2
    ./tools/makerootfs.sh imagefromtar \
        -t "$variant_dir/rootfs-core.tar" \
        -i "$installer/rootfs-core.img" \
        -f squash -a "$EVE_ARCH" >&2
}

# split_package_oci <variant_dir> <variant_ver> -- build the OCI image.
split_package_oci() {
    local variant_dir="$1" variant_ver="$2"
    local installer="$variant_dir/installer"

    echo "$PREFIX Packaging OCI ..." >&2
    cp -f "$installer/rootfs-core.img" "$installer/rootfs.img"

    # The copied build already carries a Dockerfile with the Extension label
    # substituted; regenerate only if it is missing or unlabelled.
    if ! grep -q 'org.lfedge.eci.artifact.disk-0' "$variant_dir/Dockerfile" 2>/dev/null; then
        echo "$PREFIX Regenerating Dockerfile ..." >&2
        cp images/out/*.yml "$variant_dir/"
        cp -f pkg/eve/runme.sh "$variant_dir/runme.sh"
        cp -f pkg/eve/build.yml "$variant_dir/build.yml"
        DOCKER_ARCH_TAG=$EVE_ARCH KERNEL_TAG="${KERNEL_TAG}" PLATFORM=generic \
            ./tools/parse-pkgs.sh pkg/eve/Dockerfile.in > "$variant_dir/Dockerfile"
        sed -i 's|#SPLIT_ROOTFS_LABEL#|LABEL org.lfedge.eci.artifact.disk-0="/bits/rootfs-ext.img"|' \
            "$variant_dir/Dockerfile"
    fi

    build-tools/bin/linuxkit pkg build --platforms "linux/$EVE_ARCH" \
        --hash-path "$EVE_ROOT" \
        --hash "${variant_ver}-${EVE_HV}" \
        --docker --force \
        "$variant_dir" >&2

    rm -f "$installer/rootfs.img"
}

# split_verify_version <variant_ver> -- fail unless the built image reports the
# version it was stamped with. A variant that silently kept the good version
# would send whoever runs the test hunting in entirely the wrong place.
split_verify_version() {
    local variant_ver="$1"
    local tag="${variant_ver}-${EVE_HV}-${EVE_ARCH}"
    local reported
    reported=$(docker run --rm "lfedge/eve:$tag" version 2>/dev/null | tr -d '\r\n')
    echo "$PREFIX Image reports: $reported" >&2
    if [ "$reported" != "$tag" ]; then
        echo "$PREFIX Error: image reports '$reported', expected '$tag'." >&2
        exit 1
    fi
}

# split_tag_and_push <variant_ver> -- publish, plus the HV_TAG alias.
#
# evetest resolves images as <repo>:<version>-<hv>-<arch> and knows no "uni"
# hypervisor, so it needs an alias under the hypervisor it will ask for.
split_tag_and_push() {
    local variant_ver="$1"
    local tag="${variant_ver}-${EVE_HV}-${EVE_ARCH}"
    local tags="$tag"

    if [ -n "$HV_TAG" ] && [ "$HV_TAG" != "$EVE_HV" ]; then
        local alias_tag="${variant_ver}-${HV_TAG}-${EVE_ARCH}"
        docker tag "lfedge/eve:$tag" "lfedge/eve:$alias_tag"
        tags="$tags $alias_tag"
        echo "$PREFIX Tagged alias: lfedge/eve:$alias_tag" >&2
    fi

    if [ -z "$SKIP_PUSH" ]; then
        for t in $tags; do
            echo "$PREFIX Pushing to $EVE_REGISTRY:$t" >&2
            docker tag "lfedge/eve:$t" "$EVE_REGISTRY:$t"
            docker push "$EVE_REGISTRY:$t" >&2
        done
    fi
}
