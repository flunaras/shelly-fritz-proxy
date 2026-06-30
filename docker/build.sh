#!/usr/bin/env bash
# build.sh — Build shelly-fritz-proxy inside a Docker container.
#
# Usage:
#   ./docker/build.sh [--distro <alias>|all] [--build-type Release|Debug]
#
# Available distro aliases (kept identical to the sibling fritzhome-cache
# project so a shared CI matrix can drive both):
#
#   opensuse-tumbleweed-x86_64   openSUSE Tumbleweed, x86_64
#   opensuse-tumbleweed-aarch64  openSUSE Tumbleweed, aarch64
#   debian-12-x86_64             Debian 12 (bookworm), x86_64
#   raspbian-bookworm-aarch64    Raspberry Pi OS bookworm, aarch64
#   raspbian-bookworm-armhf      Raspberry Pi OS bookworm, armhf / 32-bit
#   raspbian-bullseye-armhf      Raspberry Pi OS bullseye, armhf / 32-bit
#   all                          Build all of the above (default)
#
# Build type options (default: Release):
#   Release  Optimized, stripped binary (suitable for deployment).
#   Debug    Symbols + race detector available via go test.
#
# Unlike fritzhome-cache, this project is written in Go, which means:
#   - There is ONE builder image, not one per distro. Go's cross-compiler
#     produces self-contained ELF binaries (CGO_ENABLED=0) that run on
#     glibc-based and musl-based distros alike. Per-distro packaging is
#     done with nfpm, which emits both .deb and .rpm from the same
#     manifest in data/nfpm.yaml.
#   - The "distro" choice therefore only controls the output filename
#     convention and the package format (.deb for Debian/Raspberry Pi
#     targets, .rpm for openSUSE targets).
#
# The script:
#   1. Builds (or reuses) the docker image docker/Dockerfile.builder
#   2. Runs go build inside the container as the current host user
#      (--user $(id -u):$(id -g)) so output files are owned by you
#   3. Packages the binary with nfpm and copies the result into:
#
#        build/<family>/<distro>/<arch>/   ← intermediate (stripped binary)
#        out/<family>/<distro>/<arch>/     ← final binary + package
#
#      where <family> is "opensuse", "debian", or "raspbian", <distro>
#      is the short distro name (e.g. "tumbleweed", "12", "bookworm"),
#      and <arch> matches the convention used by that family.

set -euo pipefail

# ── Resolve project root (directory that contains go.mod) ─────────────────────
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

# ── Defaults ──────────────────────────────────────────────────────────────────
DISTRO="all"
BUILD_TYPE="Release"

# ── Argument parsing ──────────────────────────────────────────────────────────
while [[ $# -gt 0 ]]; do
    case "$1" in
        --distro)        DISTRO="$2"; shift 2 ;;
        --distro=*)      DISTRO="${1#*=}"; shift ;;
        --build-type)    BUILD_TYPE="$2"; shift 2 ;;
        --build-type=*)  BUILD_TYPE="${1#*=}"; shift ;;
        -h|--help)
            sed -n '/^# Usage:/,/^[^#]/{ /^[^#]/d; s/^# \{0,2\}//; p }' "$0"
            exit 0
            ;;
        *)
            echo "ERROR: Unknown argument: $1" >&2
            exit 1
            ;;
    esac
done

# Validate distro choice.
case "$DISTRO" in
    opensuse-tumbleweed-x86_64|\
    opensuse-tumbleweed-aarch64|\
    debian-12-x86_64|\
    raspbian-bookworm-aarch64|\
    raspbian-bookworm-armhf|\
    raspbian-bullseye-armhf|\
    all) ;;
    *)
        echo "ERROR: --distro must be one of:" >&2
        echo "  opensuse-tumbleweed-x86_64, opensuse-tumbleweed-aarch64," >&2
        echo "  debian-12-x86_64, raspbian-bookworm-aarch64," >&2
        echo "  raspbian-bookworm-armhf, raspbian-bullseye-armhf, all" >&2
        exit 1
        ;;
esac

# Normalize build type to canonical form (case-insensitive input).
BUILD_TYPE_LOWER="${BUILD_TYPE,,}"
case "$BUILD_TYPE_LOWER" in
    release) BUILD_TYPE="Release" ;;
    debug)   BUILD_TYPE="Debug"   ;;
    *)
        echo "ERROR: --build-type must be Release or Debug (got: $BUILD_TYPE)" >&2
        exit 1
        ;;
esac

# ── Per-distro mapping ────────────────────────────────────────────────────────
family_for_distro() {
    case "$1" in
        debian-*)   echo "debian"   ;;
        raspbian-*) echo "raspbian" ;;
        *)          echo "opensuse" ;;
    esac
}

# GOOS is always linux; this returns GOARCH (and GOARM where applicable).
goarch_for_distro() {
    case "$1" in
        opensuse-tumbleweed-x86_64|debian-12-x86_64) echo "amd64::" ;;
        opensuse-tumbleweed-aarch64|\
        raspbian-bookworm-aarch64)                   echo "arm64::" ;;
        raspbian-bookworm-armhf|\
        raspbian-bullseye-armhf)                     echo "arm::7"  ;;
    esac
}

# Arch label used in the out/ directory and in package filenames.
arch_for_distro() {
    case "$1" in
        opensuse-tumbleweed-x86_64) echo "x86_64"  ;;
        opensuse-tumbleweed-aarch64) echo "aarch64" ;;
        debian-12-x86_64)           echo "amd64"   ;;
        raspbian-bookworm-aarch64)  echo "arm64"   ;;
        raspbian-bookworm-armhf|\
        raspbian-bullseye-armhf)    echo "armhf"   ;;
    esac
}

# The nfpm "arch" field for each target. nfpm uses Go's GOARCH vocabulary.
nfpm_arch_for_distro() {
    case "$1" in
        opensuse-tumbleweed-x86_64|debian-12-x86_64) echo "amd64" ;;
        opensuse-tumbleweed-aarch64|\
        raspbian-bookworm-aarch64)                   echo "arm64" ;;
        raspbian-bookworm-armhf|\
        raspbian-bullseye-armhf)                     echo "arm7"  ;;
    esac
}

# Distro directory name used in build/ and out/ paths.
dir_name_for_distro() {
    case "$1" in
        opensuse-tumbleweed-*) echo "tumbleweed" ;;
        debian-12-x86_64)      echo "12"         ;;
        raspbian-bookworm-*)   echo "bookworm"   ;;
        raspbian-bullseye-*)   echo "bullseye"   ;;
    esac
}

# Package format for each distro.
packager_for_distro() {
    case "$1" in
        debian-*|raspbian-*) echo "deb" ;;
        opensuse-*)          echo "rpm" ;;
    esac
}

# ── Image build (once per script invocation) ──────────────────────────────────
IMAGE="shelly-fritz-proxy-builder"

build_image_if_needed() {
    if docker image inspect "${IMAGE}" >/dev/null 2>&1; then
        return
    fi
    echo "[image] Building Docker image '${IMAGE}' (one-time)..."
    docker build \
        --pull \
        --file "${SCRIPT_DIR}/Dockerfile.builder" \
        --tag  "${IMAGE}" \
        "${PROJECT_ROOT}"
    echo "[image] Ready."
}

# ── Per-distro build ──────────────────────────────────────────────────────────
build_for_distro() {
    local distro="$1"

    # Project version — read from the constant in CMakeLists-equivalent
    # location. For Go we keep it in cmd/shelly-fritz-proxy/version.txt
    # so the build script doesn't have to grep Go source.
    local version
    version="$(cat "${PROJECT_ROOT}/VERSION" 2>/dev/null || echo "0.1.0")"
    local release="1"

    local family arch nfpm_arch dir_name packager
    family="$(family_for_distro "${distro}")"
    arch="$(arch_for_distro "${distro}")"
    nfpm_arch="$(nfpm_arch_for_distro "${distro}")"
    dir_name="$(dir_name_for_distro "${distro}")"
    packager="$(packager_for_distro "${distro}")"

    # Split GOARCH and GOARM out of the encoded string "arch::arm".
    local goarch_spec goarch goarm
    goarch_spec="$(goarch_for_distro "${distro}")"
    goarch="${goarch_spec%%::*}"
    goarm="${goarch_spec##*::}"

    # ── Build / package dirs ──────────────────────────────────────────────
    local build_dir="${PROJECT_ROOT}/build/${family}/${dir_name}/${arch}"
    local out_dir="${PROJECT_ROOT}/out/${family}/${dir_name}/${arch}"
    mkdir -p "${build_dir}" "${out_dir}"

    # Strip flags for Release builds (smaller binary, no debug symbols).
    local ldflags=""
    case "${BUILD_TYPE}" in
        Release) ldflags='-s -w' ;;
    esac

    echo ""
    echo "════════════════════════════════════════════════════════════"
    echo "  Building for: ${distro}  (${version}  ${arch})"
    echo "════════════════════════════════════════════════════════════"
    echo "[setup] family=${family}  dir_name=${dir_name}  packager=${packager}"
    echo "[setup] GOOS=linux GOARCH=${goarch}${goarm:+ GOARM=${goarm}}  nfpm_arch=${nfpm_arch}"

    # ── Step 1: cross-compile ─────────────────────────────────────────────
    echo "[1/2] go build → ${build_dir}/shelly-fritz-proxy"
    docker run --rm \
        --user "$(id -u):$(id -g)" \
        -v "${PROJECT_ROOT}:/src:ro" \
        -v "${build_dir}:/build" \
        -v "/etc/passwd:/etc/passwd:ro" \
        -v "/etc/group:/etc/group:ro" \
        -e "HOME=/tmp" \
        -e "GOOS=linux" \
        -e "GOARCH=${goarch}" \
        -e "GOARM=${goarm}" \
        -e "GOCACHE=/build/.gocache" \
        -e "GOMODCACHE=/build/.gomod" \
        -e "CGO_ENABLED=0" \
        "${IMAGE}" \
        bash -c "
            set -euo pipefail
            cd /src
            go build -trimpath -ldflags='${ldflags}' \
                -o /build/shelly-fritz-proxy \
                ./cmd/shelly-fritz-proxy
            file /build/shelly-fritz-proxy
        "

    # ── Step 2: nfpm package ──────────────────────────────────────────────
    # nfpm's env-var substitution is fiddly: it only expands ${VAR} in a
    # subset of fields and silently leaves them literal in `contents.src`.
    # To stay robust we render a per-target manifest with simple envsubst
    # before invoking nfpm.
    echo "[2/2] nfpm pkg --packager ${packager} → ${out_dir}/"
    docker run --rm \
        --user "$(id -u):$(id -g)" \
        -v "${PROJECT_ROOT}:/src:ro" \
        -v "${build_dir}:/build" \
        -v "${out_dir}:/out" \
        -v "/etc/passwd:/etc/passwd:ro" \
        -v "/etc/group:/etc/group:ro" \
        -e "VERSION=${version}" \
        -e "RELEASE=${release}" \
        -e "NFPM_ARCH=${nfpm_arch}" \
        -e "BINARY=/build/shelly-fritz-proxy" \
        -e "MAINTAINER=flunaras <flunaras@users.noreply.github.com>" \
        -e "VENDOR=flunaras" \
        "${IMAGE}" \
        bash -c '
            set -euo pipefail
            cd /src
            # Render the manifest, expanding only the four placeholders we
            # rely on. We use a small sed pipeline rather than envsubst to
            # avoid pulling in gettext-base just for this.
            manifest=$(mktemp)
            sed \
                -e "s|\${VERSION}|$VERSION|g" \
                -e "s|\${RELEASE}|$RELEASE|g" \
                -e "s|\${NFPM_ARCH}|$NFPM_ARCH|g" \
                -e "s|\${BINARY}|$BINARY|g" \
                -e "s|\${MAINTAINER}|$MAINTAINER|g" \
                -e "s|\${VENDOR}|$VENDOR|g" \
                data/nfpm.yaml > "$manifest"
            nfpm pkg --config "$manifest" --packager '"${packager}"' --target /out/
            rm -f "$manifest"
            ls -la /out/
        '

    # ── Step 3: copy stripped binary alongside the package ───────────────
    cp "${build_dir}/shelly-fritz-proxy" "${out_dir}/shelly-fritz-proxy"

    echo "[done] Binary : ${out_dir}/shelly-fritz-proxy"
    echo "[done] Package: $(ls -1 "${out_dir}"/*.${packager} 2>/dev/null | head -1 || echo '(missing!)')"
}

# ── Dispatch ──────────────────────────────────────────────────────────────────
build_image_if_needed

if [[ "$DISTRO" == "all" ]]; then
    build_for_distro "opensuse-tumbleweed-x86_64"
    build_for_distro "opensuse-tumbleweed-aarch64"
    build_for_distro "debian-12-x86_64"
    build_for_distro "raspbian-bookworm-aarch64"
    build_for_distro "raspbian-bookworm-armhf"
    build_for_distro "raspbian-bullseye-armhf"
else
    build_for_distro "$DISTRO"
fi

echo ""
echo "Done."
